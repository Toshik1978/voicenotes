# Contract: Telegram messages (changes from 001 and 002)

Every message below is **silent**. Recording-related messages go to `MIC_NOTIFY_CHAT_ID`; they
are new messages, not replies.

## A microphone recording

| Moment | Message |
|---|---|
| Recording starts (sent best-effort; skipped if Telegram or, in proxy mode, VoiceNotes is unreachable) | `🎙 recording…` |
| Discarded: shorter than `MIC_MIN_LENGTH` | The start message is **deleted**. |
| Waiting on a dependency | Edited to the existing queued text, for example `⏳ queued — speech-to-text unavailable, will retry` |
| Saved | Edited to the existing confirmation: `✓ saved[ (no AI)][ · N task(s)] · open` |
| Saved with a placeholder (permanent failure) | Edited to the existing failure wording, for example `⚠ saved audio only — …` |
| No speech | Edited to `🔇 no speech heard — not saved` |
| Start message never sent | The outcome above is sent as a new message instead of an edit. |
| Start sent but the recording was never received (no upload within 1 day) | Edited to `⚠ recording not received` (best-effort, at purge). |

## Telegram voice and audio captures (change to 001)

| Case | Before | Now |
|---|---|---|
| Transcription succeeded and found no speech | Note "(no speech recognized)" with audio; reply `✓ saved · open` | No note; ⏳ edited to `🔇 no speech heard — not saved` |
| Transcription failed permanently | Placeholder note with audio | Unchanged |

## Recorder alerts

Each alert is sent once when its condition starts and once when it clears. `<name>` is the
recorder name (`MIC_RECORDER_NAME`).

| Condition | Alert | Recovery |
|---|---|---|
| Receiver missing | `🎙 <name>: receiver not connected — taps won't record` | `🎙 <name>: receiver connected` |
| Storage full | `🎙 <name>: storage full — new recordings refused` | `🎙 <name>: storage OK` |
| No check-in for `MIC_SILENCE_ALERT` | `🎙 <name>: not heard from since HH:MM` | `🎙 <name>: back online` |

All texts stay far below the 4096-unit limit and never include a URL with credentials, a token,
or a device serial number.
