# Research: VoiceNotes

Each item records Decision / Rationale / Alternatives. Facts about Memos were checked against the
protos and server source at tags v0.31.0 and 26.10.0-rc.2. Facts about speaches were checked
against v0.9.0-rc.3 source.

## R1. Storage engine for local state

- **Decision**: CGO-free SQLite (modernc) under `DATA_DIR`, behind a `Store` interface
  (maintainer choice A).
- **Rationale**: the state is a short-lived queue plus dedupe IDs, and durable data lives in
  Memos. SQLite needs no setup for other self-hosters and follows the maintainer's tgshd pattern.
- **Alternatives**: shared Postgres only (B); both behind config (C). Both are rejected in
  plan.md.

## R2. "Persist before acknowledging" with Telegram audio

- **Decision**:
  - The ingest handler downloads the audio (up to 20 MB, usually a few hundred KB, about 1 s).
  - It writes the file to `$DATA_DIR/audio/`, then commits the capture row with `audio_path` set
    and `step=transcribe`, and **only then** sends ⏳.
  - If the Telegram download fails, the handler commits the row with only `file_id` and
    `step=download`, and sends **no** reply. The worker's `download` step fetches the audio and
    sends ⏳ (or the "queued" text) once the audio is on disk.
  - Text captures commit the row, then send ⏳.
- **Rationale**: Constitution VI and FR-005 require the audio to be stored locally before any
  acknowledgement, so ⏳ is a promise that the thought is safe. telebot runs handlers concurrently,
  so a ~1 s download does not stall polling for a single owner. The failure path still commits a
  row, so nothing is lost even if Telegram's file API is briefly down.
- **Alternatives**:
  - Acknowledge on `file_id` alone and download later. Rejected: violates Constitution VI.
  - Amend the constitution. Rejected: the stricter rule costs about 1 s.

## R3. Speech-to-text API and audio format

- **Decision**: `POST /audio/transcriptions`, sending the Ogg/Opus bytes as they arrive.
  `response_format` is configurable (`verbose_json` by default). The language is normalized to
  ISO-639-1. There is no ffmpeg in the image.
- **Rationale**: speaches decodes with PyAV/FFmpeg built into faster-whisper, and Ogg/Opus works
  directly. OpenAI accepts `ogg`. `verbose_json` gives `language` on speaches (`"ru"`) and
  whisper-1 (`"russian"`), while the gpt-4o transcribe models accept only `json`, which has no
  language. A config value is simpler and more predictable than auto-detecting the format from a
  400 error.
- **Alternatives**: transcode to WAV (rejected: needs ffmpeg, which breaks the static image);
  automatic format fallback on a 400 (rejected: guesses from error text that differs by
  provider).

## R4. Language selection rule

- **Decision** (pure `internal/language`):

  | `SOURCE_LANGUAGES` | Behaviour |
  |---|---|
  | empty | Auto-detect and accept the result |
  | one entry | Pass it as `language` |
  | several | Auto-detect. If the detected language is not in the list, re-transcribe once with the first entry. If no language is reported, accept the result |

- **Rationale**: this is exactly what the owner approved in the spec (US5). It costs one extra
  call only on misdetection.
- **Alternatives**: always force the first language (rejected: breaks the second language);
  ask the LLM to pick (rejected: by then the transcript is already garbled).

## R5. Schema migrations

- **Decision**: embed `migrations/NNN_*.sql` with `embed.FS` and apply them in order inside one
  transaction each, tracking progress with `PRAGMA user_version`.
- **Rationale**: about three tables, no down-migrations, and no extra dependency (Constitution
  III).
- **Alternatives**: goose (used in folio). Rejected because a dependency is not justified at this
  size.

## R6. Digest scheduling

- **Decision**: stdlib `time.Timer`.
  - A pure `next(now, HH:MM, loc)` gives the next run.
  - A pure catch-up check decides whether a missed run should still be sent: today's run is past,
    `now − scheduled ≤ DIGEST_CATCHUP`, and there is no `digest_runs` row for today.
  - `time/tzdata` is embedded because distroless has no zoneinfo.
- **Rationale**: there is one job a day, so gocron is not justified. Daylight-saving transitions
  are handled by `time.Date` in the configured `loc`, and table tests cover the DST days.
- **Alternatives**: gocron v2 (in tgshd). Rejected as an unnecessary dependency.

## R7. Memos API version handling

- **Decision**: target v0.31.0. `MEMOS_API_PREFIX=auto` probes `/api/auth/me` first, then
  `/api/v1/auth/me`. A PAT is used for auth. Tags are written inline. Create is made idempotent by
  passing a deterministic `memoId`.
- **Rationale**: in 26.10 the only change that affects VoiceNotes is the prefix (`/api/v1` → `/api`,
  with the old prefix returning 410). JSON field names are unchanged and PATs are unaffected. A
  deterministic ID keeps a retried create after a timeout from producing a duplicate note.
- **Alternatives**: a fixed prefix via config only (kept as the explicit override); wait for 26.10
  (rejected: nothing to gain).

## R8. Owner filter for the inbox listing

- **Decision**: resolve `user.name` via `GET {P}/auth/me` and filter
  `creator == "<that name>"`.
- **Rationale**: the list endpoint returns everything the token can read, including other users'
  PUBLIC notes. Whether the filter takes `users/1` or `users/<username>` was not verified, so the
  value is taken from the API instead of being constructed. An integration test against a real
  Memos v0.31.0 container (quickstart) confirms it.

## R9. LLM output format

- **Decision**: a single chat call per capture that asks for one JSON object, extracts the first
  `{…}` and validates it strictly. Invalid output means the raw transcript is saved.
- **Rationale**: `response_format: json_schema` and tools behave differently across providers
  behind LiteLLM, and the owner wants to be able to switch providers.
- **Alternatives**: provider structured output (rejected: not portable); several calls, one per
  function (rejected: cost and latency).

## R10. Error classification

- **Decision**:
  - **temporary**: 5xx, 408, 429, network errors and timeouts.
  - **misconfiguration** (retried, health degraded, never a placeholder): 401, 403, 404.
  - **permanent**: 400, 413, 415, 422.
- **Rationale**: speaches returns 404 for a model that is not installed and 403 for a bad key.
  Those are fixed on the deployment side, and turning them into "audio only" notes would throw
  away transcriptions. 415 means the audio could not be decoded, which genuinely calls for a
  placeholder.

## R11. Retry and backoff numbers

- **Decision**: start at 5 s, double each time, add ±20% jitter and cap at `RETRY_MAX_INTERVAL`
  (5 m). The LLM gets 3 attempts within 2 minutes, then is skipped.
- **Rationale**: the cap of 5 m keeps SC-003 (drained within 10 min of recovery) with margin. The
  LLM is optional, so it must not hold a note back (FR-018).

## R12. Healthcheck in a distroless image

- **Decision**: a `voicenotes health` subcommand performs the HTTP GET. `degraded` returns 200 so
  Docker never restarts the service because a dependency is down.
- **Rationale**: there is no curl or shell in the image, and Constitution V says to degrade rather
  than restart. The deployment's monitor (tgshd) can alert on `status` from the body or use
  `--strict`.
