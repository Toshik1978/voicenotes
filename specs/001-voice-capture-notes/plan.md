# Implementation Plan: VoiceNotes — voice capture to notes with a morning digest

**Branch**: `001-voice-capture-notes` (work lands on `main`, per Constitution VIII) | **Date**: 2026-10-30 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/001-voice-capture-notes/spec.md`

## Summary

VoiceNotes is a single static Go service. It receives the owner's voice and text messages from a
private Telegram bot by long polling and records each one as a **capture** in a local SQLite
queue. A single worker then moves each capture through idempotent steps:

1. download the audio
2. transcribe it (any OpenAI-compatible transcription endpoint)
3. enrich it (optional; any OpenAI-compatible chat endpoint)
4. write the note (Memos)
5. finalise the reply (edit the ⏳ reply into "✓ saved")

Every step's result is stored before the next one starts, so dependency outages and restarts only
delay a capture. Once the note exists, local text and audio are deleted. A stdlib-timer scheduler
sends one loud morning digest of every Memos note still tagged `#inbox`, and a small HTTP listener
serves a health check.

## Chosen approach (maintainer decision)

| | A. SQLite on the data volume **(chosen)** | B. Shared Postgres only | C. Both, by config |
|---|---|---|---|
| Other self-hosters | Nothing to set up | Must run Postgres | Nothing to set up |
| Target deployment | One file on the data volume, backed up with it | Own DB + role + secret; container stateless | Postgres |
| Cost | Least code; same pattern as the maintainer's tgshd | Same code size, more ops | Two SQL dialects, every query tested twice |

**Why A**: the local state is a short-lived queue plus dedupe IDs. Everything durable lives in
Memos, which already sits on the shared Postgres. Putting a queue that drains within minutes on
the shared cluster adds a role, a secret and a startup dependency without making anything safer.
The store sits behind a small interface, so a Postgres implementation can be added later if a real
need appears.

Also decided: no uber/fx (explicit wiring in `main()`), Telegram long polling only, and a single
speech-to-text endpoint switched by configuration (no automatic fallback).

## Technical Context

**Language/Version**: Go 1.27 (toolchain pinned in `go.mod` and `.mise.toml`, as in tgshd)

**Primary Dependencies**: these need maintainer approval under Constitution III. Each is listed
with why the standard library is insufficient.

| Package | Solves | Why not stdlib |
|---|---|---|
| `gopkg.in/telebot.v4` | Telegram Bot API: long polling, file download, send/edit with `DisableNotification` | Re-implementing the Bot API types and the polling loop is large; the maintainer already uses it in tgshd |
| `go.uber.org/zap` | Structured, levelled logging | Matches the maintainer's other services and the constitution's Engineering Constraints. `log/slog` would also work, but zap is the house standard |
| `github.com/caarlos0/env/v11` | Env-var → struct parsing with defaults, `required`, durations, lists | Avoids hand-written parsing of about 30 variables; already used in tgshd and folio |
| `modernc.org/sqlite` | CGO-free SQLite driver for `database/sql` | stdlib has no SQLite driver, and Constitution IV forbids cgo |

**Not used**: gocron (one daily timer is about 20 lines of stdlib, see research R6), goose (schema
via embedded SQL plus `PRAGMA user_version`, see R5), uber/fx, any OpenAI/Anthropic SDK, ffmpeg.

**Storage**: SQLite file `$DATA_DIR/voicenotes.db` (default `/data`), WAL mode. Audio is kept in
`$DATA_DIR/audio/<capture-id>.<ext>` until its note is saved. See [data-model.md](data-model.md).

**Testing**: stdlib `testing` only, table-driven, with hand-written fakes and `net/http/httptest`
servers standing in for speech-to-text, the LLM and Memos. The SQLite store is tested against a
real temporary database file. The 80% coverage gate applies to the pure packages (see Structure).

**Target Platform**: Linux container (distroless `static-debian12:nonroot`), multi-arch
amd64/arm64, image `ghcr.io/toshik1978/voicenotes`.

**Project Type**: long-running single-binary service, with a `health` CLI subcommand for container
healthchecks.

**Performance Goals**: one owner, a handful of captures a day, bursts of 10–20 after an outage.
The goals are SC-001 (a 10 s clip confirmed in ≤15 s without the LLM, ≤30 s with it) and SC-003
(backlog drained within 10 min of recovery, which caps the retry interval at 5 min).

**Constraints**:
- CGO-free
- no shell or tzdata in the image (embed `time/tzdata`)
- Telegram bot downloads ≤20 MB, messages ≤4096 UTF-16 units
- Memos note content ≤8 KiB by default
- secrets only via env, never logged

**Scale/Scope**: single owner, local queue usually empty; about 15 small packages.

## Constitution Check

*GATE: checked before Phase 0 and re-checked after Phase 1. Status: PASS.*

| Principle | How the plan complies |
|---|---|
| I. Idiomatic Go, stdlib first | Each package has one responsibility. External systems sit behind small interfaces (`Store`, `Transcriber`, `Enricher`, `NotesWriter`/`NotesReader`, `Replier`, `Downloader`). The HTTP clients use `net/http` and the scheduler uses `time` |
| II. Test-first, stdlib testing | Decision logic lives in pure packages inside COVERPKG: state machine/backoff, language selection, note rendering/truncation, LLM prompt and response validation, digest composition, next-run computation, config validation, health status. Adapters are tested with httptest and temporary files |
| III. Dependency discipline | Four direct dependencies, listed above for approval. No test dependencies |
| IV. Static CGO-free | modernc SQLite; distroless static image; `time/tzdata` embedded; healthcheck via the binary's own `health` subcommand (no curl/shell) |
| V. Fail fast on config, degrade on deps | `config.Load` validates every setting and names the bad variable without echoing secrets. The LLM and digest are wired only when configured. A dependency outage only moves captures to `waiting` |
| VI. No capture lost | Audio written and row committed before ⏳ is sent. If the Telegram download fails, the row is committed without a reply, and ⏳ follows once the audio is on disk (R2). Per-step results recorded. Permanent failure produces a placeholder note. Audio deleted only after the note is saved |
| VII. Public hygiene | Examples use `example.org`, fake tokens and IDs. `.env.dist` is anonymised. The deploy-specific contract goes to the deployment repo, not this one |
| VIII. Conventional Commits | `.pre-commit-config.yaml` with conventional-pre-commit (`commit-msg`), `commit-lint.yml` in CI, `.cliff.toml` → `CHANGELOG.md` |

No violations, so Complexity Tracking stays empty.

## Project Structure

### Documentation (this feature)

```text
specs/001-voice-capture-notes/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── configuration.md     # env vars = deploy contract
│   ├── external-apis.md     # exact STT / LLM / Memos calls
│   ├── telegram-ux.md       # messages the bot sends
│   └── health.md            # HTTP health + CLI
└── tasks.md                 # /speckit-tasks
```

### Source Code (repository root)

```text
cmd/voicenotes/
├── main.go              # explicit wiring; subcommands: (none)=run, health, version
internal/
├── config/              # env → Config, validation (pure)                       [COVERPKG]
├── capture/             # Capture, Step, Status, transitions, backoff (pure)      [COVERPKG]
├── pipeline/            # worker loop driving steps via interfaces               [COVERPKG]
├── language/            # source-language selection & re-transcribe rule (pure)  [COVERPKG]
├── enrich/              # LLM prompt building + JSON response validation (pure)  [COVERPKG]
├── render/              # note Markdown layout, tags, 8 KiB truncation (pure)    [COVERPKG]
├── digest/              # composition, next-run/catch-up computation, scheduler  [COVERPKG]
├── health/              # status computation + HTTP handler                      [COVERPKG]
├── store/sqlite/        # Store over modernc SQLite, embedded schema (tmp-file tests)
├── telegram/            # telebot ingest + Replier + Downloader adapter
├── openai/              # OpenAI-compatible transcription + chat HTTP clients (httptest)
└── memos/               # NotesWriter/NotesReader over Memos REST (httptest)
.github/workflows/       # ci.yml (lint, test:coverage, build), commit-lint.yml, release.yml (image on v* tags)
Dockerfile  Taskfile.yml  .golangci.yml  .mise.toml  .pre-commit-config.yaml  .cliff.toml
.env.dist  .dockerignore  README.md  CHANGELOG.md  LICENSE  CLAUDE.md
```

**Structure Decision**: one Go module `github.com/Toshik1978/voicenotes` with a single binary
under `cmd/voicenotes`. `internal/` is used rather than `pkg/` because nothing is meant for
import. Pure logic packages form the COVERPKG gate. `store/sqlite`, `telegram`, `openai` and
`memos` are adapters: their tests run, but they sit outside the gate (Constitution II).

## Key design decisions (details in research.md)

- **One worker, FIFO.** Captures are processed strictly in arrival order by one goroutine. While
  the head capture waits on a dependency, later ones wait too, which is correct because they need
  the same dependency (FR-011). Ingest runs in telebot's handlers. They download the audio,
  insert the row, then send ⏳, all through a source-agnostic `Ingest` function that a later HTTP
  route can reuse (FR-007).
- **Step results are columns on the capture row.** The state machine is pure: given a capture
  and a step outcome (ok, temporary error, or permanent error), it returns the next status,
  next attempt time and reply text.
- **Retry**: exponential backoff from 5 s, doubling, capped at `RETRY_MAX_INTERVAL` (default
  5 m), unbounded for speech-to-text, Memos and Telegram. The LLM gets at most 3 attempts or
  2 minutes, then it is skipped (FR-018). HTTP 4xx other than 408/429 counts as permanent.
- **Note link before cleanup.** The note's link is recorded, then the reply is finalised, then
  text and audio are wiped (FR-012). The row keeps only IDs, purged after 30 days.
- **Digest "new vs from earlier"** is tracked by note name in a `digest_seen` table, rather than
  by comparing times. A note delayed past a digest by an outage still counts as new.
- **Digest summaries** take one batched LLM call per digest, not one per capture. If that fails,
  each line shows the note's first line.
- **Memos API prefix**: `MEMOS_API_PREFIX=auto` (the default) probes `{url}/api/auth/me` and
  falls back to `{url}/api/v1/auth/me` (410 or 404). An explicit value skips the probe.

## Complexity Tracking

None.
