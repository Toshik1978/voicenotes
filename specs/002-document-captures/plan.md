# Implementation Plan: Document and photo captures

**Branch**: `002-document-captures` (work lands on `main`, per Constitution VIII) | **Date**: 2026-11-01 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/002-document-captures/spec.md`

## Summary

VoiceNotes gains a third capture kind, **files**. These are photos, documents and videos sent to
the bot. Items from the same chat that arrive within a quiet period, or share a Telegram album,
merge into one **batch** capture. It yields one `#inbox` note with every file attached and a
one-or-two-line **hint** of what the batch is.

The hint comes from content. An optional, locally run **docling-serve** extracts text from each
file. The existing optional LLM step turns a bounded excerpt of that text, plus any caption, into
the hint. An optional **vision fallback** sends a single photo to an OpenAI-compatible
vision-capable chat model, and only when docling found too little text in it. With nothing
configured, files are still accepted and saved with a placeholder hint.

Internally, "one capture holds one audio file" becomes "one capture holds zero or more files". A
voice note is a capture with one audio file. The `transcribe` step becomes `extract`, which
handles one file per attempt: speech-to-text for audio, docling (plus the optional vision
fallback) for images and documents, nothing for video. Document extraction runs in a second
worker lane, so a slow scan never delays voice or text captures.

No new Go dependencies are added: docling-serve and the vision model are reached over plain
`net/http`.

## Chosen approach (maintainer decision)

| | A. One capture holds many files **(chosen)** | B. One capture per message, merged at write | C. Separate document pipeline |
|---|---|---|---|
| Album → one note | Items are appended to one capture | Sibling captures coordinate at `write_note` | Same problem as B, in a new pipeline |
| Idempotency / crash safety | Per-file results on one row set; existing step model | Cross-capture "all siblings ready" and merge recovery | Duplicates the retry, backoff, placeholder and reply machinery |
| Cost | Schema migration plus the audio → file rename (mechanical, broad) | Less renaming; hard-to-test coordination | Most code |

**Why A**: the expensive part is mechanical renaming. The risky part, batching, stays a pure rule
in `internal/capture` that can be tested on its own. B makes captures depend on each other, which
is exactly what Constitution VI makes dangerous. C duplicates everything.

Also decided with the owner:

- docling-serve is the primary reader. Full documents never leave the owner's infrastructure.
- The LLM only ever receives a bounded excerpt.
- The vision fallback is photos only, one photo per request, and off by default.
- Nothing new is a hard dependency.
- Extraction must not block other captures. Notes are ordered by receive time.
- A conversion that times out 3 times on the same file degrades to a placeholder.
- Captures from a send of more than 10 items merge across albums by quiet period.

## Technical Context

**Language/Version**: Go (toolchain in `go.mod`, currently 1.27)

**Primary Dependencies**: unchanged. The four approved ones are `gopkg.in/telebot.v4`,
`go.uber.org/zap`, `github.com/caarlos0/env/v11` and `modernc.org/sqlite`. docling-serve and the
vision model are reached with stdlib `net/http` and `mime/multipart`. No new dependency is
requested.

**Storage**: the same SQLite file. Migration `002_capture_files.sql` adds the `capture_files` table
and moves the audio columns into it. Local files live in `$DATA_DIR/files/`. The old
`$DATA_DIR/audio/` directory is renamed to `files/` once at startup.

**Testing**: stdlib `testing`, table-driven, hand-written fakes, `httptest` stand-ins for
docling-serve and the vision endpoint. All new decision logic goes into packages already in
`COVERPKG`:

- batching: `capture`
- per-file extraction policy and the vision decision: `capture`/`pipeline`
- hint prompt and validation: `enrich`
- fallback hint and the layout of file notes: `render`
- new settings: `config`
- docling as a dependency in the health report: `health`

The new adapter `internal/docling` is outside the gate (I/O only), but its tests run.
`COVERPKG` is unchanged.

**Target Platform**: Linux container, distroless, `CGO_ENABLED=0`, amd64 and arm64.

**Project Type**: single long-running web service (bot plus health endpoint).

**Performance Goals**:

- SC-106: a photo becomes a note within 60 s.
- SC-106: a 10-page document becomes a note within 3 min with all dependencies up.
- Voice capture latency is unchanged while documents are being read (FR-114a).

**Constraints**:

- Constitution I–VIII.
- Content of documents is never logged.
- The LLM excerpt is at most `HINT_EXCERPT_RUNES` (default 4000).
- At most `DOCLING_MAX_PAGES` pages (default 20) are converted.
- 20 MB per file (Telegram bot limit).
- User-facing text stays within Telegram limits.

**Scale/Scope**: single owner. A few batches a day, up to about 30 files per batch.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (below).*

| Principle | Status | How this plan complies |
|---|---|---|
| I. Idiomatic Go, stdlib first | ✅ | New code follows the existing adapter/pure-logic split. docling and vision clients use `net/http` and `mime/multipart` only. Errors wrapped with `%w`. |
| II. Test-first, stdlib testing | ✅ | Every task is test-first. Batching, per-file progress, timeout counting, the vision decision, hint prompt/parse and fallback hint are pure and table-tested in gated packages. Adapters are tested against `httptest`. `COVERPKG` unchanged. |
| III. Dependency discipline | ✅ | No new dependency. |
| IV. Static, CGO-free | ✅ | No cgo. No new binary or shell in the image: docling runs as the owner's separate service. |
| V. Fail fast on config, degrade on dependencies | ✅ | New settings validated at startup, naming the variable, never the value. Docling and vision are wired only when configured, with plain conditionals in `main()`. An unreachable docling makes captures wait (`waiting_on=docling`) and health degraded; the process never exits. |
| VI. No capture lost | ✅ | Every item's row commits on arrival. The item that creates a batch is then downloaded, written and fsynced, and its local path recorded, before its ⏳; if that download fails it stays stored without data, no ⏳ is sent on arrival, and the download step sends the ⏳ once it holds the file. Joining items stay row-first and are downloaded by the pipeline: the ⏳ acknowledges the capture as it exists then (the first item, on disk), and later items are confirmed by the final reply after the note. The quiet period restarts after the ack, so a slow first download cannot split an album. Extraction records per-file results, one file per attempt, so a crash never repeats a finished file. Permanent failures degrade to a note with the attachments and a placeholder hint. Local files are deleted only after the note is saved. |
| VII. Public repo hygiene | ✅ | Docs and examples use `docling.example.org`, `llm.example.org` and fake keys. No real endpoints or model deployments. |
| VIII. Conventional Commits | ✅ | Unchanged workflow. Commits like `feat(capture): …` and `feat(docling): …`. |

No violations. Complexity Tracking stays empty.

**Spec deviations from 001, approved in this spec**:

- FR-004 is superseded by FR-101 (photos and files are accepted).
- FR-011 strict FIFO is relaxed by FR-114a (lanes, receive-time ordering).
- FR-009 "retry forever" gets one exception (3 timeouts on the same file, FR-113).

## Design overview

### Capture model (pure, `internal/capture`)

- **Kinds**: `audio`, `text` (unchanged) and new `files`. Audio sent as a document stays `audio`
  (FR-105). Voice, audio and text never join a batch.
- **Files**: `Capture.Files []File`, loaded and persisted with the capture. A `File` holds its
  source message ID (per-item dedupe), position, kind (`audio|image|document|video`), Telegram
  file ID, MIME, original name, size, local path, extracted text and its source
  (`stt|docling|vision`), per-file state (`pending|done|empty|failed|skipped`), failure reason,
  timeout count and an attached flag.
- **Steps**: `collect → download → extract → enrich → write_note → finalize_reply → cleanup →
  done`.
  - `collect` exists only for `files` captures. Its deadline is `NextAttemptAt` (the quiet
    period); the handler just advances.
  - `transcribe` is renamed to `extract`. The migration rewrites stored step values.
- **Progress outcome**: a new `ClassProgress` outcome. A handler that finished one file (download
  or extract) but has more to do returns it. `Policy.Next` keeps the step, resets attempts and
  waiting state, and makes the capture due now. Each finished file is therefore persisted by the
  worker before the next one starts (Constitution VI).
- **Batching rule** (`capture.JoinBatch`): an incoming file item joins the chat's open batch when
  the batch is at `collect` and either its `NextAttemptAt` (last item + quiet period) is after
  `now`, or the item's album ID equals the batch's album ID. Joining pushes `NextAttemptAt` to
  `now + quiet`. After the creating item's ⏳ is sent, still under the ack hold, the deadline is
  pushed to `now + quiet` again, so items queued behind a slow ⏳ still join (T030). Otherwise the item starts a new batch. The rule is pure, and the store applies
  it inside one write transaction (see research R3).
- **Lanes** (`capture.LaneOf`): `documents` = `files` captures at `extract`; `main` = everything
  else. Each lane's `NextDue` returns the lowest-ID **due** capture in that lane (no head-of-line
  blocking). It sleeps until the earliest `NextAttemptAt` in the lane.
- **Timeouts**: the extract handler marks a docling timeout on the file. The third timeout makes
  the file `failed` with reason "couldn't read in time" and returns `Progress`. An unreachable
  service is `Temporary` with `WaitingDocling` and is retried as usual.

### Pipeline (`internal/pipeline`)

- Two `Worker` instances share the store, each with a lane filter. The `main` lane has handlers
  for every step except document extraction. The `documents` lane has only the `extract` handler.
  Health reports unhealthy if either loop stops.
- **download**: per file. Audio is normally written at ingest, and so is the item that created a
  batch. Every file item's row commits on arrival with its file ID (T029), so batching follows
  arrival order; joining items, and a first item whose arrival download failed, are fetched here
  by file ID, one per attempt (`Progress`), as is audio whose ingest download failed. A file too
  large or rejected by Telegram is marked `failed`. The batch's ⏳ is sent at arrival by the
  handler for the item that created it, once that item is on disk; download sends one only when
  that download or the ack failed.
- **extract** (per file, one per attempt):
  - audio → speech-to-text (today's logic, incl. language recheck)
  - image/document → docling if configured, then vision if allowed by
    `capture.NeedsVision(file, cfg)`
  - video → `skipped`
  - no reader configured → `skipped`
- **enrich**:
  - `audio`/`text` captures: unchanged.
  - `files` captures: one call with `enrich.BuildFilesRequest(opts, caption, excerpts)`, where the
    excerpts are bounded in total by `HINT_EXCERPT_RUNES` across files. It returns
    `Result{Hint, Text (processed caption), Tasks, Tags, Translated, Language}`.
  - No LLM, or the LLM skipped → no call. The renderer computes the fallback hint.
- **write_note**: renders with `render.Input{Caption, Hint, Files…}` and creates the note. It then
  attaches each file not yet `attached`, one per attempt (`Progress`). Per-file permanent attach
  failures are recorded and do not block the note (FR-126). Attachment names are deterministic and
  unique within the capture (research R8).
- **finalize_reply**: the reply mentions items not kept or not attached (contracts/telegram-ux.md).
- **cleanup**: removes every local file and clears extracted text, caption, enrich output and
  file IDs. File rows are kept with their source message IDs for dedupe until `PurgeDone`.

### Adapters

- `internal/docling` (new): an async conversion client.
  1. `POST /v1/convert/file/async` with multipart `files` plus options.
  2. Poll `GET /v1/status/poll/{task_id}`.
  3. `GET /v1/result/{task_id}`.
  4. Return `document.md_content`.

  Optional `X-Api-Key`. The client applies `DOCLING_TIMEOUT` per file and classifies outcomes
  into `capture.Outcome` (contracts/external-apis.md).
- `internal/openai`: a new `Vision.Describe(ctx, prompt, image, mime)` sends a chat request whose
  user content is a part array (`text` + `image_url` data URL), the OpenAI-compatible form.
  `Chat.Complete` is unchanged.
- `internal/telegram`: `fromTele` maps `Photo` (largest size), `Document`, `Video`, `Caption` and
  `AlbumID`. Audio-MIME documents map to `Audio`. `Handler` gains `files()`.
  1. Call `Ingester.IngestFile`, which joins or creates the batch in one transaction.
  2. Only when a new batch was created: download the item (bounded by
     `pipeline.ArrivalDownloadLimit`), write and fsync it, and record its local path. A joining
     item is left to the download step.
  3. Send ⏳ only for a created batch whose item is now on disk; then restart the quiet period.
     If step 2 failed, no ⏳ is sent here and the download step sends it later.
- `internal/store/sqlite`: migration 002; `Insert`/`Update`/`queryOne` handle `Files`; new
  `AppendFile` (join-or-create in one `BEGIN IMMEDIATE` transaction); `NextDue(ctx, now, lane)`;
  `FilePaths` (renamed `AudioPaths`, now over `capture_files`).
- `internal/memos`: unchanged API (`Attach` already dedupes by filename).

### Configuration (contracts/configuration.md)

New optional variables, all validated at startup:

- `DOCLING_URL`, `DOCLING_API_KEY`, `DOCLING_TIMEOUT` (10m), `DOCLING_MAX_PAGES` (20)
- `BATCH_QUIET_PERIOD` (3s), `HINT_EXCERPT_RUNES` (4000)
- `VISION_ENABLED` (false), `VISION_BASE_URL`, `VISION_API_KEY`, `VISION_MODEL`, `VISION_TIMEOUT`
  (60s)

The `VISION_*` endpoint settings default to the `LLM_*` ones when unset. `VISION_ENABLED=true`
with no resolvable URL or model is a startup error naming the variable.

## Project Structure

### Documentation (this feature)

```text
specs/002-document-captures/
├── plan.md              # This file
├── research.md          # Phase 0: decisions R1–R12
├── data-model.md        # Phase 1: capture/file model, steps, lanes, migration
├── quickstart.md        # Phase 1: validation scenarios
├── contracts/
│   ├── telegram-ux.md   # accepted messages, batching, replies (delta to 001)
│   ├── external-apis.md # docling-serve and vision requests/classification
│   └── configuration.md # new environment variables
├── checklists/requirements.md
└── tasks.md             # Phase 2 (/speckit-tasks)
```

### Source Code (repository root)

```text
cmd/voicenotes/
├── service.go        # wire docling/vision conditionally; two worker lanes; audio→files dir rename
├── adapters.go       # docling + vision adapters to pipeline interfaces
└── health.go         # both lanes in liveness
internal/
├── capture/          # KindFiles, File, StepCollect/StepExtract, ClassProgress, JoinBatch, LaneOf, NeedsVision
├── pipeline/         # per-file download/extract/attach; FileStore (renamed AudioStore); lanes; ingest of file items
├── enrich/           # BuildFilesRequest / ParseFiles (hint + caption processing), excerpt budget
├── render/           # caption + hint layout, FallbackHint, placeholder hints
├── config/           # new variables and validation
├── health/           # docling as a waiting dependency
├── docling/          # NEW adapter: async convert client (outside the coverage gate)
├── openai/           # + Vision.Describe
├── telegram/         # photo/document/video/caption/album mapping; files() ingest; reply texts
├── store/sqlite/     # migration 002, capture_files, AppendFile, lane-aware NextDue
└── memos/            # unchanged
README.md, .env.example # optional docling/vision setup, anonymised
```

**Structure Decision**: keep the existing layout. Add one adapter package (`internal/docling`) next
to `internal/openai` and `internal/memos`. Pure logic extends the gated packages, so `COVERPKG` in
`Taskfile.yml` and CI stays identical.

## Post-design Constitution Check

Re-checked against data-model.md and the contracts:

- VI holds:
  - The item that creates a batch is fsynced, and its local path recorded, before its ⏳; a
    joining item's row commits first and the pipeline downloads it. The ⏳ acknowledges the
    batch as it exists then.
  - `ClassProgress` persists every per-file result before the next one.
  - Join-or-create happens in one write transaction.
  - Cleanup runs after `write_note`.
- V holds:
  - Docling and vision are nil when unconfigured, and handlers mark files `skipped`.
  - Docling outages are `Temporary` with `WaitingDocling`.
- II holds: no I/O added to gated packages.

No new violations.

## Complexity Tracking

No Constitution violations to justify.
