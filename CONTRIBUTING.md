# Contributing to VoiceNotes

Thanks for your interest in improving VoiceNotes! This guide covers the
essentials. For the full developer and agent onboarding (the quality gate, the
coverage scope and the repository rules), see [.claude/CLAUDE.md](.claude/CLAUDE.md).
The binding project rules are in
[.specify/memory/constitution.md](.specify/memory/constitution.md).

## Before you start

VoiceNotes is a personal project. It is maintained in whatever time is left
over after everything else, and that shapes what you can expect:

- **Pull requests may not be reviewed.** Not because they are unwelcome — there
  simply may not be time. Please do not read silence as a judgement on your work.
- **Issues may not get a response,** and there is no target response time.
- **No support is offered.** The [README](README.md) and the feature docs in
  [specs/](specs/) are the support.

If you need a change on a schedule you control, **fork the project**. That is a
first-class outcome here, not a fallback — the licence permits it and it will
serve you better than waiting. Bug reports with a clear reproduction are still
genuinely useful even when they go unanswered for a while.

## Prerequisites

- **Go 1.27+** (the project compiles with `CGO_ENABLED=0`)
- **[go-task](https://taskfile.dev)** — all automation is driven through `Taskfile.yml`
- **[pre-commit](https://pre-commit.com)** — runs the format, lint and
  commit-message hooks in `.pre-commit-config.yaml`
- **[git-cliff](https://git-cliff.org)** — only needed to cut a release

`mise install` sets up the pinned toolchain from `.mise.toml`.

## Getting started

```bash
task setup         # download Go modules
pre-commit install # wire up the pre-commit, commit-msg and pre-push hooks
cp .env.dist .env  # fill in the fake values with your own
task run           # build and run with .env loaded
```

## Before you open a pull request

Run the same gates CI enforces:

```bash
task format          # apply formatters and autofixes
task lint            # 0 issues
task test:coverage   # tests with -race, coverage at or above 80%
task build           # CGO-free binary
```

The hooks run these for you, each at the cheapest stage that still catches the
problem: `commit-msg` checks the subject line, `pre-commit` runs
`golangci-lint fmt --diff`, and `pre-push` runs the full `task lint`. Do not
bypass them with `--no-verify`.

## Conventions

- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix(digest):`, `docs:`, …), enforced by a `commit-msg` pre-commit
  hook. The subject line is what shows up in the changelog, so write it for a
  reader who was not there.
- **Tests** use only the standard library `testing` package, table-driven where
  it helps, with hand-written fakes and `net/http/httptest`. Write the test
  first and see it fail.
- **New dependencies** require approval before they're added — state the
  package, what it solves, and why the standard library is insufficient.
- **The repository is public.** Use fake example values only (`example.org`,
  RFC 5737 IPs, tokens like `123456:FAKE-TOKEN`); never commit real hosts,
  tokens, chat IDs or endpoints.

## Releases

Releases are cut by hand, by the maintainer. `CHANGELOG.md` is the single
release document.

1. `task changelog TAG=v0.2.0` — prepends the generated commit list for
   everything since the last tag.
2. Write the prose highlights into that new section by hand. This is the part
   that matters; the generated list is supporting detail.
3. Preview the release body: `.github/scripts/extract-changelog.sh v0.2.0`.
4. Commit the changelog.
5. `git tag v0.2.0 && git push origin v0.2.0`.

Pushing the tag triggers `release.yml`. It first runs the full CI (lint, tests
with the coverage gate, build), then builds and pushes the multi-arch image to
GHCR as `0.2.0` and `latest`, and opens the GitHub release using that changelog
section as the body. It fails if the committed `CHANGELOG.md` has no
`## v0.2.0` section — i.e. if the tag was pushed before step 1 or step 4.

## Core constraints

VoiceNotes **never loses a capture**: it persists before acknowledging, keeps
every pipeline step idempotent and degrades to a placeholder note rather than
dropping anything. It ships as a **single CGO-free binary** in a distroless
image, and **keeps running** when Telegram, speech-to-text, the LLM or Memos is
down. Please keep changes within these invariants.
