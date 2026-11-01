# Contract: Telegram messages (delta to 001)

Everything in `specs/001-voice-capture-notes/contracts/telegram-ux.md` still holds unless changed
here. All capture replies stay silent.

## Accepted messages

| Message | Becomes |
|---|---|
| Voice, audio, audio sent as a document (`audio/*`) | `audio` capture (unchanged, never batched) |
| Text | `text` capture (unchanged, never batched) |
| Photo (compressed) | file item, kind `image` |
| Document | file item, kind `image` if `image/*`, else `document` |
| Video, animation | file item, kind `video` (attached, not read) |
| Sticker, location, contact, poll, dice, other | unsupported reply, no capture |

Forwarded messages are treated the same as the owner's own. A caption on any file item is the
owner's text.

## Batching

- File items from the same chat join the open batch while it is still collecting and either:
  - the item arrives before the batch's quiet deadline (`BATCH_QUIET_PERIOD` after the previous
    item), or
  - the item has the same album ID.
- Otherwise a new batch starts.
- One batch = one capture = one note = one reply.
- The ⏳ reply answers the batch's first item, and is sent once that item is downloaded and on
  disk (FR-103). If its download fails, the item stays stored and the ⏳ follows after the
  pipeline downloads it. Later items get no reply of their own: they are stored on arrival,
  downloaded by the pipeline, and confirmed by the final reply.
- Captions are joined in send order.

## Capture replies (new or changed rows)

| Situation | Text |
|---|---|
| Waiting on docling | `⏳ queued — document reader unavailable, will retry` |
| Done (files) | `✓ saved · <a href="URL">open</a>`, plus the same `· N tasks` / `(no AI)` rules as 001 |
| Done, some items not kept (too large, download rejected) | `⚠ saved, 1 of 4 not kept — <item> (<short reason>) · <a href="URL">open</a>` |
| Done, some items not attached (Memos refused) | `⚠ saved, 1 of 4 not attached — <item> (<short reason>) · <a href="URL">open</a>` |
| Done, no item could be kept | `⚠ files not kept — <item> (<short reason>) · please send them again · <a href="URL">open</a>` |
| Unsupported message type | `VoiceNotes takes voice, text, photos and files.` |
| Single file over 20 MB, not part of a batch | `⚠ too large for Telegram bots (max 20 MB) — not saved.` (unchanged) |

`<item>` is the file name as the note shows it (sanitised), or its place for a photo or file with
no name (`photo 3`, `file 2`). Each named item carries its own `<short reason>`, for example
`too large for Telegram bots`. At most three items are named, separated by commas; the rest are
summarised as `and N more`. When several kinds of failure occur, "not kept" wins over "not attached". Content extraction failures (no text,
couldn't read in time, unsupported by the reader) never change the reply: they show up as the
placeholder hint in the note.

Audio-capture texts ("audio not attached" and so on) are unchanged.

## Note layout for `files` captures

Sections are separated by a blank line:

```text
<caption text, processed by the LLM step when on>        (only when there is a caption)
<hint>                                                   (always; LLM hint, fallback hint or placeholder)
- [ ] <action item>                                      (when the LLM found any in the caption)
*Original (xx):* > <untranslated caption>                (when the caption was translated)
#inbox [#todo] [#suggested …]
```

Every kept file is attached in send order. The full extracted text is never pasted into the note.
The digest's first-line summary is therefore the caption when present, otherwise the hint.

Placeholder hints (R6):

- `📎 report.pdf`
- `📷 photo`
- `📷 3 photos`
- `📎 4 files`

When nothing could be read, the reason follows after ` — `, for example
`📎 scan.pdf — couldn't read in time`.

## Commands

`/start`, `/help`: `VoiceNotes: send a voice message, text, photo or file to save it as a note.`
