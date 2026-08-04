# talaria — working conventions

An OpenAPI CLI an agent can drive without ever seeing your credentials.

**This file is how the codebase is written. It is not what to build** — that is
`docs/design/DESIGN.md` (currently v0.5), the source of truth for scope and behaviour, and
`docs/plans/` for what comes next — `2026-08-02-phase-2-boundary-design.md` is the rationale,
and the phase docs beside it (`phase-2a-remediation`, `phase-2b-boundary`,
`phase-2c-distribution`) are the work, one autonomous run each. Record patterns here as
you establish them; the next session starts with no memory of this one.

## Commands

| Purpose | Command |
|---|---|
| All tests | `go test ./...` |
| One package | `go test ./<dir>/...` |
| Build | `go build ./...` |
| Lint | `test -z "$(gofmt -l .)" && go vet ./... && golangci-lint run ./...` |

`golangci-lint` v2.12.2 is installed and `.golangci.yml` selects the linter set — every
exclusion in it records why. Run lint before every commit. These four commands are the ones
in `.ralph/stack.json` and `.github/workflows/ci.yml`; `internal/ci/workflow_test.go` fails
if those two disagree, so change them together.

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
internal/replay/      re-derives a history entry into a request, through the spec
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
human explicitly allowed. `spec.Servers` (`internal/spec/servers.go`) is the only reader of
`doc.Model.Servers`: it substitutes `{variable}` placeholders from their defaults and omits any
server it cannot fully substitute, so "the hosts the spec declares" means its output, never the
raw URLs. Read the spec's servers through it or the host set is computed from templates. Redaction answers *does it print*; it does not answer *who receives
it*. Both questions need an answer for every new path that carries a credential.

`internal/request/hosts.go` is the single implementation: `request.AllowedHosts(doc, prof,
allowHosts)` builds the set (spec servers ∪ `--allow-host` ∪ a profile's `allow_hosts:`),
`HostSet.Allows(url)` is the only comparison, and `request.Host(url)` is its canonical form for
messages and envelope fields. Never compare hosts by hand — case, a default port and a trailing
root dot are all the same host, and a suffix match admits `api.example.com.attacker.com`. Where a
request goes is `request.ResolveBaseURL` (or `request.Target`, which reads "no base URL" as an
answer rather than a failure); every command that reports on or sends to a host reads both through
`hostFlags(cmd)` in `cmd/talaria/call.go`, so `auth check` cannot drift from what `call` does. An
off-spec host **withholds** rather than refuses: the credential is left off, `Request.Withheld`
carries the fact into the envelope as `credentials_withheld`, and one line goes to stderr. Both
surfaces, every time.

**Every scheme a spec declares yields a `config.Credential`**, including one whose type talaria
cannot resolve. `Supported` marks the difference; an unsupported scheme points at
`TALARIA_AUTH_BEARER`, so a token the human obtained satisfies it and `auth check` reports it
instead of hiding it (DESIGN.md §5). `config.Covers` therefore reads "absent from the credential
map" as "the document never declared this scheme" — a broken spec, exit 2 — while "declared but
unset" is exit 5. `Schemes` and `Resolve` must keep building that map the same way or the
pre-flight and the call disagree; `TestResolveAgreesWithTheCoverageAuthCheckReports` is the guard.

A history entry is data, never instruction. `history replay` re-derives through the spec —
operation, params and body from the entry; target host and credentials from the spec, the flags
and the environment. Nothing stored is resolved, and a stored credential position is dropped, not
read. New code that reads a history entry inherits this rule.

**The spec is untrusted input, and so is the history file.** Both are fetched or edited outside
this process. Bound every read, size-check before allocating, and treat any spec-derived string
that reaches the wire as hostile until checked — a media type became a header-injection vector
exactly this way. Failures must be entry-level or request-level, never process-level.

**A header value gets checked on every surface it can reach, not just the one you are editing.**
The media type is the worked example: `isMediaType` (`internal/request/body.go`) validates the
spec's `content:` key at bind time, `checkSplit` re-checks `Body.ContentType` in
`internal/curl/config.go` because `history replay` sets that field from the stored entry and never
goes through `request.Build`, and `curl.Render` returns `""` rather than emitting a command with
the offending word quietly dropped — `--dry-run` reaches `Render` without ever building a config
document. Three surfaces, one value. A new field that reaches the wire needs the same sweep: bind
time, the config document, and the emitted curl.

**The request body is a credential position, and it points inward.** Every other secret in this
tool travels agent → talaria → wire and is a `SecretRef`; a body travels human/CI → talaria →
*agent*, as plain bytes nothing upstream made symbolic. Two rules follow, and each one alone still
leaks. `callPayload` (`cmd/talaria/call.go`) redacts `request.body` through the same
`*secret.ResponseRedactor` the response and the history entry use — pass the one already built,
never construct a second. And `curl.Render` *references* a body it was not shown: `request.Body`
carries a `BodySource` (`internal/request/request.go`) that the binder sets, and a `@file` or
`stdin` body renders as `--data @path` / `--data @-` while an argv body stays inlined
(DESIGN.md §3.4). `request.curl` is not redacted by design, so inlining a file body would leak it
past a perfectly good `request.body`. `internal/canary`'s file-body case is the gate on both.

**`cmd/talaria` is wiring.** Parse flags, call a package, render the result. Decisions,
transformations and multi-step workflows belong in a package that can be tested without cobra.
The command layer holds 26% of production code (1,525 of 5,896 non-comment lines) and 12 view
structs; it is still the largest single component. Do not add to it — a new view struct belongs
beside its siblings, not in a new command file. `history replay` is the worked example of the
rule going the other way: it was a struct and eight helpers inside `history.go`, and it is now
`internal/replay`, which takes an `Inputs` and returns a `*request.Request` — same shape as
`request.Build`, and testable without a command tree.

**One `invocation` per RunE.** `newInvocation(cmd)` (`cmd/talaria/root.go`) reads the config
file, the `--profile` it names, the redaction firewalls that profile configures, and
`--base-url`/`--allow-host`, once. Every request-making command builds one at the top of its
RunE and passes it down; `inv.history()` opens the store from it. Do not re-read any of those
four things further down a command — a single `call` used to select the profile three times and
construct two independent `corpus.Redactors` from the same settings, and nothing but a shared
constructor kept the two in step. `selectProfile` and `flagString` live beside it and are the
only readers of those flags.

**Errors go through `internal/clierr`.** Exit codes are a published contract that agents branch
on. A new failure mode maps to an existing code or the design doc changes — never both silently.
Warnings go through the same package: `clierr.Warnf(stderr, format, ...)` owns the `warning: `
prefix and the trailing newline, so a warning is never a bare `fmt.Fprintf`. A warning is
something the caller should know that deliberately does *not* change the exit code — the
call that succeeded but was not recorded, the credential withheld from an off-spec host, the
response that could not be validated, the replay field that was dropped.

**One name for one thing.** `operation.Operation.Name()` is how an operation is referred to in
any message (its `operationId`, else method and path); `request.Host`/`request.AllowedHosts` are
how a host is named and compared. Both had three near-copies before Task 6 drained them. Before
writing a small helper that renders or compares a domain value, grep for it — it exists.
Likewise reach for the standard library first: `slices.Sorted(maps.Keys(m))` replaced a
hand-rolled `sortedKeys`.

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
