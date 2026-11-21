# Contract: configuration (additions)

All new variables are optional for the service. With none set, VoiceNotes behaves as before,
except the general no-speech rule (FR-210a), which needs no setting. Validation runs at startup.
Errors name the variable and never print its value (Constitution V).

## `voicenotes` (the service)

| Variable | Type | Default | Validation / effect |
|---|---|---|---|
| `MIC_UPLOAD_TOKEN` | secret | unset | When set, enables the `/mic/v1/` endpoints and the alert loop. Must be at least 32 characters, else a startup error. |
| `MIC_UPLOAD_MAX_BYTES` | bytes (int) | `31457280` (30 MiB) | 1 MiB – 1 GiB. Larger uploads get 413. |
| `MIC_NOTIFY_CHAT_ID` | int64 | `DIGEST_CHAT_ID` | Chat for start messages, outcomes and alerts. Must be non-zero when `MIC_UPLOAD_TOKEN` is set. |
| `MIC_NOSPEECH_HOLD` | duration | `168h` | 1h – 2160h. How long no-speech microphone audio is kept. |
| `MIC_SILENCE_ALERT` | duration | `5m` | 1m – 24h. A recorder with no check-in for this long is alerted on. |

## `voicenotes-mic` (the recorder)

It reads only these variables. It does not read or require any Telegram, STT, LLM, Memos or
digest variable.

| Variable | Type | Default | Validation / effect |
|---|---|---|---|
| `MIC_SERVER_URL` | URL | required | Base URL of VoiceNotes; an absolute `http`/`https` URL, for example `http://voicenotes.example.org:8080`, or `http://127.0.0.1:8080` on the same host. |
| `MIC_UPLOAD_TOKEN` | secret | required | Must match the service. At least 32 characters. |
| `MIC_RECORDER_NAME` | string | hostname | 1–64 characters of `[A-Za-z0-9._-]`. |
| `MIC_CAPTURE_DEVICE` | string | auto | A substring of the capture device name as miniaudio lists it. Must be set together with `MIC_INPUT_DEVICE`. |
| `MIC_INPUT_DEVICE` | path | auto | `/dev/input/eventN`. Must be set together with `MIC_CAPTURE_DEVICE`. Setting only one of the two is a startup error naming the missing one. |
| `MIC_DEBOUNCE` | duration | `500ms` | 0 – 5s. |
| `MIC_MIN_LENGTH` | duration | `2s` | 0 – 60s. Shorter recordings are discarded. |
| `MIC_MAX_LENGTH` | duration | `10m` | 10s – 12m. 12 minutes is about 23 MB at 16 kHz mono, within the 25 MB speech-to-text limit (FR-208). |
| `MIC_SPOOL_DIR` | path | `$STATE_DIRECTORY`, else `/var/lib/voicenotes-mic` | Must be writable. |
| `MIC_SPOOL_MIN_FREE` | bytes (int) | `268435456` (256 MiB) | At least 2 × the size of a maximum-length recording. |
| `MIC_CHECKIN_INTERVAL` | duration | `1m` | 10s – 10m. It should be below the service's `MIC_SILENCE_ALERT`; that can't be checked locally, so it is documented. |
| `MIC_RETRY_MAX_INTERVAL` | duration | `5m` | 10s – 1h. |
| `MIC_PENDING_THRESHOLD` | duration | `30m` | Health is degraded when a finished recording has waited longer than this. |
| `MIC_HTTP_ADDR` | addr | `127.0.0.1:8081` | Where the recorder serves `/healthz`. |
| `LOG_LEVEL` | enum | `info` | As for the service. |

**Automatic discovery** (research R4): picks the first USB device that has both a capture PCM and
an input device whose key capabilities include `KEY_VOLUMEUP`. It runs again every 5 s while no
receiver is present.

## Documentation

`.env.example`, `deploy/voicenotes-mic.env.example` and the README use anonymised values only:

- `MIC_SERVER_URL=http://voicenotes.example.org:8080`
- `MIC_UPLOAD_TOKEN=FAKE-0123456789abcdef0123456789abcdef`
- `MIC_NOTIFY_CHAT_ID=-1001234567890`
