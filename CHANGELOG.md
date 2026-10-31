# Changelog

What changed in each `voicenotes` release and why. Highlights are written by
hand; the commit lists under them are generated with
[git-cliff](https://git-cliff.org) via `task changelog TAG=vX.Y.Z`.

Versions follow [semver](https://semver.org). Commits follow
[Conventional Commits](https://www.conventionalcommits.org).

---

## v0.1.3 — 2026-10-31

Highlights:

- Action items of a translated note are now in the target language too. The prompt asked for them "in the language of the text", which the model read as the original language, so an English note could carry a Russian checklist.

### Bug Fixes

- fix(llm): write action items in the target language ([0509646](https://github.com/Toshik1978/voicenotes/commit/050964655cfa210071443b7902bdf4c432b47c73))

---

## v0.1.2 — 2026-10-31

Highlights:

- Notes in another language are now translated reliably. The prompt treated translation as optional, so a note with no action items often stayed in its language and silently looked as if the LLM never ran: measured over 30 runs per case, a Russian note without tasks stayed Russian 29 times and a German one 22 times. The prompt now requires the target language and repeats the instruction next to the note; every measured case gives a full note 30 times out of 30.
- If the model still keeps a foreign text, the LLM step asks again within its budget, and the reply says "(no AI)" rather than "✓ saved" when it never translates.
- At `LOG_LEVEL=debug`, the shape of each LLM answer (language, translated, task and tag counts) and whether the note used it are logged, never the text.

### Features

- feat(pipeline): log the shape of the LLM answer at debug ([6faeea9](https://github.com/Toshik1978/voicenotes/commit/6faeea97a938fb0b892a2abd5f3bb3613efd3962))

### Bug Fixes

- fix(llm): translate every note reliably ([a9edd2e](https://github.com/Toshik1978/voicenotes/commit/a9edd2efd96324797db491f5e21843cf74351bdc))

---

## v0.1.1 — 2026-10-30

Highlights:

- The LLM step now works with models that accept only their default temperature, such as Claude Haiku behind LiteLLM: VoiceNotes always sent `temperature: 0`, which those models reject, so every note was saved without AI. No temperature is sent now unless the new optional `LLM_TEMPERATURE` (0–2) sets one.

### Bug Fixes

- fix(llm): send no temperature unless LLM_TEMPERATURE is set ([0dbec65](https://github.com/Toshik1978/voicenotes/commit/0dbec65935ef831d1eb46c682720190d76512780))

---

## v0.1.0 — 2026-10-30

First release.

Highlights:

- Telegram voice and text captures become Memos notes tagged `#inbox` or `#todo`, with the audio attached.
- A durable SQLite queue keeps every capture across outages and restarts; orphaned audio is swept at startup.
- An optional OpenAI-compatible LLM step translates, tidies and tags, and builds checklists; source and target languages are configurable.
- A loud morning digest lists the `#inbox` notes, scheduled in the standard `TZ` zone.
- A health endpoint and `voicenotes health` report status; a distroless multi-arch image is published at `ghcr.io/toshik1978/voicenotes` from version tags.

### Features

- feat: add config, capture model, SQLite store and worker loop ([ece0d44](https://github.com/Toshik1978/voicenotes/commit/ece0d44059c9768bd632df13cff69e9099571d53))
- feat: capture voice and text into Memos notes ([98dfe10](https://github.com/Toshik1978/voicenotes/commit/98dfe1044736dc87f2a489e5ad52972562c90f0d))
- feat: never lose a capture when a dependency is down ([1ff31e2](https://github.com/Toshik1978/voicenotes/commit/1ff31e2ac26bf21945ecaf206a0a7f1050a15e5f))
- feat: send a morning digest of inbox notes ([f8eeeb9](https://github.com/Toshik1978/voicenotes/commit/f8eeeb9ee8839b3840ee260fca86c0c635056640))
- feat: add the optional LLM step and digest summaries ([4b76d99](https://github.com/Toshik1978/voicenotes/commit/4b76d99a42e18f71e9e7d9c3647d7f2d9ad6c057))
- feat: support configurable source languages ([f6ddb21](https://github.com/Toshik1978/voicenotes/commit/f6ddb21a4f50bea0390008664f4fbe17698ff62a))
- feat: report health and ship a container image ([3033ec7](https://github.com/Toshik1978/voicenotes/commit/3033ec7f3d13cb7fa321f3c5d3e685283e6b04e1))

### Others

- build: add project skeleton and quality gates ([88fb5ba](https://github.com/Toshik1978/voicenotes/commit/88fb5baf7d3f826534c9140c0e2fa5392ceb3d88))

---
