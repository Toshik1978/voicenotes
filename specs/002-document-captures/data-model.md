# Data Model: Document and photo captures

This extends `specs/001-voice-capture-notes/data-model.md`. Only changes are listed.

## Capture (table `captures`)

| Field | Change |
|---|---|
| `kind` | adds `files` (photo/document/video batch). `audio` and `text` unchanged. |
| `album_id` | **new**, TEXT NULL. Telegram media group of the batch's first album item. |
| `text` | for `files`: the captions of all items joined with a blank line, in position order (the owner's text). For `audio`, the transcript is now per file (see below), and `text` holds the joined transcripts once extraction finishes, so `enrich` and `render` are unchanged. |
| `enriched_json` | for `files`: `enrich.Result` with the new `hint` field. |
| `step` | adds `collect`. `transcribe` is renamed `extract`. |
| `waiting_on` | adds `docling`. |
| `file_id`, `file_size`, `mime_type`, `audio_path` | **removed** (moved to `capture_files`). |

Validation (`capture.New`):

| Kind | Allowed start step |
|---|---|
| `files` | `collect` |
| `audio` | `download` or `extract` (was `transcribe`) |
| `text` | `enrich` |

## Capture file (table `capture_files`, new)

| Column | Type | Notes |
|---|---|---|
| `id` | INTEGER PK | |
| `capture_id` | INTEGER NOT NULL | FK → `captures(id)` ON DELETE CASCADE |
| `position` | INTEGER NOT NULL | 0-based order in the send; UNIQUE with `capture_id` |
| `source`, `source_chat_id`, `source_message_id` | TEXT/INTEGER NOT NULL | UNIQUE triple: per-item dedupe (FR-108) |
| `kind` | TEXT NOT NULL | `audio` \| `image` \| `document` \| `video` |
| `file_id` | TEXT | Telegram file ID; cleared at cleanup |
| `mime_type` | TEXT | |
| `file_name` | TEXT | original name, if any; cleared at cleanup |
| `file_size` | INTEGER | reported or actual |
| `local_path` | TEXT | name relative to `$DATA_DIR/files`; empty when not held |
| `state` | TEXT NOT NULL | `pending` \| `done` \| `empty` \| `failed` \| `skipped` |
| `text` | TEXT | extracted text or transcript; cleared at cleanup |
| `text_source` | TEXT | `stt` \| `docling` \| `vision` |
| `language` | TEXT | audio only: detected language |
| `failure` | TEXT | secret-free reason for `failed`, shown in the hint/reply |
| `timeouts` | INTEGER NOT NULL DEFAULT 0 | docling timeouts on this file |
| `vision_attempts` | INTEGER NOT NULL DEFAULT 0 | failed vision-model attempts on this file (budget 3 or 2 minutes) |
| `vision_started_at` | INTEGER | Unix ns of the first failed vision attempt; NULL until then |
| `attached` | INTEGER NOT NULL DEFAULT 0 | 1 once attached to the note (or attach failed permanently, see `attach_failure`) |
| `attach_failure` | TEXT | reason the notes store refused this attachment |

The 001 `captures.UNIQUE(source, chat, message)` stays. For a batch it holds the first item's
identity, which is also the message the ⏳ reply answers. The first item is therefore inserted in
both tables, and a redelivery of it is caught by either constraint.

## File states

```text
pending ──extract ok, text found──────────▶ done
pending ──extract ok, no/too little text──▶ empty   (vision may run first, see R7)
pending ──permanent failure / 3 timeouts──▶ failed  (reason in failure)
pending ──video, or no reader configured──▶ skipped
```

`download` moves nothing. It fills `local_path`, or sets `failed` when too large or rejected.

## Steps

```text
files:  collect ─▶ download ─▶ extract ─▶ enrich ─▶ write_note ─▶ finalize_reply ─▶ cleanup ─▶ done
audio:             download ─▶ extract ─▶ enrich ─▶ write_note ─▶ finalize_reply ─▶ cleanup ─▶ done
text:                                     enrich ─▶ write_note ─▶ finalize_reply ─▶ cleanup ─▶ done
```

| Step | Behaviour |
|---|---|
| `collect` | Due at `NextAttemptAt` (last item + quiet period). The handler returns OK. A capture at `collect` is the chat's **open batch** until it is picked. |
| `download` / `extract` / `write_note` (attach part) | One file per attempt. `Progress` while files remain, `OK` after the last. |
| `extract` on `audio` | Also sets `captures.text` and `captures.language` from the file when done, keeping `enrich` and `render` unchanged for voice notes. |
| Permanent failure of a whole step | Keeps 001 semantics: placeholder note. Per-file failures never fail the step; they are recorded on the file. |

## Outcome classes

| Class | Effect |
|---|---|
| `ok`, `temporary`, `misconfig`, `permanent` | unchanged |
| `progress` | **new**: stay on the step, reset attempts, waiting, misconfig and LLM-budget fields, due now. Never sets `QueueReplyEdit`. |

## Lanes

| Lane | Contains | Handlers |
|---|---|---|
| `documents` | `kind='files' AND step='extract'` | `extract` |
| `main` | everything else unfinished | all steps; `extract` only ever sees `audio` here |

`NextDue(lane)` returns the lowest-ID capture in the lane with `next_attempt_at <= now`, or else the
minimum `next_attempt_at` in the lane as the wake time. A capture changes lane only through a
persisted step change, so the two loops never touch the same row at once.

## Batching rule (`capture.JoinBatch`)

Input: the chat's newest `files` capture at `collect` (if any), the item (album ID, receive time),
`now` and `quiet`.

- **Join** when an open batch exists and (`open.NextAttemptAt > now` or
  (`item.AlbumID != ""` and `item.AlbumID == open.AlbumID`)). On join, `NextAttemptAt = now +
  quiet`, the file gets `position = max + 1`, and a non-empty caption is appended to `text`.
- **New batch** otherwise.
- `audio` and `text` items never call it (FR-106).

## Configuration (additions)

| Field | Env | Default |
|---|---|---|
| DoclingURL | `DOCLING_URL` | unset (extraction off) |
| DoclingAPIKey | `DOCLING_API_KEY` | unset |
| DoclingTimeout | `DOCLING_TIMEOUT` | `10m` |
| DoclingMaxPages | `DOCLING_MAX_PAGES` | `20` |
| BatchQuietPeriod | `BATCH_QUIET_PERIOD` | `3s` |
| HintExcerptRunes | `HINT_EXCERPT_RUNES` | `4000` |
| VisionEnabled | `VISION_ENABLED` | `false` |
| VisionBaseURL / APIKey / Model | `VISION_BASE_URL` / `VISION_API_KEY` / `VISION_MODEL` | fall back to `LLM_*` |
| VisionTimeout | `VISION_TIMEOUT` | `60s` |

## Retention

Unchanged policy (001 FR-012):

- Cleanup removes local files.
- Cleanup clears `captures.text`, `enriched_json`, and `capture_files.text`, `file_id` and
  `file_name`.
- `capture_files` rows keep their source identity for dedupe and are deleted with their capture by
  `PurgeDone` (cascade).
