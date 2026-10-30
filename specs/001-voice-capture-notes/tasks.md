---
description: "Task list for VoiceNotes v1"
---

# Tasks: VoiceNotes — voice capture to notes with a morning digest

**Input**: Design documents from `specs/001-voice-capture-notes/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: every task is implemented test-first (Constitution II). Each task writes its own stdlib
`testing` tests, with hand-written fakes and `httptest`. There are no standalone test tasks.

**Organization**: one phase per user story, in priority order. Each phase is one commit
(Conventional Commits, Constitution VIII).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1–US6 from spec.md

## Path Conventions

Single Go module `github.com/Toshik1978/voicenotes`. The binary is in `cmd/voicenotes/` and the
packages are in `internal/` (see plan.md Structure).

---

## Phase 1: Setup (project skeleton and gates)

**Purpose**: an empty but fully gated repository that mirrors the maintainer's other Go services.

- [x] T001 Initialise the Go module and repo files: `go.mod` (module `github.com/Toshik1978/voicenotes`, go 1.27), `.mise.toml` (go 1.27, golangci-lint latest, as in tgshd), `.gitignore`, `.dockerignore`, `LICENSE` (same licence as tgshd), and a stub `cmd/voicenotes/main.go` with `main.Buildstamp`/`main.Commit` vars and a `version` subcommand.
  - The `version` output must say "VoiceNotes" (FR-035). Test: `cmd/voicenotes/main_test.go`.
  - Add only the four approved dependencies when first used: telebot.v4, zap, caarlos0/env/v11, modernc sqlite (plan.md Technical Context).
- [x] T002 [P] Create `Taskfile.yml` modelled on `tgshd/Taskfile.yml`, with these tasks:
  - `setup`, `lint`, `format`, `test`
  - `test:coverage`: `-race -coverpkg={{.COVERPKG}}` with `COVERPKG: ./internal/config,./internal/capture,./internal/pipeline,./internal/language,./internal/enrich,./internal/render,./internal/digest,./internal/health`
  - `build`: `CGO_ENABLED=0`, ldflags, output `bin/voicenotes`
  - `run` (loads `.env`), `clean`
- [x] T003 [P] Create `.golangci.yml`, copied from tgshd's configuration and adjusted for the module path.
- [x] T004 [P] Create `.pre-commit-config.yaml` with `default_install_hook_types: [pre-commit, pre-push, commit-msg]`:
  - `compilerla/conventional-pre-commit` at the `commit-msg` stage
  - `golangci-lint fmt --diff` at the `pre-commit` stage
  - `task lint` at the `pre-push` stage
  - Pattern: `~/Development/beads-viewer/.pre-commit-config.yaml`, including its explanatory comments.
- [x] T005 [P] Create `.github/workflows/ci.yml` with lint, `test:coverage` and an 80% floor check, plus a `CGO_ENABLED=0` build. Its `-coverpkg` MUST equal Taskfile `COVERPKG`. Also create `.github/workflows/commit-lint.yml` (Conventional Commits check that re-runs the repo's commit-msg hook over the push/PR range, as in beads-viewer).
- [x] T006 [P] Create `.cliff.toml` (git-cliff grouping by Conventional Commit type, as in beads-viewer) and `CHANGELOG.md` with an `Unreleased` section.
- [x] T007 [P] Create `CLAUDE.md`, the runtime guide consistent with `.specify/memory/constitution.md`:
  - gates
  - Conventional Commits
  - no fx
  - public-repo hygiene with fake example values only

---

## Phase 2: Foundational (blocking prerequisites)

**Purpose**: config, the capture state machine, the local store and the worker skeleton that every story uses.

**⚠️ CRITICAL**: no user story work begins until this phase is complete.

- [x] T008 Implement `internal/config`: a `Config` struct parsed from env with caarlos0/env, covering every variable in `contracts/configuration.md` with the defaults listed there.
  - Validation returns errors shaped `config: <VAR>: <problem>` and never includes a secret value (`TELEGRAM_BOT_TOKEN`, `STT_API_KEY`, `LLM_API_KEY`, `MEMOS_TOKEN`).
  - Derived values: `DigestChatID` defaults to the first `TELEGRAM_ALLOWED_USERS` entry. `LLMEnabled` is true when `LLM_BASE_URL` is set, and then `LLM_MODEL` is required.
  - `DIGEST_TIME` must match `HH:MM`. `DIGEST_TZ` is loaded via `time.LoadLocation`, with `time/tzdata` imported in main.
  - Language codes must be ISO-639-1. `MEMOS_API_PREFIX` must be one of `auto|/api|/api/v1`.
  - Table tests in `internal/config/config_test.go`.
- [x] T009 [P] Implement the pure capture model in `internal/capture`. It defines `Capture`, `Kind` (`audio`|`text`), `Step` (`download → transcribe → enrich → write_note → finalize_reply → cleanup → done`, where text captures start at `enrich`), `Status` (`pending|waiting|done`) and `Outcome` (`OK|Temporary|Misconfig|Permanent` with reason and `WaitingOn`). It also provides `Next(c, outcome, now) Capture`.
  - `Next` applies the transitions from data-model.md "Steps": permanent in download/transcribe/enrich sets `failure` and jumps to `write_note`; a first entry into `waiting` flags that the reply needs a "queued" edit.
  - Backoff: 5 s, doubling, ±20% jitter via an injected rand, capped by `RETRY_MAX_INTERVAL` (research R11).
  - Table tests.
- [x] T010 [P] Implement `internal/store/sqlite`, with `Store` interface methods `Insert` (dedupe on UNIQUE(source, source_chat_id, source_message_id) returns `ErrDuplicate`), `NextDue(now)` (lowest `id` with `status!='done'`), `Update`, `OldestUnfinished`, and `PurgeDone(olderThan)`.
  - Embedded `migrations/001_init.sql` creates `captures`, `digest_runs` and `digest_seen` exactly as in data-model.md.
  - Migrations apply forward-only, one transaction each, tracked by `PRAGMA user_version` (research R5). WAL is on, and the database is `$DATA_DIR/voicenotes.db`.
  - Tests use `t.TempDir()`.
- [x] T011 Implement the worker loop in `internal/pipeline`.
  - `Worker` takes a `Store`, a clock and a map of step handlers (`func(ctx, Capture) (Capture, capture.Outcome)`). It processes `NextDue` captures strictly in `id` order (FR-011), applies `capture.Next`, persists after **every** step (FR-008), sleeps until the next due time or a wake signal, and stops on ctx cancel.
  - It exposes `Wake()`, which ingest calls, and `Alive()`, which health uses.
  - Tests use a fake store and a fake clock.

**Checkpoint**: the binary parses config, opens the store and runs an idle worker.

---

## Phase 3: User Story 1 — Capture a voice thought as a note (Priority: P1) 🎯 MVP

**Goal**: voice, audio, forwarded voice and text → one Memos note (transcript, audio attached, `#inbox`) and a silent ⏳ → `✓ saved · open` reply.

**Independent Test**: quickstart scenarios 1–4 with the LLM unset.

- [x] T012 [P] [US1] Implement the speech-to-text client in `internal/openai/transcribe.go`, using `POST {STT_BASE_URL}/audio/transcriptions` multipart as in contracts/external-apis.md §1.
  - Fields: `file` (bytes as-is, filename `capture-<id>.ogg`), `model`, `response_format`, optional `language`, `temperature=0`. Bearer header only if a key is set. `User-Agent: VoiceNotes/<version>`.
  - It returns `text` and the raw `language`. Errors map to Outcome classes per research R10: 5xx/408/429/network → Temporary; 401/403/404 → Misconfig; 400/413/415/422 → Permanent.
  - Tests use httptest.
- [x] T013 [P] [US1] Implement the Memos client in `internal/memos`. `NotesWriter.Create(ctx, Note) (name, url, error)` and `Attach(ctx, name, filename, mime, bytes)` follow contracts/external-apis.md §3.
  - Prefix resolution: `auto` probes `{MEMOS_URL}/api/auth/me`, then `/api/v1/auth/me` on 404/410; an explicit prefix skips the probe. The owner's `user.name` is cached.
  - Create uses `memoId=vn-<capture-id>-<shorthash>` (≤36 chars, `^[a-zA-Z0-9]([a-zA-Z0-9-]{0,34}[a-zA-Z0-9])?$`), visibility `PRIVATE` and `createTime`=received_at. Any "already exists" response is treated as success by fetching `GET {P}/memos/<uid>`.
  - Attach uses inline base64 with `memo` set, and is skipped if an attachment with that filename already exists.
  - Links are `{MEMOS_PUBLIC_URL}/memos/<uid>`. Errors map to Outcome classes as in T012.
  - Tests use an httptest fake covering both the v0.31 and 26.10 prefixes.
- [x] T014 [P] [US1] Implement the basic note renderer in `internal/render`. `Render(in Input, limit int) string` produces the main text (transcript, or placeholder text for `failure`/empty transcript like `(no speech recognized)`), then a blank line, then the tags line `#inbox` (tag names from config).
  - It is structured so the checklist, original quote and suggested tags slot in later (FR-022 order).
  - Table tests.
- [x] T015 [US1] Implement the Telegram adapter in `internal/telegram` with telebot.v4 long polling, `allowed_updates=["message"]`.
  - Ingest handler:
    - Drop non-allowlisted senders silently (FR-002).
    - Voice/audio (including forwarded) becomes `kind=audio` with `file_id`, `mime_type` and `file_size`. A `file_size` over 20 MB gets the reply `⚠ too large for Telegram bots (max 20 MB) — not saved.` and no insert.
    - Text becomes `kind=text`.
    - `/start` and `/help` get the one-line help.
    - Anything else gets `VoiceNotes takes voice or text only.`
  - Ingest order follows research R2 and Constitution VI:
    1. For audio, download it via `Downloader` and write it to `$DATA_DIR/audio/<id>.ogg`.
    2. Commit the row with `audio_path` set and `step=transcribe`.
    3. **Only then** send the silent `⏳`, store `reply_message_id`, and call `Worker.Wake()`.
  - If the Telegram download fails, commit the row with only `file_id` at `step=download` and send **no** reply. The worker sends ⏳ later.
  - Text captures commit the row, then send ⏳. `ErrDuplicate` gets no reply.
  - The adapter maps updates to a source-agnostic `pipeline.Ingest(ctx, capture.New...)` function, which a later HTTP route can call too (FR-007).
  - `Replier.Edit(chat, msgID, text)` falls back to a new silent reply if the edit is impossible. `Downloader.Download(file_id)` returns the bytes.
  - All sends and edits use `DisableNotification: true` (FR-026). Texts come from contracts/telegram-ux.md.
  - Logic is tested via small interfaces and a fake bot API.
- [x] T016 [US1] Implement the step handlers in `internal/pipeline/steps.go`.
  - `download`: writes `$DATA_DIR/audio/<id>.ogg` and sets `audio_path`. If `reply_message_id` is NULL, it then sends the silent ⏳ and records the id, so the acknowledgement always follows local persistence (Constitution VI).
  - `transcribe`: no language rules yet; uses `SOURCE_LANGUAGES` only when it has exactly one entry.
  - `enrich`: pass-through with `llm_skipped='disabled'`.
  - `write_note`: render, create, attach audio; sets `note_name`, `note_url`, `task_count`.
  - `finalize_reply`: `✓ saved · open`, or `⚠ saved audio only — <reason> · open` when `failure` is set.
  - `cleanup`: deletes the audio file and NULLs `text`, `language`, `enriched_json`, `audio_path`, `file_id` (FR-012).
  - Tests use fakes.
- [x] T017 [US1] Wire `run` in `cmd/voicenotes/main.go` by explicit construction, with no fx: config → zap logger → store → clients → worker → Telegram poller, plus graceful shutdown on SIGTERM. `.env.dist` gets anonymised example values (example.org, fake token and IDs).

**Checkpoint**: the MVP works end to end when all dependencies are up.

---

## Phase 4: User Story 2 — Never lose a capture when something is down (Priority: P1)

**Goal**: outages and restarts only delay captures. There are no duplicates, and permanent failures still produce a note.

**Independent Test**: quickstart scenarios 5, 6 and 8 (LLM part in US4).

- [x] T018 [US2] Implement the waiting reply edits in `internal/pipeline`. On the first transition into `waiting`, edit the reply to `⏳ queued — <speech-to-text|notes|Telegram> unavailable, will retry`, mapping `WaitingOn` to these words, then finalise later. Do not edit again on later retries. Misconfig outcomes wait like Temporary ones and never produce a placeholder (research R10).
- [x] T019 [US2] Implement placeholder handling for permanent failures in `internal/pipeline/steps.go`, following data-model.md:
  - A Permanent outcome in download, transcribe or enrich produces a note with the audio attached (when available) and a placeholder text naming the failure (FR-010).
  - A Permanent outcome in `write_note` retries once with a minimal note (placeholder plus `#inbox`, no attachment), then treats the failure as Temporary.
- [x] T020 [US2] Implement restart resume and dedupe guarantees in `internal/pipeline` and `internal/store/sqlite`. Unfinished captures resume at their stored step after restart. A duplicate Telegram update gets no second row and no reply. A memo create retried after a timeout yields one note (deterministic `memoId`). Tests simulate a crash between steps on a temp DB.
- [x] T021 [US2] Implement the daily purge in `internal/pipeline/purge.go`. It deletes `status='done'` rows whose `updated_at` is older than 30 days and runs at startup and every 24 h (FR-012).

**Checkpoint**: US1 plus outage tolerance; this is the deployable baseline.

---

## Phase 5: User Story 3 — Morning reminder of unreviewed notes (Priority: P1)

**Goal**: one loud daily message listing every `#inbox` note, new ones first, then "From earlier".

**Independent Test**: quickstart scenarios 9 and 10.

- [x] T022 [P] [US3] Implement `NotesReader.ListInbox(ctx) ([]NoteSummary, error)` in `internal/memos`.
  - Request: `GET {P}/memos` with filter `"<INBOX_TAG>" in tags && creator == "<auth/me user.name>"`, `orderBy=create_time asc`, `pageSize=200`, paging via `pageToken` (research R8).
  - Each summary carries name, url, content, createTime and `property.hasTaskList`.
  - Tests use httptest.
- [x] T023 [P] [US3] Implement pure digest composition in `internal/digest/compose.go`. `Compose(notes, seen set, summaries map, max int) (text string, ok bool)` builds the layout in contracts/telegram-ux.md:
  - The header reads `☀️ VoiceNotes — N new, M from earlier`.
  - The new notes come first, then `From earlier (M)`, each section oldest first.
  - `☑` marks notes with a task list. Each line has an `open` link.
  - The fallback summary is the first non-empty line with tags stripped, at most 80 chars.
  - The list is capped at `DIGEST_MAX_NOTES` with `…and N more`, and split at 4096 UTF-16 units.
  - It returns `ok=false` when there are no notes.
- [x] T024 [P] [US3] Implement pure scheduling in `internal/digest/schedule.go`:
  - `NextRun(now, hhmm, loc) time.Time`.
  - `ShouldCatchUp(now, hhmm, loc, window, lastRunDate) bool` (research R6).
  - Table tests cover DST transition days in `Europe/Berlin` and `America/New_York`.
- [x] T025 [US3] Add digest bookkeeping to `internal/store/sqlite`:
  - `digest_runs` (`local_date` PK, `status sent|empty|skipped`, `sent_at`, `note_count`).
  - `digest_seen` (`note_name` PK, `first_shown`): insert listed notes and delete rows no longer in the inbox.
- [x] T026 [US3] Implement the digest runner in `internal/digest/runner.go`.
  - A stdlib timer loop with a catch-up check at startup.
  - When it fires: ListInbox → Compose → send **with sound** to `DIGEST_CHAT_ID` → record the run and seen notes.
  - No notes gives status `empty` and no message. If Memos is unreachable, it retries within `DIGEST_CATCHUP`, then records `skipped`.
  - At most one run per local day (FR-027–FR-031).
  - It is wired in main only when `DIGEST_ENABLED`.
  - Tests use a fake clock, reader and sender.

**Checkpoint**: the owner gets the morning reminder; the P1 scope is complete.

---

## Phase 6: User Story 4 — Translation, tidy-up and task spotting (Priority: P2)

**Goal**: an optional LLM step that translates, tidies, suggests tags and spots tasks, plus digest summaries. Any LLM failure saves the raw transcript.

**Independent Test**: quickstart scenarios 7 and 8.

- [x] T027 [P] [US4] Implement the chat client in `internal/openai/chat.go`. It sends `POST {LLM_BASE_URL}/chat/completions` with `{model, temperature:0, messages}`, no `response_format`, tools or other provider fields, and a Bearer key if set. It returns `choices[0].message.content` and classifies errors as in T012. Tests use httptest.
- [x] T028 [P] [US4] Implement pure prompt building and response validation in `internal/enrich`.
  - Building: the system prompt is assembled from the enabled functions (`LLM_TRANSLATE`, `LLM_TIDY`, `LLM_TASKS`) plus the JSON shape. The user content holds the text, the detected language and the target language.
  - Parsing: extract the first `{…}` and validate it per contracts/external-apis.md §2. `text` must be non-empty and ≤4× the input length. `tasks` allows ≤10 items, each ≤200 chars. `tags` must match `[a-z0-9_-]{1,32}`, at most `LLM_MAX_TAGS`, with the inbox and todo tags dropped.
  - Any violation returns `invalid_output`.
  - Also build and parse the digest summaries call (`{"summaries":{"<n>":"≤80 chars"}}`).
  - Table tests.
- [x] T029 [US4] Extend `internal/render` to the full FR-022 layout:
  - Main text first, then `- [ ] <task>` lines.
  - When translated, the original goes under `*Original (<lang>):*` followed by `> ` quote lines.
  - The tags line is `#inbox`, then `#todo` when there are tasks, then the suggested tags.
  - Truncation to `MEMOS_CONTENT_LIMIT` shortens the original quote first, then the main text, with `(truncated)` markers, and never cuts tags or the checklist (FR-024).
  - Table tests.
- [x] T030 [US4] Implement the real `enrich` step in `internal/pipeline/steps.go`. Text captures follow the same path (US4 scenario 7).
  - When the LLM is configured: call it and store the validated JSON in `enriched_json`, and set `task_count`.
  - The bounded budget is 3 attempts or 2 minutes, then `llm_skipped='unavailable'` (FR-018). Invalid output sets `llm_skipped='invalid_output'`.
  - With the LLM skipped, the reply finalises to `✓ saved (no AI) · open`. With tasks, it finalises to `✓ saved · N tasks · open`.
- [x] T031 [US4] Add digest summaries in `internal/digest/runner.go`.
  - When `LLM_DIGEST_SUMMARY` is on and the LLM is configured, make one batched call with the first 500 chars of each note.
  - Missing or invalid summaries fall back per line to the first line.

**Checkpoint**: the AI-assisted notes work, and every failure falls back to the US1 behaviour.

---

## Phase 7: User Story 5 — Configurable languages (Priority: P2)

**Goal**: any source-language list and target language, with the misdetection re-transcribe rule.

**Independent Test**: spec US5 scenarios 1–4 against an httptest STT fake.

- [x] T032 [P] [US5] Implement pure language rules in `internal/language`.
  - `Normalize(raw string) (iso string, ok bool)` maps whisper full names (e.g. `"russian"`) and ISO-639-1 codes to ISO-639-1, using an embedded table of Whisper's languages.
  - `Plan(sources []string)` returns either a forced language or auto-detect.
  - `Recheck(sources, detected) (retranscribeWith string, ok bool)` encodes research R4: empty list → accept; one → forced; several and detected not listed → re-transcribe once with `sources[0]`; no detected language → accept.
  - Table tests.
- [x] T033 [US5] Apply the language rules in the `transcribe` step in `internal/pipeline/steps.go`. Store the normalized `language` and pass it to enrich and render. `STT_RESPONSE_FORMAT=json` (no language reported) skips the recheck. Tests use a fake transcriber.

---

## Phase 8: User Story 6 — Another self-hoster runs it from configuration only (Priority: P3)

**Goal**: configuration-only deployment, health check, container image and docs (the deploy contract).

**Independent Test**: quickstart scenarios 11–13 and a fresh-instance run from the README alone.

- [x] T034 [P] [US6] Implement pure health status in `internal/health`. Inputs: the oldest unfinished capture age, its `waiting_on`, the last successful Telegram poll, worker liveness, store health and the last digest date.
  - `ok`, or `degraded` (200) when the oldest capture is older than `HEALTH_PENDING_THRESHOLD` or polling has failed longer than that.
  - `unhealthy` (503) when the store is unusable or the worker has stopped.
  - The `GET /healthz` handler returns the JSON body from contracts/health.md.
  - Table tests and httptest.
- [x] T035 [US6] Wire health in `cmd/voicenotes/main.go`.
  - An HTTP listener on `HTTP_ADDR` serves `/healthz`; the Telegram adapter reports the last poll time.
  - Add the subcommand `voicenotes health [--strict]`, which GETs `http://127.0.0.1:<port>/healthz`. It exits 0 on 200 and 1 otherwise; `--strict` also exits 1 on `degraded`. It prints the body.
- [x] T036 [P] [US6] Create `Dockerfile` modelled on tgshd: a builder pinned to `$BUILDPLATFORM` cross-compiling with `CGO_ENABLED=0` and ldflags, then `gcr.io/distroless/static-debian12:nonroot` with `/app/voicenotes`, `USER 65532:65532`, `VOLUME /data`, `EXPOSE 8080`, `HEALTHCHECK CMD ["/app/voicenotes","health"]`, `ENTRYPOINT ["/app/voicenotes"]`.
- [x] T037 [P] [US6] Add image publishing to the CI workflow in `.github/workflows/ci.yml`, or a separate release workflow. It builds multi-arch (amd64, arm64) and pushes `ghcr.io/toshik1978/voicenotes`: `latest` on main, `vX.Y.Z` on tags, `sha-<short>`.
- [x] T038 [US6] Write `README.md`:
  - What VoiceNotes is and the flow.
  - Quick start with docker run and `.env`.
  - The full env var table, copied from contracts/configuration.md and marking required, defaults and secrets.
  - The exact external calls and provisioning needs: speaches model pre-download, `STT_RESPONSE_FORMAT` per provider, an LLM alias, a Memos version and PAT, upload limit ≥20 MB.
  - Health, volumes, ports, and the Telegram messages.
  - Use only example.org and fake values (Constitution VII).

---

## Phase 9: Polish & Cross-Cutting Concerns

- [ ] T039 Run the full quickstart.md scenarios 1–13 against Memos v0.31.0 and a real speaches or OpenAI endpoint. Fix any findings, recording them in `specs/001-voice-capture-notes/quickstart.md`. Also:
  - Record timings for SC-001 (≤15 s without the LLM, ≤30 s with it) and SC-003 (backlog drained ≤10 min after recovery).
  - Run the SC-007 10-clip action-item check (pass at ≥9 of 10).
  - Confirm the research R8 owner filter.
- [ ] T040 Do the final hygiene and release prep:
  - Scan the repo and history for real hosts, tokens and IDs (Constitution VII).
  - Ensure `task lint` is clean, `task test:coverage` is ≥80% and `CGO_ENABLED=0` builds pass.
  - Update `CHANGELOG.md` via git-cliff for v0.1.0.

---

## Dependencies & Execution Order

- **Setup (P1)** → **Foundational (P2)** → **US1** → **US2** → **US3** → **US4** → **US5** → **US6** → **Polish**.
- US2 builds on US1's step handlers. US2 acceptance scenario 5 (LLM unavailable) is verified only after US4 (T030). US3 needs only Foundational plus the `internal/memos` client from US1. US4 and US5 extend the US1 step handlers. US6 can start once US1 is done.
- Within a phase, `[P]` tasks touch different packages and can run in parallel.

## Parallel Example: User Story 1

```text
T012 internal/openai/transcribe.go   T013 internal/memos   T014 internal/render
→ then T015 internal/telegram → T016 internal/pipeline/steps.go → T017 cmd/voicenotes
```

## Implementation Strategy

1. **MVP** = Phases 1–3 (US1): working captures when everything is up.
2. **Deployable baseline** = adding US2 (no loss) and US3 (digest). This is the full P1 scope, and the deploy contract is final at this point.
3. Then US4 (AI), US5 (languages), US6 (health, image, README). The image (T036/T037) can be pulled forward if the deployment wants to try the MVP early.

## Phase 10: Convergence

- [x] T041 Make the minimal-note retry in `internal/pipeline/steps.go` (`writeNote`/`noteInput`) render only the raw text or transcript plus `#inbox`, with no LLM output and no attachment, so a permanently rejected create is retried with a different payload. Add a test for a text capture with `EnrichedJSON` per data-model.md "Steps" and ruling vn-jmb.3.5 (a) (contradicts)
- [x] T042 Report misconfiguration (401/403/404) as `degraded` health. Carry `Capture.Misconfig` through `cmd/voicenotes/adapters.go` (`healthQueue.OldestUnfinished`), `internal/health/source.go` and `internal/health/health.go` `Compute`, with table and collector tests, per research.md R10 and contracts/external-apis.md failure classes (partial)
- [x] T043 Name Memos audio attachments `voice-<received_at local date>.<ext>` instead of the local temp file name in `internal/pipeline/steps.go` (`attach`), keeping the existing-attachment check idempotent, per contracts/external-apis.md §3 Attach audio (contradicts)
- [x] T044 Treat a bare `voicenotes` invocation as `run` in `cmd/voicenotes/main.go`, keeping `run` as an alias, and update `cmd/voicenotes/main_test.go` per plan.md Project Structure (contradicts)
- [x] T045 Replace the misleading `memos_pat_FAKE` examples for `STT_API_KEY` and `LLM_API_KEY` in `.env.dist` with neutral fake keys per contracts/configuration.md (unrequested)

## Phase 11: Convergence

- [x] T046 Prevent the final reply from racing the ingest ⏳ in `internal/telegram/handler.go`, `internal/pipeline/ingest.go` and `internal/pipeline/steps.go` (`finalizeReply`). A capture whose ⏳ send is still in flight must have that same ⏳ edited, never a second reply sent. Add a test where the worker reaches `finalize_reply` before `SetReplyMessageID`, per FR-025, US1 AS1/AS5 and contracts/telegram-ux.md (contradicts)
