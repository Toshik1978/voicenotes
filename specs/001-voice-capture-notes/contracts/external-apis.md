# Contract: External API calls

VoiceNotes makes exactly these calls, so the deployment can provision models, keys and tokens to
match. Every client sets `User-Agent: VoiceNotes/<version>`.

**Failure classes** (all three services):
- **temporary, retried:** connection errors, timeouts, 5xx, 408, 429
- **misconfiguration, retried and reported as degraded health, never a placeholder:** 401, 403, 404
- **permanent:** 400, 413, 415, 422

## 1. Speech-to-text: `POST {STT_BASE_URL}/audio/transcriptions`

`multipart/form-data`:

| Field | Value |
|---|---|
| `file` | Telegram audio bytes as-is (voice = Ogg/Opus, filename `capture-<id>.ogg`; an audio file keeps its stored extension, for example `capture-<id>.mp3`); no transcoding |
| `model` | `STT_MODEL` |
| `response_format` | `STT_RESPONSE_FORMAT` (`verbose_json` default; `json` for gpt-4o transcribe models) |
| `language` | Sent only when one language is forced: exactly one `SOURCE_LANGUAGES` entry, or the re-transcribe pass |
| `temperature` | `0` |

Header: `Authorization: Bearer {STT_API_KEY}`, only when the key is set.

Response used: `text`, plus `language` when present. The language is normalized to ISO-639-1;
whisper-1 returns full names like `"russian"`, speaches returns `"ru"`.

Verified compatible:
- speaches with any CTranslate2 Whisper model, e.g. `Systran/faster-whisper-large-v3`. **The
  model must be pre-downloaded**, because speaches answers 404 for models that aren't installed.
- OpenAI `whisper-1` with `verbose_json`.
- OpenAI `gpt-4o-transcribe` / `gpt-4o-mini-transcribe` with `STT_RESPONSE_FORMAT=json`. These
  return no detected language, so the re-transcribe rule is skipped.

## 2. LLM: `POST {LLM_BASE_URL}/chat/completions`

```json
{
  "model": "<LLM_MODEL>",
  "temperature": 0,
  "messages": [
    {"role": "system", "content": "<instructions for the enabled functions + JSON schema>"},
    {"role": "user", "content": "<capture text, detected language, target language>"}
  ]
}
```

- No `response_format`, tools or other provider-specific fields. The model is told to answer with
  one JSON object only.
- The first `{…}` block in `choices[0].message.content` is extracted and validated.
- Header: `Authorization: Bearer {LLM_API_KEY}`.

**Per-capture response** (keys present only for enabled functions):

```json
{
  "language": "ru",
  "text": "Need to call the dentist tomorrow, and the garden shed idea.",
  "translated": true,
  "tasks": ["Call the dentist tomorrow"],
  "tags": ["garden", "health"]
}
```

Validation:
- `text` must be non-empty and ≤ 4× the input length.
- `tasks`: ≤10 items, each ≤200 characters.
- `tags` are normalised, never rejected: each item is trimmed, loses its leading `#`, is
  lowercased and has inner whitespace replaced by `-`. Items that still do not match
  `[a-z0-9_-]{1,32}`, items that are not strings, the inbox and todo tags (VoiceNotes adds them
  itself) and duplicates are dropped, order is kept, and the list is cut to `LLM_MAX_TAGS`.
  A `tags` value that is not an array yields no tags; the rest of the answer is kept.
  `LLM_MAX_TAGS=0` disables tags.
- If validation of any other key fails, the capture is saved without LLM output
  (`invalid_output`).

**Digest summaries** (one call per digest):
- The input is `[{"id":"<n>","text":"<first 500 chars>"}]`.
- The expected response is `{"summaries":{"<n>":"≤80 chars", ...}}`.
- Any id that is missing or invalid falls back to the note's first line.

**Deployment needs:** one model alias, e.g. a LiteLLM model named `voicenotes`. A small, fast model
such as Claude Haiku is enough. Usage is about 1 call per capture plus 1 per day, each with a few
hundred tokens.

## 3. Memos (v0.31.0 `/api/v1`; 26.10+ `/api`)

`{P}` = resolved API prefix. Header: `Authorization: Bearer {MEMOS_TOKEN}`, a Personal Access
Token `memos_pat_…`.

| Purpose | Call |
|---|---|
| Resolve prefix and owner (startup, and again after errors) | `GET {MEMOS_URL}{P}/auth/me` → `user.name` (`users/<username>`, e.g. `users/owner`). In `auto` mode, `/api` is tried first; 404 or 410 falls back to `/api/v1` |
| Create note | `POST {P}/memos?memoId=vn-<capture-id>-<hash>` body `{"content": "...", "visibility": "PRIVATE", "createTime": "<received_at RFC3339>"}` → `name` = `memos/<uid>` |
| Attach audio | `POST {P}/attachments` body `{"filename":"voice-<date>.ogg","type":"audio/ogg","content":"<base64>","memo":"memos/<uid>"}` |
| Digest: list inbox | `GET {P}/memos?filter=<urlenc: "inbox" in tags && creator == "users/owner">&pageSize=50&orderBy=create_time asc`, paging with `pageToken` (50 keeps a page of long notes under the 1 MiB response limit; a larger body fails as "response too large"). Uses `property.hasTaskList` for the ☑ marker |

- The deterministic `memoId` makes a retried create idempotent. On 409 (already exists, gRPC code 6; verified on 0.31.0 and 26.10.0-rc.2, and any other "already exists" error is treated the same),
  VoiceNotes fetches `GET {P}/memos/<uid>` and continues. An attach is skipped if the memo
  already lists an attachment with that filename.
- Links: `{MEMOS_PUBLIC_URL}/memos/<uid>`.
- Tags are written inline (`#inbox`) in `content`; the API `tags` field is read-only.

**Deployment needs:**
- Memos v0.31.0 (or 26.10+).
- A PAT created as the owner under Settings → Access Tokens, with expiry "Never". A PAT survives
  the 26.10 upgrade.
- The upload limit must be at least 20 MB (default 30–32 MB).
- After moving to 26.10, re-apply any custom content or upload limits, because they are not
  migrated.

## 4. Telegram Bot API (via telebot, long polling)

`getUpdates` (long poll, `allowed_updates=["message"]`), `getFile` + file download (≤20 MB),
`sendMessage` and `editMessageText` with `disable_notification`. No webhook. No public ingress
required.
