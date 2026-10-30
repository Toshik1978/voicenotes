# Quickstart & Validation: VoiceNotes

## Prerequisites

- Go 1.27, `task`, `golangci-lint`, `pre-commit` (via `mise install`)
- Docker for the end-to-end checks
- A test Telegram bot token and your Telegram user ID
- A speech-to-text endpoint: speaches with a model pre-downloaded, or an OpenAI key
- Optional: an OpenAI-compatible chat endpoint

## Local gates (must pass before every commit)

```sh
pre-commit install            # wires commit-msg (Conventional Commits), pre-commit, pre-push
task lint                     # 0 issues
task test:coverage            # race on; coverage over COVERPKG ≥ 80%
CGO_ENABLED=0 task build
```

## End-to-end run

```sh
docker run -d --name memos -p 5230:5230 neosmemo/memos:0.31.0
# create the owner account in the UI → Settings → Access Tokens → new token, expiry "Never"
cp .env.dist .env   # fill TELEGRAM_*, STT_*, MEMOS_URL=http://localhost:5230, MEMOS_TOKEN, DIGEST_TIME=<now+2m>
task run            # or: docker run --stop-timeout 90 --env-file .env -v $PWD/data:/data -p 8080:8080 ghcr.io/toshik1978/voicenotes
```

Deploy with a 90 s stop grace period (`--stop-timeout 90`, compose `stop_grace_period: 90s`), not
Docker's default 10 s: on SIGTERM an in-flight Telegram file download may take up to 60 s (the Bot
API HTTP timeout) before its capture is stored, and recording a delivered digest is retried for up
to 30 s. See the README quick start.

Variables are listed in [contracts/configuration.md](contracts/configuration.md).

## Scenarios (map to spec user stories)

| # | Do | Expect |
|---|---|---|
| 1 | Send a 10 s voice message (US1) | ⏳ reply, edited within 15 s to `✓ saved · open` (SC-001: note the time; ≤30 s with the LLM on). Memos note with transcript, audio attached, `#inbox`, PRIVATE. Phone made no sound |
| 2 | Send plain text; forward a voice message (US1) | Both become notes |
| 3 | Message the bot from a non-allowlisted account (US1) | No reply, no note |
| 4 | Send a photo (US1) | `VoiceNotes takes voice or text only.` |
| 5 | Stop the STT server, send a clip, `docker restart voicenotes`, start STT (US2) | Reply shows "queued", then `✓ saved` within 10 min of STT recovery (SC-003). Exactly one note, `createTime` = send time. `/healthz` was `degraded` while waiting past the threshold |
| 6 | Stop Memos after transcription, then start it (US2) | Transcription not repeated (one STT request in STT logs), one note |
| 7 | Set `LLM_BASE_URL`, `TARGET_LANGUAGE=en`; speak another language with an action item (US4) | English text, checklist, `#todo`, original quoted with language label |
| 8 | Point `LLM_BASE_URL` at a dead port (US2) | Note saved with raw transcript; reply `✓ saved (no AI)` |
| 9 | Remove `#inbox` from one note; wait for `DIGEST_TIME` (US3) | One message with sound listing exactly the remaining inbox notes, ☑ on task notes |
| 10 | Next day, without reviewing (US3) | Same notes under "From earlier" |
| 11 | Unset `MEMOS_TOKEN` and start (US6) | Exit with `config: MEMOS_TOKEN: required`; no secret printed |
| 12 | `docker inspect` health; `voicenotes health --strict` during an outage | `healthy` in Docker; strict exit code 1 |
| 13 | Set `MEMOS_API_PREFIX=auto` against Memos 26.10 rc | Calls go to `/api/...`, scenario 1 passes |

## Action-item check (SC-007)

With the LLM on, send 10 clips that each contain one clear action item, such as "remind me to
…" or "need to …". Pass if at least 9 notes contain a matching checklist entry and `#todo`.

## Owner filter check (research R8)

Against the real Memos 0.31.0, scenario 9 confirms that the `creator == "<auth/me user.name>"`
filter returns the owner's inbox notes.
