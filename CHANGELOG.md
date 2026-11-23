# Changelog

What changed in each `voicenotes` release and why. Highlights are written by
hand; the commit lists under them are generated with
[git-cliff](https://git-cliff.org) via `task changelog TAG=vX.Y.Z`.

Versions follow [semver](https://semver.org). Commits follow
[Conventional Commits](https://www.conventionalcommits.org).

---

## v0.4.0 — 2026-11-23

Highlights:

- A microphone outage pages once, not twice: `voicenotes health --strict --ignore-recorders` fails on everything `--strict` does except recorder alerts, which the service already reports in Telegram. `/healthz` adds `"service_status": "ok"` when recorder alerts are the only reason it is `degraded`. `--ignore-recorders` without `--strict` is a usage error (exit 2).
- Upgrade: nothing to do. `voicenotes health` and `--strict` behave as before.

### Features

- feat(health): let --strict leave recorder alerts out with --ignore-recorders ([3e741f3](https://github.com/Toshik1978/voicenotes/commit/3e741f3c92492c438cdbe1c77b8f6858dbf184f6))

---

## v0.3.0 — 2026-11-23

Highlights:

- A wireless microphone captures without the phone: tap the DJI Mic Mini 2S transmitter's Linking button, talk, tap again, and the recording becomes an `#inbox` note with the WAV attached, timed at the first tap. A silent `🎙 recording…` message appears at the first tap and is edited to the outcome.
- A new recorder binary, `voicenotes-mic`, runs as a systemd unit on whichever host has the receiver: the VoiceNotes host itself, or a small box such as a Raspberry Pi that uploads over the local network. Each release attaches it for linux/amd64 and linux/arm64, with checksums. The service stays static and CGO-free and gains `/mic/v1/` only when `MIC_UPLOAD_TOKEN` is set.
- Nothing is lost when something breaks: recordings are spooled as they are made, recovered after a crash and uploaded with retries until the service has stored them. Alerts tell the owner when a receiver is missing, a recorder's storage is full or a recorder has not been heard from, and `/healthz` reports each recorder.
- No speech, no note, from every source: a transcript with no speech creates no note and the reply says so (previously silence produced a placeholder note). Microphone audio is held in `$DATA_DIR/nospeech/` for `MIC_NOSPEECH_HOLD` (7 days). With speaches, set `STT_VAD=true`: Whisper invents text for silence, and the voice activity check stops it.
- Upgrade: migration 004 runs on start. With no `MIC_*` setting, nothing changes except the no-speech rule.

### Features

- feat(mic): microphone settings, coverage scope and cgo fence ([44a59d0](https://github.com/Toshik1978/voicenotes/commit/44a59d07e6791ac8ade27209752160e414737987))
- feat(mic): recording core, store, ingest, spool and recorder loop ([f7092b9](https://github.com/Toshik1978/voicenotes/commit/f7092b94dc0cfe98c37b894324691095e329c011))
- feat(mic): tap, talk, tap — direct-mode recorder and service API ([0028174](https://github.com/Toshik1978/voicenotes/commit/00281745e610a0bb820b6682f9bb021b707d649f))
- feat(mic): proxy mode — record near the bed, process on the server ([a88abb4](https://github.com/Toshik1978/voicenotes/commit/a88abb486deea68afbad417ade0eef4c8b535cf9))
- feat(mic): nothing is lost when something breaks mid-recording ([7483626](https://github.com/Toshik1978/voicenotes/commit/74836268f6ae6453edd85a61e3bde23939543b16))
- feat: no empty notes from silence, from any source ([54871fb](https://github.com/Toshik1978/voicenotes/commit/54871fb3a1314de1ceb239e38554212aa2db0df1))

### Bug Fixes

- fix(mic): converge with the spec and act on the final review ([7a14f83](https://github.com/Toshik1978/voicenotes/commit/7a14f8335a9a4fc74654f6deea3b285e3531d093))

### Others

- ci: group Dependabot security updates ([aa221ba](https://github.com/Toshik1978/voicenotes/commit/aa221ba158568cd9dbeb0fefbdade41c4e6e138e))
- ci: run CI weekly ([f435bd5](https://github.com/Toshik1978/voicenotes/commit/f435bd50284d2d2af74dae9b157e7ee8cbafeea3))
- ci: scan for known vulnerabilities with govulncheck ([26d7148](https://github.com/Toshik1978/voicenotes/commit/26d7148c6631ed8e6cf1a536bbef4d4abe64de35))
- ci(dependabot): apply house defaults ([dcab04c](https://github.com/Toshik1978/voicenotes/commit/dcab04c35c4ebff0ed7c1971c3b1327ab92e378d))
- ci: add pre-commit file guards, actionlint and gitleaks ([671404c](https://github.com/Toshik1978/voicenotes/commit/671404cf4f591567232881054e4543f877e650fe))
- ci: scan history for secrets with gitleaks ([b1d4b71](https://github.com/Toshik1978/voicenotes/commit/b1d4b713f6f3cebd57144deec5813a5a6df077c4))
- docs: amend constitution to v2.0.0 (no-speech exception, cgo-free service with cgo recorder) ([fddcd2f](https://github.com/Toshik1978/voicenotes/commit/fddcd2fc1d25f84246fa4787f6013b13a15483f2))
- docs: document the wireless microphone feature ([4f081c1](https://github.com/Toshik1978/voicenotes/commit/4f081c11c2a4294342c0d9088571aee9abfd567b))
- refactor: drop redundant compile-time interface assertions ([476861a](https://github.com/Toshik1978/voicenotes/commit/476861a5bc8a25eabff029d99bf5a90a64722394))
- build(release): release the recorder with GoReleaser ([aceb669](https://github.com/Toshik1978/voicenotes/commit/aceb669d689a6359cee1b34430982625f80c1857))

---

## v0.2.0 — 2026-11-01

Highlights:

- Photos and files sent to the bot become `#inbox` notes with the originals attached and a one- or two-line hint of what they are, so a late-night "deal with it tomorrow" capture is recognisable in the morning digest. A caption is kept as the note's text.
- An album, or several photos and files sent within `BATCH_QUIET_PERIOD` (3 s), becomes one note with every item attached in the order sent, one ⏳ reply and one combined hint. A restart while an album is arriving still yields one note.
- Hints come from an optional, self-hosted docling-serve (`DOCLING_URL`) via the existing LLM step, which sees only a bounded excerpt (`HINT_EXCERPT_RUNES`). Large documents are capped at `DOCLING_MAX_PAGES`, a reader that is down is retried in the background, and a document that cannot be read still becomes a note with a placeholder hint. Plain-text files are read locally.
- Optional vision fallback (`VISION_ENABLED`, off by default) for photos docling reads little or no text from; documents are never sent.
- With nothing new configured, voice and text behave exactly as before, and photos and files are saved with a placeholder hint.
- Reading a document never holds back voice and text notes: documents are processed in their own lane.
- Upgrade: migrations 002 and 003 run on start, and `$DATA_DIR/audio` is renamed to `$DATA_DIR/files`.

### Features

- feat(config): document, photo and vision settings ([f6c7976](https://github.com/Toshik1978/voicenotes/commit/f6c7976642039258c526356f5fa83b59f2cbc7c0))
- feat(pipeline): one capture holds many files, with due-first worker lanes ([cfe601e](https://github.com/Toshik1978/voicenotes/commit/cfe601ebd736030d9610e9b3526decf5b3616c12))
- feat: save photos and files as notes with their originals attached ([9414f7e](https://github.com/Toshik1978/voicenotes/commit/9414f7e3e80fd24d8e976c3956cd12bbe2def6c7))
- feat: hint each photo and document from its content via docling ([bfefb73](https://github.com/Toshik1978/voicenotes/commit/bfefb73f9020de6b329a8cd2324f259afe313365))
- feat: one send, one note — batch photos and files into a single capture ([d77f2a7](https://github.com/Toshik1978/voicenotes/commit/d77f2a7056b73d9d72ff7861e0d0693e576cfca2))
- feat: describe barely read photos with an optional vision model ([e293ac0](https://github.com/Toshik1978/voicenotes/commit/e293ac0eb797b729c89b3edc053551e19f870eb4))
- feat: keep albums in send order and save the first file before ⏳ ([49f3cc8](https://github.com/Toshik1978/voicenotes/commit/49f3cc88fe8579aba72c0d12af1af45ba88e1ee8))

### Others

- docs: document photo, file and album captures; final gate and quickstart ([23c1492](https://github.com/Toshik1978/voicenotes/commit/23c1492688f9d730123b3a3458a12d35be936346))

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
