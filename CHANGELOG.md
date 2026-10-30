# Changelog

What changed in each `voicenotes` release and why. Highlights are written by
hand; the commit lists under them are generated with
[git-cliff](https://git-cliff.org) via `task changelog TAG=vX.Y.Z`.

Versions follow [semver](https://semver.org). Commits follow
[Conventional Commits](https://www.conventionalcommits.org).

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
