# Feature Specification: Document and photo captures

**Feature Branch**: `002-document-captures`

**Created**: 2026-11-01

**Status**: Draft

**Input**: User description: "Document and photo captures — a 'deal with it tomorrow' inbox: the owner sends a photo or a downloaded file, and by morning it is a short note with the original attached and a hint of what it is." (full brief in the brainstorming record; summarized below)

## Context

Late at night the owner often has something they do not want to deal with now: a photo of a letter, a receipt or a whiteboard, or a file they have just downloaded. They want to send it to their VoiceNotes bot without sorting it or typing anything, go to sleep, and find it in the morning in the notes inbox and the morning digest.

The original file is the real content; the note exists so the item is not forgotten and can be recognised at a glance. For that the note carries a one-or-two-line **hint** of what the item is (for example "Electricity bill, €84, due Nov 10"), taken from the item's content, so that the owner does not have to open it to remember what it was.

This feature extends feature 001 (voice capture). It replaces the 001 behaviour of answering photos and documents with "voice or text only" (001 FR-004, acceptance scenario 1.7).

Two constraints from the owner shape it:

- **Full documents never leave the owner's infrastructure.** Reading documents is done by a locally run document-conversion service. Only a short excerpt of extracted text may reach the language model, and only a single photo may reach an outside vision model, and only if the owner turns that on.
- **Nothing becomes a hard dependency, and the repository defines no external requirements.** Reading content is optional. Any installation of the document-conversion service and any OpenAI-compatible model gateway works. With nothing configured, VoiceNotes behaves as today and additionally accepts files, saving them with a placeholder hint.

## Clarifications

### Session 2026-11-01

- Q: Should a slow document hold up a voice note sent after it, or may quick captures go ahead of a document still being read? → A: Quick captures go ahead. Content extraction runs in the background and other captures save their notes without waiting; order in the notes store and the digest comes from each note's original receive time (001 FR-012a), not from the order notes are saved.
- Q: When a document keeps timing out while being read, should VoiceNotes keep retrying forever, or give up after a few tries? → A: After 3 timeouts on the same file, save the note with the attachment and a "couldn't read in time" placeholder hint; an unreachable service still retries indefinitely.
- Q: If more than 10 photos are sent at once (Telegram splits them into several albums), one note or one per album? → A: One note. Photos and files (single items or albums) arriving from the same chat within the quiet period form one **batch** and merge into one note; voice and text messages always stay separate captures.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Save a photo or file as a note with its original attached (Priority: P1)

At night the owner sends the bot a photo or a file. The bot replies ⏳ and then quietly turns that reply into "✓ saved · open". In the notes app there is a new `#inbox` note that names what was sent and has the original attached. This works even when nothing that reads content is configured.

**Why this priority**: This is the minimum that replaces "I'll deal with it tomorrow": the item is safely in the inbox and in tomorrow's digest. Everything else improves the hint.

**Independent Test**: Run VoiceNotes with the notes store configured and no document-conversion service, no LLM. Send a photo and a PDF from an allowed account and check the notes and the replies.

**Acceptance Scenarios**:

1. **Given** no content reader is configured, **When** the owner sends a PDF, **Then** a note is created with the PDF attached, a placeholder hint naming the file (for example "📎 report.pdf"), and the `#inbox` tag, and the ⏳ reply is edited into the confirmation.
2. **Given** no content reader is configured, **When** the owner sends a photo (compressed, not as a file), **Then** a note is created with the photo attached and a placeholder hint saying it is a photo.
3. **Given** the owner sends a photo or file with a caption, **When** it is processed, **Then** the caption appears in the note as the owner's own text, processed by the LLM step exactly like a text message (translation, tidy-up, task spotting when those are on).
4. **Given** the owner forwards a photo or file from another chat, **When** it is processed, **Then** it becomes a note exactly like one the owner sent.
5. **Given** a file larger than the messaging platform lets a bot download, **When** it arrives, **Then** the bot replies that it is too large and no note is created, as for audio today.
6. **Given** the owner sends a sticker, location, contact, poll or other non-file message, **When** it arrives, **Then** the bot still replies with the one-line "unsupported" message, now naming voice, text, photos and files as accepted.
7. **Given** any reply or edit about a photo or file capture, **When** it is delivered, **Then** it is silent, as for every capture.

---

### User Story 2 - A hint of what the item is (Priority: P1)

The owner sends a photo of a bill and a downloaded PDF contract. In the morning the digest shows "Electricity bill, €84, due Nov 10" and "Lease contract for flat 3, signed by both parties", so the owner knows what each item is without opening it.

**Why this priority**: The owner explicitly wants this: without a hint, the morning inbox is a list of anonymous files and the owner no longer remembers what they were.

**Independent Test**: Run VoiceNotes with a document-conversion service and the LLM step configured. Send a photo of a printed bill, a text PDF, a scanned PDF and a Word document, and check each note's hint.

**Acceptance Scenarios**:

1. **Given** the document-conversion service and the LLM step are configured, **When** the owner sends a photo of printed text, **Then** the note carries a one-or-two-line hint describing the item, written in the configured target language.
2. **Given** the same setup, **When** the owner sends a PDF (with a text layer or scanned), an office document (word processor, spreadsheet, presentation), an HTML page, or a Markdown/CSV/plain-text file, **Then** the note carries a hint derived from its content.
3. **Given** the document-conversion service is configured but the LLM step is off, **When** a document is processed, **Then** the hint is the first lines of the extracted text, shortened to the hint length.
4. **Given** a long document, **When** its hint is generated, **Then** the language model receives only a bounded excerpt of the extracted text, never the whole document.
5. **Given** the document-conversion service returns no text for an item (a photo of a landscape, an empty page), **When** it is processed, **Then** the note is saved with the attachment and a placeholder hint saying no text was found.
6. **Given** the note is saved, **When** the owner opens it, **Then** the hint and the caption (if any) are in the note, the original file is attached, and the full extracted text is not pasted into the note.

---

### User Story 3 - One send, one note (Priority: P2)

The owner selects four photos of a multi-page letter and sends them together. The bot replies once with ⏳ and then one confirmation. In the notes app there is one note with all four photos attached and a single hint for the whole letter.

**Why this priority**: Sending several pages at once is common for paper documents, and four notes for one letter would clutter the inbox and the digest. It builds on stories 1 and 2.

**Independent Test**: Send an album of several photos, an album of several files, 15 photos at once (split by the messaging platform into two albums), with and without a caption, and check that each send yields exactly one note and one reply.

**Acceptance Scenarios**:

1. **Given** the owner sends several photos or files together as one album, **When** they arrive, **Then** exactly one note is created with every item attached, in the order sent, and exactly one ⏳ reply is sent and later edited.
2. **Given** an album where one item carries a caption, **When** it is processed, **Then** the caption is used as the note's own text, regardless of which item carried it.
3. **Given** an album, **When** its hint is generated, **Then** it is one hint for the album as a whole, based on the content of all its items.
4. **Given** an album that mixes photos and videos, **When** it is processed, **Then** the videos are attached without content extraction, and the hint is based on the photos (or is a placeholder if only videos were sent).
5. **Given** VoiceNotes restarts while an album is still arriving, **When** it comes back, **Then** no item already received is lost, and the album still yields one note.
6. **Given** an album where one item is too large to download, **When** it is processed, **Then** the other items are still saved in the one note, and the reply says which item could not be kept.
7. **Given** the owner sends more than 10 photos at once, so they arrive as several albums, **When** they are processed, **Then** they form one batch and yield exactly one note with every photo attached.
8. **Given** the owner sends two photos one by one, a couple of seconds apart, **When** they arrive within the quiet period, **Then** they form one batch and yield one note.
9. **Given** the owner sends a photo and then, within the quiet period, a voice or text message, **When** they are processed, **Then** the voice or text message is its own capture and note; only photos and files join a batch.
10. **Given** several items in one batch carry captions, **When** it is processed, **Then** the captions are joined in the order sent as the note's own text.

---

### User Story 4 - Read hard photos with a vision model, when allowed (Priority: P3)

The owner photographs a handwritten note. The local document-conversion service cannot read handwriting and returns almost nothing. Because the owner has turned on the vision fallback, VoiceNotes sends that one photo to a vision-capable model through the owner's model gateway and gets a usable hint.

**Why this priority**: It improves hints for a specific class of photos. It is off by default and depends on the owner choosing to send photos outside, so it comes last.

**Independent Test**: With the vision fallback on, send a photo of handwriting and a PDF that yields no text. Check that only the photo was sent to the vision model and both notes were saved.

**Acceptance Scenarios**:

1. **Given** the vision fallback is on and the document-conversion service returns little or no text for a photo, **When** it is processed, **Then** that single photo is sent to the configured vision model and the hint is based on its answer.
2. **Given** the vision fallback is on, **When** a document (PDF, office file, etc.) yields little or no text, **Then** it is never sent to the vision model; it gets a placeholder hint.
3. **Given** the vision fallback is off (the default), **When** any item yields little or no text, **Then** nothing is sent outside and the item gets a placeholder hint.
4. **Given** the vision fallback is on and the document-conversion service is not configured, **When** a photo is processed, **Then** the photo goes to the vision model directly; documents get a placeholder hint.
5. **Given** the vision model is unavailable or returns unusable output, **When** a photo is processed, **Then** the note is saved with the attachment and whatever hint is available (a placeholder when nothing was read), and the reply is the normal confirmation.

---

### Edge Cases

- **Document-conversion service unreachable**: the capture waits and retries in the background like speech-to-text today; the reply says it is queued; health reports the degradation. Nothing is lost and the step is not repeated after recovery.
- **Document-conversion service rejects a file for good** (corrupt, password-protected, unsupported type): the note is still created with the file attached and a placeholder hint naming the reason.
- **A voice note sent right after a large document**: the voice note is processed and confirmed without waiting for the document to be read; both notes appear in send order by receive time.
- **Very large documents**: only the first pages (a configurable cap) are sent for conversion, so one upload cannot block the queue for long. The full file is still attached.
- **Conversion takes too long**: each item has a bounded conversion time. A timeout is retried; after the third timeout on the same file it degrades to a "couldn't read in time" placeholder hint, with the file attached. An unreachable service is not a timeout and keeps being retried.
- **A file type the converter does not handle** (archive, executable, audio sent as a document): it is attached with a placeholder hint naming the file. Audio sent as a file keeps today's behaviour (transcribed as audio).
- **The notes store refuses an attachment** (too large, type refused): the note is still saved with the text and the remaining attachments, and the reply says which file was not attached.
- **A batch arrives slowly, or its parts are delivered out of order**: photos and files from the same chat join the open batch as long as each arrives within the quiet period of the previous one; parts of one album always join the same batch even if delivered out of order. Processing starts only after the quiet period has passed since the last part.
- **The same album is delivered twice**: each part is deduplicated by its source message identity; the album still yields one note.
- **A caption is very long**: it is shortened like long text today, with a "(truncated)" marker; attachments are unaffected.
- **Extracted text contains instructions** ("ignore previous instructions…"): it is treated as data to describe, never as instructions; the hint can be wrong but nothing else happens.
- **Photos with sensitive content** (IDs, bank details): they stay local unless the owner has turned on the vision fallback.

## Requirements *(mandatory)*

### Functional Requirements

**Accepting photos and files**

- **FR-101**: The system MUST accept photos, files sent as documents, and videos, including forwarded ones, from allowlisted senders as captures, within the bot download limit. This supersedes 001 FR-004 for those message types.
- **FR-102**: The system MUST keep answering other message types (stickers, locations, contacts, polls and similar) with a one-line "unsupported" reply that names what is accepted, and MUST NOT create a note.
- **FR-103**: The system MUST persist every received file durably and locally before acknowledging the capture, as for audio (001 FR-005).
- **FR-104**: A caption sent with a photo, file or batch MUST be used as the note's own text and processed by the LLM step like a text capture.
- **FR-105**: Audio sent as a file MUST keep being handled as audio (transcribed), as today.

**One send, one note**

- **FR-106**: Photos, files and videos from the same chat MUST be grouped into a **batch**: an item joins the chat's open batch when it arrives within the quiet period after the batch's previous item, or belongs to the same album as an item already in it; otherwise it starts a new batch. Voice and text messages MUST never join a batch. A batch MUST produce exactly one capture, one note and one reply, with every item attached in the order sent. Captions from several items MUST be joined in the order sent.
- **FR-107**: Each batch item MUST be persisted on arrival. The capture MUST start processing only after a configurable quiet period (default a few seconds) has passed since its last item arrived.
- **FR-108**: Deduplication MUST work per item: a redelivered batch item MUST NOT create a second note or a second attachment.

**Reading content (optional)**

- **FR-109**: Content extraction MUST be optional. When it is not configured, photos and files MUST still become notes with their attachment and a placeholder hint naming the item.
- **FR-110**: When configured, the system MUST extract text from photos and documents through a locally run document-conversion service reached at a configured address. It MUST cover at least: images, PDF (with a text layer and scanned), word-processor, spreadsheet and presentation documents, HTML, Markdown, CSV and plain text. The repository MUST NOT require a specific installation, version or configuration of that service beyond its standard conversion interface.
- **FR-111**: The system MUST send at most a configurable number of leading pages of a document for extraction.
- **FR-112**: Videos MUST be attached without content extraction.
- **FR-113**: Extraction MUST follow the same retry rules as other dependencies (001 FR-009, FR-010): temporary failures wait and retry, permanent failures degrade to a note with the attachment and a placeholder hint naming the reason. As an exception to 001 FR-009, a conversion that times out on the same file 3 times MUST be treated as permanent ("couldn't read in time"); an unreachable service MUST keep being retried.
- **FR-114**: An extraction step completed for a capture MUST be recorded, so it is not repeated after a later failure or restart.
- **FR-114a**: Content extraction MUST NOT block other captures: a capture whose extraction is slow or waiting MUST NOT delay later captures' notes. This relaxes 001 FR-011 for every capture, not only documents: a capture waiting on any dependency no longer holds back later ones, captures may finish out of arrival order, and notes are ordered by their original receive time (001 FR-012a).

**The hint**

- **FR-115**: Every photo or file capture's note MUST carry a hint: one or two lines describing the item.
- **FR-116**: When the LLM step is on, the hint MUST be generated by the existing language-model endpoint from the extracted text, written in the configured target language. The model MUST receive at most a configurable excerpt of the extracted text (default a few thousand characters), never the whole document.
- **FR-117**: When the LLM step is off, or fails, the hint MUST be the first lines of the extracted text, shortened to the hint length. When there is no extracted text, the hint MUST be a placeholder naming the item (file name, or "photo").
- **FR-118**: For a batch, the system MUST produce one hint for the whole batch from the content of all its items, within the same excerpt bound.
- **FR-119**: Extracted text MUST be treated as content to describe. The system MUST NOT follow instructions found in it.

**Vision fallback (optional, off by default)**

- **FR-120**: The system MAY send a photo to a vision-capable model through an OpenAI-compatible chat endpoint, only when the owner has turned this on in configuration.
- **FR-121**: When on, the fallback MUST be used only for photos (images), only when the document-conversion service returned too little text or is not configured, and only one photo per request. Documents MUST never be sent to the vision model.
- **FR-122**: The vision model's endpoint, credentials and model MUST be configurable. They MAY be the same as the LLM step's. The system MUST NOT depend on provider-specific features.
- **FR-123**: A vision-model failure MUST degrade like the LLM step today, with a bounded number of attempts and no waiting state: the note is saved with the attachment and the best available hint (a placeholder when nothing was read). The reply MUST NOT change because of it; the placeholder hint is what shows the photo was not read.

**Notes, replies and clean-up**

- **FR-124**: A photo or file note MUST contain, in order: the caption text (processed as in 001 FR-022, when present), the hint, the action-item checklist if any, the original-language quote if translated, and the tags (always `#inbox`). All received files MUST be attached.
- **FR-125**: The digest MUST treat photo and file notes like any other `#inbox` note. Without an LLM summary, the digest line MUST show the caption's first line when there is a caption, otherwise the hint.
- **FR-126**: When some of a capture's files cannot be attached or cannot be downloaded, the note MUST still be saved with everything else, and the reply MUST say which items were not kept.
- **FR-127**: Once the note is saved, all of the capture's local files and extracted text MUST be deleted, as for audio (001 FR-012).
- **FR-128**: The health check MUST report the document-conversion service the same way it reports speech-to-text (captures waiting on it past the threshold degrade health), only when it is configured. The vision model, like the LLM step, MUST NOT affect health: it never makes a capture wait.

**Configuration and independence**

- **FR-129**: With none of the new settings configured, behaviour for voice and text captures MUST be unchanged, and photos and files MUST be accepted with placeholder hints.
- **FR-130**: The new settings (document-conversion address, page cap, quiet period, excerpt size, vision switch and endpoint) MUST be validated at startup like the existing ones, naming the offending setting and never printing a secret.
- **FR-131**: Documentation MAY recommend a setup for the document-conversion service and a vision model, only as anonymised examples. Nothing in the repository may require a specific deployment.

### Key Entities

- **Capture** (extended from 001): one incoming send, now holding zero or more files instead of at most one audio file. A voice capture holds one audio file; a text capture holds none; a photo/file capture (a batch) holds one or more. It gains an optional caption and a hint, and a quiet-period deadline while the batch is still open.
- **Capture file**: one received file belonging to a capture. Has its source message identity (for per-item deduplication), its order within the capture, its kind (audio, image, document, video), original file name and type, size, local copy (until the note is saved), extracted text or transcript (until the note is saved), and whether it was attached, skipped or failed and why.
- **Note** (extended from 001): may have several attachments and carries the hint.
- **Configuration** (extended): document-conversion address and page cap, batch quiet period, excerpt size, vision fallback switch, endpoint, credentials and model.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-101**: With nothing new configured, 100% of photos and files sent in a test run become notes with their originals attached, and every existing voice and text acceptance test from feature 001 still passes unchanged.
- **SC-102**: With the document-conversion service and LLM step configured, in at least 8 of 10 test items (photos of printed bills and letters, text PDFs, scanned PDFs, office documents) the owner can tell what the item is from the hint alone, without opening the attachment.
- **SC-103**: A send of up to 30 photos or files (single album or several) yields exactly one note with every item attached and exactly one reply, in 100% of test runs, including runs where VoiceNotes is restarted mid-batch.
- **SC-104**: Zero captures are lost across a test run in which the document-conversion service, the vision model, the notes store and VoiceNotes itself are each restarted while photo and file captures are in flight.
- **SC-105**: With the vision fallback off, no file content leaves the owner's infrastructure except a bounded excerpt of extracted text sent to the configured language model; with it on, only individual photos additionally do.
- **SC-106**: When all dependencies are up, a single photo becomes a note with its hint and a confirmed reply within 60 seconds of sending, and a 10-page document within 3 minutes.
- **SC-107**: A self-hoster can enable document reading by setting only the document-conversion address, using any standard installation of that service.

## Assumptions

- The document-conversion service is docling-serve or compatible, run by the owner; the plan decides which interface version to target. It runs on CPU and may take several seconds per page, which is acceptable for an overnight inbox.
- The existing LLM endpoint (OpenAI-compatible, e.g. through LiteLLM) writes the hint; no provider-specific features are used. A vision-capable model, if used, is reached the same way.
- The batch quiet period defaults to a few seconds (the messaging platform delivers album parts, and consecutive albums of one send, within about a second of each other) and is configurable. Unrelated photos sent within that period merge into one note; this is acceptable for a morning-review inbox.
- Default page cap (about 20 pages) and excerpt size (about 4,000 characters) are reasonable starting points and are configurable.
- The 20 MB bot download limit applies per file, as for audio.
- The notes store accepts several attachments per note; attachment size limits are the store's own.
- Video content is out of scope: videos are attached only. Reading archives, transcribing video audio, and extracting embedded images are out of scope.
- The hint is never regenerated later: like the rest of the note, VoiceNotes never edits a note after creating it.
