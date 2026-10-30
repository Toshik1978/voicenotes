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
| 11 | Unset `MEMOS_TOKEN` and start (US6) | Exit with `config: MEMOS_TOKEN: is required`; no secret printed |
| 12 | `docker inspect` health; `voicenotes health --strict` during an outage | `healthy` in Docker; strict exit code 1 |
| 13 | Set `MEMOS_API_PREFIX=auto` against Memos 26.10 rc | Calls go to `/api/...`, scenario 1 passes |

## Action-item check (SC-007)

With the LLM on, send 10 clips that each contain one clear action item, such as "remind me to
…" or "need to …". Pass if at least 9 notes contain a matching checklist entry and `#todo`.

## Owner filter check (research R8)

Against the real Memos 0.31.0, scenario 9 confirms that the `creator == "<auth/me user.name>"`
filter returns the owner's inbox notes.

## Opt-in Memos integration test

`internal/memos/integration_test.go` runs the client against a real, freshly started Memos. It
builds only with `-tags integration` and skips unless `VOICENOTES_IT_MEMOS_URL` is set. The test signs up the first user (the admin
owner), creates its PAT and a second user through the Memos API, with random passwords that are
never stored. It checks the prefix auto-probe, `auth/me`, create (PRIVATE, inline tags,
`createTime`, deterministic `memoId` retried on the same and on a fresh client still giving one
note), the audio attachment (uploaded once across two attempts and linked to the memo), and
`ListInbox` with the owner filter (R8).

```sh
docker run -d --rm --name vn-it-memos -p 127.0.0.1:5230:5230 neosmemo/memos:0.31.0
VOICENOTES_IT_MEMOS_URL=http://localhost:5230 VOICENOTES_IT_MEMOS_WANT_PREFIX=/api/v1 \
  go test -tags integration -race -count=1 -run Integration -v ./internal/memos
docker rm -f vn-it-memos   # the test needs a fresh server on every run
```

For 26.10, use the `neosmemo/memos:26.10.0-rc.2` image (or newer) with
`VOICENOTES_IT_MEMOS_WANT_PREFIX=/api`.

## Validation results (2026-10-30)

Run without a Telegram account, per the T039 ruling. Everything that needs a phone is in the
maintainer checklist below.

| Check | Run against | Outcome |
|---|---|---|
| Memos integration test (above) | `neosmemo/memos:0.31.0` | Pass. Auto-probe: `/api/auth/me` answers 404, falls back to `/api/v1` |
| Memos integration test, scenario 13 (API side) | `neosmemo/memos:26.10.0-rc.2`, the newest 26.10 rc on Docker Hub | Pass. Auto-probe picks `/api`; `/api/v1/*` answers 410 |
| R8 owner filter | both of the above | Confirmed, see below |
| Duplicate `memoId` | both | The second create answers HTTP 409 with gRPC code 6 (`memo with ID "…" already exists`); the client fetches the existing memo |
| Note links `{MEMOS_PUBLIC_URL}/memos/<uid>` | both | The web UI serves the path (200, HTML) |
| STT, `verbose_json` | the speaches CPU image with `Systran/faster-whisper-tiny`, Ogg/Opus clips made with espeak-ng + ffmpeg | English clip: exact text, `language: "en"`. Forced `language=ru` is honoured (`language: "ru"`). A model that is not installed answers 404, classified as misconfiguration |
| Scenario 11 | image built from the repo `Dockerfile` | Exit 1, single line `config: MEMOS_TOKEN: is required`. The fake STT key and Telegram token did not appear in the output |
| Scenario 12 | same image, real Memos 0.31.0, fake Telegram token (polls answer 401), `HEALTH_PENDING_THRESHOLD=20s` | At start `ok`, strict exit 0. After the threshold the stale poll gives `degraded`: `voicenotes health` exits 0, `--strict` exits 1. Docker health stayed `healthy`, no restarts |

**R8 confirmed.** On both versions `auth/me` returns `user.name` as `users/<username>` (here
`users/vn-it-owner`), not `users/<numeric id>`. The API answers "user not found" for `users/1`, so
building the name instead of reading it from `auth/me` would have broken the digest. An
unfiltered listing with the owner's token includes another user's PUBLIC `#inbox` note. With
`"inbox" in tags && creator == "<user.name>"` it returns exactly the owner's `#inbox` notes,
oldest first, with `property.hasTaskList` set on the task note. Dropping the creator clause makes
the test fail because the foreign note appears in the digest list.

**Fixes.** No client defects were found, so client behaviour is unchanged. Only documentation
was corrected:

- The scenario 11 text now matches the real message (`is required`). The contract shape
  `config: <VAR>: <problem>` holds.
- The `users/1` examples in contracts/external-apis.md and the client comment now read
  `users/<username>`.
- The 409 / code 6 duplicate response is now recorded as verified.

**Notes.**

- The tiny model turns robotic espeak-ng Russian into nonsense, which is a model-quality limit and
  not a client issue. Use a larger model for the real SC-007 run.
- Only the Memos API side of scenario 13 was run. Its "scenario 1 passes" part is in the
  checklist.

## Maintainer checklist (needs a real Telegram account)

Run after deployment, with the real bot, from a phone. Record the date, the image tag, the Memos
version and the STT/LLM models next to each item.

- [ ] Scenario 1: 10 s voice message. Record the SC-001 time from send to `✓ saved` (target ≤15
      s without the LLM) and check the note: transcript, audio attached, `#inbox`, PRIVATE, phone
      silent
- [ ] Scenario 1 with the LLM on. Record the SC-001 time (target ≤30 s)
- [ ] Scenario 2: plain text and a forwarded voice message both become notes
- [ ] Scenario 3: a non-allowlisted account gets no reply and no note
- [ ] Scenario 4: a photo gets `VoiceNotes takes voice or text only.`
- [ ] Scenario 5: STT outage plus restart. Record the SC-003 time from STT recovery to `✓
      saved` (target ≤10 min), with exactly one note, `createTime` = send time, and `/healthz`
      `degraded` while waiting
- [ ] Scenario 6: Memos outage after transcription. Check that STT logs show one request and
      there is one note
- [ ] Scenario 7: LLM translation, checklist, `#todo` and the quoted original with its language
      label
- [ ] Scenario 8: dead `LLM_BASE_URL`. Check for a raw transcript note and `✓ saved (no AI)`
- [ ] Scenario 9: digest at `DIGEST_TIME`. Check that one message with sound lists exactly the
      remaining inbox notes, with ☑ on task notes
- [ ] Scenario 10: next day, the same notes under "From earlier"
- [ ] Scenario 13 end to end: `MEMOS_API_PREFIX=auto` against Memos 26.10, then scenario 1
- [ ] SC-007: 10 clips, each with one action item, LLM on. Record for each clip whether the
      checklist entry and `#todo` matched. Pass at ≥9 of 10
