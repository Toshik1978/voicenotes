# Contract: configuration (additions)

All new variables are optional. With none set, behaviour for voice and text is unchanged, and
photos and files are accepted with placeholder hints (FR-129). Validation runs at startup. Errors
name the variable and never print its value (Constitution V).

| Variable | Type | Default | Validation / effect |
|---|---|---|---|
| `DOCLING_URL` | URL | unset | When set: absolute `http`/`https` URL, else a startup error. Enables content extraction. |
| `DOCLING_API_KEY` | secret | unset | Sent as `X-Api-Key`. Ignored when `DOCLING_URL` is unset. |
| `DOCLING_TIMEOUT` | duration | `10m` | > 0. Per-file wait from submit to result. |
| `DOCLING_MAX_PAGES` | int | `20` | 1–1000. Leading pages converted per document. |
| `BATCH_QUIET_PERIOD` | duration | `3s` | 500ms–1m. Quiet time that closes a batch. |
| `HINT_EXCERPT_RUNES` | int | `4000` | 500–50000. Total extracted text sent to the LLM per batch. |
| `VISION_ENABLED` | bool | `false` | Enables the photo-only vision fallback. |
| `VISION_BASE_URL` | URL | `LLM_BASE_URL` | Required (directly or by fallback) when `VISION_ENABLED=true`. |
| `VISION_API_KEY` | secret | `LLM_API_KEY` | |
| `VISION_MODEL` | string | `LLM_MODEL` | Required (directly or by fallback) when `VISION_ENABLED=true`. |
| `VISION_TIMEOUT` | duration | `60s` | > 0. |

`.env.example` and the README document these with anonymised values:

- `DOCLING_URL=http://docling.example.org:5001`
- `VISION_BASE_URL=https://llm.example.org/v1`
- `VISION_MODEL=example-vision-model`

The README describes a docling-serve container and a vision-capable model as a *suggestion*. No
version, image, OCR engine or model is required.
