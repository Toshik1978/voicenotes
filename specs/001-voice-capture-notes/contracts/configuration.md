# Contract: Configuration (environment variables)

This is the deploy contract. The README carries the same table. All variables are plain
environment variables. **Secret** variables must be passed without their value appearing in the
command line (e.g. `docker run -e TELEGRAM_BOT_TOKEN`), and are never logged. On startup, a
missing or invalid required variable stops the service with `config: <VAR>: <problem>`; the
value itself is never printed.

## Telegram

| Variable | Req. | Default | Secret | Meaning |
|---|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | yes | — | **yes** | Token of the dedicated VoiceNotes bot |
| `TELEGRAM_ALLOWED_USERS` | yes | — | no | Comma-separated Telegram user IDs allowed to capture |
| `DIGEST_CHAT_ID` | no | first of `TELEGRAM_ALLOWED_USERS` | no | Chat that receives the morning digest |

## Speech-to-text (OpenAI-compatible)

| Variable | Req. | Default | Secret | Meaning |
|---|---|---|---|---|
| `STT_BASE_URL` | yes | — | no | Base URL including `/v1`, e.g. `http://stt.example.org:8000/v1` |
| `STT_API_KEY` | no | empty (no auth header) | **yes** | Bearer key |
| `STT_MODEL` | yes | — | no | Model id passed as `model` |
| `STT_RESPONSE_FORMAT` | no | `verbose_json` | no | `verbose_json` (reports the detected language) or `json` (for models that reject verbose_json) |
| `STT_TIMEOUT` | no | `120s` | no | Per-request timeout |

## Languages

| Variable | Req. | Default | Secret | Meaning |
|---|---|---|---|---|
| `SOURCE_LANGUAGES` | no | empty = auto-detect | no | Comma-separated ISO-639-1 codes the owner speaks, first = preferred |
| `TARGET_LANGUAGE` | no | `en` | no | ISO-639-1 code that notes are translated into |

## LLM (optional, OpenAI-compatible chat)

The LLM step is enabled only when `LLM_BASE_URL` is set.

| Variable | Req. | Default | Secret | Meaning |
|---|---|---|---|---|
| `LLM_BASE_URL` | no | — | no | Base URL including `/v1` (e.g. a LiteLLM gateway) |
| `LLM_API_KEY` | no | empty | **yes** | Bearer key |
| `LLM_MODEL` | if LLM on | — | no | Model name or gateway alias |
| `LLM_TRANSLATE` | no | `true` | no | Translate to `TARGET_LANGUAGE` |
| `LLM_TIDY` | no | `true` | no | Tidy punctuation and suggest tags |
| `LLM_TASKS` | no | `true` | no | Spot action items → checklist + `#todo` |
| `LLM_DIGEST_SUMMARY` | no | `true` | no | One-line summaries in the digest |
| `LLM_MAX_TAGS` | no | `3` | no | Upper bound on suggested tags; extra suggestions are dropped, `0` disables tags |
| `LLM_TIMEOUT` | no | `60s` | no | Per-request timeout |

## Memos

| Variable | Req. | Default | Secret | Meaning |
|---|---|---|---|---|
| `MEMOS_URL` | yes | — | no | Internal base URL the service calls, e.g. `http://memos.example.org:5230` |
| `MEMOS_TOKEN` | yes | — | **yes** | Personal Access Token (`memos_pat_…`) of the owner account, no expiry |
| `MEMOS_PUBLIC_URL` | no | `MEMOS_URL` | no | Base URL used in links shown to the owner |
| `MEMOS_API_PREFIX` | no | `auto` | no | `auto` probes `/api` (26.10+) then `/api/v1` (v0.31); or set `/api/v1` or `/api` explicitly |
| `MEMOS_CONTENT_LIMIT` | no | `8192` | no | Max note content bytes; match the Memos instance setting |
| `INBOX_TAG` | no | `inbox` | no | Tag for unreviewed notes |
| `TODO_TAG` | no | `todo` | no | Tag for notes with action items |

## Digest

| Variable | Req. | Default | Secret | Meaning |
|---|---|---|---|---|
| `DIGEST_ENABLED` | no | `true` | no | Turn the morning digest on or off |
| `DIGEST_TIME` | no | `07:30` | no | Local time `HH:MM` in `TZ` |
| `DIGEST_CATCHUP` | no | `3h` | no | Send a missed digest if started within this window |
| `DIGEST_MAX_NOTES` | no | `30` | no | Lines listed before "…and N more" |

## Runtime

| Variable | Req. | Default | Secret | Meaning |
|---|---|---|---|---|
| `DATA_DIR` | no | `/data` | no | SQLite file and queued audio; mount a persistent volume here |
| `HTTP_ADDR` | no | `:8080` | no | Health listener; must listen on loopback (`:port` or `0.0.0.0:port`) for `voicenotes health` |
| `HEALTH_PENDING_THRESHOLD` | no | `30m` | no | Oldest unfinished capture older than this → `degraded` |
| `RETRY_MAX_INTERVAL` | no | `5m` | no | Backoff cap for dependency retries |
| `LOG_LEVEL` | no | `info` | no | `debug` \| `info` \| `warn` \| `error` |
| `TZ` | no | unset (UTC) | no | Standard IANA zone, for example `Europe/Berlin` (tzdata is embedded); the digest time and the dates in attachment names use it. It must be an IANA zone name; a path such as `:/etc/localtime` is rejected. An unknown zone fails startup. |

## Container facts

- Image: `ghcr.io/toshik1978/voicenotes`. Tags: `X.Y.Z` (the release tag without its `v`, for
  example `0.1.0`) and `latest`, both published only from `vX.Y.Z` tags by `release.yml`.
  Platforms: linux/amd64 and linux/arm64.
- User 65532:65532 (distroless nonroot). `DATA_DIR` must be writable by that UID.
- Volume: `/data`. Contents are a few KiB of SQLite, plus audio only while captures are queued.
- Port: `8080/tcp` (health only; no public ingress needed).
- Healthcheck: `["CMD", "/app/voicenotes", "health"]`.
- Outbound connections: Telegram API (HTTPS), `STT_BASE_URL`, `LLM_BASE_URL`, `MEMOS_URL`.
