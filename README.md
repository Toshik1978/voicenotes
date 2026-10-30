[![Lint, Test & Build](https://github.com/Toshik1978/voicenotes/actions/workflows/ci.yml/badge.svg)](https://github.com/Toshik1978/voicenotes/actions/workflows/ci.yml)
[![Tests](https://img.shields.io/endpoint?url=https%3A%2F%2Fgist.githubusercontent.com%2FToshik1978%2F7fd599021f75cc937dde5fedb2ab47cf%2Fraw%2Ftests.json)](https://github.com/Toshik1978/voicenotes/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fgist.githubusercontent.com%2FToshik1978%2F7fd599021f75cc937dde5fedb2ab47cf%2Fraw%2Fcoverage.json)](https://github.com/Toshik1978/voicenotes/actions/workflows/ci.yml)

# VoiceNotes

VoiceNotes is a self-hosted Go service that turns Telegram voice captures into notes in
Memos. Send a voice message (or text) to your private bot. The service
transcribes the audio, optionally cleans it up with an LLM, saves a note with the audio attached
and replies with a link. Each morning it sends a digest of the notes that are still tagged
`#inbox`.

## How it works

1. The bot receives a voice or text message from an allowlisted Telegram user (long polling, no
   public ingress).
2. The capture is stored durably in a local SQLite file, with the audio on disk, before anything
   else happens.
3. Speech-to-text: the audio goes to an OpenAI-compatible `/audio/transcriptions` endpoint.
4. Optional LLM step (OpenAI-compatible `/chat/completions`): translate to the target language,
   tidy punctuation, suggest tags and spot action items (checklist plus `#todo`).
5. A Memos note is created (private, tagged `#inbox`) and the audio is attached.
6. The bot edits its silent reply to `✓ saved` with a link to the note. Local audio is deleted
   only after the note is saved. At startup, audio files older than an hour that no capture
   references (left by a crash mid-write) are deleted.

If Telegram, the speech-to-text service, the LLM or Memos is down, the process keeps running, the
capture waits and is retried with backoff, and health reports the degradation. A capture is never
dropped: a permanent failure produces a placeholder note with the audio only. If even the audio
could not be kept (for example Telegram refused the download), the reply says so and asks you to
send it again.

## Quick start

Create a `.env` file from the template and fill in real values (the file is git-ignored):

```sh
cp .env.dist .env
```

Run the image. Its default command is `run` (also what the binary does with no arguments); `version` and `health` are the other subcommands:

```sh
docker run -d --name voicenotes --restart unless-stopped \
  --stop-timeout 90 \
  --env-file .env \
  -v voicenotes-data:/data \
  -p 8080:8080 \
  ghcr.io/toshik1978/voicenotes:latest
```

Tags are `<version>` (for example `0.1.0`) and `latest`, both published only by releases (see
[Releases](#releases)). Images are built for `linux/amd64` and `linux/arm64`. The container runs as the distroless nonroot user `65532:65532`.

Give the container a 90 second stop grace period (`--stop-timeout 90`, or `stop_grace_period:
90s` in Docker Compose); the Dockerfile cannot set it. Docker's default of 10 seconds is too short:
on SIGTERM the service finishes what is in flight before it exits. A voice message still being
downloaded from Telegram can take up to 60 seconds (the Bot API HTTP timeout) before its row is
stored, and recording a delivered morning digest is retried for up to 30 seconds so the digest is
not sent twice after the restart. A SIGKILL inside that window can lose a capture with no reply,
which Telegram never redelivers, or repeat the digest. 90 seconds covers the longest of these with
margin.

```yaml
services:
  voicenotes:
    image: ghcr.io/toshik1978/voicenotes:latest
    restart: unless-stopped
    stop_grace_period: 90s
    env_file: .env
    volumes:
      - voicenotes-data:/data
volumes:
  voicenotes-data:
```

Port 8080 serves health only and does not need to be published; omit `-p` unless a monitor outside
the container reads it.

For a host bind mount instead of a named volume, the directory must be writable by the container
user:

```sh
mkdir -p ./data && sudo chown 65532:65532 ./data
```

Without that, startup fails because the store cannot be opened. A named volume needs no extra
step. A startup configuration error exits with `config: <VAR>: <problem>`; the value is never
printed.

Subcommands of the binary:

| Command | Purpose |
|---|---|
| `voicenotes [run]` | Run the service (the default when no command is given) |
| `voicenotes health [--strict]` | Query the local health endpoint (see [Health](#health)) |
| `voicenotes version` | Print version, commit and build time |

## Configuration

All settings are environment variables. Secrets come only from the environment, are never logged,
and should not appear on the command line (use `--env-file` or `-e NAME` without a value).

### Telegram

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | yes | | yes | Token of the dedicated VoiceNotes bot |
| `TELEGRAM_ALLOWED_USERS` | yes | | no | Comma-separated Telegram user IDs allowed to capture |
| `DIGEST_CHAT_ID` | no | first of `TELEGRAM_ALLOWED_USERS` | no | Chat that receives the morning digest |

### Speech-to-text (OpenAI-compatible)

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `STT_BASE_URL` | yes | | no | Base URL including `/v1`, for example `http://stt.example.org:8000/v1` |
| `STT_API_KEY` | no | empty (no auth header) | yes | Bearer key |
| `STT_MODEL` | yes | | no | Model id passed as `model` |
| `STT_RESPONSE_FORMAT` | no | `verbose_json` | no | `verbose_json` (reports the detected language) or `json` (for models that reject `verbose_json`) |
| `STT_TIMEOUT` | no | `120s` | no | Per-request timeout |

### Languages

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `SOURCE_LANGUAGES` | no | empty (auto-detect) | no | Comma-separated ISO-639-1 codes the owner speaks, first is preferred |
| `TARGET_LANGUAGE` | no | `en` | no | ISO-639-1 code that notes are translated into |

### LLM (optional, OpenAI-compatible chat)

The LLM step is enabled only when `LLM_BASE_URL` is set.

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `LLM_BASE_URL` | no | | no | Base URL including `/v1`, for example a LiteLLM gateway |
| `LLM_API_KEY` | no | empty | yes | Bearer key |
| `LLM_MODEL` | if LLM is on | | no | Model name or gateway alias |
| `LLM_TRANSLATE` | no | `true` | no | Translate to `TARGET_LANGUAGE` |
| `LLM_TIDY` | no | `true` | no | Tidy punctuation and suggest tags |
| `LLM_TASKS` | no | `true` | no | Spot action items: checklist plus the todo tag |
| `LLM_DIGEST_SUMMARY` | no | `true` | no | One-line summaries in the digest |
| `LLM_MAX_TAGS` | no | `3` | no | Upper bound on suggested tags; extra suggestions are dropped, `0` disables tags |
| `LLM_TIMEOUT` | no | `60s` | no | Per-request timeout |
| `LLM_TEMPERATURE` | no | unset | no | Sampling temperature `0`–`2`; unset sends none, so the model uses its default (some models accept only that) |

### Memos

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `MEMOS_URL` | yes | | no | Internal base URL the service calls, for example `https://memos.example.org` |
| `MEMOS_TOKEN` | yes | | yes | Personal Access Token (`memos_pat_...`) of the owner account, no expiry |
| `MEMOS_PUBLIC_URL` | no | `MEMOS_URL` | no | Base URL used in links shown to the owner |
| `MEMOS_API_PREFIX` | no | `auto` | no | `auto` probes `/api` (26.10+) then `/api/v1` (v0.31); or set `/api/v1` or `/api` explicitly |
| `MEMOS_CONTENT_LIMIT` | no | `8192` | no | Max note content bytes; match the Memos instance setting |
| `INBOX_TAG` | no | `inbox` | no | Tag for unreviewed notes |
| `TODO_TAG` | no | `todo` | no | Tag for notes with action items |

### Digest

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `DIGEST_ENABLED` | no | `true` | no | Turn the morning digest on or off |
| `DIGEST_TIME` | no | `07:30` | no | Local time `HH:MM` in `TZ` |
| `DIGEST_CATCHUP` | no | `3h` | no | Send a missed digest if started within this window |
| `DIGEST_MAX_NOTES` | no | `30` | no | Lines listed before "...and N more" |

### Runtime

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `DATA_DIR` | no | `/data` | no | SQLite file and queued audio; mount a persistent volume here |
| `HTTP_ADDR` | no | `:8080` | no | Health listener; must listen on loopback (`:port` or `0.0.0.0:port`) for `voicenotes health` |
| `HEALTH_PENDING_THRESHOLD` | no | `30m` | no | Oldest unfinished capture older than this makes health `degraded` |
| `RETRY_MAX_INTERVAL` | no | `5m` | no | Backoff cap for dependency retries |
| `LOG_LEVEL` | no | `info` | no | `debug`, `info`, `warn` or `error` |
| `TZ` | no | unset (UTC) | no | Standard IANA zone, for example `Europe/Berlin` (tzdata is embedded); the digest time and the dates in attachment names use it. It must be an IANA zone name; a path such as `:/etc/localtime` is rejected. An unknown zone fails startup. |

## External calls and provisioning

VoiceNotes makes exactly these calls. Every request sends `User-Agent: VoiceNotes/<version>`.
Outbound connections go to the Telegram API (HTTPS), `STT_BASE_URL`, `LLM_BASE_URL` and
`MEMOS_URL`.

### Speech-to-text

`POST {STT_BASE_URL}/audio/transcriptions`, multipart with `file` (Telegram audio as-is, Ogg/Opus,
no transcoding), `model`, `response_format`, `temperature=0` and, only when one language is
forced, `language`. `Authorization: Bearer` is sent only when `STT_API_KEY` is set.

- speaches with any CTranslate2 Whisper model, for example `Systran/faster-whisper-large-v3`: use
  `STT_RESPONSE_FORMAT=verbose_json`. Download the model before first use; speaches answers 404
  for models that are not installed, and VoiceNotes reports that as degraded health.
- OpenAI `whisper-1`: `verbose_json`.
- OpenAI `gpt-4o-transcribe` and `gpt-4o-mini-transcribe`: set `STT_RESPONSE_FORMAT=json`. These
  return no detected language, so the re-transcribe rule is skipped.

### LLM

`POST {LLM_BASE_URL}/chat/completions` with no provider-specific fields; the model is instructed
to answer with one JSON object. Provision one model alias through any OpenAI-compatible endpoint,
for example a LiteLLM model named `voicenotes`, and set `LLM_MODEL` to it. A small, fast model is
enough: about one call per capture plus one per day, a few hundred tokens each. No `temperature`
is sent unless `LLM_TEMPERATURE` is set, because some models (Claude Haiku, for one) accept only
their default. If the output fails validation, the note is saved without LLM output (`✓ saved (no
AI)`).

### Memos

Targets Memos v0.31.0 (`/api/v1`) and 26.10+ (`/api`). Requests send
`Authorization: Bearer {MEMOS_TOKEN}`.

- Create a Personal Access Token as the owner under Settings, Access Tokens, with expiry "Never".
  A PAT survives the 26.10 upgrade.
- The attachment upload limit must be at least 20 MB (the default is 30 to 32 MB).
- After moving to 26.10, re-apply any custom content or upload limits; they are not migrated.
- Calls: `GET {P}/auth/me` (resolve prefix and owner), `POST {P}/memos?memoId=...` (create; the
  deterministic id makes retries idempotent), `POST {P}/attachments` (audio) and
  `GET {P}/memos?filter=...` (digest inbox listing). Tags are written inline in the note content.

### Error handling

| Class | Statuses and conditions | Behavior |
|---|---|---|
| Temporary | connection errors, timeouts, 5xx, 408, 429 | Retried with backoff |
| Misconfiguration | 401, 403, 404 | Retried, health `degraded`, never a placeholder |
| Permanent | 400, 413, 415, 422 | Placeholder note with the audio only |

### Telegram

Long polling with `getUpdates` (`allowed_updates=["message"]`), `getFile` plus download (bots are
limited to 20 MB files), `sendMessage` and `editMessageText`. No webhook.

## Telegram messages

Every capture reply and edit is silent (`disable_notification`). Only the morning digest makes a
sound. Messages from users not in `TELEGRAM_ALLOWED_USERS` get no reply.

| Situation | Reply |
|---|---|
| Accepted | `⏳` |
| Waiting on a dependency | `⏳ queued — <speech-to-text\|notes\|Telegram> unavailable, will retry` |
| Done | `✓ saved · open` (with a link) |
| Done with tasks | `✓ saved · 2 tasks · open` |
| LLM skipped | `✓ saved (no AI) · open` |
| Permanent failure | `⚠ saved audio only — <reason> · open` |
| Permanent failure, no audio kept | `⚠ audio not kept — <reason> · please send it again · open` |
| Audio rejected by Memos | `⚠ saved, audio not attached — <reason> · open` |
| Unsupported message type | `VoiceNotes takes voice or text only.` |
| File over 20 MB | `⚠ too large for Telegram bots (max 20 MB) — not saved.` |
| Storing failed | `⚠ not saved — please send it again.` |

`/start` and `/help` reply with `VoiceNotes: send a voice or text message to save it as a note.`

The morning digest lists new `#inbox` notes and earlier ones, oldest first, capped at
`DIGEST_MAX_NOTES` lines, each with a one-line summary and a link to
`MEMOS_PUBLIC_URL/memos/<id>`. `☑` marks notes with a task list. With no `#inbox` notes, nothing is
sent.

## Health

`GET /healthz` on `HTTP_ADDR` (default `:8080`) needs no authentication and should not be exposed
publicly. The body is JSON with `status`, `pending`, `oldest_pending_age`, `waiting_on`,
`last_poll_ok` and `last_digest` (the local date of the last digest actually sent).

| Status | HTTP | When |
|---|---|---|
| `ok` | 200 | Everything is fine |
| `degraded` | 200 | The oldest unfinished capture is older than `HEALTH_PENDING_THRESHOLD` or is waiting on a misconfiguration (401, 403, 404), or Telegram polling has failed for longer than that |
| `unhealthy` | 503 | The local store is unusable, or the worker loop has stopped |

`degraded` returns 200 on purpose, so Docker does not restart the service because a dependency is
down. Monitors that alert on a backlog read `status` from the body.

`voicenotes health` calls the endpoint on `127.0.0.1` at the port of `HTTP_ADDR` and exits `0` on
HTTP 200 and `1` otherwise, printing the body. The image uses it as its `HEALTHCHECK`, since
distroless has no curl or shell. `voicenotes health --strict` also exits `1` on `degraded`. So
`HTTP_ADDR` must listen on loopback: use `:port` or `0.0.0.0:port` (or `127.0.0.1:port`), not a
single non-loopback IP, or the `HEALTHCHECK` fails and the container shows as unhealthy.

## Volumes and ports

| Item | Value |
|---|---|
| Volume | `/data`: a small SQLite file, plus audio only while captures are queued. Must be writable by `65532:65532` |
| Port | `8080/tcp`: health only |

## Development

Requires Go, Task, golangci-lint and pre-commit.

```sh
task setup              # download modules
pre-commit install      # wires commit-msg, pre-commit and pre-push hooks
```

Before any commit the gate must pass:

| Command | Must show |
|---|---|
| `task lint` | 0 issues |
| `golangci-lint fmt --diff` | no output |
| `task test:coverage` | tests pass with `-race`, total coverage at or above 80% |
| `task build` | `bin/voicenotes` builds with `CGO_ENABLED=0` |

Other tasks: `task format`, `task test`, `task run` (builds and runs, loading `.env` if present),
`task clean` and `task changelog TAG=vX.Y.Z`. Commits follow Conventional Commits. The binding
project rules are in `.specify/memory/constitution.md`; the feature specification is under
`specs/001-voice-capture-notes/`.

### Releases

Releases are cut by pushing a `vX.Y.Z` tag. Write the release notes first with `task changelog
TAG=vX.Y.Z` and commit them; `.github/scripts/extract-changelog.sh vX.Y.Z` previews the release
body. The tag triggers `.github/workflows/release.yml`, which publishes the multi-arch image as
`ghcr.io/toshik1978/voicenotes:<version>` (the tag without its `v`) and `:latest`, attaches a
build provenance attestation and creates the GitHub release from the `CHANGELOG.md` section. A
tag without a matching section fails before anything is pushed. Pushes to `main` only run CI.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT. See [LICENSE](LICENSE).
