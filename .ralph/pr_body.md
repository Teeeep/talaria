## Summary

talaria is an OpenAPI-driven HTTP client built to be handed to an agent: it reads a spec,
tells the agent what operations exist, and executes calls **without ever letting the agent see
the credential**. Secrets stay symbolic (`<redacted:env:TALARIA_AUTH_BEARER>`) through every
surface an agent or a log can read — stdout, the copy-pasteable `curl`, dry runs, errors,
JUnit reports and the permanent history file — and are only resolved inside a `curl -q -K -`
config document written to curl's stdin, where they never appear in argv or the process table.

This branch is Phases 1–4 of the 8-phase roadmap in `docs/design/DESIGN.md` §7, stopping at the
deliberate decision gate before the twin. That is the complete differentiated core: spec
introspection, real execution behind the credential firewall, response validation, and a
CI-ready smoke-test runner.

## What changed

Greenfield: 63 commits, 130 files, ~29k insertions on an empty repo.

- **Spec layer** (`internal/spec`, `internal/operation`) — loads OpenAPI 3.0/3.1 via libopenapi
  and converts Swagger 2.0 at load time; local files and remote URLs with an on-disk cache;
  operationId synthesis and a lookup index.
- **Introspection commands** — `list`, `describe` (compact schema renderer), `search`, `uses`,
  so an agent can find an operation without being handed the raw spec.
- **The credential firewall** (`internal/secret`, `internal/curl`) — `SecretRef` keeps every
  credential symbolic in the `Request` type itself; curl is invoked as `curl -q -K -` with the
  resolved value only in the config document on stdin; request and response redaction covers
  headers, query strings and JSON bodies (including array-shaped and array-nested ones), with
  user-extensible `redact.headers` / `redact.body-paths`.
- **`internal/canary`** — the release gate DESIGN.md §5a mandates: it injects a canary secret
  and greps every output surface (json, pretty, tsv, dry-run, errors, JUnit, history, debug)
  for it, failing the build on any leak.
- **Execution** — `call` with `--dry-run`, mutation gating behind `--allow-mutations`, request
  bodies from flag/file/stdin, connect and total timeouts, signal handling that cancels curl
  and cleans up the raw response capture instead of orphaning both.
- **`auth`** — `auth check` and the exit-code-5 missing-credential contract.
- **History** (`internal/corpus`) — a redacted, append-only store behind `history list/show/replay`,
  with flock-serialised appends so a concurrent trim cannot destroy entries, and stable per-entry
  ids so `replay` cannot re-issue a different request than the one named.
- **Validation** (`internal/validate`) — response status/content-type/schema checking, the `call`
  `validation` block, `--fail-on-error` and exit code 4.
- **`run`** — smoke testing with tag/operation filters, an examples → fixtures → generated-data
  priority chain (`internal/gen`), and JSON/TSV/JUnit reports.
- **Output contract** (`internal/output`, `internal/clierr`) — one envelope rendered as
  json/pretty/tsv, and a structured error type behind the documented exit codes.
- **Docs and CI** — `AGENT.md` (the operating manual an agent reads, with tests that *execute*
  every example invocation it documents), a rewritten `README.md`, and a GitHub Actions workflow
  running build, lint and test.

## Design doc

`docs/design/DESIGN.md` (v0.3)

## Verification

- Test command: `go test ./...`
- Status: **passing.** Fresh `go test -count=1 ./...` run at PR time: all 15 packages ok, 0 failures.
- Also confirmed at PR time: `go build ./...` clean, `gofmt -l .` empty, `go vet ./...` clean.
- `go test -race -count=1 ./...`: clean, all 15 packages ok, no data races reported.

## Review

Three automated review cycles ran. Be aware of how the third one ended:

| Cycle | Result |
|---|---|
| 1 | 20 findings, 11 CRIT — all 11 fixed (commits `feb60d8`…`4cde768`), each with a regression test |
| 2 | 35 findings, 9 CRIT — all 9 fixed (commits `b26ec2b`…`d26de3b`), each with a regression test |
| 3 | Aborted. The reviewer hit a non-retryable error mid-run, with its subagents still reading files. It never wrote a findings file, and the loop read the absent file as "0 findings, clean pass" |

**So no CRIT findings are outstanding, but the cycle-2 fixes were never independently
re-reviewed.** Nine of them touch the credential firewall and the curl execution path. If you
review one thing by hand, review those nine commits.

### Outstanding non-blocking findings

Carried from cycle 2's `REVIEW_FINDINGS.md` (in the diff, findings 10–35). None were actioned —
the fix plan deliberately scoped to CRIT only.

**WARN (16)**

- `internal/curl/config.go:132` — no `--globoff`; a glob in a spec-supplied URL path turns one authenticated call into N.
- `internal/corpus/entry.go:232` — recorded *request* bodies redact only the three RFC 6749 response token fields, so `client_secret`/`password` in a body persist.
- `internal/secret/redact.go:19` — the built-in sensitive-name list misses common spellings, including `Authentication`.
- `internal/curl/exec.go:265` — `scrubber` misses name-matched sensitive literals, so curl's stderr is scrubbed only for structural secrets.
- `internal/curl/exec.go:257` — conversely, `scrubber` corrupts curl's error text when the credential is a short or common string.
- `internal/curl/version.go:42` — the timeout fix does not cover the preflight; `curl --version` can hang talaria forever.
- `internal/corpus/store.go:265` — `replace` has no fsync before the rename, so a crash mid-trim can destroy the whole history file.
- `.github/workflows/ci.yml:38` — CI never runs `-race`, so the concurrency tests guarding the corpus fix cannot catch a regression.
- `cmd/talaria/run.go:553` — `--report junit` drops the per-field validation errors `internal/validate` computed.
- `cmd/talaria/call.go:390` — response redaction runs before response validation, producing spurious validation failures and exit 4.
- `internal/corpus/entry.go:24` — `internal/corpus` depends on `internal/curl`, which will block the Phase 6 twin from using the store.
- `cmd/talaria/history.go:209` — `history replay` issues query-string credentials without the §5a warning.
- `cmd/talaria/run.go:336` — garbled missing-credential message: "no credential for security 1 scheme bearerAuth".
- `internal/spec/source.go:86` — the remote-spec cache never expires and has no refresh escape hatch.
- `internal/curl/render.go:114` — the emitted curl and `request.body` are lossy for non-UTF-8 request bodies.
- `internal/canary/canary_test.go:423` — the canary suite still omits the response-validation error path, on a premise that is now stale.

**INFO (10)**

- `internal/corpus/lock_other.go:11` — locking is silently disabled on non-unix, reintroducing the trim data-loss bug there.
- `internal/curl/exec.go:40` — curl inherits talaria's whole environment, contradicting the comment saying it does not.
- `cmd/talaria/run.go:539` — `run --report tsv` appends a prose summary line to otherwise machine-readable output.
- `README.md:6` — the status line claims a scaffolded command tree that does not exist.
- `docs/design/DESIGN.md:169` — calls `search` "fuzzy find"; it is case-insensitive substring matching.
- `internal/spec/load.go:1` — an unrecognised `openapi:` major version loads as an empty spec and exits 0.
- `internal/validate/testdata/strict-3.0.yaml:40` — `exclusiveMaximum` is in the fixture but never asserted.
- `cmd/talaria/run.go:574` — JUnit reports 0.000s for the operation that consumed the entire run.
- `internal/corpus/entry.go:71` — repeated request headers are comma-folded through the corpus, so replay sends one header where the original sent two.
- `docs/design/DESIGN.md:248` — the exit-4 row is ambiguous about whether bare `run` should exit 4.

## Notes for the reviewer

- **Scope stops at the Phase 4 gate on purpose.** `internal/twin`, the recording proxy and
  `talaria twin …` (Phases 5–8) are not here. DESIGN.md §7 says not to start them on faith, so
  the loop did not.
- **Loop artifacts are committed to the repo:** `IMPLEMENTATION_PLAN.md`, `tasks.json`,
  `REVIEW_FINDINGS.md` and `.ralph/stack.json`. They are the audit trail for this branch, not
  product files. `IMPLEMENTATION_PLAN.md` currently holds the *cycle-2 review fix plan*, not the
  original 33-task build plan, which it replaced. Say the word and they can be dropped.
- **`docs/design/DESIGN.md` was edited on this branch** (15 lines) — the repositioning commit
  `d1eecd0`, which predates the build.
- **The credential firewall is the product, not a hardening pass.** Six of cycle 2's nine CRIT
  findings were leaks that the canary suite passed straight through. Each fix ends with a new
  canary case rather than only a unit assertion, but that history is the reason to treat
  `internal/canary` as the file worth reading closely.
- **`golangci-lint` is not installed here**, so lint is stdlib-only: `gofmt -l` + `go vet`. CI
  runs the same. Worth adding before this grows further.

---
🤖 Generated with [Claude Code](https://claude.com/claude-code)
