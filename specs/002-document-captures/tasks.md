---
description: "Task list for document and photo captures"
---

# Tasks: Document and photo captures

**Input**: Design documents from `specs/002-document-captures/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: every task is implemented test-first (Constitution II). Each task writes its own stdlib
`testing` tests, with hand-written fakes and `httptest`. There are no standalone test tasks.

**Organization**: setup, a foundational generalisation (audio → files, no behaviour change), then
one phase per user story in priority order, then polish. Each phase is one commit (Conventional
Commits, Constitution VIII).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1–US4 from spec.md

## Path Conventions

Single Go module `github.com/Toshik1978/voicenotes`. The binary is in `cmd/voicenotes/` and the
packages are in `internal/` (see plan.md Structure). `COVERPKG` stays unchanged.

---

## Phase 1: Setup (configuration)

**Purpose**: every new setting parsed and validated up front, so later phases only read `config.Config`.

- [x] T001 Add the new optional settings to `internal/config/config.go` with their validation, per contracts/configuration.md (tests in `internal/config/config_test.go`):
  - Settings and defaults:
    - `DOCLING_URL`: unset
    - `DOCLING_API_KEY`
    - `DOCLING_TIMEOUT`: `10m`, "> 0"
    - `DOCLING_MAX_PAGES`: `20`, "1–1000"
    - `BATCH_QUIET_PERIOD`: `3s`, "500ms–1m"
    - `HINT_EXCERPT_RUNES`: `4000`, "500–50000"
    - `VISION_ENABLED`: `false`
    - `VISION_BASE_URL`, `VISION_API_KEY`, `VISION_MODEL`: fall back to `LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODEL` when unset
    - `VISION_TIMEOUT`: `60s`, "> 0"
  - Validation rules:
    - `DOCLING_URL`, when set, must be an absolute http/https URL.
    - `VISION_ENABLED=true` requires a resolvable base URL and model.
    - Errors name the variable, never the value (Constitution V).
  - With none set, `Config` behaves exactly as before (FR-129).

---

## Phase 2: Foundational (one capture holds many files)

**Purpose**: generalise the single audio file to a list of files, rename `transcribe` to `extract`, add per-file progress and worker lanes. No user-visible behaviour changes except FR-114a's due-first scheduling: a capture waiting on a dependency no longer holds back later ones. Notes still sort by receive time. **Checkpoint**: every existing test still passes, except strict-FIFO assertions, which are updated to the due-first rule.

- [x] T002 Extend the capture model in `internal/capture/capture.go`, `internal/capture/next.go`, `internal/capture/classify.go` (tests alongside):
  - `KindFiles` (`"files"`)
  - `File` struct (fields per data-model.md "Capture file"), `FileKind` (`audio|image|document|video`), `FileState` (`pending|done|empty|failed|skipped`), `Capture.Files []File`, `Capture.AlbumID`
  - `StepCollect`; rename `StepTranscribe` to `StepExtract` (value `"extract"`); `collect → download` in `next()`
  - `capture.New`: `files` must start at `collect`, `audio` at `download` or `extract`
  - `ClassProgress` with `Progress()` constructor. `Policy.Next` on progress keeps the step, resets `Attempts`, `WaitingOn`, `Misconfig`, `LastError` and `EnrichStartedAt`, sets `Status=pending` and `NextAttemptAt=now`, and never sets `QueueReplyEdit`.
  - `WaitingDocling = "docling"`
  - remove `FileID`/`FileSize`/`MimeType`/`AudioPath` from `Capture` (moved to `File`)
- [x] T003 Add lanes to `internal/capture/lane.go` (tests in `internal/capture/lane_test.go`):
  - `Lane` (`LaneMain`, `LaneDocuments`)
  - `LaneOf(c)`: documents = `KindFiles` at `StepExtract`, main otherwise
- [x] T004 Write migration `internal/store/sqlite/migrations/002_capture_files.sql` and update `internal/store/sqlite/store.go` per data-model.md and research R10 (tests in `internal/store/sqlite/store_test.go`, upgrade case in `internal/store/sqlite/restart_test.go`).
  - Migration:
    - create `capture_files` with "UNIQUE (source, source_chat_id, source_message_id)", "UNIQUE (capture_id, position)" and "FK → captures(id) ON DELETE CASCADE"
    - copy each capture's `file_id`/`file_size`/`mime_type`/`audio_path` into one `audio` row at position 0. State is `done` when the step is past `transcribe` (transcript already in `captures.text`), else `pending`.
    - `UPDATE captures SET step='extract' WHERE step='transcribe'`
    - add `captures.album_id`
    - drop the four moved columns
  - Store:
    - `Insert`/`Update`/`queryOne` read and write `Files` in the same transaction. `Update` only updates mutable file columns of existing rows; ingest inserts.
    - `NextDue(ctx, now, lane)` returns the lowest-ID **due** capture in the lane, else the lane's earliest `next_attempt_at` as `WakeAt` (FR-114a: replaces 001's strict-FIFO head rule; a not-yet-due head no longer blocks later due captures)
    - rename `AudioPaths` → `FilePaths` over `capture_files.local_path`
    - `PurgeDone` cascades
  - Upgrade test: a 001 database with queued audio captures migrates and the captures resume.
- [x] T005 [P] Rename the local file store, rename the data directory and update the orphan sweep (tests alongside).
  - In `internal/pipeline/audiofs.go` → `internal/pipeline/files.go`: `AudioStore` → `FileStore`, `DirAudio` → `DirFiles`.
  - In `internal/pipeline/orphans.go`: the orphan sweep scans the files dir and is fed by `FilePaths`. Rename `OrphanAudio*` to `OrphanFile*`.
  - Add a startup helper `MigrateFilesDir(dataDir)` (research R9):
    - rename `audio/` to `files/` when only `audio/` exists
    - merge entries and remove `audio/` when both exist
    - no-op otherwise
- [x] T006 Generalise the step handlers in `internal/pipeline/steps.go` to per-file work for audio, keeping 001 behaviour (existing tests in `internal/pipeline/steps_test.go` must pass unchanged in meaning):
  - `download`: fetches each file without a local copy, one per attempt, returning `Progress` until none remain. A too-large or rejected file is marked `failed`; for an audio capture this keeps today's permanent outcome.
  - `extract`: replaces `transcribe`. For an audio file it runs today's STT logic, stores `File.Text`/`File.Language`, `state=done`, and copies the text and language to `Capture.Text`/`Capture.Language` once the last file is done.
  - `writeNote`: attaches each unattached file, one per attempt (`Progress`), and records `File.Attached`. A permanent attach failure keeps today's minimal-note behaviour for audio.
  - `cleanup`: removes every local file and clears per-file text, `file_id` and `file_name`.
  - `holdsAudio` and `replyText` read from `Files`. Audio reply texts are unchanged.
- [x] T007 Run two worker lanes (tests in `internal/pipeline/worker_test.go`, `cmd/voicenotes/service_test.go`).
  - In `internal/pipeline/worker.go`: `Worker` takes a `capture.Lane`, passes it to `Store.NextDue`, and validates handlers only for the steps that lane runs (documents: `extract`; main: all).
  - In `cmd/voicenotes/service.go`:
    - build both workers
    - `Wake` both on ingest
    - call `MigrateFilesDir` before the workers start
  - In `cmd/voicenotes/health.go`: `Alive` and `StoreOK` require both lanes (research R12). Health never reads vision state (FR-128).

---

## Phase 3: User Story 1 - Save a photo or file as a note with its original attached (Priority: P1) 🎯 MVP

**Goal**: photos, documents and videos become `#inbox` notes with the originals attached and a placeholder hint, with nothing new configured.

**Independent Test**: with no `DOCLING_*`/`VISION_*` and the LLM off, send a PDF and a photo with a caption. Each becomes a note (`📎 name.pdf` / caption + `📷 photo`) with the file attached. A sticker gets the new unsupported text. Voice still works (quickstart §1).

- [x] T008 [P] [US1] Map Telegram file messages in `internal/telegram/api.go` `fromTele` (tests in `internal/telegram/api_test.go`), per research R11:
  - Add `Message.Files *FileItem` with `FileID`, `MIME`, `Name`, `Size`, `Kind`, `Caption`, `AlbumID`.
  - Map Photo (largest, `image/jpeg`), Document (`image/*` → image, `audio/*` → existing `Audio`, else document), Video and Animation (video).
  - Keep the `OnMedia`/`OnDocument`/`OnPhoto`/`OnVideo` endpoints routed to `Handle`.
- [x] T009 [P] [US1] Render file notes in `internal/render/render.go` and new `internal/render/hint.go` (tests alongside), per contracts/telegram-ux.md "Note layout":
  - `Input` gains `Caption`, `Hint`, `Files []FileSummary` (kind, name, state, failure, text).
  - Layout: caption text, hint, checklist, original quote, tags.
  - `FallbackHint(files)`: first two cleaned lines of the first extracted text, "≤ 160 runes", markdown noise stripped (headings, table pipes, `<!-- image -->`). Without text, use placeholders `📎 <name>`, `📷 photo`, `📷 N photos`, `📎 N files`, plus ` — <reason>` when every file failed.
  - Truncation keeps attachments untouched and shortens the caption with "(truncated)".
  - FR-125 test: the note's first non-empty line is the caption when there is one, otherwise the hint, so the digest's first-line summary (001 FR-029) shows it unchanged.
- [x] T010 [P] [US1] Add attachment naming in new `internal/pipeline/names.go` (tests in `internal/pipeline/names_test.go`), per research R8:
  - Audio keeps `voice-<local date>.<ext>`.
  - Other files use the sanitised original base name ("at most 100 bytes", control characters removed, extension kept), else `photo-<date>-<n>.jpg` / `video-<date>-<n><ext>` / `file-<date>-<n><ext>`.
  - Duplicates within a capture get ` (2)`, ` (3)` before the extension.
  - Deterministic for a given capture.
- [x] T011 [US1] Add file-item ingest across `internal/pipeline/ingest.go` and `internal/store/sqlite/store.go` (tests in `internal/pipeline/ingest_test.go`, `internal/store/sqlite/store_test.go`):
  - `Ingester.IngestFile(ctx, item, ack)` calls the new `Store.AppendFile`, which in this phase always creates a new `files` capture at `collect`:
    - `NextAttemptAt = received + quiet`
    - the file at position 0
    - caption into `Capture.Text`
    - capture identity = the item's identity
  - A redelivery returns `ErrDuplicate` via either UNIQUE constraint.
  - Ack runs only when a capture was created.
- [x] T012 [US1] Accept file items in the Telegram handler, in `internal/telegram/handler.go` and `internal/telegram/audio.go` → `internal/telegram/files.go` (tests in `internal/telegram/handler_test.go`):
  - `Handle` routes `m.Files` to `files()`.
  - `files()`:
    - size check against `MaxFileSize` (a standalone item over the limit gets `TextTooLarge`)
    - download
    - durable write with `tg-<chat>-<msg>-*<ext>`
    - `IngestFile` with the ⏳ ack
    - on a download or write failure, ingest the item without a local path so the download step fetches it later
  - New texts: `TextUnsupported` = `VoiceNotes takes voice, text, photos and files.`, `TextHelp` = `VoiceNotes: send a voice message, text, photo or file to save it as a note.`
- [x] T013 [US1] Run `files` captures through the pipeline in `internal/pipeline/steps.go` (tests in `internal/pipeline/steps_test.go`):
  - a `collect` handler that returns OK
  - `extract` marks image and document files `skipped` when no reader is wired, and video always `skipped`
  - `enrich` for `files` captures with only a caption runs the existing `BuildRequest` on the caption, and with no caption records no call
  - `writeNote` renders with `Caption`/`Hint=FallbackHint`, creates the note, then attaches every kept file in order using T010 names. A per-file permanent attach failure is recorded in `File.AttachFailure` and does not fail the note (FR-126).
  - `replyText` for `files` per contracts/telegram-ux.md: `✓ saved`, `⚠ saved, X of N not kept — …`, `⚠ saved, X of N not attached — …`, `⚠ files not kept — … · please send them again`
- [x] T014 [US1] Wire file ingest and the `collect` handler in `cmd/voicenotes/service.go` and `cmd/voicenotes/adapters.go` (tests in `cmd/voicenotes/service_test.go`): pass `BatchQuietPeriod` to the ingester and register the `collect` handler. An end-to-end test with fakes: a photo item yields one note with an attachment and the `📷 photo` hint.

**Checkpoint**: US1 is fully functional with nothing new configured.

---

## Phase 4: User Story 2 - A hint of what the item is (Priority: P1)

**Goal**: with docling-serve configured, each photo or document's note carries a content-derived hint, LLM-written from a bounded excerpt, or the first lines when the LLM is off.

**Independent Test**: with `DOCLING_URL` and the LLM set, a photo of a bill, a text PDF, a scanned PDF and a DOCX each get a meaningful hint. With the LLM off, the hint is the first lines. A voice message sent right after a large scan is confirmed first (quickstart §2, §4).

- [x] T015 [P] [US2] Implement the docling-serve adapter in new `internal/docling/client.go` (tests in `internal/docling/client_test.go` with `httptest`), per contracts/external-apis.md §1 and research R1:
  - First pin the multipart encoding of `page_range` and the submit, poll and result shapes against `/openapi.json` of a current docling-serve, and record them in the test fixtures.
  - `Convert(ctx, name, mime, data)`:
    - submit to `/v1/convert/file/async` with `to_formats=md`, `image_export_mode=placeholder`, `include_images=false`, `do_ocr=true`, `abort_on_error=false`, `page_range` 1..`DOCLING_MAX_PAGES` and `document_timeout`
    - poll `/v1/status/poll/{task_id}` every 2 s
    - fetch `/v1/result/{task_id}`
    - return `document.md_content`
  - Send `X-Api-Key` when set.
  - Classify per the contract table, including a distinct timeout result after `DOCLING_TIMEOUT`.
  - Never log content.
- [x] T016 [P] [US2] Add the per-file extraction policy in new `internal/capture/file.go` (tests in `internal/capture/file_test.go`): `DoclingTimeout(f)` increments `Timeouts`; at 3 it sets `state=failed` with `failure="couldn't read in time"` and reports permanent, otherwise temporary with `WaitingDocling` (FR-113).
- [x] T017 [P] [US2] Build the hint request in new `internal/enrich/files.go` (tests in `internal/enrich/files_test.go`), per contracts/external-apis.md §3 and research R5:
  - `BuildFilesRequest(opts, caption, excerpts)`:
    - excerpts are bounded in total by `HINT_EXCERPT_RUNES`, in file order, labelled `--- <file name> (excerpt) ---`, at least 200 runes per file while the budget allows
    - the system prompt marks excerpts as untrusted content to describe, never instructions (FR-119)
    - JSON keys per the contract
  - `ParseFiles`:
    - validates `hint` ("≤ 2 lines, ≤ 160 runes", non-empty, target language)
    - caption `text`/`translated` follow the same checks as `Parse`
    - tasks come from the caption only
  - Add `Result.Hint` (`json:"hint,omitempty"`).
- [x] T018 [US2] Read documents and photos in the `extract` handler in `internal/pipeline/steps.go` (tests in `internal/pipeline/steps_test.go`):
  - Add an `Extractor` interface to `Deps` (nil when docling is not configured).
  - For image and document files, one per attempt:
    - read the local file
    - call `Convert`
    - set `Text`, `text_source=docling`, and `state=done`, or `empty` when there is no text
    - a timeout goes through `capture.DoclingTimeout`
    - permanent failures set `state=failed` with the reason
    - return `Progress` while files remain
  - Unreachable docling is `Temporary` with `WaitingDocling`.
  - `QueuedText(WaitingDocling)` = `⏳ queued — document reader unavailable, will retry`.
- [x] T019 [US2] Produce the hint for `files` captures in `internal/pipeline/steps.go` (tests in `internal/pipeline/steps_test.go`):
  - `enrich` calls `BuildFilesRequest`/`ParseFiles` when any file has text or there is a caption, under the existing budget and skip rules.
  - `noteInput` uses `Result.Hint` and processed caption text, else `FallbackHint` over the file texts (FR-116, FR-117, FR-118).
  - Document content is never logged; only sizes go to the logs.
- [x] T020 [US2] Wire docling in `cmd/voicenotes/service.go` and `cmd/voicenotes/adapters.go` (tests in `cmd/voicenotes/service_test.go`):
  - build the docling client only when `DOCLING_URL` is set
  - register the documents-lane `extract` handler with it
  - add an end-to-end test with an `httptest` docling showing that a voice capture completes while a document capture waits on a slow conversion (FR-114a)
  - FR-128 test: with docling unreachable, a document capture waits with `waiting_on=docling` and health degrades past the threshold; with `DOCLING_URL` unset, no capture ever waits on docling

**Checkpoint**: US1 and US2 both work. Hints appear when docling is configured.

---

## Phase 5: User Story 3 - One send, one note (Priority: P2)

**Goal**: photos and files from the same chat within the quiet period, or in the same album, become one capture, one note and one reply.

**Independent Test**: an album of 4 photos with a caption on the 3rd, 15 photos at once, and two photos 1 s apart each yield one note. A photo followed by a voice message yields two. A restart mid-album loses nothing (quickstart §3).

- [x] T021 [P] [US3] Implement the batching rule in new `internal/capture/batch.go` (tests in `internal/capture/batch_test.go`), per data-model.md "Batching rule":
  - `JoinBatch(open *Capture, item BatchItem, now, quiet)` returns join or new.
  - Join when an open `files` capture at `collect` exists and (`open.NextAttemptAt > now` or the album IDs match and are non-empty).
  - On join: `NextAttemptAt = now + quiet`, next position, caption appended with a blank line.
  - Audio and text never batch.
- [x] T022 [US3] Join batches in `Store.AppendFile` in `internal/store/sqlite/store.go` (tests in `internal/store/sqlite/store_test.go`, `internal/store/sqlite/restart_test.go`):
  - Inside `BEGIN IMMEDIATE`, load the chat's newest `files` capture at `collect`, apply `capture.JoinBatch`, then either insert the file row and update the capture's `next_attempt_at`/`text`, or create a new capture.
  - Return whether a capture was created.
  - Tests:
    - concurrent appends from goroutines yield one batch
    - an item after the deadline starts a new batch
    - same-album items join even when late within `collect`
    - a reopened database keeps a half-received batch
- [x] T023 [US3] Handle batches in `internal/telegram/handler.go` and `internal/pipeline/ingest.go` (tests in `internal/telegram/handler_test.go`, `internal/pipeline/ingest_test.go`):
  - ⏳ is sent only when `AppendFile` created the capture.
  - An item over 20 MB that has an album ID, or arrives while the chat has an open batch, is appended as a `failed` file ("too large for Telegram bots") instead of being rejected; a standalone one keeps `TextTooLarge`.
  - End-to-end with fakes:
    - album with a caption on the 3rd item
    - 15 photos across 2 albums
    - photo + voice → 2 notes
  - Also wake both lanes when a batch's deadline is extended.

**Checkpoint**: US1–US3 work. Albums and multi-album sends yield one note.

---

## Phase 6: User Story 4 - Read hard photos with a vision model, when allowed (Priority: P3)

**Goal**: with `VISION_ENABLED=true`, a photo docling cannot read (or any photo when docling is not configured) is described by an OpenAI-compatible vision model. Documents are never sent.

**Independent Test**: with vision on, a photo of handwriting gets a usable hint through exactly one image request. An image-only PDF triggers no vision request. With vision off, no image reaches the gateway (quickstart §5).

- [x] T024 [P] [US4] Add the vision client in new `internal/openai/vision.go` (tests in `internal/openai/vision_test.go` with `httptest`), per contracts/external-apis.md §2:
  - `Vision.Describe(ctx, image, mime)` posts to `/chat/completions` with a system instruction and a user content array (`text` + `image_url` data URL), with bearer auth when a key is set.
  - Return `choices[0].message.content` trimmed and capped at "2000 runes".
  - Classify via `capture.ClassifyHTTP`/`NetworkError`.
- [x] T025 [P] [US4] Add the vision decision and budget in `internal/capture/file.go` (tests in `internal/capture/file_test.go`):
  - `NeedsVision(f, enabled, doclingConfigured)` is true only for `image` files when enabled and (docling not configured or the file's docling text has fewer than 20 non-space runes).
  - A per-file vision budget of 3 attempts or 2 minutes, after which the file is `empty` with no `waiting_on` (FR-121, FR-123).
- [x] T026 [US4] Use vision in `extract` and wire it, in `internal/pipeline/steps.go`, `cmd/voicenotes/service.go`, `cmd/voicenotes/adapters.go` (tests in `internal/pipeline/steps_test.go`, `cmd/voicenotes/service_test.go`):
  - Add a `Describer` to `Deps`, nil unless `VISION_ENABLED`.
  - After docling, or instead of it when docling is unconfigured, call it for an image file when `NeedsVision` holds, and store the answer as `Text` with `text_source=vision`.
  - Tests:
    - a document never reaches the describer
    - vision off sends nothing
    - FR-123: vision failure degrades to a placeholder hint, the capture never enters waiting, and the reply is the normal confirmation (no `(no AI)`, no warning)

**Checkpoint**: all four stories work independently.

---

## Phase 7: Polish & Cross-Cutting Concerns

- [ ] T027 [P] Document the feature in `README.md` and `.env.example`:
  - photo, file and album behaviour, and the new reply texts
  - the new optional variables, with anonymised examples only (`http://docling.example.org:5001`, `https://llm.example.org/v1`, `example-vision-model`)
  - a docling-serve container and a vision-capable model as a suggested setup, not a requirement (FR-131, Constitution VII)
- [ ] T028 Run the full gate and the quickstart scenarios against `specs/002-document-captures/quickstart.md`:
  - `task lint`, `golangci-lint fmt --diff`, `task test:coverage` (≥ 80%, `-race`), `task build`
  - time the SC-106 scenarios against real services: a single photo from sending to the confirmed reply (target ≤ 60 s) and a 10-page document (target ≤ 3 min), and record the measured times and hardware in `specs/002-document-captures/quickstart.md`
  - scan the diff for real hostnames, IPs, tokens or IDs
  - update `specs/002-document-captures/quickstart.md` if a step changed during implementation

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: none.
- **Foundational (Phase 2)**: depends on Phase 1. Blocks every story.
  - T002 → T003, T004, T006.
  - T004 → T006, T007.
  - T005 is independent of T004 (different files).
- **US1 (Phase 3)**: depends on Phase 2. MVP.
- **US2 (Phase 4)**: depends on US1 (the `files` capture flow, `FallbackHint`, names).
- **US3 (Phase 5)**: depends on US1 (`AppendFile`, `files()` ingest). Independent of US2.
- **US4 (Phase 6)**: depends on US1. It uses US2's `extract` dispatch when docling is present, but also works without docling.
- **Polish (Phase 7)**: after the stories that will ship.

### Within Each Story

Pure logic ([P] tasks in gated packages) → adapters → pipeline handlers → wiring. Each task writes
its tests first.

### Parallel Opportunities

- Phase 2: T005 alongside T003/T004.
- US1: T008, T009, T010 together, then T011 → T012 → T013 → T014.
- US2: T015, T016, T017 together, then T018 → T019 → T020.
- US3: T021, then T022 → T023.
- US4: T024, T025 together, then T026.
- After US1, US2 and US3 can proceed in parallel; they touch different handlers, with `steps.go`
  shared, so merge carefully.

## Parallel Example: User Story 2

```text
Task: "T015 docling-serve adapter in internal/docling/client.go"
Task: "T016 per-file timeout policy in internal/capture/file.go"
Task: "T017 hint request/parse in internal/enrich/files.go"
```

## Implementation Strategy

### MVP First (User Story 1)

1. Phase 1, then Phase 2 (verify all 001 tests still pass).
2. Phase 3 (US1). **Stop and validate** with quickstart §1. Photos and files are already safely
   in the inbox.

### Incremental Delivery

1. US1: files accepted with placeholder hints (deployable).
2. US2: content hints via docling (deployable; docling optional).
3. US3: one note per send.
4. US4: vision fallback, off by default.
5. Polish: docs and full quickstart.
