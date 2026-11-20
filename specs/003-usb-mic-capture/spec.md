# Feature Specification: Wireless microphone captures

**Feature Branch**: `003-usb-mic-capture`

**Created**: 2026-11-20

**Status**: Draft

**Input**: User description: "Wireless USB microphone capture — in bed, tap the button on a clip-on wireless mic transmitter, talk, tap again; the recording becomes a normal VoiceNotes note without touching the phone. Works with the receiver plugged into the VoiceNotes host (direct) or into a small box near the owner that forwards recordings (proxy)." (full brief in the brainstorming record; summarized below)

## Context

The owner's best thoughts arrive in bed, in the dark. Today capturing one means unlocking the phone, opening Telegram and holding the record button, which wakes them up. They want a button they can press without looking: tap, speak, tap, go back to sleep, and find the note in the morning inbox and digest.

The hardware is a consumer wireless microphone kit: a small clip-on **transmitter** with a button, and a USB **receiver** that appears to the computer it is plugged into as a sound input plus a one-key keyboard. A tap on the transmitter's button arrives at the computer as a single "volume up" key press. The target kit is the DJI Mic Mini 2S transmitter with the DJI Mobile Receiver, for which this behaviour has been confirmed by third parties on Linux; any receiver that behaves the same way works.

The receiver must sit within radio range of the bed. The VoiceNotes host may be out of radio range of the bed, so two deployment modes are needed:

- **Direct**: the receiver is plugged into the VoiceNotes host, and the recorder runs on that host next to the service.
- **Proxy**: the receiver is plugged into a small Linux box near the bed (for example a Raspberry Pi, "the box" below), and the recorder runs there.

In both modes recording is done by the same **recorder**: a separate program from the same release (`voicenotes-mic`), running natively on whichever host has the receiver. It records, keeps each recording safe locally, and forwards it to VoiceNotes over the network (to the same host in direct mode). The modes differ only in where the recorder runs. Both are optional; with no recorder and no upload endpoint configured, VoiceNotes behaves exactly as today. Feature 001 (FR-007) already requires the pipeline to accept captures from sources other than Telegram; this feature adds the first such source.

## Clarifications

### Session 2026-11-20

- Q: What should happen to very short or speechless recordings (an accidental tap, or a late "stop" tap after the length limit)? → A: Recordings under about 2 seconds are discarded without creating a capture. A recording in which speech-to-text finds no speech creates no note. This "no speech, no note" rule is general: it applies to every audio capture, including Telegram voice messages, and supersedes the 001 edge case "Silence or unintelligible audio" (which created a "(no speech recognized)" note). A Telegram message stays in the chat, so nothing is lost there; a microphone recording has no other copy, so its audio is held on the VoiceNotes host for a configurable period (default 7 days) before deletion, recoverable by hand.
- Q: Should the owner get a signal when a recording starts or stops? → A: Yes. A silent "🎙 recording…" Telegram message is sent when a recording starts and is later edited into the outcome (confirmation, "no speech heard", or failure), exactly like the ⏳ reply of a Telegram capture.
- Q: If the bedside recorder cannot record (receiver unplugged, box off, storage full), how does the owner find out? → A: The proxy recorder checks in with VoiceNotes regularly, reporting its state. VoiceNotes sends one silent Telegram alert when the receiver is missing, the box's storage is full, or the box has not checked in for a configurable time (default 5 minutes), and one when it recovers. This applies to every recorder, including one running on the VoiceNotes host itself.
- Q: How is recording deployed? → A: As a separate recorder program (`voicenotes-mic`) from the same repository and release, running natively (as a system service) on whichever host has the receiver, and always forwarding through the upload endpoint. The VoiceNotes service itself never touches audio devices.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Tap, talk, tap: a note from the bedside (Priority: P1)

The owner is in bed with the transmitter clipped on or on the nightstand. They tap its button; a silent Telegram message "🎙 recording…" appears. They say "remember to call the plumber about the boiler on Monday", and tap again. Shortly after, that message turns into "✓ saved · open". In the morning the note is in the notes app, tagged `#inbox`, with the recording attached and the action item listed, and it appears in the digest.

**Why this priority**: This is the whole feature. In direct mode it is a complete, usable capture path on its own.

**Independent Test**: In direct mode, with the receiver plugged into the VoiceNotes host and the transmitter in range, make several recordings by tapping and check each yields one note and one confirmation.

**Acceptance Scenarios**:

1. **Given** direct mode is configured and the receiver is connected, **When** the owner taps, speaks and taps again, **Then** exactly one capture is created from what was said between the taps, and it goes through the same steps as a Telegram voice message (speech-to-text, optional LLM step, note with the audio attached and `#inbox`).
2. **Given** the owner taps to start, **When** the recording starts, **Then** a silent "🎙 recording…" message is sent to the configured notification chat; **and when** the note has been saved, that message is edited into the confirmation with a link to the note and the number of action items when there are any.
3. **Given** a recording is in progress, **When** the owner keeps talking past the maximum recording length (default 10 minutes), **Then** the recording stops by itself at the limit and is processed like a tapped stop.
4. **Given** the owner taps the button, **When** the tap is received, **Then** the volume of the computer the receiver is plugged into does not change.
5. **Given** the note is saved, **When** the owner reads the digest, **Then** the recording's note is listed like any other `#inbox` note, ordered by the time the recording started.
6. **Given** the owner taps twice by accident (a recording under about 2 seconds), **When** the recording stops, **Then** no capture is created and the "🎙 recording…" message is deleted.
7. **Given** a recording in which speech-to-text finds no speech, **When** it is processed, **Then** no note is created, the "🎙 recording…" message is edited to say no speech was heard, and the audio is kept on the VoiceNotes host for the configured hold period (default 7 days) before it is deleted.

---

### User Story 2 - Record near the bed, process on the server (Priority: P1)

The VoiceNotes host is out of reliable radio range of the bed. The owner plugs the receiver into a small Linux box near the bed and runs the VoiceNotes recorder on it. Tapping, talking and tapping works exactly as in story 1; the box forwards each recording to VoiceNotes.

**Why this priority**: Whenever the service host is out of radio range this is the mode needed; it is only lower in build order because it reuses story 1's recording behaviour.

**Independent Test**: Run the recorder on a second machine with the receiver attached, pointed at a VoiceNotes instance with uploads enabled. Make recordings, including while the network or VoiceNotes is down, and check each yields exactly one note.

**Acceptance Scenarios**:

1. **Given** proxy mode is configured on the box and uploads are enabled on VoiceNotes, **When** the owner makes a recording, **Then** the box keeps it durably, forwards it, and VoiceNotes creates exactly one note from it.
2. **Given** VoiceNotes or the network is unreachable, **When** the owner makes recordings, **Then** the box keeps them and forwards them, oldest first, once VoiceNotes is reachable again; no recording is lost.
3. **Given** VoiceNotes received a recording but its confirmation never reached the box, **When** the box sends the same recording again, **Then** VoiceNotes confirms it without creating a second capture or note.
4. **Given** VoiceNotes has confirmed it stored a recording, **When** the box receives that confirmation, **Then** the box deletes its local copy; it MUST NOT delete it before.
5. **Given** a request to the upload endpoint without the correct credential, **When** it arrives, **Then** it is rejected and nothing is stored.
6. **Given** VoiceNotes is reachable when a recording starts, **When** the owner taps to start, **Then** the box tells VoiceNotes, which sends the "🎙 recording…" message; **given** it is not reachable, **then** recording still starts at once, no start message is sent, and the outcome later arrives as a new message.

---

### User Story 4 - No empty notes from silence, from any source (Priority: P2)

The owner sends a Telegram voice message by mistake with only rustling on it, or makes a microphone recording in which nothing was said. No empty note clutters the inbox or the digest; the reply says nothing was heard.

**Why this priority**: The owner chose this to keep the inbox clean; it is a change to existing Telegram behaviour (001), so it is specified once for every audio source.

**Independent Test**: Send a Telegram voice message of silence and make a silent microphone recording; check neither creates a note, both replies say no speech was heard, the Telegram message is untouched in the chat, and the microphone audio is still on the host within the hold period.

**Acceptance Scenarios**:

1. **Given** a Telegram voice or audio capture in which speech-to-text finds no speech, **When** it is processed, **Then** no note is created, its ⏳ reply is edited to say no speech was heard and nothing was saved, and its local audio is deleted (the original stays in the Telegram chat).
2. **Given** a microphone recording in which speech-to-text finds no speech, **When** it is processed, **Then** no note is created and its audio is held for the configured period before deletion.
3. **Given** speech-to-text fails (rather than returning no speech), **When** the failure is permanent, **Then** the existing placeholder note with the audio attached is still created (001 FR-010); only a successful empty transcript counts as "no speech".

---

### User Story 3 - Nothing is lost when something breaks mid-recording (Priority: P2)

The power flickers while the owner is mid-sentence, or the receiver is knocked out of its port. When things come back, what was recorded up to that moment is still turned into a note.

**Why this priority**: The constitution requires that no capture is lost (Principle VI); this story makes that concrete for a recording that is still in progress.

**Independent Test**: Start a recording, then kill the recorder process (or unplug the receiver), restart it, and check a note is created from the audio recorded before the interruption.

**Acceptance Scenarios**:

1. **Given** a recording is in progress, **When** the recorder process stops unexpectedly and is started again, **Then** the audio recorded up to the interruption (except at most the last few seconds) becomes a capture and a note.
2. **Given** a recording is in progress, **When** the receiver disconnects, **Then** the recording ends there and what was recorded becomes a capture and a note.
3. **Given** the receiver is not connected, **When** the recorder runs, **Then** it keeps running, waits for the receiver to appear, and reports the missing receiver through its health check.

---

### Edge Cases

- **Missed stop tap**: the recording runs until the maximum length and stops by itself. The owner's late "stop" tap then starts a new recording, which ends at the next tap or at the limit; if nothing is said in it, it creates no note.
- **Accidental tap**: a recording under about 2 seconds (configurable) creates no capture, and its "🎙 recording…" message is deleted.
- **A whisper speech-to-text misses**: it is treated as no speech and creates no note. For a microphone recording, the audio is held on the VoiceNotes host for the hold period so it can be recovered by hand; for Telegram, the original stays in the chat.
- **A start message whose recording never arrives** (the box lost the recording, or was replaced): after one day the message is edited to say the recording was not received.
- **Telegram unavailable when a recording starts**: recording starts at once regardless; the start message is skipped, and the outcome is sent as a new message once Telegram is reachable.
- **Two taps in quick succession** (a bounce or an accidental double tap): taps arriving within a short debounce window (configurable, default half a second) after a start or stop are ignored, so a double tap does not start and immediately stop a recording.
- **Two transmitters paired to one receiver**: both send the same key and their audio is mixed by the receiver; VoiceNotes cannot tell them apart and treats them as one microphone.
- **Long press on the transmitter**: it is the kit's pairing gesture and is not expected to reach the computer as a key; if it does, it is treated as a tap.
- **Speech-to-text, LLM, notes store or Telegram unavailable**: the capture waits and retries exactly like a Telegram voice capture, and the "🎙 recording…" message is edited to say it is queued, as the ⏳ reply is today; health reports the degradation.
- **Telegram unavailable when the note is saved**: the note stays saved; the edit is retried like any capture reply and never affects the note.
- **Permanent failure** (for example the audio cannot be transcribed): the note is created with the recording attached and a placeholder naming the failure, and the confirmation says so, as for voice captures.
- **The box's local storage fills up** while VoiceNotes is unreachable: the box refuses to start new recordings rather than overwrite queued ones; its health check reports it, and VoiceNotes alerts the owner once its check-in reports it (FR-227a).
- **The box is off, crashed or off the network**: its check-ins stop; after the configured silence (default 5 minutes) VoiceNotes sends one alert, and one more when check-ins resume. A box that is merely offline from VoiceNotes keeps recording and queuing meanwhile.
- **The receiver is unplugged and plugged back in**: one alert when it goes missing, one when it is back; brief flaps shorter than the check-in interval may produce no alert.
- **The box's clock is wrong**: the note's time comes from the box's recording start time; the box is expected to keep its clock synchronised (assumption below).
- **A recording uploaded with a size or format VoiceNotes does not accept**: it is rejected with a clear reason and the box keeps it and reports it through its health check; it is not deleted.
- **Both modes at once**: VoiceNotes may record from its own receiver and accept uploads from a box at the same time; each recording is its own capture.
- **The tap reaches some other program on the computer**: the recorder holds the key input exclusively while it runs, so the tap does nothing else on that computer.

## Requirements *(mandatory)*

### Functional Requirements

**Recording**

- **FR-201**: The system MUST start a recording on a tap and stop it on the next tap, where a tap is a "volume up" key press from the configured receiver's key input. It MUST work with any receiver that sends that key and MUST NOT be limited to one vendor or model.
- **FR-202**: While it runs, the recorder MUST hold the receiver's key input exclusively, so taps do not change the host's volume or reach other programs.
- **FR-203**: Taps arriving within a configurable debounce window (default 0.5 s) after a start or stop MUST be ignored.
- **FR-204**: A recording MUST stop by itself after a configurable maximum length (default 10 minutes) and be processed like a tapped stop.
- **FR-205**: Recorded audio MUST be written to durable local storage progressively while recording, so that an interruption loses at most the last few seconds. On start, the recorder MUST finish any recording left unfinished by an interruption and process it.
- **FR-206**: When the receiver disconnects during a recording, the recording MUST end at that point and be processed. When no receiver is connected, the recorder MUST keep running and pick the receiver up when it appears.
- **FR-207**: Every recording MUST have a stable identity assigned when it starts, and MUST carry its start time and the reason it ended (tap, length limit, receiver lost, interruption).
- **FR-208**: Recordings MUST be stored at a speech-quality resolution such that a recording of the maximum length stays within the upload limits of common speech-to-text services (25 MB); the configurable maximum length MUST NOT exceed the length that fits that limit.
- **FR-209**: A recording shorter than a configurable minimum (default 2 s) MUST NOT become a capture; its local audio is deleted at once.
- **FR-210**: When a recording starts, the system MUST send a silent "🎙 recording…" message to the notification chat, without delaying the start of recording. If the message cannot be sent at once, recording MUST go ahead and the start message is skipped. The message is the recording's reply: it is edited like the ⏳ reply of a Telegram capture (queued, saved, failed, no speech), or deleted for a recording under the minimum length. A recording without a start message gets its outcome as a new silent message.

**No speech (all audio captures)**

- **FR-210a**: When speech-to-text succeeds and returns no speech for an audio capture from any source, the system MUST NOT create a note. The capture's reply MUST be edited to say no speech was heard and nothing was saved. This supersedes the 001 edge case "Silence or unintelligible audio". A speech-to-text failure is not "no speech": permanent failures still produce a placeholder note (001 FR-010).
- **FR-210b**: For a Telegram capture with no speech, the local audio MUST be deleted as after a saved note. For a microphone capture with no speech, the audio MUST be kept on the VoiceNotes host for a configurable hold period (default 7 days) and then deleted; the documentation MUST say where it is kept. The capture's source identity is kept for deduplication as for any finished capture (001 FR-012).

**Recorder deployment**

- **FR-211**: Direct mode MUST be the recorder running on the VoiceNotes host and forwarding to the service through the same endpoint as proxy mode. The VoiceNotes service itself MUST NOT access audio or input devices.

**Recorder storage and forwarding**

- **FR-212**: The recorder MUST be a separate program shipped with each release for Linux on 64-bit x86 and 64-bit ARM, runnable natively as a system service, with the same behaviour in both modes.
- **FR-213**: The recorder MUST keep every finished recording durably until VoiceNotes confirms it has stored it, and only then delete it.
- **FR-214**: The recorder MUST forward recordings oldest first and retry failed forwards automatically, with increasing delays up to a maximum interval, without limit. A forward rejected for good (refused size or format) MUST be kept and reported through the recorder's health check, not deleted.
- **FR-215**: When its local storage is close to full, the recorder MUST refuse new recordings rather than discard queued ones, and report it through its health check.

**Upload endpoint**

- **FR-216**: VoiceNotes MUST offer an endpoint for recordings forwarded by a recorder, enabled only when an upload credential is configured. Requests without the correct credential MUST be rejected without storing anything.
- **FR-217**: The endpoint MUST confirm a recording only after it has been persisted durably as a capture. A recording already received (same identity) MUST be confirmed again without creating a second capture.
- **FR-218**: The endpoint MUST reject recordings above a configured size limit and recordings whose format is not accepted, with a reason the recorder can report.
- **FR-219**: The endpoint MUST be served on the service's existing network port and MUST NOT require public internet access; it is meant for the home network.

**Notes and confirmation**

- **FR-220**: A recording capture MUST go through the same steps as a Telegram voice capture (001 FR-008 onward): speech-to-text with the configured languages, the optional LLM step, a note with the audio attached tagged `#inbox`, retries and placeholder notes on failure.
- **FR-221**: The note's creation time MUST be the recording's start time (001 FR-012a).
- **FR-222**: Once the note is saved, the recording's reply (FR-210) MUST be edited into the same confirmation a Telegram capture gets: a link, the action-item count when there are any, and the failure wording when the note was created with a placeholder. The notification chat is configurable and defaults to the digest chat.
- **FR-222a**: In proxy mode, the box MUST tell VoiceNotes when a recording starts so that VoiceNotes sends the start message and links it to the recording's identity; the later upload of that recording uses the same reply. This notice is best-effort: if it fails, recording is unaffected and FR-210's no-start-message path applies.
- **FR-223**: Deduplication for recording captures MUST use the recording's identity, so a recording yields at most one note across restarts and repeated uploads; the identity is purged after 30 days like other source identities (001 FR-012).
- **FR-224**: Local audio MUST be deleted only after the note carrying it has been saved (Principle VI), on the VoiceNotes host and, on the recorder's host, only after VoiceNotes's confirmation.

**Configuration, health and independence**

- **FR-225**: The upload endpoint and the recorder MUST each be optional. With no endpoint configured, behaviour MUST be unchanged.
- **FR-226**: All new settings (receiver selection, debounce, minimum and maximum length, no-speech hold period, notification chat, check-in interval and silence threshold, upload credential and size limit; for the proxy: VoiceNotes address, credential and local storage location) MUST be validated at startup, naming the offending setting and never printing a secret. Credentials MUST come only from the environment.
- **FR-227**: VoiceNotes's health check MUST report any recorder with an outstanding alert (FR-227a) as degraded. The recorder MUST offer its own health check reporting a missing receiver, full storage, rejected recordings, and recordings waiting to be forwarded longer than a configured threshold.
- **FR-227a**: Every recorder MUST check in with VoiceNotes at a regular interval (default 1 minute), reporting whether the receiver is present and whether storage is full; the check-in uses the upload credential. VoiceNotes MUST send one silent Telegram alert to the notification chat when a recorder's receiver goes missing, its storage is full, or it has not checked in for a configurable time (default 5 minutes), and one silent message when that condition clears. Alerts MUST NOT repeat while a condition persists, and a condition that clears before it is reported MUST NOT alert. A recorder silent since VoiceNotes started is alerted on only once it has checked in at least once.
- **FR-228**: Documentation MUST describe both modes with anonymised examples only, including how to check that a receiver behaves as required (it appears as a sound input and a tap produces a "volume up" key) and how to give the service access to the receiver.

### Key Entities

- **Recording**: one tap-to-tap (or tap-to-limit) take. Has a stable identity, start time, end time, end reason, the recorded audio, and a state: recording, finished, then forwarded and confirmed (or discarded).
- **Capture** (extended from 001/002): gains the microphone as a source; its source identity is the recording's identity and its receive time is the recording's start time.
- **Upload**: one forward of a recording from a recorder to VoiceNotes; carries the recording identity, start time and audio; answered with a confirmation or a rejection reason.
- **Recorder status**: VoiceNotes's view of each recorder (on the VoiceNotes host in direct mode, the box in proxy mode): last check-in time, receiver present, storage full, and which alert, if any, is currently outstanding.
- **Configuration** (extended): receiver selection, debounce, maximum length, notification chat, upload credential and size limit; for the recorder, the VoiceNotes address, credential, storage location and waiting threshold.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-201**: In a test run of 20 tap-talk-tap recordings in each mode, every recording yields exactly one note with the audio attached, and with all dependencies up the confirmation arrives within 60 seconds of the stop tap.
- **SC-202**: Zero recordings are lost across a test run in which VoiceNotes is restarted, the network between the box and VoiceNotes is cut for an hour, and the recorder is killed mid-recording; an interrupted recording loses at most its last 5 seconds.
- **SC-203**: Retrying the same upload any number of times yields exactly one note in 100% of test runs.
- **SC-204**: Taps on the transmitter never change the volume or affect any other program on the computer the receiver is plugged into.
- **SC-205**: A recording of the maximum length (10 minutes) is transcribed and saved as a note without being rejected for size.
- **SC-206**: Each recording produces at most one message in the chat (the start message, edited into its outcome); accidental taps under the minimum length leave no message behind.
- **SC-208**: In a test run of 10 silent recordings and 10 silent Telegram voice messages, no note is created, every reply says no speech was heard, and every silent microphone recording's audio is still recoverable on the host for the hold period.
- **SC-209**: Unplugging the receiver, filling the box's storage, or powering the box off each produces exactly one alert within 6 minutes, and restoring it produces exactly one recovery message.
- **SC-207**: With none of the new settings configured, every existing acceptance test of features 001 and 002 passes unchanged, except the 001 silence case, which now follows FR-210a.

## Assumptions

- The target kit is the DJI Mic Mini 2S transmitter with the DJI Mobile Receiver. Third-party Linux tests report that the receiver appears as a USB sound input plus a "Consumer Control" key input, and that a single transmitter tap produces one "volume up" press and release about 1 ms apart, so holding the button cannot be detected. These facts are verified on the real kit (sound input listing, key events, a test recording) before the receiver-reading adapters are finished; captured output from that check becomes the test fixtures. Until the kit arrives, recording logic, the upload protocol and pipeline integration are built against fakes.
- The recorder's host runs 64-bit Linux and exposes the receiver as standard sound and input devices. The recorder runs natively as a system service whose user may use them (audio and input groups); its audio library needs the system's ALSA runtime library. Setting this up is the deployment's job, documented with anonymised examples.
- The box runs 64-bit Linux (the reference target is Ubuntu 26.04 on a Raspberry Pi) and reaches the service over the local network by Wi-Fi or Ethernet. It keeps its clock synchronised over the network, and the recorder runs on it as the release's 64-bit ARM build.
- The local network is trusted to the extent that a shared upload credential suffices; transport encryption, if wanted, is provided by the deployment (for example a reverse proxy), not required by VoiceNotes.
- The transmitter's own onboard backup recording is not used; retrieving it is out of scope.
- Out of scope: Bluetooth connections, forwarding the USB device over the network (rejected: audio over that path is unreliable and drops audio on network hiccups, which would violate Principle VI), sending recordings through a Telegram user account (rejected: bots cannot receive messages from bots, and it is fragile), several receivers on one host, telling transmitters apart, and live streaming of audio while recording.
- Audio capture uses one newly approved dependency (a mature cross-platform audio library, which requires cgo), confined to the recorder program by constitution v2.0.0 Principle IV. Reading the button uses the standard library. The service gains no dependency.
