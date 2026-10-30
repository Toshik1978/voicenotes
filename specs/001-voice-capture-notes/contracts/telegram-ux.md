# Contract: Telegram messages

Every capture-related message and edit is sent with `disable_notification=true`. The digest is the
only message sent with sound. All text is plain or HTML-escaped. Lines are kept short.

## Capture replies (always a reply to the capture message; the same message is edited)

| Situation | Text |
|---|---|
| Accepted, processing | `⏳` |
| Waiting on a dependency | `⏳ queued — <speech-to-text\|notes\|Telegram> unavailable, will retry` |
| Done | `✓ saved · <a href="URL">open</a>` |
| Done with tasks | `✓ saved · 2 tasks · <a href="URL">open</a>` |
| Done, LLM skipped | `✓ saved (no AI) · <a href="URL">open</a>` |
| Done with placeholder (permanent failure) | `⚠ saved audio only — <short reason> · <a href="URL">open</a>` |
| Unsupported message type | `VoiceNotes takes voice or text only.` |
| File over 20 MB | `⚠ too large for Telegram bots (max 20 MB) — not saved.` |
| Non-allowlisted sender | *(no reply)* |

If the edit fails because the reply was deleted or is too old, a new silent reply is sent with the
same text.

## Morning digest (one message, with sound)

```text
☀️ VoiceNotes — 3 new, 2 from earlier

• ☑ Call the dentist about Thursday — open
• Garden shed: use the old fence boards — open
• Book title Anna mentioned — open

From earlier (2)
• ☑ Renew passport — open
• Idea for the backup script — open
…and 4 more
```

- `☑` marks notes that contain a task list. `open` is a link to `MEMOS_PUBLIC_URL/memos/<uid>`.
- Each section is ordered oldest first. The total number of lines is capped at
  `DIGEST_MAX_NOTES`.
- A summary is ≤80 characters: the LLM summary, or the first non-empty line of the note with tags
  stripped.
- With no `#inbox` notes, nothing is sent.
- The message is split if it would exceed 4096 UTF-16 units. With the default cap this does not
  happen.

## Commands

`/start` and `/help` reply silently with one line: `VoiceNotes: send a voice or text message to
save it as a note.` No other commands in v1.
