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

Within a package, a file is one concern. The splits that exist, and what belongs in each:
`request/build.go` is the binder, `request/server.go` everything that answers *where does this
go*, `request/wire.go` the charset rules for what may go on the wire, `request/hosts.go` the
allowed host set; `curl/config.go` builds the document, `curl/firewall.go` is the half that
resolves, re-checks and zeroes; `cmd/talaria/history.go` is the list/show views and the store
plumbing, `history_replay.go` the replay command. Tests split the same way, with the same names.

## House rules

**Package boundaries.** `operation` and `validate` may not import `curl`, `corpus` or `twin` — they are shared with the future twin server, which uses `net/http` directly. `corpus`
may not import `curl` either: it takes a local observation struct, not a `*curl.Response`.
`internal/e2e/boundary_test.go` asserts this against the real import graph; if you add a package
that must not cross a boundary, add it there in the same commit.

**Secrets are `SecretRef` everywhere except inside `internal/curl` at exec time.** If you are
about to put a resolved credential in a field typed `string`, stop — that is the bug class the
design exists to prevent. `SecretRef` cannot print itself; keep it that way, so leaking requires
a deliberate, greppable call.

**A server URL is a template.** `servers[].url` carries `{name}` spans filled from
`servers[].variables`, and §5a defines the allowed host set as the servers *after*
substitution. `internal/request/server.go` owns it — `ServerURLs(doc)` for the whole set,
`firstServer(doc)` for the base URL. Nothing else reads `doc.Model.Servers[i].URL`, because a
raw one names the host `{region}.api.example.com`. A variable's value may not hold a character that could move
the URL's authority (`/?#@:[]\{}`, space, control), and a server that fails to substitute is
left out of the result: the host set may be narrower than the spec, never wider.

**There is one answer to "where would this call go".** `request.Destination(Inputs)` is
DESIGN.md §4's precedence — `--base-url`, then the profile, then the spec's first server — and
`binder.baseURL` reads the same `baseURLCandidates` list, differing only in that it validates
and reports. `auth check` asks `Destination` rather than re-deriving the order; a pre-flight
that names a different host than the call reaches is worse than no pre-flight.
`TestDestinationAgreesWithTheBaseURLBuildChooses` is what holds them together.

**Credentials bind to hosts.** A resolved credential goes only to a host the spec declares or a
human explicitly allowed. Redaction answers *does it print*; it does not answer *who receives
it*. Both questions need an answer for every new path that carries a credential.
The set itself is `request.HostSet` (`internal/request/hosts.go`):
`NewHostSet(specURLs, allowFlags, profileHosts)` unions `request.ServerURLs(doc)`, `--allow-host`
(persistent, repeatable, on the root) and the profile's `allow_hosts`. Ask it `Allows(rawURL)`;
`Key(rawURL)` is the `host:port` form `credentials_withheld[].host` prints. A malformed *spec*
server contributes nothing, silently; a malformed *human* entry is exit 2, because a dropped one
would read as allowed. There is no wildcard and the empty set allows nothing — never add an
"empty means allow everything" shortcut.

Enforcement is `request.Inputs.Hosts` (a `HostSet`): `binder.credentials` asks `Allows(BaseURL)`
once and, on a no, appends `request.Withheld{Scheme, Reason, Host}` to `req.Withheld` instead of
attaching the credential. The zero `HostSet` withholds everything, so forgetting to build one
fails closed. `cmd/talaria/hosts.go` is the one place the set is assembled — `allowedHosts(cmd,
doc, prof)` — and the one place the stderr line is written — `warnWithheld`. Every command that
resolves a credential calls both; do not build a `HostSet` anywhere else, or `call`, `auth check`
and `history replay` will start disagreeing about where a credential may go.

**An unsupported security scheme is reported, never hidden.** `config.Credential.Supported` is
false for a scheme outside v1's set — `oauth2`, `openIdConnect`, `mutualTLS`, an `apiKey` in a
place a request does not have — and its `Ref` then points at `TALARIA_AUTH_BEARER`: the only
thing that can satisfy it is a token the caller brought (§5 Auth). `config.Schemes` returns every
declared scheme, so `auth check` reports `{"scheme":…,"supported":false,"present":…}` — the
`supported` field is a `*bool` on `cmd/talaria`'s `authScheme` because it must appear precisely
when it is false, and an unsupported entry carries no `source`. `Covers` reads
"declared, unsupported and absent" as `Unsupported`, which is why `call --dry-run` exits 5 on it
rather than previewing a request nothing could authenticate. A name missing from `byName` now
means only *the document never declared it* — exit 2, since no variable will fix a broken spec.
Never re-add a supportedness filter to `Schemes` or `declaredCredentials`: `credentialFor`'s
apiKey branch is guarded, but the report is what an agent acts on, and an omitted scheme reads
as "nothing to do here".

**A media type is a header value, so it is checked like one.** The key of the spec's `content:`
map becomes `Content-Type:` on the wire, and it was the one header value that reached the socket
without passing a CRLF check. Two gates now, deliberately: `binder.contentType`
(`internal/request/body.go`) refuses anything that is not `type/subtype` with optional
`; parameter=value` — `isMediaType` in `internal/request/wire.go`, beside the other charset
rules, a whole-grammar check because the token charset rules out CR,
LF, NUL, whitespace and the colon in one pass — and `curl.bodyContentType` (`internal/curl/render.go`)
re-checks with `checkSplit` as the last gate, because `history replay` builds a `Request` with no
binder. `bodyContentType` is the single seam both surfaces read: `document.body` returns its error,
`bodyArgs` drops the directive, so the executed document and the printed reproduction cannot
disagree. Do not re-inline `req.Body.ContentType` at either call site.

**`auth check` and `call` may not disagree.** DESIGN.md:329 is a contract, not a nicety: the
verdict lives in `internal/config` — `Covers`, `Resolve` and `Unsatisfied(ops, creds)`, which is
the whole of `auth check`'s exit code — and `cmd/talaria/auth.go` only calls it. Never re-derive
"can this spec be called" in the command layer; that is how the two drifted before. Any change
to one side needs the matrix test in `cmd/talaria/auth_test.go`
(`TestAuthCheckAndCallAgreeOnUnsupportedSchemes`) and the two-process one in `internal/e2e` to
still pass — they compare the two commands' exit codes cell by cell.

**`call` withholds and runs; `history replay` refuses.** Same host set, deliberately different
outcomes (§5a). An off-set `--base-url` is a human pointing at a twin, so the call still exits 0
with `credentials_withheld` in the envelope. An off-set *stored* host is a line in a file asking
to be sent somewhere, so replay is exit 2. Do not unify them.

**The spec is untrusted input, and so is the history file.** Both are fetched or edited outside
this process. Bound every read, size-check before allocating, and treat any spec-derived string
that reaches the wire as hostile until checked — a media type became a header-injection vector
exactly this way. Failures must be entry-level or request-level, never process-level.

**Reading a stored entry back belongs to `internal/corpus`.** `Entry.Replay(op)` returns a
`Replayable` — the `name=value` strings `request.Inputs` takes — and it is where the path-template
match, the redaction-marker drops and the body refusals live, so they are testable without cobra.
`history replay` only wires: spec → `index.Lookup` → `Entry.Replay` → `config.Resolve` →
`request.Build`. Two rules the shape depends on: a stored value whose name the operation declares
as a parameter goes back through `Inputs.Params`, not `Query`/`Headers`, so a *required* one is
bound rather than reported missing; and a decoded body is handed over as `Inputs.Body = {"-"}`
with `Inputs.Stdin` set, never as an argv-style literal, because a stored body starting with `@`
or `-` would otherwise become a file read chosen by a line in a JSONL file.
`internal/corpus` may not import `internal/config`: the store has to stay usable by the twin,
which has no profiles. That is why `buildReplay` stays in `cmd/talaria/history_replay.go` —
it needs the profile and the flags — and why the parameter locations both packages name live
in `internal/operation` (`operation.InPath/InQuery/InHeader/InCookie`), the one package
`config`, `request` and `corpus` may all import. Alias them; do not re-spell them.

**`cmd/talaria` is wiring.** Parse flags, call a package, render the result. Decisions,
transformations and multi-step workflows belong in a package that can be tested without cobra.
The command layer holds 29% of production code (2,368 of 8,023 non-comment, non-blank lines
outside the test-only packages) and 12 view structs; it is the largest single component. Do not
add to it — a new view struct belongs beside its siblings, not in a new command file.

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
