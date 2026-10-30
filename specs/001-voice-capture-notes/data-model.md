# Data Model: VoiceNotes

Local state is a SQLite file `$DATA_DIR/voicenotes.db` (WAL). The schema is embedded in the binary
and versioned with `PRAGMA user_version`, with forward-only migrations applied at startup. Notes
themselves live in Memos and are never mirrored locally.

## Capture (`captures`)

One incoming thought, from receipt until its note exists and the reply is final.

| Field | Type | Notes |
|---|---|---|
| `id` | INTEGER PK | Arrival order; the worker processes ascending `id` |
| `source` | TEXT | `telegram` (later `http`) |
| `source_chat_id` | INTEGER | Telegram chat |
| `source_message_id` | INTEGER | Telegram message; **UNIQUE(source, source_chat_id, source_message_id)** = dedupe (FR-006) |
| `kind` | TEXT | `audio` \| `text` |
| `received_at` | TIMESTAMP | Telegram message date; becomes the note's `createTime` (FR-012a) |
| `file_id` | TEXT NULL | Telegram file id (audio) |
| `file_size` | INTEGER NULL | Rejected at ingest if over 20 MB |
| `mime_type` | TEXT NULL | e.g. `audio/ogg` |
| `audio_path` | TEXT NULL | Relative to `$DATA_DIR/audio`; set after download, cleared after cleanup |
| `text` | TEXT NULL | Original text (text captures) or transcript |
| `language` | TEXT NULL | ISO-639-1 detected/forced language; NULL if unknown |
| `enriched_json` | TEXT NULL | Validated LLM result (see contracts/external-apis.md) |
| `llm_skipped` | TEXT NULL | Reason if the LLM step was skipped (`disabled`, `unavailable`, `invalid_output`) |
| `failure` | TEXT NULL | Permanent failure reason that produced a placeholder note |
| `note_name` | TEXT NULL | Memos `memos/<uid>`; set once written |
| `note_url` | TEXT NULL | Public link |
| `task_count` | INTEGER | For the confirmation line |
| `reply_message_id` | INTEGER NULL | The ⏳ reply to edit; NULL until ⏳ has been sent (the download step sends it when ingest could not) |
| `step` | TEXT | Next step to run (see below) |
| `status` | TEXT | `pending` \| `waiting` \| `done` |
| `waiting_on` | TEXT NULL | `telegram` \| `stt` \| `memos`, used for the reply text and health |
| `attempts` | INTEGER | Attempts at the current step |
| `next_attempt_at` | TIMESTAMP | Backoff schedule |
| `updated_at` | TIMESTAMP | |

### Steps (state machine, pure in `internal/capture`)

```text
audio: download → transcribe → enrich → write_note → finalize_reply → cleanup → (done)
text:                          enrich → write_note → finalize_reply → cleanup → (done)
```

- `enrich` is a no-op pass-through when the LLM is not configured. Its outcome is stored in
  `llm_skipped`.
- Outcome per attempt:
  - `ok`: advance the step, reset `attempts`, status `pending`.
  - `temporary`: status `waiting`, `attempts++`, `next_attempt_at = now + backoff(attempts)`. On
    the *first* transition into `waiting`, the ⏳ reply is edited to the "queued" text.
  - `permanent` in `download`, `transcribe` or `enrich`: set `failure` and jump to `write_note`
    with placeholder text (FR-010).
  - `permanent` in `write_note` (e.g. Memos rejects the payload): retry once with a minimal note
    (placeholder text plus `#inbox`, no attachment). If that also fails, treat it as temporary.
    The capture is never dropped.
  - `permanent` in `finalize_reply` (message deleted or not editable): send a new silent reply
    instead, then treat the step as `ok`.
- The LLM step has a bounded budget: after 3 attempts or 2 minutes it becomes `ok` with
  `llm_skipped='unavailable'`.
- `cleanup` deletes the audio file and NULLs `text`, `language`, `enriched_json`, `audio_path`,
  `file_id`. The row (IDs only) remains for dedupe.
- **Purge**: rows with `status='done'` and `updated_at < now − 30 days` are deleted daily.
- **Ingest invariant** (Constitution VI, research R2): for audio, the file is written and the row
  committed with `audio_path` set *before* ⏳ is sent. If the Telegram download fails, the row is
  committed with only `file_id` at `step=download` and no reply is sent; the `download` step sends
  ⏳ (or the "queued" text) after the audio is on disk. A failed ⏳ send never undoes the row:
  `finalize_reply` falls back to a new message.

### Validation rules

- `kind='audio'` requires `file_id`. `kind='text'` requires non-empty `text`.
- Files over 20 MB are not inserted. The ingest handler replies "too large" (Edge Cases).
- The allowlist is checked before any insert. Non-allowlisted updates are dropped without a reply.

## Digest run (`digest_runs`)

| Field | Type | Notes |
|---|---|---|
| `local_date` | TEXT PK | `YYYY-MM-DD` in `TZ`; at most one per day (FR-027) |
| `status` | TEXT | `sent` \| `empty` \| `skipped` (catch-up window passed or Memos unreachable) |
| `sent_at` | TIMESTAMP | |
| `note_count` | INTEGER | |

## Digest seen (`digest_seen`)

| Field | Type | Notes |
|---|---|---|
| `note_name` | TEXT PK | Memos `memos/<uid>` |
| `first_shown` | TEXT | `local_date` of the first digest that listed it |

A note is **new** if it has no row here. After each sent digest, listed notes are inserted and
rows for notes no longer in `#inbox` are deleted, so the table stays as small as the inbox.

## Note (in Memos, not stored locally)

Rendered by `internal/render` (FR-022):

```text
<main text: tidied/translated, or raw transcript, or placeholder>

- [ ] <action item 1>
- [ ] <action item 2>

> <original transcript>          ← only when translated; preceded by "*Original (ru):*"

#inbox #todo #<suggested>…        ← #todo only with action items
```

Visibility is `PRIVATE`. `createTime` is set to `received_at`. Audio is attached as a Memos
attachment. When the content exceeds the limit (default 8 KiB, configurable), the original quote
is shortened first, then the main text, each marked "(truncated)". Tags and the checklist are
never cut.
