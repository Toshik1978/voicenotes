# Research: Wireless microphone captures

Decisions R1–R13 for `specs/003-usb-mic-capture`, revised after the redesign to a separate
recorder binary (constitution v2.0.0). Each lists the decision, why, and what else was considered.

## R1. Hardware behaviour of the target kit

- **Decision**: target the DJI Mic Mini 2S transmitter with the DJI Mobile Receiver (USB
  `2ca3:4011`). The working assumptions until checked on the real kit:
  - The receiver is one USB device with a USB Audio Class capture interface and a HID "Consumer
    Control" interface.
  - One transmitter tap produces `EV_KEY KEY_VOLUMEUP (115)` value 1, then value 0, about 1 ms
    apart.
  - A long press is pairing and is not expected to reach the host.
- **Rationale**: these are the only Linux reports of the kit, from three independent third-party
  sources: kristofferR/sottoniox issues #4 and #9 (Linux evtest and hidraw), piedol/lavdeck
  PROTOCOL.md (macOS), and caezium/dji-mic-wispr-flow.
- **Verification gate** (when the kit arrives, before the device tasks close):
  - `lsusb` lists the receiver.
  - `arecord -l` shows a capture device.
  - `evtest` shows one press and release of `KEY_VOLUMEUP` per tap.
  - A test recording through `voicenotes-mic` plays back clearly.
  - The captured outputs, anonymised with serials removed, become test fixtures.
- **Alternatives**: the Hollyland Lark M2, which has no Linux evidence. The fallback if the kit
  fails verification is a Zigbee button through Home Assistant.

## R2. Reading the button: evdev with the standard library

- **Decision**: read `/dev/input/eventN` directly.
  - Find the device: the input whose `/sys/class/input/eventN/device/capabilities/key` bitmap
    contains bit 115 and that sits under the same USB device as the chosen capture card (R4).
  - Grab it with `ioctl(fd, EVIOCGRAB = 0x40044590, 1)` via `syscall.Syscall`.
  - Decode 24-byte `struct input_event` records. Both targets are 64-bit, so there is one layout:
    two 64-bit timeval words, then `u16` type, `u16` code and `s32` value.
  - A tap is `EV_KEY`/`KEY_VOLUMEUP` with value 1. Release (0) and autorepeat (2) are ignored.
  - A read error of `ENODEV` (receiver unplugged) becomes `DeviceLost`. This is the reliable unplug
    signal, because miniaudio's ALSA backend may not report a lost capture device promptly.
- **Rationale**: about 50 lines, with no cgo and no dependency. The grab keeps the key from
  changing the host's volume or reaching other programs (FR-202). Decoding and bitmap parsing are
  pure and tested on fixture bytes.
- **Alternatives**:
  - `holoplot/go-evdev` (MIT, maintained, 63 stars): too small a gain to justify a dependency.
  - hidraw reports: vendor-specific.

## R3. Recording audio: miniaudio through `malgo` (cgo), in the recorder only

- **Decision**: `github.com/gen2brain/malgo` (Unlicense, actively maintained), approved as the
  fifth dependency and fenced to the recorder (R13).
  - `InitContext([]Backend{BackendAlsa}, …)`, then `ctx.Devices(malgo.Capture)` to list capture
    devices.
  - Choose the device whose ALSA ID names the card found by discovery (`hw:<card>,…`). An explicit
    `MIC_CAPTURE_DEVICE` name substring overrides discovery.
  - `InitDevice` with `DeviceType: Capture` and `Capture.Format = FormatS16`, `Channels = 1`,
    `SampleRate = 16000`. miniaudio's data converter downmixes and resamples from the device's
    native format.
  - The `Data` callback copies frames into a bounded channel, which the spool writer drains. The
    callback never blocks or allocates. A full channel drops frames, counts the drop and logs it;
    it should never happen at 32 kB/s.
  - The `Stop` callback fires when the device stops unexpectedly and becomes `DeviceLost`.
- **Rationale**: miniaudio is a mature single-file C library used widely. Using it removes the
  hand-written ALSA ioctls and the resampler, about 500 lines and the riskiest hardware-specific
  code. miniaudio loads `libasound` at runtime, so the build needs only a C compiler; each host
  needs `libasound2t64` (Ubuntu 24.04 and later) or `libasound2`.
- **Alternatives**:
  - Hand-written ALSA ioctls: the rejected plan.
  - `yobert/alsa` (pure Go, stale, x86_64-centric).
  - `jfreymuth/pulse` (needs a sound server).
  - `gordonklaus/portaudio` (cgo plus a heavier C dependency).

## R4. Device discovery

- **Decision**: discovery pairs a sound card and an input device that share a USB parent.
  - Walk `/sys/class/sound/card*/` and `/sys/class/input/event*/`, resolve each `device` link up
    to its USB device node, and pick the first USB device (by sysfs path) that has a capture PCM
    (`pcmC<card>D*c`) and an input with bit 115.
  - The result is a card number (for R3) and a `/dev/input/eventN` path (for R2).
  - `MIC_CAPTURE_DEVICE` and `MIC_INPUT_DEVICE` together override discovery. Setting only one is a
    configuration error.
  - Discovery repeats every 5 s while nothing is found (FR-206).
- **Rationale**: this avoids matching a laptop's built-in microphone and keyboard volume key by
  accident. It is pure over an injected filesystem (`fs.FS`), so tests run on a fixture tree.
- **Alternatives**: matching by device name. That is vendor-specific and contradicts the spec's
  "any receiver".

## R5. Audio file format

- **Decision**: 16-bit mono 16 kHz PCM WAV at 32 kB/s, about 19.2 MB per 10 minutes. The maximum
  length is capped at 12 minutes (about 23 MB), under the 25 MB speech-to-text limit (FR-208) and
  Memos' default 32 MiB attachment limit. The WAV header encode, parse and repair code lives in
  `internal/mic` and is pure.
- **Rationale**: needs no encoder or dependency, and speech-to-text models work at 16 kHz mono
  internally.
- **Alternatives**: Opus or OGG (a codec dependency), FLAC (no need given the limits).

## R6. Crash-safe recording files (FR-205)

- **Decision**: each recording lives in the recorder's **spool**:
  - `<id>.json` sidecar (identity, start time, recorder name) is written and fsynced, along with
    the spool directory, **before** the first sample.
  - `<id>.part.wav` has a placeholder header. Samples are appended and fsynced every 2 s.
  - On stop: patch the sizes, fsync, rename to `<id>.wav`, fsync the directory, and update the
    sidecar.
  - **Recovery at start**: each `.part.wav` is repaired (sizes recomputed from the file length and
    truncated to whole frames) and finished with reason `interrupted`. Unmatched halves move to
    `spool/broken/` and are never silently deleted.
- **Rationale**: the header is the only state that needs repair, and the file length is enough to
  recover it. At most about 2 s is lost, within SC-202's 5 s.
- **Alternatives**: raw PCM with a later conversion; SQLite on the box.

## R7. Recording identity

- **Decision**: a random positive `int63` from `crypto/rand`, drawn at start, written as decimal
  on the wire and in file names.
- **Rationale**: it maps onto the existing unique dedupe key
  `(source, source_chat_id, source_message_id)` with `source = "mic"`,
  `source_chat_id = <notification chat>` and `source_message_id = <recording ID>`. The schema needs
  no new dedupe column, and the ack-hold key works unchanged.
- **Alternatives**: UUID strings (a new column); per-recorder counters (collide across recorders).

## R8. One hand-off path: the upload endpoint

- **Decision**: every recorder, including one on the service's own host, uploads over HTTP.
  - On the service, the handler streams the body into `files/` (temp file, hash, fsync, rename,
    directory fsync) and calls `pipeline.Ingester.IngestRecording(ctx, meta, path)`.
  - `IngestRecording` builds the audio capture at `extract` with `ReceivedAt = meta.StartedAt`,
    attaches the start reply (R10), and runs `Ingest`. On `ErrDuplicate` it removes the new file.
  - The recorder deletes its spool copy only on 200 or 201.
- **Rationale**: one path means one set of crash cases.
  - A crash before the insert: the spool keeps the recording and re-uploads it.
  - A crash after the insert: the re-upload hits a duplicate (200) and the spool is cleaned.
  - An unreferenced file in `files/` is removed by the existing orphan sweep.
  - Uploading to localhost costs nothing.
- **Alternatives**: in-process direct mode, which would put device code and cgo into the service
  (rejected by the redesign).

## R9. HTTP protocol

- **Decision**: plain HTTP/1.1 on the service's existing `HTTP_ADDR` mux under `/mic/v1/`, with
  `Authorization: Bearer <MIC_UPLOAD_TOKEN>` compared in constant time.
  - The upload is an idempotent `PUT /mic/v1/recordings/{id}` with a WAV body and
    `X-Content-SHA256`. The service verifies length, hash and WAV header before inserting.
  - The start notice, discard notice and check-in are small JSON calls.
  - The server gets `ReadTimeout` 2m and `WriteTimeout` 2m30s (Go's `WriteTimeout` runs from the
    end of the request headers through the response, so it is the read timeout plus a 30 s margin
    for the reply). An upload extends both for its own request, sized from its length at no less
    than 64 KiB/s plus 30 s (never under 2m), so a long recording over weak Wi-Fi still lands. The
    upload route wraps the body in `http.MaxBytesReader(MIC_UPLOAD_MAX_BYTES)`.
  - The full contract is in [contracts/mic-http-api.md](contracts/mic-http-api.md).
  - Client retry uses `capture.Backoff`: exponential with jitter, capped at
    `MIC_RETRY_MAX_INTERVAL`, oldest recording first, without limit. Handling of 401, 413 and 415
    is per the contract.
- **Rationale**: a `PUT` to a client-chosen ID is idempotent by construction (FR-217). The hash
  catches bodies truncated on flaky Wi-Fi. Using the existing listener keeps the service's
  deployment unchanged (FR-219), and TLS is left to the deployment.
- **Alternatives**: multipart, gRPC, or routing through Telegram.

## R10. The recording's Telegram reply (FR-210, FR-222, FR-222a)

- **Decision**: add a table `mic_replies(recording_id PK, chat_id, message_id, created_at)`.
  - `POST /mic/v1/recordings/{id}/started` makes the service send a silent "🎙 recording…" with
    `Replier.Send(chat, 0, …)` asynchronously and store the message ID.
  - `IngestRecording` takes the row in the insert transaction and sets `ReplyMessageID`, so the
    existing queued, saved and failed edits apply unchanged. Without a row, `finalize_reply`
    already sends a new message.
  - `DELETE …/started` (a discard below the minimum length) deletes the message through a new
    `telegram.API.Delete`.
  - Rows still present after 1 day are edited to "⚠ recording not received" and deleted.
  - The recorder sends the start notice from a goroutine with a 5 s timeout and never waits on it.
- **Rationale**: this reuses the existing reply machinery. The notice is best-effort and never
  delays recording.
- **Alternatives**: the recorder holding the bot token (spreads the secret); creating the capture
  at start (it would break "audio on disk before insert").

## R11. No speech, no note (FR-210a, FR-210b; constitution v1.1.0+ VI)

- **Decision**:
  - `extract` sets `Capture.NoSpeech` when a successful transcription of an audio capture is empty
    after trimming.
  - `Policy.Next` sends OK with `NoSpeech` to `finalize_reply`, which replies "🔇 no speech heard —
    not saved".
  - `cleanup` removes files for Telegram captures. For `source = "mic"` it moves them to
    `$DATA_DIR/nospeech/<id>.wav`, and `PurgeLoop` deletes them after `MIC_NOSPEECH_HOLD`
    (default 168h) by mtime.
  - `render.NoSpeech` is removed.
  - A failed transcription keeps the placeholder-note path.
- **Rationale**: a pure policy change in gated packages, permitted by the amended Principle VI.
- **Alternatives**: deciding at `write_note`; a separate voice-activity model.

## R12. Recorder status and alerts (FR-227, FR-227a)

- **Decision**: a pure alert machine, `mic.Alerts`.
  - Alerts are per condition: `silent` (no check-in for `MIC_SILENCE_ALERT`), `storage_full` and
    `missing_receiver` are tracked separately as a set (`mic.AlertSet`). Each condition sends one
    alert when it starts and one recovery when it clears, independently of the others, so a
    receiver unplugged while storage is full still alerts. While a recorder is silent only the
    silence is reported; on "back online" the other conditions are re-evaluated and whatever
    changed meanwhile alerts or recovers. Each message is sent before its new set is stored, so a
    crash repeats at most one message and loses none.
  - The outstanding conditions are stored in `mic_recorders(name PK, last_seen, receiver_present,
    storage_full, alert)`, as `ok` or the condition names joined by commas.
  - Every recorder checks in over HTTP every `MIC_CHECKIN_INTERVAL` (default 1m), including one on
    the service's host, so there is no in-process special case.
  - The service's `AlertLoop` evaluates every 30 s, sends transitions silently, and degrades
    `/healthz` while any alert is outstanding.
- **Rationale**: storing the outstanding alert prevents repeats across restarts, and a dead box is
  detected by its silence.
- **Alternatives**: alerting from the box itself (it has no bot token, and a dead box can't
  report).

## R13. Building, fencing and shipping the recorder

- **Decision**:
  - **Binary**: `cmd/voicenotes-mic`, in the same module.
  - **cgo packages**: only `internal/micaudio` (the `malgo` adapter) and the `cmd/voicenotes-mic`
    main use cgo.
  - **Fence** (constitution IV):
    - golangci-lint `depguard` denies `github.com/gen2brain/malgo` and `internal/micaudio`
      everywhere except those two paths.
    - A test in `cmd/voicenotes` runs `go list -deps ./cmd/voicenotes` and fails if either
      appears.
    - `task build` keeps `CGO_ENABLED=0` for `./cmd/voicenotes`.
  - **Build**: `task build:mic` builds `bin/voicenotes-mic` with `CGO_ENABLED=1`. CI builds and
    tests it on the `ubuntu-24.04` and `ubuntu-24.04-arm` runners, natively with no
    cross-compiler.
  - **Release**: `release.yml` runs GoReleaser (`.goreleaser.yaml`) on one Ubuntu 24.04 runner.
    malgo links only libc, libdl, libpthread and libm (miniaudio loads ALSA with `dlopen`), so
    arm64 cross-compiles with `gcc-aarch64-linux-gnu`. It produces
    `voicenotes-mic_<version>_linux_{amd64,arm64}.tar.gz` containing the binary, `LICENSE`, and
    `deploy/voicenotes-mic.service` (an example systemd unit with `User=voicenotes-mic`,
    `SupplementaryGroups=audio input`, `StateDirectory=voicenotes-mic` and
    `EnvironmentFile=/usr/local/etc/voicenotes-mic.env`) plus the env example, and a checksums file, and
    creates the GitHub release with them and the notes.
    The images are unchanged (amd64 and arm64). Building on 24.04 (glibc 2.39) runs on Ubuntu
    26.04.
  - **Health address**: the recorder serves `/healthz` on `MIC_HTTP_ADDR` (default
    `127.0.0.1:8081`), so it can share a host with the service on `:8080`.
- **Rationale**: it runs natively, so hotplug and permissions work with plain group membership.
  The service image and its static build are untouched.
- **Alternatives**:
  - A recorder container: bind mounts and cgroup rules for hotplug, and a cgo image.
  - A second Go module: more machinery for little isolation gain.
  - A separate repository: protocol drift.
