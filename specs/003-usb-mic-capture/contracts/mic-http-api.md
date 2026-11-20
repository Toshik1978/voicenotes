# Contract: recorder ↔ VoiceNotes HTTP API

The service serves these on its existing `HTTP_ADDR` listener, next to `/healthz`, only when
`MIC_UPLOAD_TOKEN` is set. Otherwise every `/mic/v1/` path returns 404.

Every request carries `Authorization: Bearer <MIC_UPLOAD_TOKEN>`, compared in constant time. A
missing or wrong token gets **401**, with nothing stored and nothing logged beyond the path and
the status. Response bodies are JSON `{"status": "...", "reason": "..."}`. A reason never
contains a secret.

`{id}` is the recording ID: a decimal int64 > 0. Anything else gets **400**.

## PUT /mic/v1/recordings/{id}: upload a finished recording

Request headers:

| Header | Required | Meaning |
|---|---|---|
| `Content-Type` | yes | `audio/wav`. Anything else gets 415. |
| `Content-Length` | yes | Byte length. Over `MIC_UPLOAD_MAX_BYTES` gets 413 before the body is read. |
| `X-Content-SHA256` | yes | Lowercase hex SHA-256 of the body. |
| `X-Recording-Started-At` | yes | RFC 3339 with nanoseconds, UTC. Becomes the note time. |
| `X-Recording-End-Reason` | yes | `tap`, `max_length`, `receiver_lost` or `interrupted`. |
| `X-Recorder` | yes | Recorder name. |

Server behaviour, in order:

1. Authenticate, then validate the headers.
2. Stream the body to a temp file in `files/`, hashing it on the way. A length or hash mismatch
   gets **400** `{"status":"corrupt"}`; the temp file is deleted and the client retries.
3. Parse the WAV header: PCM, 16-bit, mono, 16 kHz. Anything else gets **415**.
4. fsync, rename, fsync the directory, then run `IngestRecording` (research R8).
5. Responses:
   - **201** `{"status":"stored"}`: a new capture was inserted.
   - **200** `{"status":"duplicate"}`: this ID was already stored. The new file is removed.
   - **503** `{"status":"unavailable"}`: the store or disk is failing. The client retries.

The client deletes its spool copy only after a 200 or a 201.

## POST /mic/v1/recordings/{id}/started: start notice (best-effort)

Body: `{"started_at":"<RFC3339Nano>","recorder":"<name>"}`.

- It sends the silent "🎙 recording…" message to `MIC_NOTIFY_CHAT_ID` and stores the message ID
  in `mic_replies`.
- **202** `{"status":"accepted"}` comes back immediately; the Telegram send happens
  asynchronously.
- A repeat for the same ID is a no-op with 202.

The client sends this with a 5 s timeout and never retries. Recording does not wait for it.

## DELETE /mic/v1/recordings/{id}/started: discard notice (best-effort)

This is for a recording below the minimum length. If a start message exists, it is deleted from
Telegram and its row is removed. The response is **204**, even when nothing exists. The client
sends it once, with a 5 s timeout.

## PUT /mic/v1/recorders/{name}: check-in

Body:

```json
{"receiver_present": true, "storage_full": false, "pending": 0, "oldest_pending_at": null}
```

- It upserts `mic_recorders` with `last_seen = now` (server clock).
- **204** on success. A name that doesn't match `[A-Za-z0-9._-]{1,64}` gets **400**.

## Client retry policy (`voicenotes-mic`)

| Response | Action |
|---|---|
| 200, 201 | Delete the spool copy and continue with the next recording. |
| 400 `corrupt`, 5xx, network error | Retry with backoff (exponential with jitter, capped at `MIC_RETRY_MAX_INTERVAL`). |
| 401 | Keep the recording. Health shows "misconfigured". Retry at the maximum interval. |
| 413, 415, 400 (other) | Keep the recording, mark it rejected, and report it in health. Skip it and continue with the next. Retried only after a recorder restart. |

Uploads are sent oldest `started_at` first, one at a time.
