# CLAUDE.md

Guidance for an agent working *on* this repository. VoiceNotes is a Go service
that turns voice captures into notes. The binding rules live in
[`.specify/memory/constitution.md`](../.specify/memory/constitution.md); this
file is the runtime guide and must stay consistent with it. On conflict the
constitution wins and this file is corrected.

## The gate

Before any commit, all of these must hold:

| Command | Must show |
|---|---|
| `task lint` | 0 issues |
| `golangci-lint fmt --diff` | no output |
| `task test:coverage` | tests pass with `-race`, total coverage at or above 80% |
| `task build` | `bin/voicenotes` builds with `CGO_ENABLED=0` |

Other tasks: `task setup` (download modules), `task format` (apply formatters
and autofixes), `task test` (plain verbose run), `task run` (builds, loads
`.env` if present and runs the binary), `task clean`, and `task changelog
TAG=vX.Y.Z` (prepend the generated section to `CHANGELOG.md`).

Do not bypass hooks with `--no-verify`.

### The coverage scope

`COVERPKG` in `Taskfile.yml` and `-coverpkg` in `.github/workflows/ci.yml` must
stay identical: `internal/{config,capture,pipeline,language,enrich,render,digest,health}`.
They hold the pure decision logic. I/O adapters that only run against real
external services are outside the scope, though their tests still run. Packages
named there may not exist yet; Go only warns for a pattern that matches
nothing, and `.github/scripts/check-coverage-floor.sh` passes with a notice
while there are no statements to measure, then enforces 80%.

## Hooks and CI

Run `pre-commit install` once. The config declares the hook types, so that one
command wires all three stages:

| Stage | Runs |
|---|---|
| `commit-msg` | Conventional Commits (`compilerla/conventional-pre-commit`) |
| `pre-commit` | `golangci-lint fmt --diff` |
| `pre-push` | `task lint` |

CI (`ci.yml`) runs lint, the tests with the 80% coverage gate, and a
`CGO_ENABLED=0` build; it never publishes. On pushes to `main` it updates the
Tests and Coverage README badges in a public gist (repository secrets
`GIST_ID` and `GIST_SECRET_TOKEN`, a token with the `gist` scope); on pull
requests it posts a sticky coverage comment. `release.yml` runs only on `v*` tags:
it builds the release body from `CHANGELOG.md` via
`.github/scripts/extract-changelog.sh`, pushes the multi-arch image as
`<version>` and `latest`, and creates the GitHub release. `commit-lint.yml` re-runs the commit-msg hook over the
pushed or PR range and must never gain a path filter.

## Commits

Conventional Commits: `feat: ...`, `fix(digest): ...`, `chore(spec): ...`,
`build(deps): ...`. No attribution lines (no `Co-Authored-By`, no "generated
with"). `.cliff.toml` builds `CHANGELOG.md` from these prefixes.

Work is committed directly on `main`. **Push only when the maintainer
explicitly asks.** Scan unpushed changes for private data before pushing.

## Code rules

- Idiomatic, standard-library-first Go. Wrap errors with `%w` when adding
  context; never discard them silently.
- **No uber/fx and no other DI framework.** `main()` wires components
  explicitly with constructors. Optional features (LLM step, morning digest)
  are wired only when configured, with plain conditionals.
- **Tests use only the standard library `testing` package**: table-driven where
  it helps, hand-written fakes, `net/http/httptest` for HTTP. No testify, no
  mock generators. Develop test-first: write the test, see it fail, make it
  pass.
- Decision logic is pure (no I/O) and lives in packages inside the coverage
  scope; I/O sits in thin adapters behind small interfaces.
- Builds are static and CGO-free; the Docker image is distroless.
- Validate configuration at startup and name the offending setting, never its
  value. Secrets come only from the environment.
- Never lose a capture: persist durably before acknowledging, keep every
  pipeline step idempotent, degrade to a placeholder note rather than drop.

## Dependencies

The approved direct dependencies are exactly four:

- `gopkg.in/telebot.v4` (Telegram, long polling)
- `go.uber.org/zap` (logging)
- `github.com/caarlos0/env/v11` (environment parsing)
- `modernc.org/sqlite` (CGO-free local state)

Adding any other, or promoting an indirect one, needs the maintainer's explicit approval:
state the package, what it solves and why the standard library is
insufficient. Do not add it until approved.

## Public repository hygiene

This repository is public. Never commit real hostnames, IPs, URLs, container
names, bot tokens, chat or user IDs, API keys, or the real Memos, LiteLLM or
speech-to-text endpoints. Use fake example values only:

- hosts: `example.org` (for example `https://memos.example.org`)
- IPs: RFC 5737 ranges (`192.0.2.10`, `198.51.100.7`, `203.0.113.5`)
- tokens and IDs: obviously fake (`123456:FAKE-TOKEN`, chat ID `-1001234567890`)

The only real URLs allowed are `github.com/Toshik1978/voicenotes` and
`ghcr.io/toshik1978/voicenotes`. Nothing may reference private repositories.
Deployment configuration lives outside the repo; it ships only anonymised
examples. Applies equally to code, tests, fixtures, specs, commit messages and
tracker (`.beads/`) data.

## Workflow

Features are specified with Spec Kit (`/speckit-specify`, `/speckit-clarify`,
`/speckit-plan`, `/speckit-tasks`, `/speckit-analyze`) and executed with
superpowers (`/speckit-superpowers-execute`). `/speckit-implement` is not used.
Every plan carries a Constitution Check against Principles I-VIII. Each change
is reviewed for spec compliance and code quality before it lands on `main`.
