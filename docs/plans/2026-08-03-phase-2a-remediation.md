# Phase 2a — remediation

*Design document for one autonomous build. Self-contained: everything needed to plan and
execute this phase is here or in the files it names.*

## The one thing to know first

**`go test ./...` is green right now, with every defect below live in the tree.** A passing
suite is not evidence that this phase is done and must never be treated as an acceptance
signal. Each item is accepted only when a test exists that **fails on `main` and passes after
the fix**. Write that test first; if it passes before the change, it is testing the wrong thing.

## Context

talaria is an OpenAPI CLI an agent drives without seeing credentials. `docs/design/DESIGN.md`
(v0.5) is the specification; this document is the work.

An autonomous build completed all 33 planned tasks and merged. A four-reviewer pass then found
32 defects — 8 critical — and escalated rather than converging. Each finding in
**`REVIEW_FINDINGS.md`** names a file, a line, and an empirical reproduction; they are verified,
not speculative.

**`REVIEW_FINDINGS.md` is the source of fix detail for this phase.** This document says what is
in scope, in what order, and why. That file says exactly what is broken and where. Read the
finding before writing the fix.

## Scope

**In:** 25 findings, listed below.

**Explicitly out** — do not drift into these:

- `internal/boundary`, `internal/fileguard`, `talaria doctor`, exit code 6, uid separation.
  That is phase 2b (`2026-08-03-phase-2b-boundary.md`).
- Packaging, release binaries, the Claude Code skill. That is phase 2c.
- Anything under `internal/twin` or `talaria twin …`.
- Reintroducing `talaria run`, `internal/gen` or JUnit output — deliberately removed.
- Findings 27, 28 and 32, deferred on purpose (see the end of this document).

Findings 7, 12 and 26 were made moot by deletions already merged. If a plan lists them, the
plan is stale.

## Commands

| Purpose | Command |
|---|---|
| All tests | `go test ./...` |
| One package | `go test ./<dir>/...` |
| Build | `go build ./...` |
| Lint | `test -z "$(gofmt -l .)" && go vet ./...` |

`golangci-lint` is not installed. House rules are in `CLAUDE.md`.

## The organising idea

**Findings 1, 2, 3 and 22 are one rule seen through four doors:**

> The destination of a request, and the credentials attached to it, are derived from the spec
> and the flags — never from stored or off-spec input.

They must be fixed as **one change**, not four. Splitting them is exactly how the previous cycle
produced a fix that narrowed *which* credential replay resolves while leaving *where it is sent*
untouched — finding 2 is already marked a repeat for that reason. A third failure is likely if
this is four tasks.

## Order

Two hard constraints. Everything else may be sequenced freely.

1. **Finding 11 before finding 1.** DESIGN.md §5a defines the allowed host set as the spec's
   `servers[]` *after server-variable substitution*. Substitution does not exist
   (`Server.Variables` is read nowhere). Fix 1 without 11 and the host set is computed from
   unsubstituted URLs — it will pass a fixture test and be wrong in production.
2. **Findings 1, 2, 3, 22 land together.** See above.

Finding 21 is the documentation half of finding 4 and ships in the same commit, or README
contradicts the code.

## Tier 1 — credential containment and the gate that should have caught it

| Finding | What |
|---|---|
| **1** | A resolved credential is transmitted to whatever host `--base-url` names. Implement DESIGN.md §5a host binding: allowed set = spec `servers[]` after variable substitution ∪ `--allow-host` ∪ profile `allow_hosts`. Off-set credentials divert to a `[]Withheld` rendered as `credentials_withheld`, plus one stderr line. `config.Profile` parses with `KnownFields(true)`, so `allow_hosts` must be added to the struct or profiles break |
| **11** | Server variables in `servers[].url` are never substituted. Prerequisite for 1 |
| **2, 3, 22** | `history replay` takes host, request shape and body from the stored entry. Re-derive through `loadSpec` → `index.Lookup` → `request.Build` → `config.Resolve`; emit a `validation` block; an entry whose operationId is gone fails exit 2. `replayableEnv` is deleted, not narrowed — verify the symbol is absent, not merely unreachable |
| **4** + **21** | `auth check`, `call` and `run` disagree on an unsupported scheme. DESIGN.md calls agreement non-negotiable. 21 rewrites the README passages that document the old behaviour |
| **5** | A spec-supplied media type injects arbitrary headers onto the wire — the one header path that skips `checkSplit`, inside the component that holds resolved credentials |
| **6** | A request body containing a secret prints unredacted on stdout while history redacts it. The firewall backwards. The emitted-curl half is settled in DESIGN.md §3.4: reference the source (`--data @file`, `--data @-`), inline only an argv-supplied body |
| **9** | Unbounded read of a remote spec OOMs the process |
| **13** | History entries are not size-bounded on read, where the design requires entry-level failure |
| **19** | The canary gate has no case for the validation error path. Its `run --report junit` half is gone; the `call` exit-4 half remains |
| **20** | The canary suite never injects a credential through a profile `auth:` reference — one of the two credential sources DESIGN.md §5 names |
| **29** | `config.go:120` copies the config document and `Reset()`s the builder without zeroing, so the resolved token stays readable on the heap while `cleanupWith` scrubs the copy. The comment at `config.go:85` asserts the opposite, inside the one component §5a designates as the sole holder of resolved secrets |
| **30** | `internal/corpus` imports `internal/curl`. `e2e/boundary_test.go` does not guard it, so it passes today and drags the executor into the twin at Phase 6. Fix the import *and* the guard |

## Tier 2 — correctness, hangs and integrity

| Finding | What |
|---|---|
| **8** | `--output tsv` emits structurally invalid rows — unescaped cells split one row into several with differing column counts, so `cut -f3` silently returns garbage |
| **10, 14, 15, 17** | The hang set. A basic credential with no colon makes curl prompt on the TTY; SIGINT and SIGTERM are trapped for the process lifetime while three blocking calls ignore the context; the version preflight is uncancellable; the history lock has no deadline. 14 is the worst: Ctrl-C and `kill -TERM` both do nothing, only Ctrl-D or `kill -9` |
| **23, 24** | Both make the tool lie about history. 23 reports "not recorded" for a call that *was* recorded, so an operator re-runs a mutation. 24 lets `history replay <id>` re-issue a different request than `history show <id>` displayed |
| **25** | The spec cache is served forever. Policy is settled in DESIGN.md §4: 24h TTL, conditional revalidation with `ETag`/`Last-Modified`, `--refresh` to force |
| **31** | `history.go:48` uses `int64,omitempty` where `run.go` correctly used `*int64`, so a 0 ms call against a local service is indistinguishable from no response |
| **16** | Every recorded call reads and parses the whole history file twice under the exclusive lock. Lowest priority here — `run` was its pathological case and is gone. **Cut this first if the phase runs long** |

## One task with no finding number

Four findings (19, 29, 30, 31) share a shape: **a comment asserts a property the code does not
have.** `config.go:85` claims a buffer is zeroed; `boundary_test.go` claims to guard an import it
does not; `canary_test.go:427` says "whoever adds `--fail-on-error` adds the case" after it was
added.

Add a task that greps for this pattern beyond the four the review caught: comments claiming an
invariant, a guarantee, or a guard, checked against whether a test enforces it. Either write the
test or delete the claim. A finding-by-finding loop will not do this — there is no line number to
anchor on, so it needs its own task.

## Acceptance

Per finding: a test that fails on `main` and passes after the fix. For finding 1 specifically the
assertion must be **wire-level** — point `--base-url` at a capture listener and assert the
credential is absent from what the server received, and that `credentials_withheld` is present in
the envelope. An output-level assertion cannot catch this: redaction answers *does it print*, not
*who receives it*, which is why the existing canary suite passes today.

For the phase: `go build ./...`, `go test ./...`, and lint clean — necessary, not sufficient.

## Deliberately deferred

Not defects to fix here:

- **27** — `ErrWaitDelay` on a successful curl reported as "cannot run curl". Needs
  `--proxy socks5h://` to trigger.
- **28** — a `--timeout` over ~9.2e9 overflows into an expired context.
- **32** — the Swagger 2.0 body parameter is never exercised end to end. A coverage gap; the
  conversion was confirmed correct by hand. Belongs with phase 2c dogfooding, where real 2.0
  specs appear.

## What the review confirmed is sound — do not re-litigate

The `-q -K -` config-on-stdin mechanism with `-q` correctly first and no secret in argv;
`escapeDirective` matching curl's `unslashquote` exactly, including the longest-match `Replacer`
that avoids the double-escape bug; `--dry-run`'s emitted curl proven byte-identical to the
executed request by replaying it; `secret` resolving only inside `internal/curl`; profile mode
enforcement; history store 0700/0600; path params escaped and query credentials symbolic in every
display form.

**The architecture held. These are failures at its edges.**
