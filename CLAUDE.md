# talaria — working conventions

An OpenAPI CLI an agent can drive without ever seeing your credentials.

**This file is how the codebase is written. It is not what to build** — that is
`docs/design/DESIGN.md` (currently v0.6), the source of truth for scope and behaviour, and
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
resolves, re-checks and zeroes; `corpus/store.go` is the `Store` API and id assignment,
`corpus/file.go` the file mechanics under it — find, bounded read, append, trim, replace —
and `corpus/lock_unix.go` the append lock with the two ways out of waiting for it;
`cmd/talaria/call.go` is the `call` command with its binder wiring and view structs,
`record.go` the recording plumbing `call` and `history replay` share, `history.go` the
list/show views and the store plumbing, `history_replay.go` the replay command. Tests split
the same way, with the same names: `curl/firewall_test.go` holds the resolve, CRLF-refusal and
zeroing cases, `config_test.go` the ones whose subject is the shape of the document.

## House rules

**Package boundaries.** `operation` and `validate` may not import `curl`, `corpus` or `twin` — they are shared with the future twin server, which uses `net/http` directly. `corpus`
may not import `curl` either: `NewEntry` takes `corpus.Observed{Status, Headers, Body, TimingMS}`,
not a `*curl.Response`. The one place the two types meet is `observed()` in `cmd/talaria/call.go`,
called from `recordCall` so every command that records goes through it — do not translate at a
call site. `internal/e2e/boundary_test.go` asserts all of this against the real import graph: its
`boundaries` table is one entry per constrained package, and a package with no entry is not
checked, which is how `corpus → curl` survived being written down. If you add a package that must
not cross a boundary, give it its own entry there in the same commit.

**Secrets are `SecretRef` everywhere except inside `internal/curl` at exec time.** If you are
about to put a resolved credential in a field typed `string`, stop — that is the bug class the
design exists to prevent. `SecretRef` cannot print itself; keep it that way, so leaking requires
a deliberate, greppable call.

**A buffer holding a resolved credential is one this package can address.** `document.b`
(`internal/curl/config.go`) is a `[]byte` the document owns, appended to by `directive`/`flag`,
and never a `strings.Builder`: `Builder.Reset` drops the array without touching the bytes and
`Builder.String` aliases it into an immutable string that can never be zeroed, so the old code
scrubbed a copy while the original stayed readable. `discard()` clears the whole *capacity* —
the bytes past `len` are what the last append walked over — and it runs on both paths:
`buildDocument` calls it immediately after copying out, and `cleanupWith` calls it again plus
`clear(config)`, so cleanup is idempotent and non-nil even when the build fails. The seam the
tests use is `buildDocument`, which hands back the `*document` so `owned(doc)` can assert on the
builder's own array rather than the copy — assert only on the returned `config` and the defect
passes.

Owning the buffer is not enough, because `append` does not keep it. Every write goes through
`write` → `grow`, which reallocates *and clears the array it abandons* before dropping the last
reference to it: plain `append` left five readable copies of the document prefix on a minimal
request, credential included, in memory `discard()` could no longer reach. `owned(doc)` is blind
to exactly that, so `TestBuildZeroesTheArrayItAbandonsWhenTheBufferGrows` seeds a `document`
with a capacity ending where the credential's directive ends — the credential lands in an array
the *test* holds, and the directives after it force the abandonment. Its precondition is
positional rather than a read of the array, since a correct `grow` has already cleared it.
`document.cookies` joins into `d.b` a piece at a time for the same reason, and `directive`
writes its four pieces separately rather than concatenating: a concatenation, and the copy
`escapeDirective` makes when it must, are strings nothing can zero. `configEscape`'s olds are
all single bytes so the replacer returns its argument untouched when there is nothing to escape
— `TestEscapingAValueNeedingNoEscapeDoesNotCopyIt` is what keeps that true.

Never introduce another accumulator for credential-bearing text without both properties. And
scope the claim when you write it down: what is scrubbed is the arrays this package allocated
plus the copy it returns. `os.Getenv`'s own string is immutable and outlives the call, and
curl's stdin is downstream of the return — neither is reachable, and a comment implying
otherwise is the unenforced-invariant class the house rules forbid.

**A server URL is a template.** `servers[].url` carries `{name}` spans filled from
`servers[].variables`, and §5a defines the allowed host set as the servers *after*
substitution. `internal/request/server.go` owns it — `ServerURLs(doc)` for the whole set,
`firstServer(doc)` for the base URL. Nothing else reads `doc.Model.Servers[i].URL`, because a
raw one names the host `{region}.api.example.com`. A variable's value may not hold a character that could move
the URL's authority (`/?#@:[]\{}`, space, control), and a server that fails to substitute is
left out of the result: the host set may be narrower than the spec, never wider.

**There is one answer to "where would this call go", and the path is half of it.**
`request.Destination(Inputs)` is DESIGN.md §4's precedence — `--base-url`, then the profile, then
the spec's first server — *joined to `in.Op.Path`*, and `binder.baseURL` reads the same
`baseURLCandidates` list, differing only in that it validates and reports. The join is textual
(`Request.URL` is `BaseURL + Path`), so the path decides the host as much as the base does: a
`paths:` key beginning `@` makes an allowed server the userinfo of a request to somewhere else.
A destination computed without it names a host the call never reaches.
`auth check` asks `request.Withholds(in, ops)` — `Destination` per operation against the host
set — rather than re-deriving anything, because the base URL is one string for the whole spec
and the path is not. A pre-flight that names a different host than the call reaches is worse
than no pre-flight. `TestDestinationAgreesWithTheBaseURLBuildChooses` compares `Destination`
against `BaseURL + Path`, and `TestAuthCheckReportsACredentialWithheldByAHostilePathKey` is the
two-command half.

**An operation's path is a template, and it is spec-controlled text on the wire.**
`isPathTemplate` (`internal/request/wire.go`) requires a leading `/` and no space or control
character, and `binder.path` refuses anything else with exit 2 before substituting a single
parameter. The leading slash is the whole rule: past it the authority is fixed, so `/pets@archive`
stays a legal path while `@evil.example.com/steal` and `.evil.example.com/steal` are refused
where they are read. This is the same argument `authorityChars` (`internal/request/server.go`)
makes for a server variable, and it is deliberately *two* gates with `binder.credentials` —
the refusal turns a hostile document into an exit code, and the host check closes the class
wherever a path comes from. Do not drop either half.

**Credentials bind to hosts.** A resolved credential goes only to a host the spec declares or a
human explicitly allowed. Redaction answers *does it print*; it does not answer *who receives
it*. Both questions need an answer for every new path that carries a credential.
The set itself is `request.HostSet` (`internal/request/hosts.go`):
`NewHostSet(specURLs, allowFlags, profileHosts, profileBaseURL)` unions §5a's four sources —
`request.ServerURLs(doc)`, `--allow-host` (persistent, repeatable, on the root), the profile's
`allow_hosts`, and the *selected* profile's own `base-url`. Ask it `Allows(rawURL)`;
`Key(rawURL)` is the `host:port` form `credentials_withheld[].host` prints. A malformed *spec*
server contributes nothing, silently; a malformed *human* entry is exit 2, because a dropped one
would read as allowed. There is no wildcard and the empty set allows nothing — never add an
"empty means allow everything" shortcut.

Source 4 is a URL where the other human sources are `host[:port]` entries, so `allowBase` reads
it with `splitHost` — the same reading `Allows` gives a destination, which is what makes it mean
"the host a call under this profile would reach" and not an approximation of it. Only the host
survives: a `base-url` with userinfo contributes its host, and `binder.absoluteBase` is still
what refuses the userinfo, with the message naming `TALARIA_AUTH_BASIC`. It is the profile the
caller *selected* — an unselected one in the same file allows nothing — and `--base-url` stays
outside the set, with a profile active or without, because a flag is a per-invocation
redirection and the twin case is exactly why withholding is right there.
`cmd/talaria/hosts_test.go` is that boundary, one test per edge.

Enforcement is `request.Inputs.Hosts` (a `HostSet`): `binder.credentials` asks
`Allows(req.BaseURL + req.Path)` once — the string the executor will use, never `BaseURL` alone —
and, on a no, appends `request.Withheld{Scheme, Reason, Host}` to `req.Withheld` instead of
attaching the credential. `Key` is asked about the same string, so the host it prints is the one
that would have received the credential. The zero `HostSet` withholds everything, so forgetting
to build one fails closed. `cmd/talaria/hosts.go` is the one place the set is assembled — `allowedHosts(cmd,
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

**A request body is redacted where it is displayed, and referenced where it was not typed.**
`request.Body` carries its origin — `BodyArgv` (the zero value), `BodyFile` with `Path`, or
`BodyStdin` — set by `binder.bodyData` from which branch of `--body` ran. Two consequences, and
neither changes a byte on the wire. `curl.bodyDirective` (`internal/curl/render.go`) inlines
only `BodyArgv` as `--data-raw`; the other two become `--data-binary @path` / `--data-binary @-`,
because a body a human or a CI job wrote is not a body the agent reading stdout already has
(§3 principle 0). `--data-binary`, not `--data`: `--data` strips newlines out of a file, and
`TestThePreviewedCommandSendsWhatTheCallSends` fails on a pretty-printed body file if you change
it. And `callPayload` builds *every* display field from `displayRequest(req, red)` — a copy of
the request whose `Body.Data` has been through `redactors.Response.Body`, the same list history
uses — because the body is raw `[]byte` and never becomes a `request.Value`, so it is the one
display field no `Value` method protects. One copy rather than a redaction per field, because
the body feeds two surfaces — `request.body` and the `--data-raw` of `request.curl` — and
redacting at each separately is how they drifted: the JSON field printed `<redacted>` while the
curl line beside it carried the live token, in the field §5a promises is *"useless to
exfiltrate"*. It is a copy, not a mutation, because `req` is what the executor sends and what
`recordCall` stores. `assertCurlInlinesTheShownBody` (`cmd/talaria/call_redact_test.go`) is the
agreement between the two. Never assign `string(req.Body.Data)` to a field a caller reads, and
never hand `curl.Render` the unredacted `req`.

**A table cell is escaped by the renderer, not by the command that builds it.** Every cell in
`output.Table` is untrusted — a spec summary or description routinely contains real newlines,
and a `history show` cell is a recorded response body, which is raw bytes. `escapeCell`
(`internal/output/cell.go`) is applied by *both* the TSV and pretty renderers, on rows and
headers, so one row in is one line out with a fixed column count: a raw `\n` used to split a
TSV row in two and `cut -f3` returned garbage with no way to detect it, and `text/tabwriter`
reads an embedded tab as a cell terminator and re-partitions the whole column block. It escapes
rather than strips (`\t`, `\r`, `\n`, `\\`, `\xNN` for other C0 controls, DEL and invalid UTF-8)
because a consumer has to be able to tell a tab inside a value from a column break. Never escape
at a call site — the nine commands that build tables would drift — and never hand a renderer
pre-escaped text. A command that *sizes* a column measures `output.CellWidth` and cuts with
`output.TruncateCell`, never a rune count: escaping happens after `fitSummaries`
(`cmd/talaria/list.go`) runs, so a summary of tabs doubles in width on the way out and overflows
the line the budget was meant to bound.

**One redaction firewall per invocation.** `newRedactors(cfg)` (`cmd/talaria/record.go`, beside
`recordCall` and `observed` — the recording plumbing `call` and `history replay` share) is called
exactly once, in the command's `RunE`, and the `corpus.Redactors` it returns is threaded from
there into all three surfaces that redact: the binder (`buildRequest`/`buildReplay` take it as
`red` and pass `red.Request` to `request.Inputs.Redactor`), the view (`callPayload`'s
`displayRequest`), and the store (`recordCall`). Both commands used to build it twice — once for the
binder, once for history — so a change making one surface's list configurable would have applied
to only one of them, silently. Never call `newRedactors` below `RunE`; pass the value down.

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

**Every wait has two ways out, and the second one is not optional.** `signalContext()`
(`cmd/talaria/root.go`) is `signal.NotifyContext` plus a goroutine that calls `stop()` on the
first cancellation, because `NotifyContext` leaves its registration installed for the rest of
the process's life: without the `stop()` every later SIGINT is delivered to a channel nobody
reads, so `kill -9` becomes the only way out of anything the context does not reach. Never
replace it with a bare `NotifyContext`. The first way out is the context itself, and it has to
arrive wherever this process waits on something it does not control — `request.Inputs.Ctx`
carries it to `stdinBody` (`internal/request/body.go`), which reads in a goroutine and selects
against `Done()` because an `io.ReadAll` already in progress cannot be interrupted; `internal/curl`
gets it through cobra for `exec.CommandContext`. A cancellation is reported with
`binder.stop` → `clierr.RequestFailed` (exit 1), never `binder.fail` (exit 2): the caller's
command line was fine, and it is the same interruption `curl.ExecuteWith` already exits 1 on.
`b.fatal` beats the collected problems for that reason — half-bound inputs produce
missing-parameter complaints that are artefacts of the interruption. The tests are
`cmd/talaria/root_test.go`, which re-execs the test binary (`TALARIA_TEST_SIGNAL_CHILD`, branched
in `TestMain` before any fixture exists) because the assertion is that a signal kills the
process, and this process is the suite.

**The history lock is a wait, so it has both of them too.** `lock(ctx, path)`
(`internal/corpus/lock_unix.go`) took `LOCK_EX` with no deadline and no way out: Go installs its
handlers with `SA_RESTART`, so a `talaria call` waiting on a wedged writer could not be Ctrl-C'd
either, and `kill -9` was the answer. It now runs the blocking `flock` in a goroutine and selects
it against `ctx` and `lockTimeout` — the `stdinBody` shape, for the same reason: a `flock` already
in progress cannot be interrupted. **The uncontended acquisition is a `LOCK_EX|LOCK_NB` attempt
made before `ctx` is consulted**, because `recordCall` runs *after* the signal context is
cancelled — `call` records the request it was interrupted in the middle of — and a context check
in front of it turns every Ctrl-C into a lost entry for a call that was actually made.
`TestAppendRecordsUnderACancelledContextWhenNothingHoldsTheLock` is that ordering. Only the
*wait* is cancellable, and a wait that gave up hands its descriptor to `abandon`, which closes it
when the flock finally returns — a lock granted after the give-up that nothing closed would be
held by this process for the rest of its life, having already reported it could not be taken.
Do not poll `LOCK_NB` against a sleep instead: it is unfair in exactly this store's shape, where a
writer appending in a loop re-takes the lock while every other waiter is mid-sleep, and the
starved one then hits the deadline during ordinary contention. `lockTimeout` is a minute because
a legitimate holder can be slow — one append over a store at `maxStoreBytes` reads it, rewrites it
and scans it again — so reaching it means the holder is not making progress rather than that it is
busy; the give-up costs a `recordCall` warning on stderr, never a silent loss
(`cmd/talaria/record_test.go`). The seam that keeps that minute out of the suite's runtime is
`lockWith(ctx, path, timeout)`, exactly as `preflightWith` names `preflightTimeout`: the deadline
cases drive `lockWith` with 300ms and the Append-level tests assert only the wiring.

**The version preflight is a subprocess like any other, and gets the same three bounds.**
`preflight(ctx, path)` (`internal/curl/version.go`) execs `curl --version` before talaria has done
anything at all, so a wrapper script on `PATH` blocking on an NFS stall used to wedge the process
where nothing could reach it. It now runs under `exec.CommandContext` with `preflightTimeout` (5s,
its own deadline — the call's `--max-time` has not started), `WaitDelay = killGrace` and
`isolate`, exactly as `ExecuteWith` bounds the call. Its stdout goes into `boundedBuffer`, never
`Output()`: the first `maxVersionBytes` (64 KiB) is kept and the rest discarded with the write
still reported as accepted, so a curl that writes gigabytes on `--version` neither exhausts this
process nor gets an EPIPE for trying. **The banner is the answer**, so `runErr` is reported only
when `versionLine` did not match — a wrapper that exits non-zero after printing it, or leaves a
grandchild holding the pipe past `WaitDelay`, has still said which curl this is. The memo is
`preflightCache`, keyed by *path* and holding only a verdict about a binary that answered. The
process-wide `sync.Once` it replaced cached the first result forever, so one transient failure —
now including the caller's own Ctrl-C, since the preflight takes the context — poisoned every
later call. Never re-add a memo that caches a result the binary did not produce.

**A basic credential is `user:password` or it is refused.** `basicPair`
(`internal/curl/firewall.go`), called from `document.auth`, is the gate: curl reads a `-u` value
with no username and colon as a prompt for the password on `/dev/tty` — which is not the config
pipe and never answers — so the call blocked for its whole `max-time` and then reported *"curl
outlived its 30s timeout"*, sending the reader after a slow API that was working fine. It is
`clierr.CredentialMissing` (exit 5), naming the variable and never the value, because it runs
downstream of `resolve`. `user:` is legal (an empty password) and so is `user:pass:word` (curl
splits at the first colon); `:password` and a bare `:` are not. There is deliberately no
counterpart on the render path: `headerArgs` emits `-u "$TALARIA_AUTH_BASIC"` from
`Ref().Symbolic()` and never resolves, so it has no value whose shape it could check — `--dry-run`
cannot diagnose a malformed credential any more than it can an expired one.

**The spec is untrusted input, and so is the history file.** Both are fetched or edited outside
this process. Bound every read, size-check before allocating, and treat any spec-derived string
that reaches the wire as hostile until checked — a media type became a header-injection vector
exactly this way. Failures must be entry-level or request-level, never process-level.

**Every read of the history file is bounded.** `readStore(path)` in `internal/corpus/file.go`
loads the store for `Read` and `storedIDs`, and `tail(path)` loads it for `trim`; both bound the
bytes *actually read* through an `io.LimitReader` rather than trusting `os.Stat`, because a store
that is a symlink to `/dev/zero` or a FIFO stats as empty and reads forever. Never re-introduce
an `os.ReadFile` on this path. Two bounds, both
entry-level where they can be: `maxStoreBytes` (64 MiB) refuses the whole file, since a file that
size has stopped being the store `trim` maintains; `maxEntryBytes` (256 KiB) is applied in
`lines`, which drops an over-long line exactly as an unparseable one is dropped, so the entries
recorded *after* a hostile one still come back. `corpus.Body.Bytes` carries the third bound:
`MaxBody` is enforced on the way in by `newBody` (truncating) *and* on the way out, checking the
encoded length before the decode — a base64 `data` is an amplifier, a few hundred bytes of line
naming hundreds of megabytes of allocation — and the decoded length after, because base64 rounds
to three-byte groups and cannot tell `MaxBody` from `MaxBody+1`.

`corpus.readStore` and `spec.(*Loader).fetch` write the same four-line idiom —
`io.ReadAll(io.LimitReader(r, max+1))`, then `len(data) > max` — and they are deliberately *not*
one helper. Task 12 tried it: what differs between them is the error, and that is the part that
matters. `spec` classifies through `clierr.SpecLoad` so the bound becomes an exit code, while
`corpus` needs a *different* message for the over-bound case than for an I/O error ("move it
aside" is wrong advice for a permission denial), so a shared helper would need a sentinel and an
`errors.Is` at each call site and save nothing. Copy the four lines and the `+1`; the tests that
hold them honest (`requireOverBound`, and the store's own) already require the error to name the
limit.

**The retention policy and the read bound are one arithmetic, and `Append` trims before it
writes.** `maxPerSource` counts entries and `readStore` bounds bytes, so the two only agree if
someone writes the multiplication down: 1000 entries × 2 sources × `maxEntryBytes` is 500 MiB
against a 64 MiB bound, and talaria wrote itself into stores it then refused to read.
`maxKeptBytes = maxStoreBytes - (maxEntryBytes + 1)` is that relationship —
`trim` leaves at most `maxKeptBytes`, `Append` writes one line after it, so the file `readStore`
is handed is `maxStoreBytes` at worst. `TestTheReadBoundAdmitsWhatTrimLeavesPlusOneMaximalLine`
fails if either constant moves alone. The entry cap stays per source; the byte budget is over the
whole file, because what a reader must hold is the file rather than any one source's share.

The order inside the lock is `trim` → `storedIDs` → `write`, and both halves of that matter.
`trim` reads the *tail* (`tail(path)` — the last `maxKeptBytes` from the first line boundary
inside the window, `os.Stat` choosing only where to seek), never `readStore`, because trim is the
only thing that shrinks the store: if repairing an over-bound file needed the whole file, the
state would be absorbing and `rm` would be the only way out. And the write goes *last* because a
retention pass after it can only report a failure for an entry already on disk — the shipped
binary told operators "the call was not recorded" for an entry that was, which is how a mutating
call gets re-run. `trim` therefore takes the incoming `Source` and counts the line the caller is
about to write, so the cap still means `maxPerSource` and not one more. For the same reason
`storedIDs` returns an error: a store that exists and cannot be read is not an empty one, and an
empty id set silently retires `uniqueID`'s collision check.

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

**The canary suite is the release gate, and every part of it must be able to fire.** A case in
`internal/canary` proves nothing unless the credential actually reached the server, so each one
asserts that first (`mech.received`, `srv.received()`) and treats a miss as `Fatalf` — the leak
assertions after it are vacuous otherwise. Three shapes carry that: a `mechanism` is one way of
authenticating (its `env` names the variables, and `config`+`args` cover the profile path, whose
credential is named by a file rather than by talaria's `TALARIA_AUTH_*` convention); a *stage* in
`TestErrorPathsDoNotLeakTheCredential` is one failure point, and its optional `check` asserts the
stage failed for the reason it was written for — exit 4 alone does not distinguish a validation
error from an HTTP error status; and the standalone tests are one named threat each. `canary.Value`
splices `escapable` (`internal/canary/surfaces.go`) into the middle of every canary precisely so
`url.QueryEscape` is not the identity on it — a hex-only canary meant the `percent` needle was
never constructed, so a credential that reached a URL field encoded went unseen.
`TestACanaryIsAlwaysDistinctFromItsPercentEncoding` is what keeps that true. The config directory
is deliberately outside `h.written()`: it is an input the user wrote, and a case that plants a
canary there would otherwise catch its own fixture.

**A test for a size bound is written so that failing it costs nothing.** An unbounded generator —
a reader that never returns EOF, a handler that writes forever — is the faithful adversary, but
its failure mode is an out-of-memory kill of the machine running the suite rather than a red
test. `corpus.test` and `spec.test` each took 20 GB and were OOM-killed on 2026-08-03, both
during the TDD red phase of the task adding the bound they test. Cap the generator at a small
multiple of the bound (`endlessBody` in `internal/spec/source_test.go` stops at `4*maxSpecBytes`):
past every limit under test, so a missing bound still fails, and finite, so it fails as a test.

**A hostile-input test asserts *which* refusal happened, not that something failed.** Oversized
bodies in these tests are `x` repeated, which no parser accepts: assert only `err != nil` and the
test passes against no bound at all, because the unbounded read reaches the parser and fails
there. `requireOverBound` in `internal/spec/source_test.go` is the shape — the error must name the
limit. Same for a policy net/http would enforce anyway: `TestLoaderRefusesARedirectAwayFromHTTP`
matches the message `checkRedirect` writes, since the transport refuses `file://` on its own and
an error alone would prove nothing about the redirect policy. Neuter the implementation and watch
the test go red before you believe it.

## Scope note

`talaria run`, `internal/gen` and the JUnit report were removed on 2026-08-03 — see
`docs/plans/2026-08-02-phase-2-boundary-design.md` §2.1. Do not reintroduce spec-driven smoke
testing without a design-doc change; it is the one capability deliberately ceded to Schemathesis
and Hurl.
