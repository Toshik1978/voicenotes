# Contract: Health

## HTTP

`GET /healthz` on `HTTP_ADDR` (default `:8080`). It needs no authentication and is not meant for
public exposure.

| Status | HTTP | When |
|---|---|---|
| `ok` | 200 | Everything below is fine |
| `degraded` | 200 | Oldest unfinished capture is older than `HEALTH_PENDING_THRESHOLD`, **or** Telegram polling has failed for longer than that threshold |
| `unhealthy` | 503 | Local store unusable (open or write failure), or the worker loop has stopped |

Body (JSON):

```json
{
  "status": "degraded",
  "pending": 3,
  "oldest_pending_age": "47m12s",
  "waiting_on": "stt",
  "last_poll_ok": "2026-10-30T02:14:05Z",
  "last_digest": "2026-10-30"
}
```

`degraded` returns 200 on purpose: a dependency being down must not make Docker restart
VoiceNotes. Monitors that want to alert on a backlog read `status` from the body.

## CLI

`voicenotes health` sends `GET http://127.0.0.1<HTTP_ADDR port>/healthz`.
- It exits `0` on HTTP 200 (`ok` or `degraded`) and `1` otherwise. It prints the body to stdout.
- It is used as the container `HEALTHCHECK`, because the distroless image has no curl or shell.
- `voicenotes health --strict` also exits `1` on `degraded`, for monitors that want to alert.

`voicenotes version` prints the version, commit and build time.
