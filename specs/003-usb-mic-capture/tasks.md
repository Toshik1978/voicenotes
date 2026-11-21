---
description: "Task list for wireless microphone captures"
---

# Tasks: Wireless microphone captures

**Input**: Design documents from `specs/003-usb-mic-capture/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: every task is implemented test-first (Constitution II). Each task writes its own stdlib
`testing` tests, using hand-written fakes and `httptest`. There are no standalone test tasks.

**Organization**:

1. Setup: configuration, coverage scope, the cgo fence.
2. Foundational: the pure recording core, the store, the ingest path, the spool and the recorder
   loop.
3. One phase per user story, in priority order.
4. Polish, including verification on the real kit.

Each phase is one commit (Conventional Commits, Constitution VIII).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1–US4 from spec.md

## Path Conventions

- One Go module, `github.com/Toshik1978/voicenotes`.
- Two binaries:
  - `cmd/voicenotes`: the service; CGO-free, distroless.
  - `cmd/voicenotes-mic`: the recorder; cgo.
- New packages:
  - `internal/mic`: pure, gated.
  - `internal/recorder`: gated.
  - `internal/micapi`: gated.
  - `internal/micdev`: stdlib device adapter, not gated.
  - `internal/micaudio`: cgo `malgo` adapter, not gated, imported only by `cmd/voicenotes-mic`.
- Real-device tests run only with `VN_MICDEV=1`.

---

## Phase 1: Setup (configuration, coverage scope, cgo fence)

**Purpose**: every new setting is parsed and validated up front, the coverage gate covers the new
packages, and constitution IV's fence exists before any cgo code does.

- [x] T001 Add the service's `MIC_*` settings to `internal/config/config.go` with validation, per contracts/configuration.md "`voicenotes` (the service)". Tests go in `internal/config/config_test.go`.
  - Settings:
    - `MIC_UPLOAD_TOKEN`: unset by default. When set: "at least 32 characters, else a startup error".
    - `MIC_UPLOAD_MAX_BYTES`: `31457280`, range "1 MiB – 1 GiB".
    - `MIC_NOTIFY_CHAT_ID`: defaults to `DIGEST_CHAT_ID`; "must be non-zero when `MIC_UPLOAD_TOKEN` is set".
    - `MIC_NOSPEECH_HOLD`: `168h`, range "1h – 2160h".
    - `MIC_SILENCE_ALERT`: `5m`, range "1m – 24h".
  - Errors name the variable and never print a value.
  - With no `MIC_*` set, `Config` is unchanged.
- [x] T002 Add `config.ParseRecorder(environ) (RecorderConfig, error)` in `internal/config/recorder.go`, per contracts/configuration.md "`voicenotes-mic` (the recorder)". Tests go in `internal/config/recorder_test.go`.
  - Settings:
    - `MIC_SERVER_URL`: required; an absolute http/https URL.
    - `MIC_UPLOAD_TOKEN`: required; "at least 32 characters".
    - `MIC_RECORDER_NAME`: defaults to the hostname; "1–64 characters of `[A-Za-z0-9._-]`".
    - `MIC_CAPTURE_DEVICE` and `MIC_INPUT_DEVICE`: "must be set together", and setting only one is an error naming the missing one.
    - `MIC_DEBOUNCE`: `500ms`, range "0 – 5s".
    - `MIC_MIN_LENGTH`: `2s`, range "0 – 60s".
    - `MIC_MAX_LENGTH`: `10m`, range "10s – 12m".
    - `MIC_SPOOL_DIR`: `$STATE_DIRECTORY`, else `/var/lib/voicenotes-mic`.
    - `MIC_SPOOL_MIN_FREE`: `268435456`; "at least 2 × the size of a maximum-length recording", at 32 000 B/s.
    - `MIC_CHECKIN_INTERVAL`: `1m`, range "10s – 10m".
    - `MIC_RETRY_MAX_INTERVAL`: `5m`, range "10s – 1h".
    - `MIC_PENDING_THRESHOLD`: `30m`.
    - `MIC_HTTP_ADDR`: `127.0.0.1:8081`.
    - `LOG_LEVEL`.
  - It MUST NOT read or require any Telegram, STT, LLM, Memos or digest variable.
- [x] T003 Set up the coverage scope and the cgo fence (constitution v2.0.0 IV):
  - Add `./internal/mic,./internal/recorder,./internal/micapi` to `COVERPKG` in `Taskfile.yml` **and** to `-coverpkg` in `.github/workflows/ci.yml`, keeping the two identical.
  - Update the coverage-scope list in `.claude/CLAUDE.md` to `internal/{config,capture,pipeline,language,enrich,render,digest,health,mic,recorder,micapi}`.
  - Add a `depguard` rule to `.golangci.yml` that denies `github.com/gen2brain/malgo` and `github.com/Toshik1978/voicenotes/internal/micaudio` in every file except `internal/micaudio/**` and `cmd/voicenotes-mic/**`.
  - Add `cmd/voicenotes/fence_test.go`. It runs `go list -deps ./cmd/voicenotes` (via `os/exec`, skipped when `go` is not on `PATH`) and fails if either import path appears.

---

## Phase 2: Foundational (recording core, store, ingest, spool, recorder loop)

**Purpose**: the building blocks every story uses.

**Checkpoint**: all existing tests pass. A WAV file passed to `IngestRecording` becomes a note
through the unchanged pipeline, and the recorder loop runs against fakes.

- [x] T004 [P] Implement the tap state machine `mic.Toggle` in `internal/mic/toggle.go`. Tests go in `internal/mic/toggle_test.go`.
  - States: `idle`, and `recording{id, started_at, last_toggle_at}`.
  - Events: `Tap(t)`, `Tick(t)`, `DeviceLost(t)`.
  - Actions: `Start(id, t)`, `Stop(id, reason)`, `Discard(id)`, `Refuse(reason)`.
  - Implement **every row** of the data-model.md "Tap state machine" table:
    - Debounce when `t − last_toggle < debounce`.
    - `Refuse` "storage full or no audio device".
    - Below the minimum length, a Tap or DeviceLost discards.
    - A `Tick` with `t − started ≥ max length` stops with `max_length`.
    - `last_toggle_at` is updated on every Start, Stop and Discard.
  - End reasons are exactly `tap`, `max_length`, `receiver_lost`, `interrupted`.
  - IDs come from an injected generator. The production generator in `internal/mic/id.go` draws a "random positive int63 from `crypto/rand`" (research R7).
- [x] T005 [P] Implement WAV handling in `internal/mic/wav.go`. Tests go in `internal/mic/wav_test.go`.
  - `Header(dataBytes)` returns a 44-byte PCM header for 16 kHz, mono, 16-bit.
  - `Parse(r)` accepts only PCM, 16-bit, mono, 16 kHz. Anything else returns a typed error, which the service later maps to 415.
  - `Repair(fileLen)` computes the RIFF and `data` sizes from the file length, truncated to whole 2-byte frames (research R6).
  - Table cases: an empty data chunk, an odd length, a placeholder header, a non-PCM file.
- [x] T006 [P] Implement the evdev and discovery logic as pure code in `internal/mic/evdev.go` and `internal/mic/match.go`. Tests go in `internal/mic/evdev_test.go` and `internal/mic/match_test.go`, using fixtures in `internal/mic/testdata/`.
  - `DecodeEvent(b [24]byte)` decodes the 64-bit `input_event` layout (research R2).
  - `IsTap(ev)` is true only for `EV_KEY`, code 115, value 1.
  - `HasKey(capabilitiesKeyHex, 115)` parses the sysfs bitmap: space-separated 64-bit hex words, most significant first.
  - `MatchReceiver(snapshot)` takes a snapshot of sound cards (card number, has capture PCM, USB parent path) and input devices (event path, key bitmap, USB parent path). It returns the first USB parent, by path, that has both a capture PCM and an input with bit 115, as `{Card, InputPath}`, or "none" (research R4).
- [x] T007 Add the microphone source and its store support.
  - Add `capture.SourceMic = "mic"` in `internal/capture/capture.go`.
  - Add migration `internal/store/sqlite/migrations/004_mic.sql`, exactly as in data-model.md: `captures.no_speech`, `mic_replies`, and `mic_recorders` with `alert TEXT NOT NULL DEFAULT 'ok'`.
  - In `internal/store/sqlite/store.go` and a new `internal/store/sqlite/mic.go`:
    - `InsertRecording(ctx, *capture.Capture)`: inserts the capture and, **in the same transaction**, selects and deletes the `mic_replies` row with `recording_id = SourceMessageID` and sets `ReplyMessageID`.
    - `PutStartReply`, `TakeStartReply`, `StaleStartReplies(olderThan)`.
    - `UpsertRecorder`, `Recorders`, `SetRecorderAlert`.
    - Persist `NoSpeech`.
  - Tests go in `internal/store/sqlite/mic_test.go` and `restart_test.go`. Cover a duplicate recording ID (`ErrDuplicate`) and migrating a 003 database.
- [x] T008 Implement `pipeline.Ingester.IngestRecording(ctx, RecordingMeta{ID, StartedAt, EndReason, Recorder}, localPath string) (RecordingResult, error)` in `internal/pipeline/ingest_recording.go`, per research R8 and data-model.md "Capture changes".
  - The caller has already fsynced `localPath` into `files/`.
  - Build the capture:
    - `Kind=audio`, first step `extract`, `ReceivedAt=StartedAt`.
    - `Source=SourceMic`, `SourceChatID` = notify chat, `SourceMessageID` = ID.
    - One `File{Kind: audio, MimeType: "audio/wav", LocalPath}` with the same identity.
  - Insert through `InsertRecording` inside `Ingest`, with a nil ack, then wake the workers.
  - Return `Stored`, or `Duplicate` on `ErrDuplicate`. On `Duplicate` the caller removes its file.
  - Tests go in `internal/pipeline/ingest_recording_test.go`, using fakes:
    - stored, duplicate, and an insert failure
    - an end-to-end run through the existing handlers to a note with the WAV attached and note time = `StartedAt`
- [x] T009 [P] Add message deletion.
  - Add `Delete(ctx, chatID, msgID int64) error` to `pipeline.Replier` in `internal/pipeline/steps.go`, and update every fake.
  - Add `telegram.API.Delete` in `internal/telegram/api.go`, using `bot.Delete` with errors wrapped like `wrapAPI` (400/403 become `pipeline.ErrRejected`). Tests go in `internal/telegram/api_test.go`.
- [x] T010 Implement `recorder.DirSpool` in `internal/recorder/spool.go`, per research R6 and the data-model.md "Spool layout". Tests go in `internal/recorder/spool_test.go` and run on `t.TempDir()`.
  - `Begin(meta)`: write the `<id>.json` sidecar and fsync it and the directory **before** returning a writer.
  - The writer appends to `<id>.part.wav` (placeholder header) and fsyncs at most every 2 s.
  - `Finish(id, endedAt, reason)`: patch the sizes with `mic.Repair`, fsync, rename to `<id>.wav`, fsync the directory, update the sidecar.
  - `Discard(id)`.
  - `Finished()`: oldest `started_at` first.
  - `Open(id)` and `Remove(id)`.
  - `Recover()`: repair every `.part.wav` and finish it with reason `interrupted`. Move orphan halves to `broken/`; they are "never silently deleted".
  - `FreeBytes()` via `syscall.Statfs`.
- [x] T011 Implement `recorder.Recorder` in `internal/recorder/recorder.go`. Tests go in `internal/recorder/recorder_test.go`, with a hand-written fake for every interface and an injected clock; nothing sleeps for real.
  - Interfaces (plan.md "Recorder"): `KeySource`, `AudioSource`, `Discovery`, `Spool`, `Handoff` (results `Stored`, `Duplicate`, `Retry`, `Misconfigured`, `Rejected`), `Notifier`, `CheckIn`.
  - **Startup**: run `Recover()` first, then rediscover every 5 s while no receiver is found.
  - **Events**: taps, 1 s ticks and device-lost events go through `mic.Toggle`.
  - **Start**:
    1. Check free space against `MIC_SPOOL_MIN_FREE` (`Refuse` if lower).
    2. `Begin`.
    3. `AudioSource.Open(card)`. Frames arrive on a bounded channel and the writer goroutine drains it. Dropped frames are counted and logged.
    4. `Notifier.Started` runs in a goroutine and never blocks recording.
  - **Stop**: `Finish`. **Discard**: `Discard` plus `Notifier.Discarded`.
  - **Hand-off loop**:
    - Process recordings oldest first.
    - Delete the spool files **only** on `Stored` or `Duplicate`.
    - On `Retry`, back off with `capture.Backoff`, capped at `MIC_RETRY_MAX_INTERVAL`.
    - On `Misconfigured`, retry at the maximum interval.
    - Keep `Rejected` recordings and skip them.
  - **Status**: expose receiver present, storage full, pending count, oldest pending time, rejected count and misconfigured, for health and check-ins.

---

## Phase 3: User Story 1 - Tap, talk, tap: a note from the bedside (Priority: P1) 🎯 MVP

**Goal**: `voicenotes-mic` on the service's own host (direct mode, `MIC_SERVER_URL=http://127.0.0.1:8080`) turns tap–talk–tap into an `#inbox` note with the audio attached.
- A "🎙 recording…" message is edited into the outcome.
- An accidental tap leaves nothing behind.

**Independent Test**: quickstart.md §2 and §3 on one host. Use the laptop microphone and keyboard (explicit device settings) until the kit arrives.

- [x] T012 [US1] Implement `micapi.Starts` in `internal/micapi/starts.go`. Tests go in `internal/micapi/starts_test.go`.
  - `Started(ctx, id, startedAt)`:
    - Send a silent `🎙 recording…` with `Replier.Send(notifyChat, 0, …)` under a 10 s timeout, then `PutStartReply`.
    - A repeat for the same ID is a no-op.
  - `Discarded(ctx, id)`: `TakeStartReply` plus `Replier.Delete`. A missing row is a no-op.
  - Extend `pipeline.PurgeLoop` in `internal/pipeline/purge.go`: `mic_replies` rows "older than 1 day" are edited to `⚠ recording not received` (best-effort), then deleted.
  - Use exactly the texts in contracts/telegram-ux.md.
- [x] T013 [US1] Implement `micapi.Handler` in `internal/micapi/handler.go`, with pure request validation in `internal/micapi/headers.go`. Tests go in `internal/micapi/handler_test.go` and `headers_test.go`, using `httptest` and fakes. Follow contracts/mic-http-api.md exactly.
  - **Auth**: Bearer token, compared with `crypto/subtle`. On 401, nothing is stored and nothing is logged beyond the path and status.
  - **`{id}`**: decimal int64 > 0, else 400.
  - **`PUT /mic/v1/recordings/{id}`**:
    - Header checks: `Content-Type: audio/wav` (else 415); `Content-Length` over `MIC_UPLOAD_MAX_BYTES` gives 413 before the body is read.
    - Required headers: `X-Content-SHA256`, `X-Recording-Started-At` (RFC 3339 with nanoseconds), `X-Recording-End-Reason`, `X-Recorder`.
    - Stream the body through `http.MaxBytesReader` into a temp file in `files/`, hashing as it goes. A length or hash mismatch gives 400 `corrupt`, and the temp file is removed.
    - `mic.Parse` failure gives 415.
    - Otherwise fsync, rename, fsync the directory, then `IngestRecording`: 201 `stored`, 200 `duplicate` (new file removed), or 503 `unavailable`.
  - **Other routes**:
    - `POST …/started`: 202, calling `Starts.Started` asynchronously.
    - `DELETE …/started`: 204.
    - `PUT /mic/v1/recorders/{name}`: upserts `mic_recorders` with server `last_seen` and returns 204; a bad name gives 400.
  - Response bodies are `{"status","reason"}` and never contain a secret.
- [x] T014 [US1] Wire the API into the service in `cmd/voicenotes/service.go`. Tests go in `cmd/voicenotes/service_test.go`.
  - Mount `micapi.Handler` under `/mic/v1/` only when `cfg.MicUploadToken != ""`, as a plain conditional. Otherwise `/mic/v1/…` returns 404.
  - Set `serveHTTP`'s `ReadTimeout: 2m` and `WriteTimeout: 30s`.
- [x] T015 [US1] Implement the recorder's HTTP side in `internal/recorder/client.go`. Tests go in `internal/recorder/client_test.go`, against `httptest`.
  - `HTTPHandoff` streams the spool file with the contract headers and its SHA-256, and maps responses per the contract's "Client retry policy" table:
    - 201 → `Stored`; 200 → `Duplicate`.
    - 400 `corrupt`, 5xx and network errors → `Retry`.
    - 401 → `Misconfigured`.
    - 413, 415 and other 400s → `Rejected`.
  - `HTTPNotifier`: `POST` and `DELETE …/started`, with a 5 s timeout and no retries.
  - `HTTPCheckIn`: `PUT /mic/v1/recorders/{name}` with `{receiver_present, storage_full, pending, oldest_pending_at}`. The recorder calls it every `MIC_CHECKIN_INTERVAL`.
- [x] T016 [P] [US1] Implement the stdlib device adapter in `internal/micdev/keys.go` and `internal/micdev/discover.go`.
  - `Keys` opens the input device, calls `ioctl(fd, EVIOCGRAB=0x40044590, 1)` via `syscall.Syscall`, and reads 24-byte records into `mic.DecodeEvent`.
    - It emits `Tap` for `mic.IsTap`.
    - `ENODEV`, EOF or a read error emits `DeviceLost` and closes.
  - `Discover` walks `/sys/class/sound/card*` and `/sys/class/input/event*` through an injected `fs.FS`, resolves USB parents, and calls `mic.MatchReceiver`.
  - Tests go in `internal/micdev/keys_test.go` (a decode loop over a pipe fed with fixture bytes) and `discover_test.go` (a fake sysfs tree in `internal/micdev/testdata/sys/`). The real grab and read are tested only with `VN_MICDEV=1`.
- [x] T017 [P] [US1] Add `github.com/gen2brain/malgo` to `go.mod` (`build(deps)`), and implement `internal/micaudio/capture.go` (cgo) per research R3.
  - Initialise an ALSA-only context.
  - `Devices(Capture)` selects the device whose ALSA ID names the discovered card, or whose name contains `MIC_CAPTURE_DEVICE`.
  - `InitDevice`: Capture, `FormatS16`, `Channels=1`, `SampleRate=16000`.
  - The `Data` callback copies into a caller-supplied bounded channel without blocking. The `Stop` callback emits `ErrDeviceLost`.
  - It implements `recorder.AudioSource`.
  - Tests go in `internal/micaudio/capture_test.go`: a test that it compiles and has the right config always runs; a 1 s recording from the configured device runs with `VN_MICDEV=1`.
  - The depguard rule from T003 must allow only this package and `cmd/voicenotes-mic`.
- [x] T018 [US1] Add the recorder binary `cmd/voicenotes-mic/main.go` and `cmd/voicenotes-mic/run.go`. Tests go in `cmd/voicenotes-mic/main_test.go`.
  - **Commands**:
    - `run` (the default) wires `config.ParseRecorder`, `micdev`, `micaudio`, `recorder.DirSpool`, `recorder.Recorder` and the HTTP clients.
    - `health` GETs `http://<MIC_HTTP_ADDR>/healthz`.
    - `version` prints the version.
  - **`/healthz` on `MIC_HTTP_ADDR`**:
    - unhealthy if the recorder loop has stopped
    - degraded on: receiver missing, storage full, any rejected recording, misconfigured (401), or a pending recording older than `MIC_PENDING_THRESHOLD`
  - **Build tooling**:
    - Add the `build:mic` task to `Taskfile.yml`: `CGO_ENABLED=1 go build -o bin/voicenotes-mic ./cmd/voicenotes-mic`, with the same ldflags.
    - Add a `build-mic` job to `.github/workflows/ci.yml`, a matrix over `ubuntu-24.04` and `ubuntu-24.04-arm`.
    - In `.claude/CLAUDE.md`, add `task build:mic` to the gate table and update the CI description.
- [x] T019 [US1] Add an end-to-end direct-mode test in `cmd/voicenotes/mic_e2e_test.go`.
  - Setup: a real `recorder.Recorder` with fake keys and audio, `HTTPHandoff` against the real service mux in `httptest`, and a fake STT, Memos and Telegram.
  - Cover acceptance scenarios 1.1–1.6:
    - one capture per tap pair
    - `🎙 recording…` edited to `✓ saved · 1 task · open`
    - auto-stop at `MIC_MAX_LENGTH`
    - the input is grabbed (fake records it)
    - the note time equals the first tap
    - a double tap within `MIC_MIN_LENGTH` deletes the start message and creates no capture

---

## Phase 4: User Story 2 - Record near the bed, process on the server (Priority: P1)

**Goal**: the same recorder on the Pi forwards over the home network. Each recording yields exactly one note, however many retries it takes. The recorder installs from release tarballs as a systemd unit.

**Independent Test**: quickstart.md §3 on the Pi: service down, re-upload, wrong token, then install from the tarball.

- [x] T020 [US2] Add an end-to-end remote-resilience test in `cmd/voicenotes/mic_e2e_test.go`, covering acceptance scenarios 2.1–2.6:
  - one note per recording
  - three recordings made while the service is down are delivered oldest first once it is back
  - a repeated upload yields one note
  - the spool is deleted only after 200/201
  - a wrong token gives 401 and stores nothing; the recorder reports misconfigured
  - when the start notice failed, the outcome is sent as a new message
  - a 415-rejected recording is kept, skipped and reported
- [x] T021 [US2] Ship the recorder.
  - Add `deploy/voicenotes-mic.service`: `User=voicenotes-mic`, `SupplementaryGroups=audio input`, `StateDirectory=voicenotes-mic`, `EnvironmentFile=/etc/voicenotes-mic.env`, `ExecStart=/usr/local/bin/voicenotes-mic`, `Restart=always`.
  - Add `deploy/voicenotes-mic.env.example` with anonymised values only.
  - Add a matrix job to `.github/workflows/release.yml` on `ubuntu-24.04` and `ubuntu-24.04-arm`. It builds `voicenotes-mic_<version>_linux_{amd64,arm64}.tar.gz` (binary, `LICENSE`, both deploy files) and attaches them to the GitHub release created by the existing job. Leave the images unchanged.
  - Verify with `actionlint`.

---

## Phase 5: User Story 3 - Nothing is lost when something breaks mid-recording (Priority: P2)

**Goal**: interruptions keep whatever was recorded. A recorder that can't record shows up in health and sends one Telegram alert per change.

**Independent Test**: quickstart.md §3. Kill the recorder mid-recording, unplug the receiver, power the Pi off, fill its disk.

- [x] T022 [P] [US3] Implement `mic.Alerts` in `internal/mic/alerts.go`. Tests go in `internal/mic/alerts_test.go`.
  - `Eval(status, now, silence) → (newAlert, message?)`, per data-model.md "Recorder status".
  - Priority: `now − LastSeen > silence` → `silent`; else `StorageFull` → `storage_full`; else `!ReceiverPresent` → `missing_receiver`; else `ok`.
  - Emit only when the computed alert "differs from the stored `Alert`", and "never emit while the alert is unchanged".
  - Use exactly the texts in contracts/telegram-ux.md "Recorder alerts". `HH:MM` is in the configured `Location`.
- [x] T023 [US3] Implement `micapi.AlertLoop` in `internal/micapi/alerts.go`. Tests go in `internal/micapi/alerts_test.go`, with fake store, replier and clock.
  - Every 30 s, evaluate `mic.Alerts` for each `Recorders()` row.
  - Send each transition silently with `Replier.Send(notifyChat, 0, …)`, then call `SetRecorderAlert`. If the send fails, leave the stored alert unchanged so the next pass retries.
  - Wire it in `cmd/voicenotes/service.go` when `MIC_UPLOAD_TOKEN` is set.
- [x] T024 [US3] Report recorder alerts in the service health.
  - `internal/health/health.go`: add `Input.RecorderAlerts`. Any outstanding alert makes the status degraded. The report gains `recorders: [{name, alert, last_seen}]`.
  - Add a `Recorders` source in `internal/health/source.go` and the shim in `cmd/voicenotes/adapters.go`.
  - Tests go in `internal/health/health_test.go` and `source_test.go`.
- [x] T025 [US3] Add an end-to-end interruption and alert test in `cmd/voicenotes/mic_e2e_test.go`, covering acceptance scenarios 3.1–3.3 and SC-202, SC-209:
  - A spool with a `.part.wav` (a simulated kill after N s of fixture audio) is recovered at start into a note containing at least the audio that was fsynced.
  - Fake audio reporting `ErrDeviceLost` mid-recording gives a note that ends there, with reason `receiver_lost`.
  - No receiver: recorder health is degraded, and after its check-in the service sends one `receiver not connected` alert. Reconnecting sends one `receiver connected`.
  - A check-in with `storage_full` gives one alert, and a tap is refused.
  - No check-ins for `MIC_SILENCE_ALERT` gives one `not heard from since` alert; resuming sends `back online`.

---

## Phase 6: User Story 4 - No empty notes from silence, from any source (Priority: P2)

**Goal**: a successful transcript with no speech creates no note, for Telegram and the microphone alike. The reply says so, and microphone audio is held for `MIC_NOSPEECH_HOLD`.

**Independent Test**: quickstart.md §3 (silent recording) and §4 (Telegram silence regression).

- [ ] T026 [US4] Add the no-speech policy in `internal/capture/capture.go` and `internal/capture/next.go`, with tests alongside.
  - Add `Capture.NoSpeech bool`.
  - In `Policy.Next`: an OK outcome at `extract` for a capture with `NoSpeech` advances to `finalize_reply`, skipping `enrich` and `write_note`. Every other transition is unchanged.
  - A `Permanent` failure at `extract` still goes to `write_note` with `Failure` (a placeholder note).
- [ ] T027 [US4] Apply the rule in the pipeline. Tests go in `internal/pipeline/steps_test.go` and `internal/render/render_test.go`; update the 001 silence case in `cmd/voicenotes/pipeline_e2e_test.go` to FR-210a.
  - In `internal/pipeline/steps.go`:
    - `extract` sets `NoSpeech` when an audio capture's successful transcript is empty after `strings.TrimSpace`.
    - `replyText` returns `🔇 no speech heard — not saved` for `NoSpeech`.
    - `cleanup`, for `NoSpeech` with `Source == capture.SourceMic`, moves each local file to `$DATA_DIR/nospeech/<id>.wav` (rename, then directory fsync) instead of deleting it. Telegram files are removed as before.
  - In `internal/render/render.go`: remove `NoSpeech` and its use in `mainText`.
- [ ] T028 [US4] Purge held audio in `internal/pipeline/purge.go`. Tests go in `internal/pipeline/purge_test.go`.
  - `PurgeLoop` deletes files in `$DATA_DIR/nospeech/` whose mtime is older than `MIC_NOSPEECH_HOLD` (default `168h`).
  - Create the directory at startup in `cmd/voicenotes/service.go`, next to `files/`, with mode 0700.

---

## Phase 7: Polish & Cross-Cutting Concerns

- [ ] T029 [P] Document the feature in `README.md` and `.env.example`, using anonymised values only (`voicenotes.example.org`, `FAKE-…` tokens, chat `-1001234567890`). Cover:
  - what the receiver must do
  - the hardware check (quickstart.md §1)
  - installing `voicenotes-mic` from the release tarball with `libasound2t64`, the system user in the `audio` and `input` groups, and the systemd unit
  - direct mode (`MIC_SERVER_URL=http://127.0.0.1:8080`) and Pi mode
  - Pi notes: 5 GHz Wi-Fi or Ethernet, `dtoverlay=disable-bt`
  - service settings, the no-speech rule and the `nospeech/` hold, and the alerts
- [ ] T030 Verify on the real kit. This needs the DJI Mic Mini 2S and the Mobile Receiver; follow quickstart.md §1.
  - Capture `lsusb -v` (serials removed), the sysfs paths involved, and raw evtest bytes. Add them as fixtures to `internal/mic/testdata/` and `internal/micdev/testdata/`, and extend the T006 and T016 tests with them.
  - Run the `VN_MICDEV=1` tests on the server (amd64) and on the Pi (arm64, Ubuntu 26.04).
  - Update research.md R1 if anything differs.
- [ ] T031 Run the full quickstart.md §0–§4 with the recorder in both places, including the box out of the service host's radio range. Fix whatever it surfaces through the owning task's tests.

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (T001–T003)** comes first.
- **Foundational (T004–T011)** depends on Setup.
  - T004–T006 and T009 are independent ([P]).
  - T007 must precede T008.
  - T010 must precede T011, which also needs T004 and T005.
- **US1 (T012–T019)** depends on Foundational.
  - T012 must precede T013, which must precede T014.
  - T015 needs T011.
  - T016 and T017 are [P] with each other and with T012–T015.
  - T018 needs T015–T017. T019 needs T014 and T018.
- **US2 (T020–T021)** depends on US1. T021 needs T018.
- **US3 (T022–T025)** depends on US1. T022 is pure and can start any time after Setup.
- **US4 (T026–T028)** depends only on Setup (T001, for `MIC_NOSPEECH_HOLD`) and T007 (the `no_speech` column). It may run before US1, and it ships value for Telegram on its own. US1 scenario 1.7 (a silent recording) is complete once US4 lands.
- **Polish**:
  - T029 can run any time after T021.
  - T030 needs the hardware.
  - T031 comes last.

### Hardware timeline

Everything except T030 and the `VN_MICDEV` runs can be built and tested before the kit arrives.
T016 and T017 are exercised with the laptop's keyboard volume key and built-in microphone,
selected through `MIC_INPUT_DEVICE` and `MIC_CAPTURE_DEVICE`.

## Parallel Example: Foundational

```text
T004 mic.Toggle      T005 mic WAV      T006 mic evdev + MatchReceiver      T009 Replier.Delete
```

## Parallel Example: User Story 1

```text
T016 micdev (keys + discovery)      T017 micaudio (malgo)      (alongside T012–T015)
```

## Implementation Strategy

1. **MVP**: Setup, Foundational and US1. The recorder runs on the service's host. If the radio
   reaches the service host, this is the whole feature.
2. **Add US2**: release tarballs and the box near the bed.
3. **Add US3**: alerts and interruption safety.
4. **Add US4**: the general no-speech rule. It can be pulled forward.
5. **Polish**: docs, kit verification, the full quickstart.
