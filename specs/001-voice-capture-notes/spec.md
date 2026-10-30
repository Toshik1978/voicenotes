# Feature Specification: VoiceNotes — voice capture to notes with a morning digest

**Feature Branch**: `001-voice-capture-notes`

**Created**: 2026-10-29

**Status**: Draft

**Input**: User description: "VoiceNotes — a self-hosted, single-owner service that turns quick voice and text messages into notes, with an optional LLM step and a morning digest of unreviewed notes." (full brief in the brainstorming record; summarized below)

## Context

The owner captures fleeting thoughts by voice at any time, mostly at home in bed at night and sometimes outdoors. Clips are mostly about 10 seconds long, occasionally 1–2 minutes. The next morning the owner reviews what was captured, elaborates some notes and turns some into tasks, but tends to forget everything by morning, so a reliable reminder is essential.

The product is named **VoiceNotes** in everything user-facing. The repository, binary and image are lowercase `voicenotes`. It is public and generic: nothing deployment-specific lives in it, and every environment detail is configuration. Everything is self-hosted and open source. The only paid component is the optional LLM, accessed through the owner's own gateway.

## Clarifications

### Session 2026-10-30

- Q: If more than one Telegram account is on the allowlist, where should the morning digest go? → A: One configured digest chat ID, defaulting to the first allowlisted user.
- Q: After a capture becomes a note, how long should VoiceNotes keep its own copy of the transcript and LLM output? → A: Delete text and audio right after the note is saved. Keep only the source message IDs for duplicate detection, purged after 30 days.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Capture a voice thought as a note (Priority: P1)

At night the owner sends a short voice message to their private VoiceNotes bot. A moment later the bot's reply quietly turns into "✓ saved · open". In the notes app there is a new note containing the transcript, with the original audio attached, tagged `#inbox`.

**Why this priority**: This is the core value. Without it nothing else matters, and it alone replaces "I'll remember it tomorrow".

**Independent Test**: Run VoiceNotes with speech-to-text and the notes store configured and the LLM disabled. Send a voice message from an allowed account, then check the note and the bot reply.

**Acceptance Scenarios**:

1. **Given** the service is running and the owner is on the allowlist, **When** the owner sends a voice message, **Then** the bot immediately replies with a pending marker (⏳), and when processing finishes that same reply is edited into a short confirmation with a link to the note.
2. **Given** a voice message has been processed with the LLM disabled, **When** the owner opens the note, **Then** it holds the transcript in the spoken language, has the audio attached and carries the `#inbox` tag.
3. **Given** the owner forwards someone else's voice message to the bot, **When** it is processed, **Then** it becomes a note exactly like the owner's own voice message.
4. **Given** the owner sends an audio file (not a voice message), **When** it is processed, **Then** it becomes a note like a voice message.
5. **Given** the owner sends a plain text message, **When** it is processed, **Then** it becomes a note with that text (no transcription), tagged `#inbox`.
6. **Given** someone who is not on the allowlist messages the bot, **When** the message arrives, **Then** nothing is stored, no note is created and the bot does not reply.
7. **Given** the owner sends a photo, sticker or document, **When** it arrives, **Then** the bot replies with one line saying it accepts voice or text only, and no note is created.
8. **Given** any reply or edit from the bot about a capture, **When** it is delivered, **Then** it is sent without a notification sound.

---

### User Story 2 - Never lose a capture when something is down (Priority: P1)

The owner records a thought while the speech-to-text server, the LLM gateway or the notes store is restarting or unreachable. The bot's reply says the capture is queued. When the dependency comes back, the note is created and the same reply is edited into the confirmation, without the owner doing anything.

**Why this priority**: The owner explicitly cannot afford lost thoughts. A note-capture tool that silently drops input is worse than none.

**Independent Test**: Stop the speech-to-text server (or the notes store), send a voice message, restart VoiceNotes itself while it waits, then bring the dependency back. Verify exactly one note appears and the reply is updated.

**Acceptance Scenarios**:

1. **Given** speech-to-text is unreachable, **When** the owner sends a voice message, **Then** the bot reply states the capture is queued, and once speech-to-text is reachable again the note is created and the reply is edited into the confirmation.
2. **Given** the notes store is unreachable, **When** a capture has already been transcribed, **Then** transcription is not repeated once the notes store comes back. Only the remaining steps run.
3. **Given** a capture is waiting on a dependency, **When** VoiceNotes itself is restarted, **Then** the capture resumes after restart and is not lost or duplicated.
4. **Given** the messaging platform delivers the same message more than once, **When** VoiceNotes receives the duplicate, **Then** only one note is created.
5. **Given** the LLM step is enabled but the LLM is unreachable or returns unusable output, **When** a capture reaches that step, **Then** the note is saved with the raw transcript without waiting for the LLM, and the confirmation marks that the LLM step was skipped.
6. **Given** a dependency rejects a capture permanently (for example, the audio cannot be decoded), **When** retrying cannot help, **Then** the capture is still saved as a note with the audio attached and a placeholder text explaining what failed, and the reply says so.

---

### User Story 3 - Morning reminder of unreviewed notes (Priority: P1)

Every morning at the configured time, the owner's phone buzzes once with a single message from the bot. It lists every note still tagged `#inbox`, one short line each with a link, and marks notes that contain tasks. Notes the owner did not review yesterday appear again until the owner removes `#inbox` from them.

**Why this priority**: The owner forgets night-time captures by morning. The owner called the digest a must-have and the reminder is what makes the captures useful.

**Independent Test**: Create several notes, remove `#inbox` from one of them in the notes app, set the digest time to a minute from now, and check that the message lists exactly the remaining `#inbox` notes.

**Acceptance Scenarios**:

1. **Given** the digest is enabled and there are notes tagged `#inbox`, **When** the configured time arrives in the configured timezone, **Then** one message is sent listing every `#inbox` note with a short summary and a link.
2. **Given** some `#inbox` notes were already listed in a previous digest, **When** the next digest is sent, **Then** the notes created since the previous digest are listed first, and the older ones follow under a separate "from earlier (N)" heading.
3. **Given** the owner removed `#inbox` from a note, **When** the next digest is sent, **Then** that note is not listed.
4. **Given** no notes are tagged `#inbox`, **When** the digest time arrives, **Then** no message is sent.
5. **Given** a note contains action items, **When** it appears in the digest, **Then** its line carries a task marker.
6. **Given** the digest is the only scheduled message, **When** it is sent, **Then** it is sent with a notification sound, unlike all capture replies.
7. **Given** the digest is disabled in configuration, **When** the time arrives, **Then** nothing is sent.

---

### User Story 4 - Translation, tidy-up and task spotting (Priority: P2)

With the LLM step enabled, the owner says in Russian, "need to call the dentist tomorrow, and that idea about the garden shed." The note arrives in English, the configured target language, with clean punctuation. It has a checklist item "Call the dentist", the original Russian transcript quoted below, and the tags `#inbox #todo #garden`.

**Why this priority**: This makes morning review faster and turns captures into actionable items, but the core capture works without it.

**Independent Test**: Enable the LLM step with a target language and send a clip in a different language that includes an action item. Check the note's text, checklist, original quote and tags.

**Acceptance Scenarios**:

1. **Given** translation is enabled and the clip's language differs from the target language, **When** the note is created, **Then** its main text is in the target language and the original transcript appears below it, labeled with its language.
2. **Given** the clip is already in the target language, **When** the note is created, **Then** there is no translation and no original quote.
3. **Given** task spotting is enabled and the clip contains one or more action items, **When** the note is created, **Then** the note contains a checklist with one unchecked item per action item, is tagged `#todo` and is still one single note.
4. **Given** task spotting finds no action items, **When** the note is created, **Then** there is no checklist and no `#todo` tag.
5. **Given** tidy-up and tag suggestion is enabled, **When** the note is created, **Then** punctuation is cleaned and suggested tags are added next to `#inbox`.
6. **Given** any single LLM function is disabled in configuration, **When** a capture is processed, **Then** the other enabled functions still apply and the disabled one does not.
7. **Given** a plain text message, **When** the LLM step is enabled, **Then** the same functions apply to it as to a transcript.

---

### User Story 5 - Configurable languages (Priority: P2)

The owner speaks two languages, but another self-hoster might speak a different pair or just one. The owner configures a list of source languages they speak (possibly empty) and one target language. Recognition becomes more accurate when the language is known, and a clip is never transcribed as a language the owner does not speak because of a misdetection.

**Why this priority**: Accuracy for short, quiet night clips depends on it, and the owner explicitly wants no fixed language pair.

**Independent Test**: Configure one source language and verify it is used. Configure two and send a short clip that tends to be misdetected, then verify it is transcribed in a listed language.

**Acceptance Scenarios**:

1. **Given** exactly one source language is configured, **When** a clip is transcribed, **Then** recognition is told to use that language.
2. **Given** no source languages are configured, **When** a clip is transcribed, **Then** recognition detects the language on its own and the result is accepted as is.
3. **Given** several source languages are configured, **When** recognition detects a language that is not in the list, **Then** the clip is transcribed again using the first listed language, and that result is used.
4. **Given** the recognition service does not report which language it detected, **When** a clip is transcribed, **Then** the check in scenario 3 is skipped and the note is still created.

---

### User Story 6 - Another self-hoster runs it from configuration only (Priority: P3)

A different person deploys VoiceNotes with their own bot, their own speech-to-text server or a hosted one, their own notes store and optionally their own LLM. They do it by setting configuration alone, without changing any code.

**Why this priority**: The project is public and generic, and it is the reason nothing deployment-specific may be hard-coded.

**Independent Test**: Run the service against a fresh notes store with a different bot and different endpoint addresses, all supplied through configuration.

**Acceptance Scenarios**:

1. **Given** a required setting is missing or invalid, **When** the service starts, **Then** it refuses to start and names the problem setting, without printing any secret value.
2. **Given** speech-to-text is switched from a local server to a hosted provider that speaks the same API, **When** only the address, key and model settings are changed, **Then** captures work without any other change.
3. **Given** the LLM provider behind the gateway changes, **When** only the model name setting changes (or nothing changes, if the gateway alias is repointed), **Then** captures work without any other change.
4. **Given** the service is running, **When** a monitor queries its health check, **Then** it reports whether the service is healthy and whether captures are piling up.

### Edge Cases

- **Silence or unintelligible audio**: the note is still created with the audio attached and placeholder text such as "(no speech recognized)", so a quiet whisper is never thrown away.
- **Audio larger than the messaging platform lets a bot download**: the bot replies that the clip is too large. No note is created, because the audio cannot be retrieved.
- **A very long transcript, possibly together with its translation, exceeds the notes store's per-note size limit**: the original quote is shortened first, then the main text, with a visible "(truncated)" marker. The full audio stays attached.
- **The owner edits or deletes a message after sending it**: VoiceNotes ignores the change and the note created from the original stands.
- **Several captures arrive in a burst while a dependency is down**: all are queued and processed in arrival order once it recovers.
- **The service is down at digest time**: on startup it sends the missed digest if it is still within a catch-up window after the scheduled time (default 3 hours), otherwise it skips that day.
- **The notes store is unreachable at digest time**: the digest retries within the catch-up window, then gives up for that day without failing anything else.
- **Many `#inbox` notes**: the digest lists up to a cap (default 30) and then says "…and N more", so the message stays readable.
- **The bot cannot edit the pending reply** (for example, it was deleted): VoiceNotes sends a new silent reply instead.
- **The clip mixes two languages**: recognition treats each clip as a single language, so the parts in the other language may be transcribed poorly. This is a known limitation; the LLM tidy-up may partially repair it.

## Requirements *(mandatory)*

### Functional Requirements

**Capture**

- **FR-001**: The system MUST receive messages from a dedicated Telegram bot by polling, and MUST NOT require any public inbound network access.
- **FR-002**: The system MUST accept messages only from a configured allowlist of Telegram user IDs and silently ignore all others.
- **FR-003**: The system MUST accept voice messages, audio files, forwarded voice and audio messages, and plain text messages as captures.
- **FR-004**: The system MUST answer any other message type from an allowed user with a one-line "voice or text only" reply and MUST NOT create a note.
- **FR-005**: The system MUST persist each capture, including its audio, durably and locally before acknowledging it, so that a later failure or restart cannot lose it.
- **FR-006**: The system MUST deduplicate captures so that a message delivered more than once yields at most one note.
- **FR-007**: The capture pipeline MUST accept captures from sources other than Telegram, so that a later HTTP upload route can feed it without changing the downstream steps. That route is out of scope for this feature.

**Processing and reliability**

- **FR-008**: The system MUST process each capture through distinct steps: retrieve audio → speech-to-text (audio only) → optional LLM → write note → update reply. The result of each completed step MUST be recorded so that a step is never repeated after a later failure or restart.
- **FR-009**: The system MUST retry temporary failures of any external dependency automatically, with increasing delays up to a maximum interval, and MUST keep retrying until the step succeeds or fails permanently.
- **FR-010**: When a step fails permanently, the system MUST still create the note with whatever it has: at minimum the audio attached and a placeholder text naming the failure.
- **FR-011**: Captures MUST be processed in arrival order.
- **FR-012**: Once a capture's note has been saved, the system MUST delete the capture's local audio, transcript and LLM output. Only the source message identity needed for deduplication is kept, and it MUST be purged after 30 days.
- **FR-012a**: A note MUST carry the capture's original receive time as its creation time, so that captures delayed by an outage appear in the order they were recorded.

**Speech-to-text**

- **FR-013**: The system MUST transcribe audio through an endpoint that implements the OpenAI-compatible audio transcription API, with address, credentials and model set in configuration.
- **FR-014**: The system MUST support a configured list of zero or more source languages and exactly one target language:
  - With exactly one source language, it is passed to recognition.
  - With none, recognition auto-detects.
  - With several, a detected language outside the list triggers one re-transcription using the first listed language.
  - If the endpoint does not report the detected language, that check is skipped.
- **FR-015**: The system MUST NOT fall back automatically to another speech-to-text endpoint. Switching endpoints is a configuration change.

**LLM post-processing (optional)**

- **FR-016**: The LLM step MUST be optional as a whole. Translation, tidy-up with tag suggestion, task spotting and digest summaries MUST each be individually switchable.
- **FR-017**: The system MUST use an endpoint that implements the OpenAI-compatible chat completions API, with address, credentials and model name set in configuration. It MUST NOT depend on provider-specific features.
- **FR-018**: The system MUST validate the LLM's output. If the output is malformed, or the LLM is unavailable after a short bounded retry, the note MUST be saved with the raw transcript and the reply MUST indicate the LLM step was skipped.
- **FR-019**: When translation is on and the capture's language differs from the target language, the note MUST contain the translation as its main text and the original transcript below it, labeled with its language.
- **FR-020**: When task spotting is on, every detected action item MUST appear as an unchecked checklist item within the same note, and the note MUST be tagged `#todo`.

**Notes**

- **FR-021**: Each capture MUST produce exactly one note, written through a notes-writer abstraction. Memos is the only implementation for now.
- **FR-022**: Each note MUST contain, in order:
  - the main text
  - the action-item checklist, if any
  - the original transcript quote, if translated
  - the tags: always `#inbox`, `#todo` when action items exist, plus any suggested tags

  Audio captures MUST have their audio attached.
- **FR-023**: Note links shown to the owner MUST use a configured public base address, which is separate from the address the service uses internally to reach the notes store.
- **FR-024**: The system MUST respect the notes store's per-note size limit by shortening text as described in Edge Cases. It MUST NOT fail the note because of size.

**Replies**

- **FR-025**: The system MUST reply to each capture right away with a pending marker. It MUST then edit that same reply into a short confirmation with a link, and include the number of action items when there are any. When processing is delayed by a dependency, the reply MUST say so, and the system MUST edit it again later.
- **FR-026**: All capture replies and edits MUST be sent silently.

**Morning digest**

- **FR-027**: The system MUST send at most one digest per day, at a configured local time in a configured timezone, to a single configured digest chat. The digest chat defaults to the first allowlisted user. The digest MUST be disableable.
- **FR-028**: The digest MUST list every note currently tagged `#inbox` in the notes store. Notes created since the previous digest come first, then older ones under a "from earlier (N)" heading.
- **FR-029**: Each digest line MUST show a short summary, the task marker when the note contains a checklist, and a link. The summary is generated by the LLM when that function is on, and is otherwise the first line of the note.
- **FR-030**: If there are no `#inbox` notes, the system MUST NOT send a digest. The digest MUST be sent with a notification sound.
- **FR-031**: The system MUST catch up a missed digest within a configured window after the scheduled time, and MUST cap the number of listed notes, showing the remainder as a count.

**Operation**

- **FR-032**: All behavior MUST be driven by configuration. Secrets MUST be supplied without appearing in command lines or logs.
- **FR-033**: The system MUST validate its configuration at startup and refuse to start on missing or invalid required settings, naming the offending setting.
- **FR-034**: The system MUST expose a health check that a monitor can query. It MUST report unhealthy when the service cannot function, and MUST report a degraded state when captures have been waiting longer than a configured threshold.
- **FR-035**: User-facing text MUST name the product "VoiceNotes".

### Key Entities

- **Capture**: one incoming thought.
  - Source: Telegram for now; other routes later.
  - Source message identity, used for deduplication.
  - Kind: voice/audio or text.
  - Received time.
  - Local audio, transcript and LLM result, kept only until the note is saved. After that only the source message identity is retained, for 30 days, for deduplication.
  - Results of each completed step: transcript, detected language and LLM result, all deleted once the note is saved.
  - Current status: pending, waiting on a dependency, done or failed with a reason.
  - The pending reply it is linked to.
- **Note**: the item created in the notes store from a capture. It has text, an optional checklist, an optional original quote, tags and an optional audio attachment. It is identified by a link. VoiceNotes does not track its review state; the `#inbox` tag in the notes store is the source of truth.
- **Digest run**: one scheduled sending, with its date and send time. It is used to tell "new since last digest" from "from earlier", and to avoid sending twice in a day.
- **Configuration**:
  - allowlist
  - endpoints and credentials for speech-to-text, the LLM and the notes store
  - public base address
  - languages
  - LLM function switches
  - digest chat, time, timezone, window and cap
  - health thresholds

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: When all dependencies are up, a 10-second voice clip becomes a note and the reply shows the confirmation within 15 seconds of sending, without the LLM step, and within 30 seconds with it.
- **SC-002**: Zero captures are lost across a test run in which each dependency, and VoiceNotes itself, is restarted while captures are in flight. Every capture yields exactly one note.
- **SC-003**: After a dependency outage of up to 24 hours, all queued captures become notes within 10 minutes of the dependency's recovery, without any owner action.
- **SC-004**: On 30 consecutive days with at least one `#inbox` note, a digest arrives within 2 minutes of the configured time, and it lists exactly the notes tagged `#inbox` at that moment, subject to the cap.
- **SC-005**: Confirming a capture needs no more than one short line of reading, and no capture-related message makes a sound.
- **SC-006**: A new self-hoster can bring up a working instance using only the README and configuration, with no code changes.
- **SC-007**: With the LLM step enabled, in at least 9 of 10 test clips that contain a clear action item, that item appears as a checklist entry.

## Assumptions

- There is a single owner, with optionally several allowlisted Telegram accounts belonging to that owner. There are no user accounts or permissions inside VoiceNotes.
- The owner reviews notes in the notes store's own interface, and reviewing means removing the `#inbox` tag. VoiceNotes never edits a note after creating it.
- Telegram retains messages while VoiceNotes is offline and delivers them when it returns, so offline capture outdoors needs no special handling.
- Clips are rarely over 2 minutes, well under the bot download limit of the messaging platform.
- The notes store is reachable by the service on an internal address, while links for the owner use a separate public address that may sit behind an access gateway.
- The target deployment pins Memos v0.31.0 and will later move to 26.10. Both must work, and the only API difference that matters, the path prefix, is handled by configuration or detection.
- The digest summary falls back to the note's first line when the LLM is off or fails.
- Default values for the digest catch-up window (3 hours), the digest cap (30 notes) and the maximum retry interval are reasonable starting points and are configurable.
- Out of scope: multi-user accounts, a web UI, running speech recognition inside the service, pictures/OCR, hardware/firmware, editing notes from Telegram, a separate task manager, automatic speech-to-text fallback, webhook mode and the HTTP upload route. The last two are later additions the design must not preclude.
