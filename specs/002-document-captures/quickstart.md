# Quickstart: validating document and photo captures

Prerequisites:

- a working 001 setup (bot, allowlist, Memos, optionally STT and LLM)
- `task build` passing

The automated gate covers every scenario below with fakes (`task test:coverage`). This guide is
the manual end-to-end check against real services.

## 0. Gate

```sh
task lint && golangci-lint fmt --diff && task test:coverage && task build
```

## 1. Nothing new configured (US1, FR-129, SC-101)

Run with no `DOCLING_*` / `VISION_*` variables.

| Action | Expected |
|---|---|
| Send a PDF | `⏳` → `✓ saved · open`. The note has `📎 <name>.pdf`, `#inbox`, and the PDF attached. |
| Send a compressed photo with caption "pay this" | Note: caption text (LLM-processed if the LLM is on), `📷 photo`, the photo attached. |
| Send a sticker | `VoiceNotes takes voice, text, photos and files.` No note. |
| Send a voice message | Exactly as before (001 quickstart). |

## 2. With docling-serve (US2, SC-102)

Run any docling-serve (e.g. a container on `docling.example.org:5001`) and set
`DOCLING_URL=http://docling.example.org:5001`.

| Action | Expected |
|---|---|
| Photo of a printed bill (LLM on) | Hint like "Electricity bill, €84, due Nov 10" in `TARGET_LANGUAGE`. |
| Text PDF, scanned PDF, DOCX, XLSX (LLM on) | A hint derived from the content of each. |
| Same with `LLM_BASE_URL` unset | The hint is the first lines of extracted text. |
| Photo of a landscape | Placeholder `📷 photo`, photo attached. |
| 300-page PDF | Only the first `DOCLING_MAX_PAGES` are converted. The note arrives within minutes, with the full PDF attached. |

## 3. Batches (US3, SC-103)

| Action | Expected |
|---|---|
| Album of 4 photos with a caption on the 3rd | One `⏳`, one note, 4 attachments in order, caption as text, one hint. |
| 15 photos at once (Telegram splits into 2 albums) | One note, 15 attachments. |
| Two photos sent separately 1 s apart | One note. |
| Photo, then a voice message 1 s later | Two notes. |
| Restart VoiceNotes while an album is arriving | Still one note, nothing missing. |

## 4. Non-blocking and resilience (FR-114a, FR-113, SC-104)

| Action | Expected |
|---|---|
| Send a 20-page scan, then immediately a voice message | The voice note is confirmed first. Both notes are ordered by receive time in Memos. |
| Stop docling, send a PDF | `⏳ queued — document reader unavailable, will retry`. Health degraded after the threshold. Start docling → note created, reply edited. |
| Set `DOCLING_TIMEOUT=5s`, send a large scan | After 3 timeouts: the note has `📎 scan.pdf — couldn't read in time`, file attached. |

## 5. Vision fallback (US4, SC-105)

Set `VISION_ENABLED=true` (vision endpoint defaults to the LLM gateway; the model must accept
images).

| Action | Expected |
|---|---|
| Photo of handwriting | A usable hint. The gateway logs show one image request. |
| Image-only PDF with no OCR text | Placeholder hint. No vision request. |
| `VISION_ENABLED=false` | No image ever reaches the gateway. |

## 6. Upgrade from 001

With captures queued in a 001 instance (some audio mid-pipeline), deploy this version:

- Migration 002 runs.
- `$DATA_DIR/audio` becomes `$DATA_DIR/files`.
- The queued captures finish normally.
