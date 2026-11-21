# Implementation Plan: Wireless microphone captures

**Branch**: `003-usb-mic-capture` (work lands on `main`, per Constitution VIII) | **Date**: 2026-11-20 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/003-usb-mic-capture/spec.md`

## Summary

A new recorder program, **`voicenotes-mic`**, runs natively as a systemd unit on whichever host has
the wireless microphone's USB receiver. It turns tap-to-start and tap-to-stop on the transmitter
into audio recordings and uploads them to VoiceNotes.

- **Buttons**: it reads them with hand-written, standard-library evdev code.
- **Audio**: it captures through miniaudio via `malgo` (cgo), which delivers 16 kHz mono directly.
- **Durability**: it writes a crash-safe WAV into a local spool, and deletes it only after
  VoiceNotes confirms storage.

**Direct mode** is the recorder running on the service's own host and uploading to `localhost`.
**Proxy mode** is the recorder on a small aarch64 Linux box near the bed, such as a Raspberry Pi
running Ubuntu 26.04.

The **service** stays CGO-free, distroless and device-free. It gains:

- a token-authenticated `/mic/v1/` API on its existing port: upload, start and discard notices,
  check-ins
- `IngestRecording`, which feeds the unchanged pipeline (speech-to-text, LLM, `#inbox` note,
  digest)
- the silent "🎙 recording…" message, edited into the outcome
- recorder alerts and their health signal
- the general **no speech, no note** rule for every audio source, with microphone audio held for
  7 days

## Chosen approach (maintainer decisions)

| | **A. Separate host-native recorder, cgo audio (chosen)** | B. Recorder inside the service, hand-written ALSA | C. Recorder in its own container |
|---|---|---|---|
| Where cgo and devices live | Only `voicenotes-mic` on the receiver's host | Nowhere (raw ioctls); devices passed into the service container | A cgo image with device passthrough |
| Hardware risk | Low: miniaudio handles ALSA quirks and conversion | High: about 500 lines of hand-written ALSA and resampling | Low |
| Hotplug / permissions | Plain `audio`/`input` groups | Bind mounts plus device cgroup rules | Bind mounts plus device cgroup rules |
| Service impact | API only; image unchanged | Device code and a direct mode in the service | API only |

**Why A**: the service keeps its static, distroless image. The riskiest code is replaced by a
mature library. Running natively makes USB hotplug and permissions trivial. Because every
recorder uses the same upload path, direct and proxy are the same program configured differently.

Rejected earlier:

- Forwarding the USB device over the network (USB/IP): drops audio on network hiccups, which
  breaks Principle VI.
- Sending through a Telegram user account: fragile, since bots can't receive from bots.

Also decided with the owner:

- **Hardware**: DJI Mic Mini 2S with the DJI Mobile Receiver. Any receiver that sends
  `KEY_VOLUMEUP` is accepted.
- **Recording limits**: recordings under 2 s are discarded and their start message deleted;
  recordings stop at 10 m (12 m maximum).
- **Start message**: a silent "🎙 recording…" message per recording.
- **No speech**: no note, for every audio source. Constitution v1.1.0 Principle VI.
- **Alerts**: from check-ins, for a missing receiver, full storage, or a silent recorder (5 m).
- **Build rules**: cgo is confined to the recorder (constitution v2.0.0 Principle IV), and `malgo`
  is the approved fifth dependency.
- **Repository**: same repo and module; the recorder ships as amd64 and arm64 tarballs.
- **Order of work**: the hardware is not yet available. Everything is built against fakes, the
  laptop's microphone and keyboard first, then verified on the kit (research R1).

## Technical Context

**Language/Version**: Go (toolchain in `go.mod`, currently 1.27), plus a C toolchain (gcc) for the
recorder only.

**Primary Dependencies**:

- The service is unchanged: telebot.v4, zap, caarlos0/env v11, modernc sqlite.
- The recorder adds `github.com/gen2brain/malgo` (Unlicense; approved by the maintainer; solves
  cross-device capture and format conversion that the standard library can't do without about 500
  lines of hand-written ALSA code). It is imported only by `internal/micaudio` and
  `cmd/voicenotes-mic`.
- At runtime the recorder's host needs `libasound2t64` (or `libasound2`), which miniaudio loads
  dynamically.
- evdev, WAV handling, HTTP, hashing and IDs use the standard library.

**Storage**:

- **Service**: the same SQLite database, plus migration `004_mic.sql` (`captures.no_speech`,
  `mic_replies`, `mic_recorders`), and the new directory `$DATA_DIR/nospeech/`.
- **Recorder**: a spool directory (`$STATE_DIRECTORY`, i.e. `/var/lib/voicenotes-mic`) with no
  database.

**Testing**: stdlib `testing`, table-driven, hand-written fakes, `httptest`.

- Gated packages, added to `COVERPKG` in `Taskfile.yml` **and** `-coverpkg` in `ci.yml`, and to
  CLAUDE.md:
  - `internal/mic` (pure): tap machine, WAV, evdev decoding, capability bitmap, discovery
    matching, alerts, IDs
  - `internal/recorder`: spool, recorder loop, upload client, check-ins, all behind interfaces
  - `internal/micapi`: HTTP handlers, start messages, alert loop
- Outside the gate (tests still run):
  - `internal/micdev` (stdlib): evdev reader with grab, sysfs walk
  - `internal/micaudio` (cgo `malgo` adapter): real-device tests behind `VN_MICDEV=1`

**Target Platform**:

- **Service**: Linux container, distroless, `CGO_ENABLED=0`, amd64 and arm64 (unchanged).
- **Recorder**: native Linux binaries for amd64 and arm64, built with cgo. CI builds them natively
  on `ubuntu-24.04` and `ubuntu-24.04-arm`; the release builds both with GoReleaser on Ubuntu 24.04
  (glibc 2.39), cross-compiling arm64 with `gcc-aarch64-linux-gnu`. They run on Ubuntu 26.04.

**Project Type**: a long-running service plus a small host daemon, from one module.

**Performance Goals**:

- SC-201: a confirmation within 60 s of the stop tap.
- The recorder on a Pi 3 uses under 5% of one core (miniaudio conversion plus a 32 kB/s write).
- SC-209: alerts within 6 min.

**Constraints**:

- Constitution I–VIII (v2.0.0).
- At most about 2 s of audio lost on a crash (fsync every 2 s; SC-202 allows 5 s).
- `MIC_MAX_LENGTH` is at most 12 m, about 23 MB, under the 25 MB speech-to-text limit (FR-208).
- Upload bodies are capped by `MIC_UPLOAD_MAX_BYTES`.
- Neither the network nor Telegram can delay recording.
- No device serial numbers or real hosts in fixtures or docs.

**Scale/Scope**: single owner; one or two recorders; a few recordings a night.

## Constitution Check (v2.0.0)

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (below).*

| Principle | Status | How this plan complies |
|---|---|---|
| I. Idiomatic Go, stdlib first | ✅ | Everything except audio capture uses the stdlib. Device I/O sits behind `KeySource`, `AudioSource` and `Discovery` interfaces in thin adapters (`micdev`, `micaudio`). Errors are wrapped with `%w`. |
| II. Test-first, stdlib testing | ✅ | Every task is test-first. Decision logic is pure and table-tested in `mic` and `capture`. `recorder` and `micapi` use fakes and `httptest`. `COVERPKG` gains `mic`, `recorder` and `micapi`, identically in Taskfile and CI. |
| III. Dependency discipline | ✅ | `github.com/gen2brain/malgo` was approved by the maintainer in this planning session, with its purpose and the stdlib gap stated. It is recorded here and in CLAUDE.md. The service adds nothing. |
| IV. Static, CGO-free service | ✅ | `voicenotes` still builds with `CGO_ENABLED=0` into the same distroless image. cgo lives only in `internal/micaudio` and `cmd/voicenotes-mic`, fenced by a `depguard` rule and a `go list -deps` test. The recorder ships as amd64 and arm64 binaries built in CI. |
| V. Fail fast on config, degrade on dependencies | ✅ | `MIC_*` is validated at startup, naming the variable and never the token. The `/mic/v1/` API and alert loop are wired only when `MIC_UPLOAD_TOKEN` is set, with a plain conditional. The recorder has its own minimal config. A missing receiver, an unreachable service or Telegram degrades health and alerts; nothing exits. |
| VI. No capture lost (v1.1.0 exception) | ✅ | The sidecar is fsynced before the first sample, audio is fsynced every 2 s, and interrupted files are repaired at start. The spool copy is deleted only after 200 or 201, which the service sends only after the file is fsynced and the capture inserted. Uploads are idempotent by recording ID, and a hash rejects truncated bodies. Full storage refuses new recordings rather than overwriting. No-speech audio is deleted without a note only under the v1.1.0 exception: Telegram keeps the original, and microphone audio is held for `MIC_NOSPEECH_HOLD`. |
| VII. Public repo hygiene | ✅ | Docs and examples use `voicenotes.example.org`, fake tokens and chat `-1001234567890`. Hardware fixtures are scrubbed of serial numbers. Only the allowed GitHub/GHCR URLs appear. |
| VIII. Conventional Commits | ✅ | Commits such as `feat(mic): …`, `feat(recorder): …`, `build(deps): add malgo for voicenotes-mic`, `ci: build and release voicenotes-mic`. |

No violations, so Complexity Tracking is empty.

**Spec deviation from 001, approved in this spec**: the edge case "Silence or unintelligible
audio" is superseded by FR-210a, allowed by constitution v1.1.0.

## Design overview

### Recorder (`cmd/voicenotes-mic`, `internal/recorder`, `internal/mic`, `internal/micdev`, `internal/micaudio`)

**Interfaces**, used by `recorder.Recorder.Run(ctx)`:

- `KeySource` (`micdev.Keys`): taps and `DeviceLost`
- `AudioSource` (`micaudio.Capture`): open by card, stream s16 16 kHz mono frames, close
- `Discovery` (`micdev.Discover` over `mic.MatchReceiver`)
- `Spool` (`recorder.DirSpool`)
- `Handoff` (`recorder.HTTPHandoff`)
- `Notifier` (`recorder.HTTPNotifier`)
- `CheckIn` (`recorder.HTTPCheckIn`)

**Tap actions** (`mic.Toggle`):

- **Start**:
  1. Check free space (`Refuse` if low).
  2. Write the sidecar and fsync.
  3. Start capture. miniaudio's `Data` callback feeds a bounded channel, and a writer goroutine
     appends to `<id>.part.wav` with an fsync every 2 s.
  4. Fire the start notice in a goroutine.
- **Stop**: stop capture, finish the WAV, update the sidecar, queue for hand-off.
- **Discard**: stop, delete the spool files, send the discard notice.

**Other events**:

- A 1 s `Tick` enforces the maximum length.
- `DeviceLost` comes from evdev `ENODEV` or the miniaudio `Stop` callback.

**Hand-off loop**:

- Recordings go oldest first through `PUT /mic/v1/recordings/{id}`.
- The spool copy is deleted only on stored or duplicate.
- Failures retry with `capture.Backoff`; a rejected recording is kept and skipped (contract
  table).

**Startup**: `Recover()` runs first.

**Health and check-ins**:

- `/healthz` on `MIC_HTTP_ADDR` reports the receiver, storage, pending count and age, rejected
  recordings, and 401 misconfiguration.
- A check-in is sent every `MIC_CHECKIN_INTERVAL`.

**Commands**:

- `voicenotes-mic` (or `run`): start the recorder.
- `voicenotes-mic health [--strict]`: GET the recorder's own `/healthz` on `MIC_HTTP_ADDR` (an empty or
  unspecified host is reached over loopback), print the body, and exit `0` on HTTP 200 and `1` otherwise
  (including a request failure or timeout of 5 s). `--strict` additionally exits `1` when the body's `status`
  is not `ok` (so `degraded`, or an unparsable body). It is a container or supervisor probe: without
  `--strict` a `degraded` recorder still passes.
- `voicenotes-mic version`: print the version.

### Service (`cmd/voicenotes`, `internal/micapi`, `internal/pipeline`, `internal/capture`, …)

- **`pipeline.IngestRecording(ctx, RecordingMeta{ID, StartedAt, EndReason, Recorder}, localPath)`**:
  1. Build the audio capture: `source = "mic"`, chat = `MIC_NOTIFY_CHAT_ID`, message = ID, first
     step `extract`, `ReceivedAt = StartedAt`, one `audio/wav` file.
  2. Insert through the new store method `InsertRecording`. It takes the `mic_replies` row in the
     same transaction and sets `ReplyMessageID`.
  3. Return Stored or Duplicate; on Duplicate the caller removes the file.
- **`micapi.Handler`**: the four routes of contracts/mic-http-api.md, including pure header
  validation in `micapi/headers.go`. It is mounted under `/mic/v1/` only when `MIC_UPLOAD_TOKEN`
  is set.
- **`micapi.Starts`**: sends "🎙 recording…" asynchronously and deletes it on discard (new
  `Replier.Delete` and `telegram.API.Delete`).
- **`micapi.AlertLoop`**: runs `mic.Alerts` over `mic_recorders` every 30 s and sends
  transitions.
- **Server timeouts**: `serveHTTP` gains `ReadTimeout: 2m` and `WriteTimeout: 2m30s` (the read
  timeout plus 30 s, since Go's `WriteTimeout` runs from the end of the headers through the reply). The upload
  handler extends both for its request from `Content-Length` at no less than 64 KiB/s (floor 2m).
- **No speech**:
  - `capture.NoSpeech` plus a `Policy.Next` rule: extract OK with no speech goes to
    `finalize_reply`.
  - `extract` sets the flag: no speech region from speaches' voice activity detector
    (`STT_VAD`), or an empty transcript, or every segment at or above
    `STT_NO_SPEECH_THRESHOLD`. `replyText` gains its text.
  - `cleanup` moves microphone audio to `nospeech/`.
  - `PurgeLoop` purges `nospeech/` and stale `mic_replies`.
  - `render.NoSpeech` is removed.
- **Health**: `Input.RecorderAlerts`. Any outstanding alert makes the status degraded, and the
  report lists the recorders.

### Build, fence, release

- **`Taskfile.yml`**:
  - `build` stays `CGO_ENABLED=0 go build ./cmd/voicenotes`.
  - New `build:mic`: `CGO_ENABLED=1 go build -o bin/voicenotes-mic ./cmd/voicenotes-mic`.
  - `COVERPKG` gains three packages.
- **`.golangci.yml`**: a `depguard` rule denies `github.com/gen2brain/malgo` and
  `…/internal/micaudio` outside `internal/micaudio/**` and `cmd/voicenotes-mic/**`.
- **Fence test**: `cmd/voicenotes/fence_test.go` runs `go list -deps ./cmd/voicenotes` and fails
  if either appears.
- **`ci.yml`**:
  - The `-coverpkg` update.
  - A `build-mic` matrix on `ubuntu-24.04` and `ubuntu-24.04-arm` running `task build:mic`.
  - Lint and test jobs need gcc, which the runners already have.
  - A `release-snapshot` job builds the release archives with GoReleaser, publishing nothing.
- **`.goreleaser.yaml`** and **`release.yml`**: GoReleaser builds
  `voicenotes-mic_<version>_linux_{amd64,arm64}.tar.gz` (binary, `LICENSE`,
  `deploy/voicenotes-mic.service`, `deploy/voicenotes-mic.env.example`) and a checksums file,
  cross-compiling arm64 with `gcc-aarch64-linux-gnu`, and creates the GitHub release with them and
  the notes. The images are unchanged.
- **`deploy/voicenotes-mic.service`**: `User=voicenotes-mic`, `SupplementaryGroups=audio input`,
  `StateDirectory=voicenotes-mic`, `EnvironmentFile=/usr/local/etc/voicenotes-mic.env`, `Restart=always`.
- **`.claude/CLAUDE.md`**: the gate table gains `task build:mic`, and the CI description and
  coverage list are updated.

## Project Structure

### Documentation (this feature)

```text
specs/003-usb-mic-capture/
├── plan.md              # This file
├── research.md          # Phase 0: decisions R1–R13
├── data-model.md        # Phase 1: recording, tap machine, capture changes, alerts, migration 004
├── quickstart.md        # Phase 1: hardware verification and validation scenarios
├── contracts/
│   ├── mic-http-api.md  # recorder ↔ service HTTP API
│   ├── telegram-ux.md   # start/outcome messages, no-speech change, alerts
│   └── configuration.md # service and recorder environment variables
├── checklists/requirements.md
└── tasks.md             # Phase 2 (/speckit-tasks)
```

### Source Code (repository root)

```text
cmd/voicenotes/              # service: mount /mic/v1 + alert loop (conditional); server timeouts; fence_test.go
cmd/voicenotes-mic/          # NEW recorder binary (cgo): main, wiring, /healthz, health/version commands
internal/
├── mic/                     # NEW (gated, pure): Toggle, WAV, evdev decode, capability bitmap, MatchReceiver, Alerts, IDs
├── recorder/                # NEW (gated): Recorder loop, DirSpool + recovery, HTTP hand-off/notifier/check-in, health
├── micapi/                  # NEW (gated): /mic/v1 handlers + header validation, Starts, AlertLoop
├── micdev/                  # NEW adapter (stdlib, not gated): evdev reader + EVIOCGRAB, sysfs walk
├── micaudio/                # NEW adapter (cgo/malgo, not gated, recorder-only)
├── capture/                 # SourceMic, NoSpeech, Policy.Next rule
├── pipeline/                # IngestRecording, NoSpeech in extract/reply/cleanup, purge, Replier.Delete
├── render/                  # drop NoSpeech main text
├── health/                  # recorder alerts → degraded
├── telegram/                # API.Delete
├── config/                  # service MIC_* settings; ParseRecorder
└── store/sqlite/            # migration 004; InsertRecording; mic_replies; mic_recorders
deploy/                      # NEW: voicenotes-mic.service, voicenotes-mic.env.example
Taskfile.yml, .golangci.yml, .github/workflows/{ci,release}.yml, .claude/CLAUDE.md, README.md, .env.example
```

**Structure Decision**: one module with two binaries. All recorder logic that has decisions in it
lives in gated, cgo-free packages (`mic`, `recorder`), so it is tested with fakes on any machine.
The two device adapters are thin. Only `micaudio` uses cgo, and the fence keeps the service from
ever importing it.

## Delivery order (the hardware arrives later)

1. The no-speech rule: independent, and immediately useful for Telegram.
2. The pure `mic` package.
3. The service side: migration, `IngestRecording`, `/mic/v1/`, start messages, alerts, health.
4. The `recorder` package against fakes.
5. `micdev` and `micaudio` against the laptop's microphone and keyboard; the `voicenotes-mic`
   command; build, fence and release.
6. Kit verification, then the full quickstart in both modes on the Pi.

## Post-design Constitution Check

- **VI holds**:
  - Sidecar first, periodic fsync, repair on start.
  - Spool deletion is gated on 200/201.
  - `InsertRecording` takes the reply row in the same transaction.
  - The no-speech hold keeps the only copy for 7 days, per the v1.1.0 exception.
- **IV holds**: cgo is only in `micaudio` and `cmd/voicenotes-mic`, with the depguard rule and the
  fence test, and the service image is unchanged.
- **II holds**: `syscall`, `unsafe` and cgo live only in the two adapters, and every decision is in
  a gated package.
- **V holds**: everything is conditional, and the recorder needs no service secrets beyond the
  upload token.

No new violations.

## Complexity Tracking

No Constitution violations to justify.
