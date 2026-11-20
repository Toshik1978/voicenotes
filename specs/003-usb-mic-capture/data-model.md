# Data Model: Wireless microphone captures

This file covers the changes from feature 002's model. Pure types live in `internal/mic` (new,
gated; shared by the service and the recorder) and `internal/capture`. Persistence is in `internal/store/sqlite` (migration
`004_mic.sql`) and the recorder spool.

## Recording (recorder side, `internal/mic` + `internal/recorder`)

| Field | Type | Notes |
|---|---|---|
| ID | int64 > 0 | Random int63 drawn at start (research R6). Decimal in file names and URLs. |
| Recorder | string | `MIC_RECORDER_NAME`. Defaults to the hostname. |
| StartedAt | time (UTC, ns) | Taken from the first tap. It becomes the capture's `ReceivedAt` (FR-221). |
| EndedAt | time | Set when the recording stops. |
| EndReason | enum | `tap`, `max_length`, `receiver_lost`, `interrupted` (FR-207). |
| Audio | WAV, 16 kHz, mono, s16le | Spool file `<id>.part.wav` while recording, then `<id>.wav`. |
| State | enum | `recording` → `finished` → (`discarded` \| `handed_over`). `handed_over` deletes the spool files. |

**Spool layout** (`MIC_SPOOL_DIR`, on the recorder's host; default `$STATE_DIRECTORY`, else `/var/lib/voicenotes-mic`):

```text
<id>.json        sidecar: {id, recorder, started_at, ended_at?, end_reason?}; written before the first sample
<id>.part.wav    while recording; fsync every 2 s
<id>.wav         finished; header sizes patched
broken/          unrecoverable leftovers (never silently deleted)
```

**Rules**

- Under `MIC_MIN_LENGTH` (default 2 s): `discarded`. The spool files are deleted and the discard
  notice is sent (FR-209).
- At `MIC_MAX_LENGTH` (default 10 m): stops with reason `max_length` (FR-204).
- Hand-off happens oldest `StartedAt` first. A finished recording stays in the spool until the
  hand-off returns **stored** or **duplicate** (FR-213).
- When the spool's filesystem has less than `MIC_SPOOL_MIN_FREE` free (default 256 MiB, about 13
  maximum-length recordings), a tap does not start a recording and `storage_full` is reported
  (FR-215).

## Tap state machine (`mic.Toggle`, pure)

The state is `idle` or `recording{id, started_at, last_toggle_at}`. Events go in, actions come out:

| State | Event | Condition | Next state | Actions |
|---|---|---|---|---|
| idle | Tap(t) | t − last_toggle < debounce | idle | — |
| idle | Tap(t) | storage full or no audio device | idle | `Refuse(reason)` |
| idle | Tap(t) | otherwise | recording | `Start(id, t)` |
| recording | Tap(t) | t − last_toggle < debounce | recording | — |
| recording | Tap(t) | t − started < min length | idle | `Discard(id)` |
| recording | Tap(t) | otherwise | idle | `Stop(id, tap)` |
| recording | Tick(t) | t − started ≥ max length | idle | `Stop(id, max_length)` |
| recording | DeviceLost(t) | t − started < min length | idle | `Discard(id)` |
| recording | DeviceLost(t) | otherwise | idle | `Stop(id, receiver_lost)` |

`last_toggle_at` is updated on every Start, Stop and Discard. After a `max_length` stop, the next
tap starts a new recording, as the spec edge case "Missed stop tap" describes.

## Capture changes (`internal/capture`)

- `Source` gains a value `"mic"` (constant `capture.SourceMic`). For a recording:
  - `SourceChatID` = the notification chat (`MIC_NOTIFY_CHAT_ID`, defaulting to `DIGEST_CHAT_ID`)
  - `SourceMessageID` = the recording ID
  - `Kind` = `audio`
  - first step `extract`
  - `ReceivedAt` = `StartedAt`

  The single `File` has `Source = "mic"` and the same chat and message identity, so it meets the
  unique key on `capture_files`. Its `Kind` is `audio` and `MimeType` is `audio/wav`.
- New field **`NoSpeech bool`**, set by `extract` when an audio capture's transcript is empty
  after a successful transcription.
- `Policy.Next`: when an OK result at `extract` has `NoSpeech`, the next step is `finalize_reply`,
  skipping `enrich` and `write_note`. The step list itself is unchanged.
- Reply text for `NoSpeech`: "🔇 no speech heard — not saved".
- `cleanup` for `NoSpeech && Source == "mic"` moves each local file to
  `$DATA_DIR/nospeech/<id>.wav` (rename and directory fsync) instead of deleting it. Telegram
  sources delete as before.

## Recorder status (`mic.Alerts`, pure; stored in `mic_recorders`)

| Field | Type | Notes |
|---|---|---|
| Name | string | Recorder name; primary key. |
| LastSeen | time | Server time of the last check-in. Every recorder checks in over HTTP, including one on the service's host. |
| ReceiverPresent | bool | From the check-in. |
| StorageFull | bool | From the check-in. |
| Alert | enum | `ok`, `missing_receiver`, `storage_full`, `silent`. The outstanding alert. |

The evaluation function is pure:

```text
Eval(status, now, silence) → (newAlert, message?)
```

1. Compute the desired alert, in priority order: `now − LastSeen > silence` → `silent`; otherwise
   `StorageFull` → `storage_full`; otherwise `!ReceiverPresent` → `missing_receiver`; otherwise
   `ok`.
2. If it differs from the stored `Alert`, emit one message (the alert, or recovery when it is
   `ok`) and store the new alert.
3. Never emit while the alert is unchanged.

## SQLite migration `004_mic.sql`

```sql
ALTER TABLE captures ADD COLUMN no_speech INTEGER NOT NULL DEFAULT 0;

CREATE TABLE mic_replies (
    recording_id INTEGER PRIMARY KEY,
    chat_id      INTEGER NOT NULL,
    message_id   INTEGER NOT NULL,
    created_at   TEXT    NOT NULL
);

CREATE TABLE mic_recorders (
    name             TEXT PRIMARY KEY,
    last_seen        TEXT    NOT NULL,
    receiver_present INTEGER NOT NULL,
    storage_full     INTEGER NOT NULL,
    alert            TEXT    NOT NULL DEFAULT 'ok'
);
```

- `mic_replies` rows are taken (selected and deleted) inside the ingest transaction. Leftovers
  older than 1 day are purged by `PurgeLoop`. A start that never led to an upload leaves its
  message as "🎙 recording…"; the purge edits it to "⚠ recording not received" before deleting
  the row, best-effort.
- `mic_recorders` rows are kept indefinitely. There is one per recorder name, and they are tiny.

## Directories under `DATA_DIR` (server)

| Path | Contents | Lifetime |
|---|---|---|
| `files/` | Capture files, recordings included (unchanged) | Until cleanup |
| `nospeech/` | Microphone recordings with no speech | `MIC_NOSPEECH_HOLD` (default 168h), by mtime |

The orphan sweep (`SweepOrphanFiles`) keeps covering `files/` only. `nospeech/` is outside it.
