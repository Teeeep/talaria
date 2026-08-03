# talaria — working conventions

An OpenAPI CLI an agent can drive without ever seeing your credentials.

**This file is how the codebase is written. It is not what to build** — that is
`docs/design/DESIGN.md` (currently v0.5), the source of truth for scope and behaviour, and
`docs/plans/2026-08-02-phase-2-boundary-design.md` for what comes next. Record patterns here as
you establish them; the next session starts with no memory of this one.

## Commands

| Purpose | Command |
|---|---|
| All tests | `go test ./...` |
| One package | `go test ./<dir>/...` |
| Build | `go build ./...` |
| Lint | `test -z "$(gofmt -l .)" && go vet ./...` |

`golangci-lint` is not installed. Run lint before every commit.

**A green suite is not evidence of correctness here.** `go test ./...` passed with 32 review
findings live, 8 of them critical, including credential exfiltration. If you are about to claim
something works because tests pass, you need a test that fails without your change.

## Layout

```
cmd/talaria/      cobra wiring
internal/spec/        load (file/URL/cache), v2→v3 convert
internal/operation/   THE core model — id, method, path, params, schemas, auth
internal/secret/      SecretRef, redaction — the trust boundary
internal/request/     the Request type; secret fields are structurally refs
internal/curl/        config-document builder + executor (os/exec)
internal/validate/    response vs schema
internal/corpus/      history store
internal/output/      json / pretty / tsv renderers, versioned envelope
internal/config/      profiles, env vars, auth mapping
internal/clierr/      exit-code contract
```

`internal/canary`, `internal/e2e`, `internal/ci` are test-only.

## House rules

**Package boundaries.** `operation` and `validate` may not import `curl`, `corpus` or `twin` — they are shared with the future twin server, which uses `net/http` directly. `corpus`
may not import `curl` either: it takes a local observation struct, not a `*curl.Response`.
`internal/e2e/boundary_test.go` asserts this against the real import graph; if you add a package
that must not cross a boundary, add it there in the same commit.

**Secrets are `SecretRef` everywhere except inside `internal/curl` at exec time.** If you are
about to put a resolved credential in a field typed `string`, stop — that is the bug class the
design exists to prevent. `SecretRef` cannot print itself; keep it that way, so leaking requires
a deliberate, greppable call.

**Credentials bind to hosts.** A resolved credential goes only to a host the spec declares or a
human explicitly allowed. Redaction answers *does it print*; it does not answer *who receives
it*. Both questions need an answer for every new path that carries a credential.

**The spec is untrusted input, and so is the history file.** Both are fetched or edited outside
this process. Bound every read, size-check before allocating, and treat any spec-derived string
that reaches the wire as hostile until checked — a media type became a header-injection vector
exactly this way. Failures must be entry-level or request-level, never process-level.

**`cmd/talaria` is wiring.** Parse flags, call a package, render the result. Decisions,
transformations and multi-step workflows belong in a package that can be tested without cobra.
The command layer holds 28% of production code (1,589 of 5,511 non-comment lines) and 12 view
structs; it is the largest single component. Do not add to it — a new view struct belongs beside
its siblings, not in a new command file.

**Errors go through `internal/clierr`.** Exit codes are a published contract that agents branch
on. A new failure mode maps to an existing code or the design doc changes — never both silently.

**Never write a comment asserting a property no test enforces.** "This buffer is zeroed",
"callers must hold the lock", "validated upstream". Either add the test or drop the claim. Four
review findings were comments that documented an intention as if it were an invariant.

## Tests

Colocated (`internal/spec/load_test.go`), fixtures in `testdata/` beside the package.

Every behaviour needs the happy path **and** the hostile one: malformed, oversized, cyclic,
attacker-controlled, or crossing a trust boundary. The existing suite is 2.4× the production
code and caught none of the review's findings because it only ever asserted what the feature
should do.

## Scope note

`talaria run`, `internal/gen` and the JUnit report were removed on 2026-08-03 — see
`docs/plans/2026-08-02-phase-2-boundary-design.md` §2.1. Do not reintroduce spec-driven smoke
testing without a design-doc change; it is the one capability deliberately ceded to Schemathesis
and Hurl.
