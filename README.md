[![Lint, Test & Build](https://github.com/Toshik1978/voicenotes/actions/workflows/ci.yml/badge.svg)](https://github.com/Toshik1978/voicenotes/actions/workflows/ci.yml)
[![Tests](https://img.shields.io/endpoint?url=https%3A%2F%2Fgist.githubusercontent.com%2FToshik1978%2F7fd599021f75cc937dde5fedb2ab47cf%2Fraw%2Ftests.json)](https://github.com/Toshik1978/voicenotes/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fgist.githubusercontent.com%2FToshik1978%2F7fd599021f75cc937dde5fedb2ab47cf%2Fraw%2Fcoverage.json)](https://github.com/Toshik1978/voicenotes/actions/workflows/ci.yml)

# VoiceNotes

VoiceNotes is a self-hosted Go service that turns Telegram voice captures into notes in
Memos. Send a voice message (or text) to your private bot. The service
transcribes the audio, optionally cleans it up with an LLM, saves a note with the audio attached
and replies with a link. Each morning it sends a digest of the notes that are still tagged
`#inbox`.

It also saves photos and files. A document or photo becomes a note with the original attached, and
a short hint describing it ([Photos, files and albums](#photos-files-and-albums)).

A wireless microphone captures too: tap the transmitter, talk, tap again, and the recording becomes
a note the same way a voice message does ([Wireless microphone](#wireless-microphone)).

## How it works

1. The bot receives a voice or text message from an allowlisted Telegram user (long polling, no
   public ingress). Photos and files are accepted too (see
   [Photos, files and albums](#photos-files-and-albums)).
2. The capture is stored durably in a local SQLite file, with the audio on disk, before anything
   else happens.
3. Speech-to-text: the audio goes to an OpenAI-compatible `/audio/transcriptions` endpoint.
4. Optional LLM step (OpenAI-compatible `/chat/completions`): translate to the target language,
   tidy punctuation, suggest tags and spot action items (checklist plus `#todo`).
5. A Memos note is created (private, tagged `#inbox`) and the audio is attached.
6. The bot edits its silent reply to `✓ saved` with a link to the note. Local audio is deleted
   only after the note is saved. At startup, audio files older than an hour that no capture
   references (left by a crash mid-write) are deleted.

If Telegram, the speech-to-text service, the LLM or Memos is down, the process keeps running, the
capture waits and is retried with backoff, and health reports the degradation. A capture is never
dropped: a permanent failure produces a placeholder note with the audio only. If even the audio
could not be kept (for example Telegram refused the download), the reply says so and asks you to
send it again.

## Photos, files and albums

Send a photo, a document, a video or an animation to the bot, with or without a caption. The
original is attached to the note; a caption is your own text and goes through the LLM step like a
voice note. The note gets one hint line (from the LLM when it is on, otherwise a placeholder such as
`📎 report.pdf`, `📷 photo`, `📷 3 photos` or `📎 4 files`). Extracted text is never pasted into the
note. Voice and text captures behave as before and are never batched.

- **One send, one note.** Files from the same chat are collected into one batch, and one batch is
  one note and one reply. An item joins the open batch when it has the same album ID, or arrives
  before the quiet period (`BATCH_QUIET_PERIOD`, default `3s`) has passed since the previous
  item; otherwise it starts a new batch. The `⏳` reply answers the first item only. Captions are
  joined in send order.
- **A restart while an album is arriving.** Items are stored before they are acknowledged, so
  nothing is lost. The item that starts a batch is downloaded and written to disk before its `⏳`; if
  that download fails, the `⏳` comes once the pipeline has downloaded it. Later items are downloaded
  by the pipeline and confirmed by the final reply. On startup, the quiet period of every batch
  still collecting restarts, so items that Telegram redelivers after the restart join their
  half-built batch instead of starting a second note.
- **A file over 20 MB.** Telegram bots cannot download more. Sent alone, it gets `⚠ too large for
  Telegram bots (max 20 MB) — not saved.`. If Telegram did not report it as too large and that shows
  only at download, it is already stored, so it becomes a placeholder note instead, with the reply
  `⚠ files not kept — <name> (too large for Telegram bots) · please send them again`. As part of a
  batch (an album, or sent inside the quiet period) it does not stop the rest: the other files are
  saved and the reply reads `⚠ saved, 1 of 4 not kept — photo 2 (too large for Telegram bots) ·
  open`.
- **Reading content (optional).** With `DOCLING_URL` set, each file is sent to a
  docling-serve instance that returns text,
  which the LLM uses to write the hint. Only the leading `DOCLING_MAX_PAGES` pages (default `20`)
  of a document are converted. `DOCLING_TIMEOUT` (default `10m`) is the wait per file, from submit
  to result; a file that times out is retried and, on its third timeout, gets the placeholder hint
  with `couldn't read in time`. A file the reader cannot read or does not support also keeps the
  placeholder hint. Without `DOCLING_URL`, files are still saved, with placeholder hints.
- **No OCR options are sent.** VoiceNotes deliberately sends none of `ocr_engine`, `ocr_preset`,
  `ocr_lang` or `force_ocr`, so the server's own OCR preset decides (for example a Cyrillic preset
  configured on the docling-serve side).
- **Vision (optional, photos only).** With `VISION_ENABLED=true`, a photo that the document reader
  gave fewer than 20 non-space characters of text for, or that was not read because there is no `DOCLING_URL`, is
  described by a vision-capable model, one photo per request. It is a best-effort fallback: when it
  fails or is out of budget the placeholder hint is used, and it is never part of health, so a down
  vision model does not make the service `degraded`. Documents and videos are never sent to it.
  The model must accept images in an OpenAI-compatible `/chat/completions` request.

A docling-serve container and a vision-capable model are a suggested setup, not a requirement: any
server speaking the same API will do, and no image, version, OCR engine or model is mandated.

## Wireless microphone

A DJI Mic Mini 2S transmitter and its Mobile Receiver (USB-C) can feed VoiceNotes without a phone.
Plug the receiver into a Linux host, tap the transmitter's record button, talk, and tap again. The
recording is saved and goes through the same pipeline as a Telegram voice message: the chat that
receives microphone replies shows a silent `🎙 recording…` at the first tap, which the pipeline later
edits to `✓ saved · open`. The note is `#inbox`, carries the WAV and is timed at the first tap.

Two programs take part:

- **`voicenotes`** (the service) accepts uploads on `/mic/v1/` of its `HTTP_ADDR` listener. The
  endpoints exist only when `MIC_UPLOAD_TOKEN` is set; otherwise they return 404.
- **`voicenotes-mic`** (the recorder) runs on the host the receiver is plugged into. It records
  16 kHz mono WAV, keeps it in a local spool until the service has stored it, and uploads it oldest
  first with backoff. It is a separate binary because it links the audio library with cgo; the
  service stays static and CGO-free.

### What the receiver must do

The recorder needs two things from the receiver, both over USB: an ALSA capture device for the audio,
and an input device that reports the transmitter's record button as `KEY_VOLUMEUP` (one press and
release per tap). It grabs that input device exclusively, so a tap does not change the host's volume.
The hardware check below confirms both before anything is installed. When auto-discovery picks the
wrong devices, name them with `MIC_CAPTURE_DEVICE` and `MIC_INPUT_DEVICE` (set both or neither).

### Hardware check

On the host the receiver is plugged into:

```sh
sudo apt install libasound2t64 evtest alsa-utils
lsusb | grep -i 2ca3      # expect 2ca3:4011
arecord -l                # a capture device for the receiver
sudo evtest               # pick the receiver's "Consumer Control", tap the transmitter:
                          # expect EV_KEY KEY_VOLUMEUP value 1, then value 0, per tap
arecord -D hw:<card>,0 -f S16_LE -r 48000 -c 2 -d 5 test.wav && aplay test.wav
```

If `lsusb` shows a different ID, or no `KEY_VOLUMEUP` events appear, stop and report it before going
further.

### Install the recorder

Each release attaches `voicenotes-mic_<version>_linux_amd64.tar.gz` and
`voicenotes-mic_<version>_linux_arm64.tar.gz` (built on Ubuntu 24.04, so they run on 26.04). Each
holds the binary, `LICENSE`, `voicenotes-mic.service` and `voicenotes-mic.env.example`; the last two
are also in [`deploy/`](deploy). Download the tarball for the host's architecture from the
[releases page](https://github.com/Toshik1978/voicenotes/releases), unpack it and run, from the
unpacked directory:

```sh
sudo apt install libasound2t64
sudo install -m 0755 voicenotes-mic /usr/local/bin/
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --groups audio,input voicenotes-mic
sudo install -m 0600 voicenotes-mic.env.example /usr/local/etc/voicenotes-mic.env   # then edit it
sudo install -m 0644 voicenotes-mic.service /etc/systemd/system/
sudo systemctl enable --now voicenotes-mic
```

The system user must be in the `audio` group (capture device) and the `input` group (the receiver's
key events). The unit also adds both as supplementary groups, keeps the spool in
`/var/lib/voicenotes-mic` (`StateDirectory`) and restarts the recorder on failure. The recorder
refuses to start on an invalid setting and names it, never its value.

Edit `/usr/local/etc/voicenotes-mic.env` (see [`deploy/voicenotes-mic.env.example`](deploy/voicenotes-mic.env.example)
for every setting and its default). `MIC_SERVER_URL` and `MIC_UPLOAD_TOKEN` are required; the token
must equal the service's `MIC_UPLOAD_TOKEN`.

### Direct mode and Pi mode

- **Direct mode.** The receiver is plugged into the host that runs the service. Set
  `MIC_SERVER_URL=http://127.0.0.1:8080` (the service's `HTTP_ADDR` port).
- **Pi mode (proxy).** The receiver is plugged into a small box, for example a Raspberry Pi with
  64-bit Ubuntu 26.04, which records and uploads to the server over the network. Set
  `MIC_SERVER_URL=http://voicenotes.example.org:8080`, and publish the service's port (`-p 8080:8080`).
  The listener also serves `/healthz`: keep it on a trusted network or behind a TLS reverse proxy, and
  use an `https://` URL when it crosses an untrusted one, since the bearer token travels in every
  request.

Notes for the Pi:

- Use 5 GHz Wi-Fi or Ethernet. The microphone link uses the 2.4 GHz band, which a 2.4 GHz Wi-Fi
  connection would crowd.
- Disable the Pi's own Bluetooth for the same reason: add `dtoverlay=disable-bt` to the boot
  configuration (`/boot/firmware/config.txt` on Ubuntu) and reboot.
- Keep the clock synchronised (the Ubuntu default, `systemd-timesyncd`). The service stamps check-ins
  with its own clock, but the first-tap time is taken from the recorder's.

Nothing is lost when something breaks. A recording is written to the spool as it is made, so a crash
or a lost receiver keeps what was recorded; if the service is down, uploads wait in the spool and are
sent in order when it returns, with no start message for recordings that finished while it was away.
A recording shorter than `MIC_MIN_LENGTH` (a stray double tap) discards the start message and creates no capture.

### Service settings

Set these on the service (all optional except `MIC_UPLOAD_TOKEN`, which turns the feature on):

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `MIC_UPLOAD_TOKEN` | to enable | unset (feature off) | yes | Bearer token the recorder sends; at least 32 characters. Generate one, for example with `openssl rand -hex 32` |
| `MIC_NOTIFY_CHAT_ID` | no | `DIGEST_CHAT_ID` | no | Chat for start messages and recorder alerts; must be non-zero once uploads are on |
| `MIC_UPLOAD_MAX_BYTES` | no | `31457280` (30 MiB) | no | `1 MiB`-`1 GiB`; larger uploads get 413. The default covers about 16 minutes of 16 kHz mono audio, above the recorder's 12 minute maximum |
| `MIC_NOSPEECH_HOLD` | no | `168h` (7 days) | no | `1h`-`2160h`; how long microphone audio with no speech is kept |
| `MIC_SILENCE_ALERT` | no | `5m` | no | `1m`-`24h`; a recorder not heard from for this long raises an alert |

The recorder reads its own settings, listed in the example file: for instance `MIC_RECORDER_NAME`
(default: the hostname), `MIC_MIN_LENGTH` (`2s`), `MIC_MAX_LENGTH` (`10m`, maximum `12m`),
`MIC_SPOOL_MIN_FREE` (256 MiB), `MIC_CHECKIN_INTERVAL` (`1m`) and `MIC_HTTP_ADDR`
(`127.0.0.1:8081`).

### No speech

A transcript with no speech creates no note, from any source. The `⏳` reply becomes
`🔇 no speech heard — not saved`. A Telegram voice message's audio is deleted, because Telegram keeps
the original. A microphone recording is the only copy, so its audio is moved to
`$DATA_DIR/nospeech/` and deleted after `MIC_NOSPEECH_HOLD`; listen to it there if a recording was
dropped by mistake. A transcription that fails is a different case and still produces a placeholder
note.

### Alerts and health

The recorder checks in every `MIC_CHECKIN_INTERVAL`, and the service sends one silent message to
`MIC_NOTIFY_CHAT_ID` when a condition starts and one when it clears. A recorder alerts only after its
first check-in.

| Condition | Alert | Recovery |
|---|---|---|
| Receiver unplugged | `🎙 <name>: receiver not connected — taps won't record` | `🎙 <name>: receiver connected` |
| Spool below `MIC_SPOOL_MIN_FREE` | `🎙 <name>: storage full — new recordings refused` | `🎙 <name>: storage OK` |
| No check-in for `MIC_SILENCE_ALERT` | `🎙 <name>: not heard from since HH:MM` | `🎙 <name>: back online` |

The service's `/healthz` gains a `recorders` list (`name`, `alert`, `last_seen`) and reports
`degraded` while any recorder has an alert outstanding. The recorder serves its own
`GET /healthz` on `MIC_HTTP_ADDR` (default `127.0.0.1:8081`, loopback only): `receiver`, `storage`,
`pending`, `rejected` and so on; `degraded` for a missing receiver, full storage, a rejected
recording, a refused token or a recording stuck longer than `MIC_PENDING_THRESHOLD`, and `unhealthy`
(HTTP 503) once the recorder's loops have stopped.

```sh
curl -s 127.0.0.1:8081/healthz
```

The upload protocol between the two programs is in
[`specs/003-usb-mic-capture/contracts/mic-http-api.md`](specs/003-usb-mic-capture/contracts/mic-http-api.md),
and the step-by-step validation in [`specs/003-usb-mic-capture/quickstart.md`](specs/003-usb-mic-capture/quickstart.md).

## Quick start

Create a `.env` file from the template and fill in real values (the file is git-ignored):

```sh
cp .env.dist .env
```

Run the image. Its default command is `run` (also what the binary does with no arguments); `version` and `health` are the other subcommands:

```sh
docker run -d --name voicenotes --restart unless-stopped \
  --stop-timeout 90 \
  --env-file .env \
  -v voicenotes-data:/data \
  -p 8080:8080 \
  ghcr.io/toshik1978/voicenotes:latest
```

Tags are `<version>` (for example `0.1.0`) and `latest`, both published only by releases (see
[Releases](#releases)). Images are built for `linux/amd64` and `linux/arm64`. The container runs as the distroless nonroot user `65532:65532`.

Give the container a 90 second stop grace period (`--stop-timeout 90`, or `stop_grace_period:
90s` in Docker Compose); the Dockerfile cannot set it. Docker's default of 10 seconds is too short:
on SIGTERM the service finishes what is in flight before it exits. A voice message still being
downloaded from Telegram can take up to 60 seconds (the download bound on arrival) before its row
is stored, and recording a delivered morning digest is retried for up to 30 seconds so the digest is
not sent twice after the restart. A SIGKILL inside that window can lose a capture with no reply,
which Telegram never redelivers, or repeat the digest. 90 seconds covers the longest of these with
margin.

```yaml
services:
  voicenotes:
    image: ghcr.io/toshik1978/voicenotes:latest
    restart: unless-stopped
    stop_grace_period: 90s
    env_file: .env
    volumes:
      - voicenotes-data:/data
volumes:
  voicenotes-data:
```

On upgrade, a `$DATA_DIR/audio` directory left by an earlier version is renamed to
`$DATA_DIR/files` at startup, before anything uses it; queued captures keep working. If both exist,
the entries are moved across without overwriting, and `audio/` is removed once empty. Nothing needs
to be done by hand.

Port 8080 serves health, and the microphone API when `MIC_UPLOAD_TOKEN` is set. It does not need to be
published unless a monitor or a recorder reaches it. A recorder on the same host (direct mode) needs
`-p 127.0.0.1:8080:8080`, loopback only; a Pi on the network needs a wider binding such as `-p 8080:8080`.

For a host bind mount instead of a named volume, the directory must be writable by the container
user:

```sh
mkdir -p ./data && sudo chown 65532:65532 ./data
```

Without that, startup fails because the store cannot be opened. A named volume needs no extra
step. A startup configuration error exits with `config: <VAR>: <problem>`; the value is never
printed.

Subcommands of the binary:

| Command | Purpose |
|---|---|
| `voicenotes [run]` | Run the service (the default when no command is given) |
| `voicenotes health [--strict]` | Query the local health endpoint (see [Health](#health)) |
| `voicenotes version` | Print version, commit and build time |

## Configuration

All settings are environment variables. Secrets come only from the environment, are never logged,
and should not appear on the command line (use `--env-file` or `-e NAME` without a value).

### Telegram

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | yes | | yes | Token of the dedicated VoiceNotes bot |
| `TELEGRAM_ALLOWED_USERS` | yes | | no | Comma-separated Telegram user IDs allowed to capture |
| `DIGEST_CHAT_ID` | no | first of `TELEGRAM_ALLOWED_USERS` | no | Chat that receives the morning digest |

### Speech-to-text (OpenAI-compatible)

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `STT_BASE_URL` | yes | | no | Base URL including `/v1`, for example `http://stt.example.org:8000/v1` |
| `STT_API_KEY` | no | empty (no auth header) | yes | Bearer key |
| `STT_MODEL` | yes | | no | Model id passed as `model` |
| `STT_RESPONSE_FORMAT` | no | `verbose_json` | no | `verbose_json` (reports the detected language) or `json` (for models that reject `verbose_json`) |
| `STT_TIMEOUT` | no | `120s` | no | Per-request timeout |

### Languages

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `SOURCE_LANGUAGES` | no | empty (auto-detect) | no | Comma-separated ISO-639-1 codes the owner speaks, first is preferred |
| `TARGET_LANGUAGE` | no | `en` | no | ISO-639-1 code that notes are translated into |

### LLM (optional, OpenAI-compatible chat)

The LLM step is enabled only when `LLM_BASE_URL` is set.

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `LLM_BASE_URL` | no | | no | Base URL including `/v1`, for example a LiteLLM gateway |
| `LLM_API_KEY` | no | empty | yes | Bearer key |
| `LLM_MODEL` | if LLM is on | | no | Model name or gateway alias |
| `LLM_TRANSLATE` | no | `true` | no | Translate to `TARGET_LANGUAGE` |
| `LLM_TIDY` | no | `true` | no | Tidy punctuation and suggest tags |
| `LLM_TASKS` | no | `true` | no | Spot action items: checklist plus the todo tag |
| `LLM_DIGEST_SUMMARY` | no | `true` | no | One-line summaries in the digest |
| `LLM_MAX_TAGS` | no | `3` | no | Upper bound on suggested tags; extra suggestions are dropped, `0` disables tags |
| `LLM_TIMEOUT` | no | `60s` | no | Per-request timeout |
| `LLM_TEMPERATURE` | no | unset | no | Sampling temperature `0`–`2`; unset sends none, so the model uses its default (some models accept only that) |

### Documents and photos (optional)

All optional. With none set, voice and text behave as before and files are saved with placeholder
hints.

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `DOCLING_URL` | no | unset | no | Base URL of a docling-serve instance, for example `http://docling.example.org:5001`; enables content extraction |
| `DOCLING_API_KEY` | no | unset | yes | Sent as `X-Api-Key`; ignored without `DOCLING_URL` |
| `DOCLING_TIMEOUT` | no | `10m` | no | Wait per file, from submit to result; the third timeout of a file gives up on it |
| `DOCLING_MAX_PAGES` | no | `20` | no | `1`–`1000`; leading pages converted per document |
| `BATCH_QUIET_PERIOD` | no | `3s` | no | `500ms`–`1m`; quiet time that closes a batch |
| `HINT_EXCERPT_RUNES` | no | `4000` | no | `500`–`50000`; total extracted text sent to the LLM per batch |
| `VISION_ENABLED` | no | `false` | no | Describe photos with a vision model when no text was read |
| `VISION_BASE_URL` | if vision is on | `LLM_BASE_URL` | no | Base URL including `/v1`, for example `https://llm.example.org/v1` |
| `VISION_API_KEY` | no | `LLM_API_KEY` | yes | Bearer key. **Set it whenever `VISION_BASE_URL` points to another host:** otherwise the LLM key is sent to that host |
| `VISION_MODEL` | if vision is on | `LLM_MODEL` | no | Vision-capable model, for example `example-vision-model` |
| `VISION_TIMEOUT` | no | `60s` | no | Per-request timeout |

### Memos

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `MEMOS_URL` | yes | | no | Internal base URL the service calls, for example `https://memos.example.org` |
| `MEMOS_TOKEN` | yes | | yes | Personal Access Token (`memos_pat_...`) of the owner account, no expiry |
| `MEMOS_PUBLIC_URL` | no | `MEMOS_URL` | no | Base URL used in links shown to the owner |
| `MEMOS_API_PREFIX` | no | `auto` | no | `auto` probes `/api` (26.10+) then `/api/v1` (v0.31); or set `/api/v1` or `/api` explicitly |
| `MEMOS_CONTENT_LIMIT` | no | `8192` | no | Max note content bytes; match the Memos instance setting |
| `INBOX_TAG` | no | `inbox` | no | Tag for unreviewed notes |
| `TODO_TAG` | no | `todo` | no | Tag for notes with action items |

### Digest

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `DIGEST_ENABLED` | no | `true` | no | Turn the morning digest on or off |
| `DIGEST_TIME` | no | `07:30` | no | Local time `HH:MM` in `TZ` |
| `DIGEST_CATCHUP` | no | `3h` | no | Send a missed digest if started within this window |
| `DIGEST_MAX_NOTES` | no | `30` | no | Lines listed before "...and N more" |

### Wireless microphone (optional)

Off unless `MIC_UPLOAD_TOKEN` is set. See [Wireless microphone](#wireless-microphone) for the meaning
of each setting.

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `MIC_UPLOAD_TOKEN` | no | unset (feature off) | yes | Bearer token shared with the recorder; at least 32 characters |
| `MIC_NOTIFY_CHAT_ID` | no | `DIGEST_CHAT_ID` | no | Chat for start messages and recorder alerts |
| `MIC_UPLOAD_MAX_BYTES` | no | `31457280` | no | `1048576`-`1073741824`; maximum upload size in bytes |
| `MIC_NOSPEECH_HOLD` | no | `168h` | no | `1h`-`2160h`; how long microphone audio with no speech is kept in `nospeech/` |
| `MIC_SILENCE_ALERT` | no | `5m` | no | `1m`-`24h`; silence from a recorder that raises an alert |

### Runtime

| Variable | Required | Default | Secret | Meaning |
|---|---|---|---|---|
| `DATA_DIR` | no | `/data` | no | SQLite file and queued audio and files (`files/`), plus held no-speech microphone audio (`nospeech/`); mount a persistent volume here |
| `HTTP_ADDR` | no | `:8080` | no | Health listener; must listen on loopback (`:port` or `0.0.0.0:port`) for `voicenotes health` |
| `HEALTH_PENDING_THRESHOLD` | no | `30m` | no | Oldest unfinished capture older than this makes health `degraded` |
| `RETRY_MAX_INTERVAL` | no | `5m` | no | Backoff cap for dependency retries |
| `LOG_LEVEL` | no | `info` | no | `debug`, `info`, `warn` or `error` |
| `TZ` | no | unset (UTC) | no | Standard IANA zone, for example `Europe/Berlin` (tzdata is embedded); the digest time and the dates in attachment names use it. It must be an IANA zone name; a path such as `:/etc/localtime` is rejected. An unknown zone fails startup. |

## External calls and provisioning

VoiceNotes makes exactly these calls. Every request sends `User-Agent: VoiceNotes/<version>`.
Outbound connections go to the Telegram API (HTTPS), `STT_BASE_URL`, `LLM_BASE_URL` and
`MEMOS_URL`.

### Speech-to-text

`POST {STT_BASE_URL}/audio/transcriptions`, multipart with `file` (Telegram audio as-is, Ogg/Opus,
no transcoding), `model`, `response_format`, `temperature=0` and, only when one language is
forced, `language`. `Authorization: Bearer` is sent only when `STT_API_KEY` is set.

- speaches with any CTranslate2 Whisper model, for example `Systran/faster-whisper-large-v3`: use
  `STT_RESPONSE_FORMAT=verbose_json`. Download the model before first use; speaches answers 404
  for models that are not installed, and VoiceNotes reports that as degraded health.
- OpenAI `whisper-1`: `verbose_json`.
- OpenAI `gpt-4o-transcribe` and `gpt-4o-mini-transcribe`: set `STT_RESPONSE_FORMAT=json`. These
  return no detected language, so the re-transcribe rule is skipped.

### LLM

`POST {LLM_BASE_URL}/chat/completions` with no provider-specific fields; the model is instructed
to answer with one JSON object. Provision one model alias through any OpenAI-compatible endpoint,
for example a LiteLLM model named `voicenotes`, and set `LLM_MODEL` to it. A small, fast model is
enough: about one call per capture plus one per day, a few hundred tokens each. No `temperature`
is sent unless `LLM_TEMPERATURE` is set, because some models (Claude Haiku, for one) accept only
their default. If the output fails validation, the note is saved without LLM output (`✓ saved (no
AI)`).

### Document reader (optional)

`POST {DOCLING_URL}/v1/convert/file/async`, then polling `GET /v1/status/poll/{task_id}` and
`GET /v1/result/{task_id}`; one file per conversion, Markdown output, the first
`DOCLING_MAX_PAGES` pages. No OCR options are sent (`ocr_engine`, `ocr_preset`, `ocr_lang`,
`force_ocr`): the server's OCR preset decides, so configure it on the docling-serve side. A
docling-serve container is a suggestion, not a requirement. `DOCLING_API_KEY`, when set, is sent as
`X-Api-Key`. A down reader (connection errors, 5xx, 408, 429, 401, 403, 404) queues the capture
with `⏳ queued — document reader unavailable, will retry`; a file it rejects (400, 413, 415, 422)
keeps the placeholder hint.

### Vision model (optional)

When `VISION_ENABLED=true`: `POST {VISION_BASE_URL}/chat/completions` with the photo as a base64
`image_url` part, one photo per request, at most 3 attempts or 2 minutes per photo. Failure falls
back to the placeholder hint and never queues the capture or affects health.

### Memos

Targets Memos v0.31.0 (`/api/v1`) and 26.10+ (`/api`). Requests send
`Authorization: Bearer {MEMOS_TOKEN}`.

- Create a Personal Access Token as the owner under Settings, Access Tokens, with expiry "Never".
  A PAT survives the 26.10 upgrade.
- The attachment upload limit must be at least 20 MB (the default is 30 to 32 MB).
- After moving to 26.10, re-apply any custom content or upload limits; they are not migrated.
- Calls: `GET {P}/auth/me` (resolve prefix and owner), `POST {P}/memos?memoId=...` (create; the
  deterministic id makes retries idempotent), `POST {P}/attachments` (audio, photos and files) and
  `GET {P}/memos?filter=...` (digest inbox listing). Tags are written inline in the note content.

### Error handling

| Class | Statuses and conditions | Behavior |
|---|---|---|
| Temporary | connection errors, timeouts, 5xx, 408, 429 | Retried with backoff |
| Misconfiguration | 401, 403, 404 | Retried, health `degraded`, never a placeholder |
| Permanent | 400, 413, 415, 422 | Placeholder note with the audio only |

### Telegram

Long polling with `getUpdates` (`allowed_updates=["message"]`), `getFile` plus download (bots are
limited to 20 MB files), `sendMessage` and `editMessageText`. No webhook. Bot API calls time out
after 1 minute; a file download has its own client with a 10 minute timeout, so a 20 MB file
still arrives over a link of about 270 kbit/s. A download on arrival (a voice message, or the
item that starts a batch) is cut off after 1 minute and left to the pipeline's download step.

## Telegram messages

Every capture reply and edit is silent (`disable_notification`). Only the morning digest makes a
sound. Messages from users not in `TELEGRAM_ALLOWED_USERS` get no reply.

| Situation | Reply |
|---|---|
| Accepted | `⏳` |
| Waiting on a dependency | `⏳ queued — <speech-to-text\|notes\|Telegram> unavailable, will retry` |
| Done | `✓ saved · open` (with a link) |
| Done with tasks | `✓ saved · 2 tasks · open` |
| LLM skipped | `✓ saved (no AI) · open` |
| Permanent failure | `⚠ saved audio only — <reason> · open` |
| Permanent failure, no audio kept | `⚠ audio not kept — <reason> · please send it again · open` |
| Audio rejected by Memos | `⚠ saved, audio not attached — <reason> · open` |
| Waiting on the document reader | `⏳ queued — document reader unavailable, will retry` |
| Files saved, some not kept | `⚠ saved, 1 of 4 not kept — scan.pdf (<reason>) · open` |
| Files saved, some not attached | `⚠ saved, 1 of 4 not attached — photo 3 (<reason>) · open` |
| No file could be kept | `⚠ files not kept — scan.pdf (<reason>), photo 2 (<reason>) · please send them again · open` |
| Microphone recording started | `🎙 recording…` (silent, to `MIC_NOTIFY_CHAT_ID`; becomes the outcome reply) |
| Transcript has no speech (any source) | `🔇 no speech heard — not saved` |
| Unsupported message type | `VoiceNotes takes voice, text, photos and files.` |
| Single file over 20 MB | `⚠ too large for Telegram bots (max 20 MB) — not saved.` |
| Storing failed | `⚠ not saved — please send it again.` |

`/start` and `/help` reply with `VoiceNotes: send a voice message, text, photo or file to save it as a note.`

The morning digest lists new `#inbox` notes and earlier ones, oldest first, capped at
`DIGEST_MAX_NOTES` lines, each with a one-line summary and a link to
`MEMOS_PUBLIC_URL/memos/<id>`. `☑` marks notes with a task list. With no `#inbox` notes, nothing is
sent.

## Health

`GET /healthz` on `HTTP_ADDR` (default `:8080`) needs no authentication and should not be exposed
publicly. The body is JSON with `status`, `pending`, `oldest_pending_age`, `waiting_on`,
`last_poll_ok` and `last_digest` (the local date of the last digest actually sent), plus a
`recorders` list when a microphone recorder has checked in.

| Status | HTTP | When |
|---|---|---|
| `ok` | 200 | Everything is fine |
| `degraded` | 200 | The oldest unfinished capture is older than `HEALTH_PENDING_THRESHOLD` or is waiting on a misconfiguration (401, 403, 404), or Telegram polling has failed for longer than that, or a microphone recorder has an alert outstanding |
| `unhealthy` | 503 | The local store is unusable, or the worker loop has stopped |

`degraded` returns 200 on purpose, so Docker does not restart the service because a dependency is
down. Monitors that alert on a backlog read `status` from the body.

`voicenotes health` calls the endpoint on `127.0.0.1` at the port of `HTTP_ADDR` and exits `0` on
HTTP 200 and `1` otherwise, printing the body. The image uses it as its `HEALTHCHECK`, since
distroless has no curl or shell. `voicenotes health --strict` also exits `1` on `degraded`. So
`HTTP_ADDR` must listen on loopback: use `:port` or `0.0.0.0:port` (or `127.0.0.1:port`), not a
single non-loopback IP, or the `HEALTHCHECK` fails and the container shows as unhealthy.

## Volumes and ports

| Item | Value |
|---|---|
| Volume | `/data`: a small SQLite file, plus audio and files (in `files/`) only while captures are queued. Must be writable by `65532:65532` |
| Port | `8080/tcp`: health, and the microphone upload API when `MIC_UPLOAD_TOKEN` is set |

## Development

Requires Go, Task, golangci-lint and pre-commit.

```sh
task setup              # download modules
pre-commit install      # wires commit-msg, pre-commit and pre-push hooks
```

Before any commit the gate must pass:

| Command | Must show |
|---|---|
| `task lint` | 0 issues |
| `golangci-lint fmt --diff` | no output |
| `task test:coverage` | tests pass with `-race`, total coverage at or above 80% |
| `task build` | `bin/voicenotes` builds with `CGO_ENABLED=0` |
| `task build:mic` | `bin/voicenotes-mic` builds with cgo (`CGO_ENABLED=1`) |

Other tasks: `task format`, `task test`, `task run` (builds and runs, loading `.env` if present),
`task clean` and `task changelog TAG=vX.Y.Z`. Commits follow Conventional Commits. The binding
project rules are in `.specify/memory/constitution.md`; the feature specifications are under
`specs/`.

### Releases

Releases are cut by pushing a `vX.Y.Z` tag. Write the release notes first with `task changelog
TAG=vX.Y.Z` and commit them; `.github/scripts/extract-changelog.sh vX.Y.Z` previews the release
body. The tag triggers `.github/workflows/release.yml`, which publishes the multi-arch image as
`ghcr.io/toshik1978/voicenotes:<version>` (the tag without its `v`) and `:latest`, attaches a
build provenance attestation and creates the GitHub release from the `CHANGELOG.md` section, with
`voicenotes-mic_<version>_linux_amd64.tar.gz` and `..._linux_arm64.tar.gz` attached (see
[Wireless microphone](#wireless-microphone)). A
tag without a matching section fails before anything is pushed. Pushes to `main` only run CI.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT. See [LICENSE](LICENSE).
