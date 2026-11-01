# Research: Document and photo captures

Each decision is given as Decision / Rationale / Alternatives considered. Technical Context left
no NEEDS CLARIFICATION. The open points were integration details, resolved here.

## R1. Document reader: docling-serve, async API

- **Decision**: use docling-serve's v1 async endpoints:
  1. `POST /v1/convert/file/async` (multipart, file part `files`, options as form fields).
  2. `GET /v1/status/poll/{task_id}` until `task_status` is `success` or `failure`.
  3. `GET /v1/result/{task_id}`.
  4. Read `document.md_content` and `status`.

  Options sent:

  | Field | Value |
  |---|---|
  | `to_formats` | `md` |
  | `image_export_mode` | `placeholder` |
  | `include_images` | `false` |
  | `do_ocr` | `true` |
  | `abort_on_error` | `false` |
  | `page_range` | 1 and `DOCLING_MAX_PAGES` |
  | `document_timeout` | `DOCLING_TIMEOUT` in seconds |

  Send `X-Api-Key` when `DOCLING_API_KEY` is set.
- **Rationale**:
  - The sync endpoint is capped server-side by `DOCLING_SERVE_MAX_SYNC_WAIT` (default 120 s).
    A 20-page CPU OCR job can exceed that, and the resulting status code is not documented.
  - With async, the client owns the wait (`DOCLING_TIMEOUT`), so "3 timeouts on the same file"
    is measured by us and does not depend on the server's settings.
  - One file per request avoids the zip response that multi-file requests return.
- **Alternatives**: the sync endpoint (simpler, but bound to a server limit we must not impose on
  self-hosters); sending a URL (`/v1/convert/source`; the file is local and must not be exposed);
  docling's VLM pipeline (a server-side choice, left to the owner).
- **Risk and mitigation**: the docs do not specify how `page_range` is encoded in multipart (it is
  typed as a tuple), nor the error status codes. The first adapter task checks this against
  `/openapi.json` of a current docling-serve and pins it in contract tests. The status-code
  mapping follows `capture.ClassifyHTTP`. A `failure` or `skipped` body status is permanent for
  that file. `partial_success` counts as success with whatever text came back.

## R2. Non-blocking extraction: two worker lanes, due-first scheduling

- **Decision**:
  - `capture.LaneOf(c)` puts `files` captures at `extract` in lane `documents` and everything else
    in lane `main`.
  - Run two `pipeline.Worker` loops with a lane filter.
  - `Store.NextDue(ctx, now, lane)` returns the lowest-ID capture in the lane whose
    `NextAttemptAt` has passed, else the earliest `NextAttemptAt` in the lane as the wake time.
  - Audio extraction (speech-to-text) stays in `main`.
- **Rationale**:
  - FR-114a requires that slow or waiting extraction never delays other captures.
  - A single goroutine blocked for minutes in docling would delay voice notes; a second lane
    isolates it.
  - Due-first (instead of strict head) stops a capture waiting in backoff, or collecting a batch,
    from blocking the lane. Order is still preserved in the notes store by `ReceivedAt`
    (001 FR-012a).
  - Each row belongs to exactly one lane at a time (the lane is a function of the persisted step),
    so the two loops never update the same row concurrently.
- **Alternatives**:
  - A goroutine per extraction (unbounded concurrency, harder crash semantics).
  - Async docling with a "submitted" state (keeps one lane but adds persisted task IDs and a
    polling state machine).
  - Keep strict FIFO (rejected in clarification).

## R3. Batching: join-or-create in one transaction, `collect` step

- **Decision**:
  - A `files` capture starts at step `collect`, with `NextAttemptAt = received + BATCH_QUIET_PERIOD`
    and the item's `AlbumID`.
  - `Store.AppendFile` runs inside `BEGIN IMMEDIATE`:
    1. Find the newest `files` capture of the chat at `collect`.
    2. Apply the pure `capture.JoinBatch(open, item, now, quiet)`.
    3. Either insert the file row and push `NextAttemptAt`, or insert a new capture with this file.
  - Per-item dedupe is a UNIQUE constraint on `capture_files(source, source_chat_id,
    source_message_id)`.
  - The `collect` handler simply advances to `download`.
- **Rationale**:
  - Telegram delivers each album item as its own update. The handler takes one chat's file
    messages one at a time, in delivery order (`chatQueues`), so items join in arrival order.
    The single SQLite connection plus `BEGIN IMMEDIATE` still serialises join-or-create, so two
    items can never both open a batch.
  - The worker only takes a capture whose deadline has passed. A join needs the deadline to still
    be in the future (or a matching album), so the two cannot interleave on an open batch.
  - The one exception is a late album item arriving after the worker has moved the batch past
    `collect`. It finds no open batch at `collect` and starts a new capture, with a note of its
    own. That is safe (nothing lost) and rare: album items arrive within about a second.
- **Alternatives**: an in-memory aggregator with a timer (loses items on crash before the row
  exists; violates VI); grouping only by `AlbumID` (rejected in clarification: sends of more than
  10 items span albums).

## R4. Per-file progress: `ClassProgress`

- **Decision**:
  - Add `capture.ClassProgress`.
  - `download`, `extract` and `write_note` (attachments) each handle **one** file per attempt.
  - They return `Progress` while files remain and `OK` when none are left.
  - `Policy.Next` on `Progress` keeps the step, clears attempts, waiting and misconfig state and
    the LLM budget clock, and sets `NextAttemptAt = now`.
  - The worker persists the capture, including its `Files`, after every attempt.
- **Rationale**: keeps the existing contract ("the handler returns the capture, the worker persists
  it") and makes each finished file durable before the next starts. A crash repeats at most the
  file in flight.
- **Alternatives**: handlers writing file rows directly (splits persistence between two places);
  one step per file (unbounded step list).

## R5. Hint generation: one LLM call per batch, excerpt budget

- **Decision**:
  - `enrich.BuildFilesRequest(opts, caption, excerpts)` builds one request.
  - The system prompt says the excerpts are untrusted document content to describe, never
    instructions.
  - It asks for JSON `{language, hint, text?, translated?, tasks?, tags?}`:
    - `hint`: at most 2 lines and 160 runes, in the target language.
    - `text`: the processed caption, present only when there was a caption.
  - Excerpts are taken in file order, each labelled with its file name, until a total of
    `HINT_EXCERPT_RUNES` runes is reached (default 4000; at least 200 per file while the budget
    allows).
  - `enrich.ParseFiles` validates it like `Parse`:
    - An empty or oversized hint is truncated, or rejected when empty.
    - Caption text goes through the same translation checks as text captures.
  - Validation failure follows today's rule: `LLMSkipped=invalid_output`, and the renderer's
    fallback hint is used.
- **Rationale**:
  - One call per batch matches FR-118 and keeps cost and latency low.
  - The excerpt bound implements FR-116 and the owner's "never the whole document".
  - Reusing the enrich budget, skip reasons and storage (`enriched_json`) avoids a new step.
- **Alternatives**: one call per file plus a merge call (more cost, no gain); sending docling's full
  markdown (violates FR-116).

## R6. Fallback hint (no LLM or LLM skipped)

- **Decision**: `render.FallbackHint(files)`:
  - Take the first non-empty extracted text in file order.
  - Strip markdown noise: heading marks, table pipes, image placeholders (`<!-- image -->`) and
    blank lines.
  - Take the first two lines, cap at 160 runes, append "…" when cut.
  - With no extracted text, use a placeholder:
    - `📎 <name>` for one document
    - `📷 photo` for one photo
    - `📎 N files` / `📷 N photos` for batches
    - plus the reason when every file failed, for example "couldn't read in time".
- **Rationale**: pure and deterministic (FR-117). Placeholders name the item, as the spec requires.
- **Alternatives**: the first N characters raw (markdown noise in the digest).

## R7. Vision fallback

- **Decision**:
  - `capture.NeedsVision(f, enabled)` is true only when:
    - vision is enabled, and
    - the file's kind is `image`, and
    - docling is unconfigured, or returned fewer than 20 non-space runes.
  - Request: `POST {VISION_BASE_URL}/chat/completions` with `model` and messages
    `[system: describe-and-transcribe instructions, user: [ {type:text}, {type:image_url,
    image_url:{url:"data:<mime>;base64,…"}} ]]`.
  - The answer, capped at 2000 runes, is stored as the file's extracted text with source `vision`.
  - Failure handling matches the LLM step:
    - bounded attempts (3 attempts or 2 minutes, per file)
    - then the file is marked `empty` and a placeholder hint is used
    - no `waiting_on`, no queued reply
- **Rationale**:
  - The OpenAI-compatible content-part format is accepted by LiteLLM and most gateways, so there
    are no provider-specific features (FR-122).
  - Keeping vision output as "extracted text" lets the normal hint step summarise it, or the
    fallback hint use it.
- **Alternatives**: asking vision directly for the final hint (bypasses caption processing and
  batch-level hinting); sending documents rendered to images (forbidden by the owner).

## R8. Attachment names

- **Decision**:
  - Audio keeps today's `voice-<local date>.<ext>`.
  - Other files use their original Telegram file name when present: sanitised to a base name,
    control characters removed, at most 100 bytes, extension kept.
  - Otherwise `photo-<local date>-<n>.jpg`, `video-<local date>-<n>.<ext>` or
    `file-<local date>-<n><ext>`.
  - Duplicates within a capture get ` (2)`, ` (3)` before the extension.
  - Names are computed deterministically from the capture's files in order.
- **Rationale**: Memos `Attach` dedupes by filename, so names must be stable across retries and
  unique within the note. Original names help the owner in the morning.
- **Alternatives**: always generated names (loses useful "invoice-2026-10.pdf").

## R9. Local file storage: `$DATA_DIR/files`

- **Decision**:
  - Rename the store abstraction from `AudioStore`/`DirAudio` to `FileStore`/`DirFiles`.
  - The directory becomes `$DATA_DIR/files`.
  - On startup, before the workers run: if `audio/` exists and `files/` does not, `os.Rename`
    it. If both exist, move the entries of `audio/` into `files/` and remove the empty `audio/`.
  - Stored paths stay relative names, so rows remain valid.
  - Ingest file names follow `tg-<chat>-<msg>-*<ext>`, as today.
  - The orphan sweep scans `files/` and references `capture_files.local_path`.
- **Rationale**: the name matches the content. The one-time rename is atomic on the same volume
  and keeps queued captures valid across the upgrade.
- **Alternatives**: keep `audio/` (misleading forever).

## R10. Migration 002

- **Decision**:
  1. Create `capture_files`.
  2. For every capture with a `file_id` or `audio_path`, insert one row: kind `audio`, position 0,
     the capture's source identity, `file_id`, `file_size`, `mime_type`, `local_path=audio_path`,
     state `pending` (or `done` with the transcript in `captures.text` when step is past
     `transcribe`).
  3. `UPDATE captures SET step='extract' WHERE step='transcribe'`.
  4. Add `captures.album_id`.
  5. Drop `file_id`, `file_size`, `mime_type` and `audio_path` from `captures` (SQLite
     `ALTER TABLE … DROP COLUMN`, supported by modernc's SQLite).
- **Rationale**: one model for all kinds, forward-only like 001, and in-flight captures survive
  the upgrade. A restart test covers it (`restart_test.go` pattern).
- **Alternatives**: keep the legacy columns for audio (two code paths forever).

## R11. Telegram mapping and limits

- **Decision**: `fromTele` maps:

  | Telegram field | Becomes |
  |---|---|
  | `m.Photo` | kind image, MIME `image/jpeg` (the Bot API serves the largest size as JPEG) |
  | `m.Document` | audio if MIME is `audio/*` (FR-105), image if `image/*`, else document; name and MIME kept |
  | `m.Video` / `m.Animation` | kind video |
  | `m.Caption` | caption |
  | `m.AlbumID` | album ID |

  Other behaviour:
  - Files over 20 MB, judged by reported size or by the download error:
    - standalone (no album, no open batch) and reported as over 20 MB: rejected with the existing
      "too large" reply
    - standalone, but found too large only when downloaded: it is already stored as a batch of
      one, so it is marked `failed` and becomes a placeholder note with the reply
      `⚠ files not kept — <item> (too large for Telegram bots) · please send them again`
    - otherwise: recorded in the batch as `failed` ("too large for Telegram bots"), and the reply
      names it (FR-126)
  - `TextUnsupported` changes to "VoiceNotes takes voice, text, photos and files."
  - `TextHelp` is updated the same way.
- **Rationale**: matches the spec's accepted types and keeps the existing size behaviour for
  single items.
- **Alternatives**: downloading the smaller photo sizes (worse OCR).

## R12. Health

- **Decision**:
  - Add `capture.WaitingDocling = "docling"`.
  - Health's degraded rule already reports the oldest unfinished capture's `WaitingOn` past the
    threshold, and docling shows up through it.
  - The collector's `OldestUnfinished` is unchanged (lowest ID across lanes).
  - `Worker.Alive` becomes "both lanes alive" in `cmd/voicenotes/health.go`.
  - Vision never appears in health: like the LLM, it degrades silently (FR-128 is satisfied for
    docling; vision has no waiting state).
- **Rationale**: minimal change, consistent with how speech-to-text is reported.
- **Alternatives**: active probing of docling (`/version`) (adds network calls from health; not
  done for other dependencies either).
