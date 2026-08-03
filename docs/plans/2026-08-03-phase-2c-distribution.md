# Phase 2c — self-distribution and the agent skill

*Design document for one autonomous build. Self-contained. Depends on phases 2a and 2b being
merged — this phase installs talaria into live agent sessions, so it must not run before the
credential-containment work lands.*

## Why

Phases 1–3 built a tool nobody can install. Every test runs against `httptest` and fixtures; it
has never been pointed at a real API and no agent has ever driven it.

That gap blocks a decision. DESIGN.md §7 sets a gate after Phase 4: *"Do not start [the twin] on
faith; start them because using Phases 1–4 made the absence of a twin painful."* That evidence
requires usage, and usage requires installation.

**The deliverable of this phase is the tool in daily use, and a written record of what broke.**

## Scope

**In:** version stamping, `make install`, a `v0.1.0` tag, the Claude Code skill, and a dogfooding
findings document.

**Explicitly out** — these serve strangers, and the first consumer is this machine:

- Cross-platform release matrices and GitHub release artifacts.
- The curl-able install script and the Homebrew tap.
- The pi package.
- Anything under `internal/twin`.

The repository is already public, so `go install github.com/Teeeep/talaria/cmd/talaria@latest`
works as soon as a tag exists. Self-distribution is nearly free; the rest waits for a public
v0.1 that has survived contact with a real API.

## Commands

| Purpose | Command |
|---|---|
| All tests | `go test ./...` |
| Build | `go build ./...` |
| Lint | `test -z "$(gofmt -l .)" && go vet ./...` |

House rules are in `CLAUDE.md`.

## The work

### 1. Version stamping

`cmd/talaria/root.go` carries a `Version` string defaulting to `dev`, overridable via
`-ldflags -X`. Wire the build so `talaria version` reports a real version, the commit, and the
build date. Keep the existing output shape — it goes through the `talaria/v1` envelope and a test
already asserts that.

### 2. `make install`

A `Makefile` with `build`, `install` (to `~/.local/bin`), `test` and `lint`. The commands must
come from the table above rather than being reinvented. Tag `v0.1.0` once the tree is green.

### 3. The Claude Code skill

A skill at `~/.claude/skills/talaria/SKILL.md` wrapping the binary, with `AGENT.md` as its body.
It must teach the one thing that is not guessable: **how to name a credential you cannot see** —
the `TALARIA_AUTH_*` convention, `auth check`, and exit code 5 meaning *ask the human to set
`$NAME`*.

Two things it must also carry, because they change what an agent should do:

- **Host binding** (phase 2a): pointing `--base-url` outside the spec's `servers[]` runs the
  request *without* credentials and reports `credentials_withheld`. An agent seeing a 401 against
  a local twin needs to read that field rather than assume a broken credential.
- **The boundary verdict** (phase 2b): `talaria doctor` reports `enforced`, `policy-only` or
  `none`.

Keep it in the repo under `skills/talaria/` and install by copy, so it versions with the binary.

### 4. Dogfooding, and the findings document

This is the part that matters, and the part a loop cannot finish alone.

**Targets:**

- **Large public specs** — GitHub and Stripe. What to measure: `describe` output quality on deeply
  nested schemas, operationId synthesis collisions, load time and memory on multi-megabyte
  documents, and whether `list` output stays usable at thousands of operations.
- **Real 3.0 specs** — settle the question the original plan left open by design: does
  libopenapi-validator's 3.1-strict JSON Schema behaviour produce false failures on 3.0 documents?
  Exercise `nullable`, `exclusiveMinimum`/`exclusiveMaximum` as booleans, and `example` vs
  `examples`.
- **A real Swagger 2.0 spec** — this also closes review finding 32, deferred from 2a: the 2.0 body
  parameter is never exercised end to end, so `consumes:` → `requestBody.content` is unproven on
  the wire.
- **Local services** — anything on this machine with an OpenAPI document, for the full
  list → describe → dry-run → call → history loop under a real agent.

**Output:** `docs/findings/2026-XX-XX-dogfooding.md`. One entry per problem: what was attempted,
what happened, and whether it is a bug, a UX gap, or a missing feature. **This document, not
intuition, is what answers the §7 twin gate and what feeds the "deepen the CLI" phase after it.**

An empty findings document is a real result and should be recorded as one — it means the tool
survived contact.

## Acceptance

- `go install github.com/Teeeep/talaria/cmd/talaria@v0.1.0` produces a working binary that reports
  its version.
- The skill is installed and a Claude Code session completes list → describe → call against a real
  API without the credential appearing in the transcript.
- The findings document exists and names real targets attempted.

`go build ./...`, `go test ./...` and lint clean throughout.

## After this phase

1. **Deepen the CLI** — driven by the findings document. Candidates include request chaining via
   OpenAPI `links`, search ranking, and spec overlays. DESIGN.md §9 lists several of these as
   non-goals; the findings decide whether that stance survives contact.
2. **The twin** — roadmap phases 5–8, still behind the §7 gate, and now with evidence to judge it
   by.
