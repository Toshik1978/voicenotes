# VoiceNotes Constitution

## Core Principles

### I. Idiomatic Go, Standard Library First

- Code MUST be idiomatic Go and follow the existing patterns of the file being edited.
- The standard library MUST be the first choice. A third-party package is used only when the
  standard library is insufficient (see Principle III).
- Errors MUST be wrapped with `%w` when they cross a function boundary with added context, and
  MUST NOT be silently discarded.
- Each package MUST stay focused on one responsibility. Business logic sits behind small
  interfaces, and the concrete implementations are thin adapters over external systems
  (Telegram, speech-to-text, LLM, notes store, local state).

Rationale: a small service stays maintainable only if every package reads the same way and the
dependency surface stays minimal.

### II. Test-First, Standard Library Testing (NON-NEGOTIABLE)

- Every behaviour change MUST be developed test-first: the test is written, seen to fail, then
  made to pass.
- Tests MUST use only the standard library `testing` package, table-driven where it helps, with
  hand-written fakes for interfaces. testify, mock generators and any other test dependency are
  forbidden. `net/http/httptest` stands in for external HTTP services.
- Decision logic MUST be pure (no I/O) and live in packages inside the coverage gate. This covers
  pipeline step transitions, retry/backoff, language selection, note rendering and digest
  composition. I/O MUST be confined to thin adapters behind small interfaces.
- The coverage gate measures the `COVERPKG` scope, and total coverage over it MUST stay at or
  above 80%. `COVERPKG` in `Taskfile.yml` and `-coverpkg` in the CI workflow MUST be kept
  identical. Adapters that only run against real external services are excluded from the gate,
  but their tests still run.

Rationale: pure, table-tested logic is what makes the pipeline trustworthy without real Telegram,
speech-to-text, LLM or Memos instances present.

### III. Dependency Discipline

- A new third-party dependency (direct, or an indirect one promoted to direct) MUST be approved
  by the maintainer before it is added. The request MUST state the package, what it solves and
  why the standard library is insufficient.
- Only well-known, popular and actively maintained packages are eligible.
- Approved dependencies MUST be recorded in the feature plan's Technical Context.

Rationale: every dependency is code shipped in a service that handles the owner's private
thoughts. It must earn its place.

### IV. Static, CGO-Free Builds

- Builds MUST succeed with `CGO_ENABLED=0`, and no dependency may require cgo.
- The Docker image is distroless and static. Nothing may assume a libc or a shell inside it.

Rationale: a single static binary deploys anywhere and keeps the image minimal.

### V. Fail Fast on Configuration, Degrade on Dependencies

- The service MUST validate its configuration at startup. It MUST refuse to start on a missing or
  invalid required setting, naming that setting and never printing a secret value.
- Optional features (the LLM step and each of its functions, the morning digest) MUST be wired
  only when configured, through plain conditionals in `main()`.
- The process MUST keep running when Telegram, speech-to-text, the LLM or the notes store is
  unreachable. Affected captures wait and retry, the health check reports the degradation, and
  the process does not exit.

Rationale: a misconfiguration should be loud at deploy time, but a dependency restarting at night
must never take capture down with it.

### VI. No Capture Is Ever Lost

- A capture, including its audio, MUST be durably persisted locally before it is acknowledged to
  the sender.
- Every pipeline step MUST be idempotent, and its result MUST be recorded, so that a step is never
  repeated after a later failure or restart and a capture yields at most one note.
- A permanent failure MUST degrade to a note containing whatever is available (at minimum the
  audio and a placeholder naming the failure), never to silent loss.
- Local audio MUST be deleted only after the note carrying it has been saved.

Rationale: the whole point of VoiceNotes is that a thought spoken half-asleep is still there in
the morning.

### VII. Public Repository Hygiene

- The repository is public. Code, docs, tests, fixtures, specs, commit messages and tracker data
  MUST NOT contain real deployment data or personal data, other than the maintainer's public git
  identity. That includes:
  - hostnames, IP addresses (use RFC 5737 / `example.org`), URLs and container names
  - Telegram bot tokens, chat IDs and user IDs (use obviously fake values)
  - API keys
  - the real Memos, LiteLLM or speech-to-text endpoints
- Nothing in the repository may reference private repositories.
- Unpushed changes MUST be scanned for private data before they are pushed.

Rationale: once published, data cannot be reliably withdrawn.

### VIII. Conventional Commits and Workflow Discipline

- Commit messages MUST follow Conventional Commits (`feat: …`, `fix(digest): …`,
  `chore(spec): …`, `build(deps): …`) and MUST NOT carry attribution lines.
- The rule MUST be enforced locally by a `commit-msg` hook through pre-commit
  (`compilerla/conventional-pre-commit`, with
  `default_install_hook_types: [pre-commit, pre-push, commit-msg]` so one `pre-commit install`
  wires every stage). It MUST be re-checked in CI by a commit-lint workflow.
- `CHANGELOG.md` is generated from these prefixes (git-cliff).
- Work is committed directly on `main`. Pushing to the shared remote happens only on the
  maintainer's explicit request.

Rationale: parseable history drives the changelog and release notes, the same as in the
maintainer's other repositories.

## Engineering Constraints

- Language and runtime: Go (current toolchain in `go.mod`).
- No dependency-injection framework (no uber/fx): `main()` wires components explicitly with
  constructors. The framework's overhead is not justified for a small service.
- Logging uses zap. Telegram uses telebot in long-polling mode, and a webhook mode may only ever
  be optional.
- Configuration comes from environment variables, and secrets are supplied only through the
  environment (never as command-line arguments or in logs).
- Local state lives in CGO-free SQLite (modernc) under a single data directory.
- Speech-to-text and the LLM are reached only through OpenAI-compatible HTTP APIs, with no
  provider SDKs. The notes store sits behind a notes-writer interface, and Memos is the only
  implementation.
- User-facing messages MUST respect Telegram limits (4096 UTF-16 units) and MUST NOT leak secrets
  (tokens, keys, URL credentials, headers).
- Deployment-specific configuration lives outside the repository, which ships only anonymised
  examples.

## Development Workflow & Quality Gates

- Features are specified with Spec Kit (`/speckit-specify` → `/speckit-clarify` →
  `/speckit-plan` → `/speckit-tasks` → `/speckit-analyze`) and executed with superpowers
  (`/speckit-superpowers-execute`). `/speckit-implement` is not used.
- Every plan MUST include a Constitution Check against Principles I–VIII. A violation MUST be
  justified in the plan's Complexity Tracking or removed.
- Before any commit lands, all of these MUST hold:
  - pre-commit hooks are installed
  - `task lint` reports 0 issues
  - `task test:coverage` passes with the race detector on and the 80% floor met
  - `CGO_ENABLED=0` builds succeed
- Each change is reviewed for spec compliance and code quality before it lands on `main`.

## Governance

- This constitution supersedes other practice documents. `.claude/CLAUDE.md` is the runtime
  guide for agents and developers and MUST stay consistent with it. On conflict the constitution
  wins and the guide is corrected.
- Amendments are made with `/speckit-constitution`, committed with a Conventional Commit subject,
  and versioned semantically:
  - MAJOR for removing or redefining a principle
  - MINOR for a new principle or materially expanded guidance
  - PATCH for clarifications
- Plan Constitution Checks, task reviews and `/speckit-converge` treat MUST statements here as
  binding. A MUST violation is a critical finding.

**Version**: 1.0.0 | **Ratified**: 2026-10-30 | **Last Amended**: 2026-10-30
