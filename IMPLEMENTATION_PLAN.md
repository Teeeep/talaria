# talaria — Phase 2a remediation plan

Design document: [docs/plans/2026-08-03-phase-2a-remediation.md](docs/plans/2026-08-03-phase-2a-remediation.md).
Specification: [docs/design/DESIGN.md](docs/design/DESIGN.md) (v0.5). House rules: `CLAUDE.md`.

## Read this before picking up any task

**The findings file is not where the design document says it is.** The design doc names
`REVIEW_FINDINGS.md` at the repo root; that file does not exist. The 32 findings live at
**`docs/review/2026-08-03-review-findings.md`**, one `## Finding N:` section each, with a file,
a line and an empirical reproduction. Every task below cites its finding numbers. **Read the
finding before writing the fix** — the finding has the reproduction, this plan has the shape.

**Line numbers in the findings file are stale.** They predate the removal of `talaria run` and
`internal/gen` (2026-08-03). Locate code by symbol name, not by line.

**`go test ./...` is green right now with all 25 of these defects live.** A passing suite is not
an acceptance signal. Each task is done only when a test exists that **fails before the change
and passes after it**. If a test you write passes before you touch the implementation, it is
testing the wrong thing — rewrite it.

**Two findings are already settled and must not be re-litigated:**

- **Finding 18** (replay's resolvable env-var set) was `Blocked-by: design`. DESIGN.md v0.5 §5a
  settled it: the replay table's *Env var names* row now reads *"Not applicable — nothing in a
  stored entry is resolved."* That makes `replayableEnv` dead, not narrower. It is deleted in
  Task 2; there is no separate task for 18.
- **Finding 25** (cache policy) was `Blocked-by: design`. DESIGN.md v0.5 §4 settled it: 24h TTL,
  conditional revalidation with `ETag`/`Last-Modified`, `--refresh` to force. Task 16 implements
  that policy as written — do not invent a different one.

**Findings 7, 12 and 26 are moot** — the code they name (`internal/gen`, the `!unix` lock stub)
was deleted. **Findings 27, 28 and 32 are deferred on purpose.** Do not plan or write work for
any of these six.

**Explicitly out of scope.** `internal/boundary`, `internal/fileguard`, `talaria doctor`, exit
code 6, uid separation (phase 2b); packaging, release binaries, the Claude Code skill (phase 2c);
anything under `internal/twin`; reintroducing `talaria run`, `internal/gen` or JUnit output.

**Two `//nolint:noctx // phase-2a task 1` waivers are tracked debt**, at
`internal/spec/source.go:122` and `internal/curl/version.go:45`. **Both are about *context and
cancellation*, not size** — bounding a read does not license removing either. Only the spec one's
comment says to remove it in the commit that fixes it. Task 16 removes the spec waiver (it threads
the context), Task 13 the curl one. A waiver left behind after its fix lands is a lint failure
waiting to happen; a waiver removed before its fix lands is a lint failure now.

**Every file:line in this plan was verified against the tree on 2026-08-04.** Symbols were checked
to exist (or, for `(create)`, to not exist). Where the finding's own line numbers are stale, this
plan carries the current one. Still locate by symbol — a task ahead of you in the queue may have
moved a line.

## Commands

| Purpose | Command |
|---|---|
| All tests | `go test ./...` |
| One package | `go test ./<dir>/...` |
| Build | `go build ./...` |
| Lint | `test -z "$(gofmt -l .)" && go vet ./... && golangci-lint run ./...` |

`golangci-lint` v2.12.2 **is** installed at `~/go/bin/golangci-lint` (the design document says it
is not — that is stale). The loop runs build, lint and test after every commit and rolls the
commit back if any fails.

---

### Task 1: Substitute server variables and expose the spec's server URLs

**Depends on:** none

**Fixes finding:** 11

**Test files:**
- `internal/spec/servers_test.go` (create) — substitution of `{var}` from each variable's
  `default`, enum validation, and the hostile inputs below
- `internal/request/request_test.go` (modify) — a spec whose only server carries a variable is
  now callable without `--base-url`

**Implementation files:**
- `internal/spec/servers.go` (create) — `func Servers(doc *Document) []string`
- `internal/request/build.go` (modify) — `firstServer` calls `spec.Servers`

**Red — write failing tests:**
1. `Servers` on a document with `url: "https://{region}.api.example.com/v1"` and
   `variables.region.default: "eu"` returns `["https://eu.api.example.com/v1"]`.
2. A URL with two variables substitutes both in one pass.
3. A variable whose `default` is absent from a non-empty `enum` is rejected: that server is
   omitted from the result, not emitted with the bad default.
4. A server whose URL names a variable the spec does not declare is omitted from the result
   (it cannot be substituted, so it is not a usable server).
5. Order is preserved and every substitutable server is returned — not just index 0.
6. `Build` on a fixture whose `servers[0].url` carries a variable **in the host**
   (`https://{region}.api.example.com/v1`) succeeds and produces the substituted base URL. This is
   the finding's exact reproduction: `url.Parse` rejects `{` in the host, so `binder.baseURL`
   (`internal/request/build.go:158-162`) exits 2 today with *"base URL … is not an absolute
   http(s) URL"*.
7. **A variable in the *path* fails differently and the test must say so.**
   `https://api.example.com/{basePath}` parses fine today — `Host` is non-empty and the scheme is
   https — so `Build` **succeeds** and sends a request to a URL containing a literal `{basePath}`.
   `b.path()` (`build.go:243`) rejects leftover braces in the *operation* path but never in the
   base URL. A red test written as "a spec with a path-position variable errors today" will be
   **green before your change** and prove nothing. Assert the wrong-URL outcome instead: today the
   built URL contains `{basePath}`; after the fix it contains the substituted value.

**Adversarial — what does hostile or malformed input do here?**
The spec is untrusted input (`CLAUDE.md`), and a server variable's `default` is a spec-controlled
string spliced into a URL, so it is an authority-rewriting primitive. Write these:
1. **Cyclic defaults** — `{a}` with default `{b}`, `{b}` with default `{a}`. Substitution must be
   **single-pass**: substituted text is never rescanned. Assert the call terminates and the
   result still contains a literal `{`, so that server is omitted rather than expanded forever.
2. **Authority injection** — a default of `evil.com/` under `https://{sub}.example.com`, and a
   default containing `@` (`x@attacker.com`). Assert the substituted URL still goes through the
   existing `request.Userinfo` and absolute-http(s) checks in `binder.baseURL`, i.e. a
   userinfo-bearing result is refused exactly as a hand-typed one is
   (`TestBuildRejectsCredentialsInABaseURL` is the model).
3. **Volume** — a URL naming 1,000 variables, and a default 1 MB long. Assert `Servers` returns
   without unbounded growth; bound the substituted length and omit a server that exceeds it.
4. **Empty and nil** — no `servers[]`, a server with an empty URL, a nil `Variables` map.
   `Servers` returns an empty slice; `firstServer` returns `""` and `Build` still reports the
   existing "no base URL" usage error.

**Green — minimal implementation:**
1. Add `internal/spec/servers.go` with `Servers(doc *Document) []string`. Iterate
   `doc.Model.Servers`; for each, walk the URL once replacing each `{name}` with
   `Variables[name].Default` (iterate the `orderedmap` with `FromOldest()`, the pattern already
   used in `internal/operation/extract.go`). Reject the server — omit it — if a placeholder has
   no declared variable, if a non-empty `Enum` does not contain the `Default`, or if the result
   still contains `{` or exceeds the length bound.
2. Change `firstServer` (`internal/request/build.go:173`) to return the first element of
   `spec.Servers(doc)`, or `""`. Keep its existing nil guards (`doc`, `doc.Model`, empty slice,
   `Servers[0] == nil`) — move them into `Servers` rather than dropping them.
3. Do **not** add a host set or an allowlist here. Task 2 owns that.

`internal/spec` exports only `Resolve`, `EnvSpec`, `Document` and the loaders today; `Servers` is
a new exported symbol and `firstServer` is currently the only server reader in the tree.

**Verify:** `go test ./...`

**Why:** DESIGN.md §5a defines the allowed host set as the spec's `servers[]` *after* server-
variable substitution. Task 2 computes that set; computed from unsubstituted URLs it would pass a
fixture test and be wrong in production. It also makes a large class of real 3.x specs callable
without `--base-url`, which they are not today.

---

### Task 2: Bind credentials to hosts, and re-derive `history replay` through the spec

**Depends on:** Task 1

**Fixes findings:** 1, 2, 3, 22 (and closes 18, which DESIGN.md v0.5 settled)

**This task is deliberately not split, and must not be split.** Findings 1, 2, 3 and 22 are one
rule seen through four doors:

> The destination of a request, and the credentials attached to it, are derived from the spec and
> the flags — never from stored or off-spec input.

The previous cycle split them and shipped a fix that narrowed *which* credential replay resolves
while leaving *where it is sent* untouched; finding 2 is marked a repeat for exactly that reason.
It is the largest task in this phase. Work it in the order given below.

**Test files:**
- `internal/request/request_test.go` (modify) — the host check and the `Withheld` result
- `internal/config/config_test.go` (modify) — `allow_hosts:` parses under `KnownFields(true)`
- `cmd/talaria/history_test.go` (modify) — replay re-derivation and its refusals
- `cmd/talaria/call_test.go` (modify) — `--allow-host`, the envelope field, the stderr warning
- `internal/e2e/e2e_test.go` (modify) — **the wire-level assertion the acceptance criteria
  require**; reuse the existing `newServer`/`requests()`/`recordedRequest` helpers

**Implementation files:**
- `internal/request/request.go` (modify) — `Withheld` type, `Request.Withheld` field
- `internal/request/build.go` (modify) — `Inputs.AllowHosts`, host check in `binder.credentials`
- `internal/config/config.go` (modify) — `Profile.AllowHosts`
- `cmd/talaria/root.go` (modify) — persistent `--allow-host` flag
- `cmd/talaria/call.go` (modify) — thread the allowed set, render `credentials_withheld`, warn
- `cmd/talaria/history.go` (modify) — rewrite the replay path; delete the stored-value machinery
- `README.md`, `AGENT.md` (modify) — replay now needs a spec; document `--allow-host`

**Red — write failing tests:**

*Host binding (finding 1):*
1. **Wire-level, and this one is the acceptance criterion.** With `servers[0].url` naming one
   host, `TALARIA_AUTH_BEARER` set to a canary, and `--base-url` pointing at an httptest capture
   listener, the listener records **no** `Authorization` header and the canary appears nowhere in
   what it received. An output-level assertion cannot catch this bug — redaction answers *does it
   print*, not *who receives it*, which is why the canary suite passes today.
2. The same call's envelope carries
   `"credentials_withheld":[{"scheme":"bearerAuth","reason":"…","host":"127.0.0.1:PORT"}]`, and
   one line naming the withheld scheme and the offending host goes to stderr.
3. The call still **runs** and exits 0 — withholding, not refusing. Pointing at a local twin is
   the common case and must not need a flag (DESIGN.md §5a).
4. `--allow-host` naming that host puts the credential back on the wire.
5. A profile carrying `allow_hosts: [...]` does the same, and a profile file containing
   `allow_hosts:` **parses** — `Config` decodes with `KnownFields(true)`, so without the struct
   field every such profile is a hard parse error.
6. A `--base-url` inside the spec's `servers[]` attaches credentials as it does today (the
   no-regression case).

*Replay re-derivation (findings 2, 3, 22):*
7. A `history.jsonl` line hand-edited to `"url":"http://127.0.0.1:PORT/steal"` replays to the
   **spec's** host, not the stored one; the capture listener at the stored host receives nothing.
8. `history replay <id> --base-url https://elsewhere.example` retargets the request. Today the
   flag is accepted and silently discarded, which is worse than erroring.
9. A stored host outside the currently allowed set makes replay exit **2**, not silently
   retarget (DESIGN.md §5a replay table).
10. An entry whose `operation_id` is no longer in the spec fails **that entry** with exit 2.
11. A replay emits a `validation` block, like `call`.
12. A stored request body containing `secret.Placeholder` is **refused** with exit 2 rather than
    sent verbatim. Today the replayed POST arrives as
    `{"grant":"x","refresh_token":"<redacted>"}` with empty stderr.
13. The symbol `replayableEnv` is **absent** from the tree. Grep for it in the test or assert its
    absence in review — the finding requires deletion, not narrowing.

**Adversarial — what does hostile or malformed input do here?**
Both inputs here are hostile by the design's own premise: `--base-url` is attacker-reachable via
the agent, and the history file is *"data, never instruction"* (DESIGN.md §5a).
1. **Host comparison must not be fooled.** Test: uppercase host vs lowercase spec host (equal);
   `api.example.com:443` vs `https://api.example.com` (equal — normalise the default port);
   `api.example.com.attacker.com` (**not** equal — suffix match is the classic bug here);
   `api.example.com:8443` when the spec declares no port (**not** equal); IPv6 literals
   (`[::1]:9000`); a trailing dot (`api.example.com.`).
2. **A stored entry that is not a valid request.** Empty `operation_id`; a `url` whose path has
   a different segment count than the operation's path template; a path segment that will not
   percent-decode; a `method` disagreeing with the spec's. Each fails **that entry** with exit 2
   — never the process, never a panic.
3. **A stored entry carrying a credential-shaped header.** After this task nothing in the entry
   is resolved, so a stored `Authorization: <redacted:env:AWS_SECRET_ACCESS_KEY>` must be
   *dropped* and re-supplied by `config.Resolve` — assert the literal string `<redacted:env:…>`
   never reaches the wire.
4. **`--allow-host` with junk**: an empty value, a full URL instead of a host, a value containing
   `\r\n`. Reject as a usage error rather than admitting a malformed entry to the set.

**Green — minimal implementation, in this order:**
1. `internal/config/config.go`: add `AllowHosts []string \`yaml:"allow_hosts"\`` to `Profile`.
2. `internal/request/request.go`: add
   `type Withheld struct { Scheme, Reason, Host string }` with the json tags from DESIGN.md
   §5a's sketch, and `Withheld []Withheld \`json:"credentials_withheld,omitempty"\`` on `Request`.
3. `internal/request/build.go`: add `AllowHosts []string` to `Inputs`. Add one exported helper
   that computes the allowed set — `spec.Servers(doc)` hosts ∪ `Inputs.AllowHosts` ∪
   `Profile.AllowHosts` — with **one** normalising comparison function used everywhere. In
   `binder.credentials`, compare the chosen base URL's host against the set; on a miss append a
   `Withheld` instead of the `Pair`.
4. `cmd/talaria/root.go`: register `--allow-host` as a **persistent** flag with `StringArrayVar`,
   beside `--base-url`, because both `call` and `history replay` need it. Extend
   `TestProfileAndBaseURLArePersistentOnRoot` in `cmd/talaria/flags_test.go` to cover it.
5. `cmd/talaria/call.go`: pass the flag into `buildRequest`; add
   `CredentialsWithheld []request.Withheld` to `callView`; emit the one-line stderr warning
   (follow `secret.QueryKeyWarner`'s shape — it is the existing one-line-warning convention).
6. `cmd/talaria/history.go`: rewrite the replay `RunE` (an anonymous closure at
   `history.go:175`, inside `newHistoryReplayCmd` at `:163`) to `loadSpec` → `index.Lookup(
   entry.OperationID)` → recover path params by matching the stored path against the operation's
   path template → `config.Resolve` → `request.Build` (passing the stored query/headers/body as
   *inputs*, and the allowed host set) → `curl.ExecuteWith` → `validateResponse`.

   **`replayRequest` (`history.go:554`) is the function this task exists to delete.** It is where
   the host comes from the stored entry — `BaseURL: parsed.Scheme + "://" + parsed.Host` at
   `:578`, body from `entry.Request.Body` at `:603-611`. The five helpers below are its callees;
   deleting them without replacing `replayRequest` fixes nothing. Delete `replayValue` (`:681`),
   `replayableEnv` (`:721`), `encodingPrefix` (`:739`), and the ref-parsing halves of
   `replayPairs` (`:651`) and `replayQuery` (`:615`). Delete the comment at `history.go:230-231`
   claiming *"replay reads a recorded request and needs no spec to send one"* — it is now false.

   Replay also currently calls `curl.Execute`, not `ExecuteWith`; switching it is part of this
   rewrite. Note `--spec` is inherited and silently ignored today for the same reason
   `--base-url` is: `history.go` never reads either. Both must work after this task.
7. Update `README.md` and `AGENT.md`: `history replay` now resolves a spec (positional, `--spec`
   or `TALARIA_SPEC`) and honours `--base-url`; document `--allow-host` and `allow_hosts:`.

   **`cmd/talaria/agentdoc_test.go` is stricter than "documentation consistency" suggests — read
   it before writing prose.** It (a) cross-checks AGENT.md's exit-code table against the
   `clierr.Code` constants by parsing the AST, both directions; (b) requires every runnable
   command path to appear literally in AGENT.md and every `` `talaria …` `` span in AGENT.md to
   resolve to a real command; (c) **executes every fenced line starting `talaria `** with
   `TALARIA_SPEC=testdata/call.yaml`, auto-appending `--dry-run`, and fails if it exits 2 — so a
   `--allow-host` example must really work, or be listed in `notRunnable` (`:163`) keyed by the
   exact line; and (d) requires every `TALARIA_[A-Z0-9_]*` token in AGENT.md to be in a hardcoded
   allowlist at `:75-80`. This task adds no new env var, but (c) and (d) are how doc edits break
   the suite.

**Verify:** `go test ./...`

**Why:** This is the credential-exfiltration finding and the three findings that share its rule.
Finding 1 delivers a production bearer token to any host the agent names; finding 2 does the same
through a file the agent can write. Everything else in this phase is smaller than these four.

---

### Task 3: Report unsupported security schemes instead of hiding them

**Depends on:** none

**Fixes findings:** 4, 21 (21 is the documentation half and ships in the **same commit**, or the
README contradicts the code)

**Test files:**
- `internal/config/auth_test.go` (modify) — `Resolve` and `Schemes` on an unsupported scheme
- `cmd/talaria/auth_test.go` (modify) — the `auth check` report shape and exit code
- `cmd/talaria/call_test.go` (modify) — `call` on an oauth2-only spec
- `cmd/talaria/testdata/` (create) — a fixture spec whose only scheme is `oauth2`

**Implementation files:**
- `internal/config/auth.go` (modify) — `Credential.Supported`, `Schemes`, `credentialFor`,
  the error at the end of `Resolve`
- `cmd/talaria/auth.go` (modify) — `authScheme.Supported`, `unsatisfied`
- `README.md` (modify) — lines 165–170 and 254; `AGENT.md` (modify) — add the new behaviour

**Red — write failing tests:**
1. On a spec whose only scheme is `oauth2`, with nothing exported, `auth check` reports
   `{"scheme":"oauth2","supported":false,"present":false}` and exits **5**. It reports
   `{"schemes":[]}` and exits 0 today, because `Schemes` filters on `schemeReason` != "".
2. The same spec with `TALARIA_AUTH_BEARER` set: the scheme is **satisfied by that token** —
   `auth check` exits 0 and `call` puts the token on the wire. This is DESIGN.md §5's
   bring-your-own-token clause; `TALARIA_AUTH_BEARER` is inert today.
3. `call --dry-run` on that spec with nothing exported exits **5** naming the scheme *and*
   `TALARIA_AUTH_BEARER`. It exits 2 with "no usable security scheme" today.
4. `auth check` and `Resolve` agree on every arrangement the test covers — the non-negotiable
   clause of §5. Extend `TestResolveAgreesWithTheCoverageAuthCheckReports` rather than writing a
   parallel assertion; that test is the guard against the two drifting again.
5. A supported scheme's report is unchanged (`supported:true`), so the field is additive.
6. `openIdConnect` and `mutualTLS` behave as `oauth2` does; an `apiKey` in an unsupported `in`
   does too.

**Adversarial — what does hostile or malformed input do here?**
The spec declares the schemes, so this is spec-controlled input.
1. A `securityScheme` with an empty `type`, an unknown `type`, a null `in`, or a name that
   `envSuffix` mangles into a colliding variable. **This collision is confirmed live, not
   hypothetical:** `envSuffix` (`internal/config/auth.go:405-420`) upper-cases and maps every rune
   outside `[A-Z0-9]` to `_`, so `key-a`, `key.a`, `key a` and `key+a` all read
   `TALARIA_AUTH_APIKEY_KEY_A`, and each non-ASCII *rune* becomes one `_` byte, so `ké` and `k_`
   collapse too. Nothing detects it — `credentialFor` is called per scheme name and both callers
   key by scheme name. Assert two distinct scheme names never silently share one env var, or that
   the collision is reported.
2. A requirement naming a scheme absent from `components.securitySchemes` still produces an
   actionable error, not a nil-map panic.
3. A spec mixing supported and unsupported alternatives (`[{oauth2:[]},{bearerAuth:[]}]`):
   `Resolve` must pick the satisfiable one, and `auth check` must list **both**.

**The interaction that makes this task bigger than it looks.** `Resolve`'s exit-2
(`internal/config/auth.go:167`) and `unsatisfied`'s exit-5 (`cmd/talaria/auth.go:155`) live in
different packages, and the thing currently making an oauth2-only spec exit 0 is a *chain*:
`Schemes` (`auth.go:184`) drops the scheme → `unsatisfied` (`cmd/talaria/auth.go:121`) builds an
empty `byName` → `Covers`'s `!ok` branch returns `Unsupported` (`config/auth.go:111-112`) →
`satisfied` leaves `usable=false` → `blocking` is empty → nil error. Emitting unsupported schemes
from `Schemes` changes what `byName` contains, which changes `Covers`'s semantics for every spec,
not just unsupported ones. Trace that chain before editing, and keep red-test 5 (a supported
scheme's report is unchanged) as the guard that you did not move the supported path.

**Green — minimal implementation:**
1. Add `Supported bool` to `config.Credential` (fields today: `Scheme, Kind, In, Name, Ref` at
   `internal/config/auth.go:53-64`) and `Supported bool \`json:"supported"\`` to
   `cmd/talaria.authScheme` (fields today: `Scheme, Source, Present` at `cmd/talaria/auth.go:24-28`).
2. `Schemes`: stop dropping schemes `schemeReason` rejects. Emit them with `Supported: false` and
   no ref, so `Present()` is false. `credentialFor` (`auth.go:288`) is currently unreachable for
   such a scheme — both `Schemes` (`:184`) and `supportedCredentials` (`:259`) gate on
   `schemeReason`, which is why `TALARIA_AUTH_BEARER` is inert. Open exactly one of those gates.
3. `credentialFor`: when the scheme is unsupported and `secret.Env(EnvBearer)` is set, return a
   bearer credential carrying it, marked `Supported: false`.
4. `Resolve`: change the terminal `clierr.Usage` to `clierr.CredentialMissing`, naming the
   scheme and `TALARIA_AUTH_BEARER`. Exit 5 is the published contract for *"credential missing
   for a required security scheme"*; exit 2 sends an agent looking for a usage mistake.
5. Rewrite `README.md:254` (*"schemes talaria cannot supply at all … are left out rather than
   reported missing"* — now forbidden) and `README.md:165–170`'s bring-your-own-token passage,
   which does not describe a reachable workaround today. Add the behaviour to `AGENT.md`, which
   is silent on unsupported schemes.

**Verify:** `go test ./...`

**Why:** DESIGN.md calls the `auth check`/`call` agreement non-negotiable — *"or `auth check` is
worthless to an agent."* Three §5 clauses break at once today, and the exit-code contract that
agents branch on is wrong for the one case it exists to serve.

---

### Task 4: Refuse a spec-supplied media type that would inject headers

**Depends on:** none

**Fixes finding:** 5

**Test files:**
- `internal/curl/config_test.go` (modify) — the last-gate check
- `internal/request/body_test.go` (modify) — rejection at bind time
- `internal/curl/render_test.go` (modify) — the emitted reproduction command

**Implementation files:**
- `internal/curl/config.go` (modify) — `document.body` (`:284`), the `Content-Type` directive
  (`:289-290`)
- `internal/curl/render.go` (modify) — `:129-130` builds the same `-H Content-Type: …` word with
  no `checkSplit`. Fixing only `config.go` leaves the emitted-curl surface injectable.
- `internal/request/body.go` (modify) — `binder.contentType` (`:136-148`)

**The `internal/curl` gate is mandatory, not belt-and-braces.** `history replay` sets
`req.Body.ContentType` straight from the stored entry (`cmd/talaria/history.go:610`) and never
goes through `internal/request` at all, so a bind-time check alone leaves the whole replay path
open. Conversely a user-supplied `--header Content-Type=…` has already passed
`binder.pairs` → `SplitsRequest` (`internal/request/build.go:394`) — the unchecked sources are the
spec's `content:` map key and the history file.

**Red — write failing tests:**
1. A spec whose `content:` map key is `"application/json\r\nX-Injected: pwned"` is rejected at
   **bind** time with a usage error (exit 2), naming the operation and not echoing the value.
2. `BuildConfig` on a hand-built `Request` whose `Body.ContentType` carries `\r\n` returns an
   error rather than a config document — the last gate, matching how every other header pair is
   treated by `checkSplit`.
3. A well-formed media type (`application/json`, `application/vnd.api+json;charset=utf-8`) is
   unaffected: it still produces the `Content-Type` directive.
4. `curl.Render` on the same hostile request does not emit a raw newline inside a single-quoted
   argument.

**Adversarial — what does hostile or malformed input do here?**
The media type comes from a key in the spec's `content:` map, which is untrusted by the product's
own premise, and this is the one header path that skips `checkSplit` — inside the component that
holds resolved credentials.
1. A **double** CRLF (`application/json\r\n\r\nGET /admin HTTP/1.1`) terminates the header block
   and smuggles a second request on the connection. Assert it is refused.
2. A bare `\n` and a bare `\r` separately — `checkSplit` must catch both, not just the pair.
3. A media type of only whitespace, an empty string (already means "no content type"), a NUL
   byte, and a 64 KB media type.
4. Assert explicitly that `escapeDirective` is **not** relied on as the fix. Its own comment says
   it protects curl's parser and curl un-escapes `\r\n` back to two bytes on the wire; a test
   asserting the escaped form is present would pass while the request still splits.

**Green — minimal implementation:**
1. In `binder.contentType` (`internal/request/body.go`), reject a media type that is not a valid
   token/subtype with optional parameters. Report it as a usage error via `b.fail`, following the
   existing convention of naming the field and never quoting the offending value.
2. In `document.body` (`internal/curl/config.go:284`), call
   `checkSplit("header", "Content-Type", req.Body.ContentType)` before writing the directive —
   `checkSplit` is at `:273` and is already applied to headers (`:221`) and cookies (`:250`).
3. Gate the same value in `curl.Render` (`internal/curl/render.go:129-130`), or have `Render`
   refuse to emit for a request `BuildConfig` would reject. Say which you chose in a comment.

**Verify:** `go test ./...`

**Why:** A hostile spec smuggles arbitrary headers — and a whole second request — onto the wire
from inside `internal/curl`, the one component that holds resolved credentials. `CLAUDE.md`
records this exact vector: *"a media type became a header-injection vector exactly this way."*

---

### Task 5: Stop printing a secret-bearing request body, and reference it in the emitted curl

**Depends on:** none

**Fixes finding:** 6

**Test files:**
- `cmd/talaria/call_redact_test.go` (modify) — the stdout half
- `internal/curl/render_test.go` (modify) — the emitted-curl half
- `internal/canary/canary_test.go` (modify) — a canary in a request body
- `internal/e2e/e2e_test.go` (modify) — the dry-run/real-call equivalence still holds

**Implementation files:**
- `cmd/talaria/call.go` (modify) — redact `view.Request.Body` in `callPayload`
- `internal/request/request.go`, `internal/request/body.go` (modify) — record the body's source
- `internal/curl/render.go` (modify) — emit `--data @file` / `--data @-` / inline

**Red — write failing tests:**
1. `call … --body '{"refresh_token":"CANARY"}'` prints `{"refresh_token":"<redacted>"}` in
   `request.body` on stdout. Today stdout carries the canary while history redacts it — the
   firewall backwards, and `buildRequest`'s own comment states the rule it violates.
2. `--body @file` renders `--data @<path>` in the emitted curl; `--body -` renders `--data @-`;
   an argv `--body '…'` is still inlined. This is DESIGN.md §3.4, settled: a body read from a
   file or stdin may carry a credential the agent never saw; an argv body is already in the
   agent's hands.
3. The canary suite gains a case injecting the canary through `--body @file` and scanning every
   surface — stdout, stderr, history store, cache.

**Adversarial — what does hostile or malformed input do here?**
The body crosses a trust boundary in the direction the rest of the tool does not guard: it comes
*from* a human or CI and is read *by* the agent.
1. A body that is **not JSON** (binary, or `not json at all`) — the redactor's JSON-path walk
   must leave it alone rather than corrupting or dropping it, and must not panic.
2. A body whose secret is nested in an array, and one whose key matches a configured path at
   depth — reuse the existing `secret.ResponseRedactor` so both cases behave as they already do
   for responses.
3. A `--body @file` path containing a space, a quote, or a newline: the emitted `--data @path`
   must still be a single shell word, or the "portable reproduction" is a shell-injection
   primitive on paste. Assert the rendered command round-trips through `sh -c`.
4. A file body containing newlines: curl's `--data @file` **strips** newlines, so the emitted
   command may send different bytes than the executed call. Assert what actually happens. If it
   diverges, that is a DESIGN.md §3.4 question — record it in `.ralph/refactor-backlog.md` and
   flag it. Do **not** silently switch to `--data-binary`; the design doc says `--data`.

**Green — minimal implementation:**
1. In `callPayload` (the raw assignment is `cmd/talaria/call.go:450-452`,
   `view.Request.Body = string(req.Body.Data)`), apply `redactors.Response.Body` — a **method** on
   `*secret.ResponseRedactor` (`internal/secret/response.go:98`), not a field. It redacts the
   built-in JSON paths `access_token`/`refresh_token`/`id_token` plus `redact.body-paths`, and
   returns a non-JSON body verbatim (`response.go:104-106`), which is what adversarial case 1
   below asserts. Thread in the redactor built at `call.go:164`. Note `newRedactors`
   (`call.go:237`) is called **twice** on the call path (`:164` and again at `:305` inside
   `buildRequest`) — thread, do not add a third; note the duplication in
   `.ralph/refactor-backlog.md` for Task 6. The history path already redacts here
   (`internal/corpus/entry.go:166`), which is the asymmetry the finding names.
2. Add a source discriminator to `request.Body` (argv / file / stdin, with the path for file).
   `bodyKind` in `internal/request/body.go` already computes this string — promote it onto the
   type rather than re-deriving it.
3. In `curl.Render` (`internal/curl/render.go:133`, today an unconditional
   `append(args, "--data-raw", …)`), switch on that source: `--data @path`, `--data @-`, or the
   current inline form.

**A divergence that already exists here — do not mistake it for one you caused.** The *executed*
request does not always inline either: `internal/curl/config.go:305-318` falls back to a 0600 temp
file plus `data-binary @path` when `inlinable` is false (body over `maxInlineBody`, invalid UTF-8,
or containing NUL). So the emitted curl and the sent request already disagree for large and binary
bodies, independently of this task's provenance work. The e2e dry-run/real-call equivalence test
constrains the bodies it uses accordingly. If your change widens that gap, record it in
`.ralph/refactor-backlog.md`; do not "fix" it here.

**Verify:** `go test ./...`

**Why:** A credential the human put in a request body is printed to the agent on stdout — the
exact asymmetry §1 exists to prevent, on the surface DESIGN.md §3 principle 0 names first.

---

### Task 6: Compaction — tasks 1–5

**Depends on:** Task 5

**Deliverable is negative.** No new behaviour, no new files, no new tests beyond ones that
replace several others. The suite is green before and after, and the diff is net-negative in
lines.

1. Read `.ralph/refactor-backlog.md` first — build iterations record structural problems there as
   they hit them, because each one is visible only from inside the task that caused it. That file
   is the work list; this task drains it. Delete each entry as you resolve it, and leave anything
   you deliberately did not do, with one line on why. (Task 5 is instructed to add an entry about
   `--data @file` newline stripping — that one is a design question, not a compaction job. Leave
   it with a note.)
2. Read every file touched since the start of this phase.
3. Consolidate what tasks 1–5 duplicated. Specific candidates: **host normalisation and
   comparison** must exist once, not once in `request` and once in `history`; **the
   spec→lookup→resolve→build sequence** now exists in both `call.go` and `history.go` and should
   be one helper; the **one-line stderr warning** shape is now used by query-key warnings,
   withheld credentials and unreplayable fields. Two known pre-existing duplications to fold in
   while you are here: `newRedactors` is constructed twice on the `call` path (`call.go:164` and
   `:305`), and orderedmap iteration uses `FromOldest()` throughout `internal/operation/extract.go`
   but the older `m.First()`/`.Next()` form at `internal/config/auth.go:378`.
4. `cmd/talaria` holds 28% of production code and is the largest single component (`CLAUDE.md`).
   Tasks 2, 3 and 5 all added to it. Move logic down into the package that owns it and **measure
   it** — report the non-comment line count of `cmd/talaria` before and after.
5. Delete commented-out code, superseded helpers, and any comment asserting a property no test
   enforces. Task 2 deletes a whole machinery (`replayableEnv` and friends) — confirm no orphaned
   helper, constant or comment survived it.
6. Record every pattern you consolidated in `CLAUDE.md`, so later iterations follow it instead of
   re-inventing it.

**Verify:** `go test ./...` green, `test -z "$(gofmt -l .)" && go vet ./... && golangci-lint run ./...`
clean, and report the net line delta in the commit message.

---

### Task 7: Bound every read of untrusted input — the remote spec and the history file

**Depends on:** none

**Fixes findings:** 9, 13

**Test files:**
- `internal/spec/source_test.go` (modify) — an oversized HTTP response
- `internal/corpus/store_test.go` (modify) — an oversized history file and entry
- `internal/corpus/entry_test.go` (modify) — an oversized decoded body

**Implementation files:**
- `internal/spec/source.go` (modify) — `fetch` (`io.ReadAll` at `:132`), **and the cache read at
  `:87`** (`os.ReadFile`, equally unbounded, on a file the tool itself wrote but another process
  can replace)
- `internal/spec/load.go` (modify) — the local-spec read at `:49`, the same unbounded shape
- `internal/corpus/store.go` (modify) — **three** unbounded `os.ReadFile` on the same file, not
  one: `Read` (`:187`), `storedIDs` (`:161`/`:208`) and `trim` (`:248`/`:295`). Bounding only
  `Read` leaves every *append* reading the whole file unbounded twice.
- `internal/corpus/entry.go` (modify) — `Body.Bytes` (`:124` base64 path, and `:120-122`, the
  no-encoding path, also unbounded)

**Red — write failing tests:**
1. An httptest server streaming past the cap makes `Load` return a `clierr.SpecLoad` error
   (exit 3), and **nothing is written to the cache**. Today `io.ReadAll` is unbounded on a URL
   the product's premise treats as untrusted, and `client.Get` follows redirects.
2. A `history.jsonl` whose total size exceeds the file cap fails as a bounded read, not an
   unbounded `os.ReadFile`.
3. A single line over the entry cap is skipped as a **malformed entry** — the other entries in
   the file still load, and `history` still lists them. DESIGN.md §5a: *"A corrupt or hostile
   entry fails that entry, never the process."*
4. `Body.Bytes()` on a base64 payload whose decoded length exceeds `MaxBody` returns an error
   rather than allocating. `MaxBody` is enforced only at write today (`newBody`).

**Adversarial — what does hostile or malformed input do here?**
Both of these files are named in `CLAUDE.md` as untrusted: one is fetched, one is edited outside
this process.
1. **A lying `Content-Length`** — a response declaring 10 bytes and sending 10 MB. The cap must
   come from the reader (`io.LimitReader`), not the header.
2. **A redirect chain** to a large body, and a redirect from `https` to `http`. `client.Get`
   (`source.go:122`) sets no `CheckRedirect` anywhere in the repo, so Go's default 10-hop follow
   applies. Assert the cap still applies after the redirect. Decide, and say in a comment, whether
   the policy is forced on a caller-injected `Loader.Client` (`source.go:60`) or only on the
   default one — tests inject that client, so the choice is observable.
3. **A base64 zip bomb** — a small `data` field whose decoded length is enormous. Check the
   decoded length *before* decoding (base64 length is computable from the encoded length), so the
   allocation never happens.
4. **A file that is one enormous line with no newline** — the line splitter must not accumulate
   unboundedly before it discovers there is no delimiter.
5. Assert the failure is **entry-level or request-level, never process-level**: no `panic`, no
   `os.Exit`, and a structured error with the documented exit code.

**Green — minimal implementation:**
1. `internal/spec/source.go`: wrap `resp.Body` in `io.LimitReader` with an explicit exported cap —
   a few tens of MB, enough for the 2 MB `swagger.json` the design cites — and return
   `clierr.SpecLoad` when the limit is reached. Do not cache a truncated body.
2. `internal/corpus/store.go`: bound the file read and the per-line length; treat an over-cap line
   the way an unparseable line is already treated at `Read` (skip it).
3. `internal/corpus/entry.go`: reject an over-`MaxBody` decoded body in `Body.Bytes()` as a
   `clierr.Usage` (exit 2), matching the existing undecodable-base64 (`:126`) and unknown-encoding
   (`:131`) refusals. `MaxBody` (`:34`) is referenced today only at `:258-260` inside `newBody` —
   the write side. `Bytes()` is reached from `history show` (`cmd/talaria/history.go:361`) and
   `history replay` (`:601`), i.e. from a hand-edited file.

**Do not remove the `//nolint:noctx` waiver at `internal/spec/source.go:122` in this task.** Its
debt is context and cancellation; a size bound does not discharge it. Task 16 removes it.

**Note for the malformed-line assertion.** An unparseable line survives every `Read` invisibly
(`store.go:196-206` `continue`s) and is only destroyed when `trim` rewrites the file
(`store.go:279-284` drops unreadable lines). So an over-cap line "skipped as malformed" is skipped
on read but silently deleted on the next trim. Assert whichever you intend; do not assume it
persists.

**Verify:** `go test ./...`

**Why:** `CLAUDE.md`'s standing rule: *"Bound every read, size-check before allocating … Failures
must be entry-level or request-level, never process-level."* Both of these are process death from
input the tool does not control.

---

### Task 8: Zero the config document that actually held the credential

**Depends on:** none

**Fixes finding:** 29

**Test files:**
- `internal/curl/config_test.go` (modify) — `TestBuildConfigCleanupZeroesTheDocument` exists and
  **passes today while the bug is live**. It asserts on the copy. Rewrite it to assert on the
  buffer the document actually built into.

**Implementation files:**
- `internal/curl/config.go` (modify) — `document.b` (`:128-131`), `BuildConfigWith` (`:114-124`),
  `cleanupWith` (`:380-389`), `discard` (`:368-370`), **and `cookies()` (`:243`)**, which builds a
  *second* `strings.Builder` holding every resolved cookie value and is never reset or zeroed at
  all. Same bug class, not mentioned in the finding.

**There is no seam to assert on yet.** `doc` is a local inside `BuildConfigWith`, so the existing
test can only see the returned copy. Creating that seam — a `[]byte` field the test can reach,
`document` exercised directly, or a test hook — is part of this task, not incidental to it. The
test is already in `package curl`, so an unexported seam is enough.

**Red — write failing tests:**
1. After `cleanup()`, the document's own backing storage contains no byte of the resolved
   credential. Today `config = []byte(doc.b.String())` copies and `doc.b.Reset()` sets
   `b.buf = nil` without zeroing, so `cleanupWith` zeroes the copy while the original heap buffer
   — holding the bearer token or `user:password` — stays readable until the GC reuses it.
2. The existing behaviour that the returned `config` slice is zeroed is preserved.
3. `discard()` zeroes as well — it is the error path, and error paths are where redaction bugs
   live (DESIGN.md §5a). It is reachable with a credential already in the buffer: `build` writes
   the resolved URL at `config.go:138` before the first error return at `:143`.
4. The cookie builder at `config.go:243` is zeroed too.

**Adversarial — what does hostile or malformed input do here?**
No external input reaches this code — it is an in-process memory-hygiene defect. The hostile
condition is a **failure part-way through building the document**: assert that a `build` error
after the credential has been written still zeroes the buffer, since the early-return path is the
one nobody tests.

**Green — minimal implementation:**
1. Replace `document.b strings.Builder` with a `[]byte` the document owns, appended to by
   `directive`/`flag`.
2. `cleanup`, `cleanupWith` and `discard` `clear()` that slice as well as the returned copy.
3. Fix **both** doc comments that assert this today: `config.go:85-88` (*"zeroes the document, so
   the resolved values do not linger in a buffer the rest of the process can still reach"*) and
   the narrower duplicate at `config.go:380-381`. Either they are true after this change, or the
   claims go. **Never write a comment asserting a property no test enforces** (`CLAUDE.md`).

**Verify:** `go test ./internal/curl/...` then `go test ./...`

**Why:** This is inside the one component DESIGN.md §5a designates as the sole holder of resolved
secrets, and the comment asserting the opposite is what made it invisible. The existing test's
name says it is covered; it is not.

---

### Task 9: Cut `internal/corpus`'s dependency on `internal/curl`, and guard it

**Depends on:** none

**Fixes finding:** 30

**Test files:**
- `internal/e2e/boundary_test.go` (modify) — the guard that does not exist
- `internal/corpus/store_test.go` (modify) — the tests construct a `*curl.Response` today

**Implementation files:**
- `internal/corpus/entry.go` (modify) — `NewEntry` takes a local struct
- `cmd/talaria/call.go` (modify) — `recordCall` fills it from `curl.Response`

**Red — write failing tests:**
1. `boundary_test.go` fails when `internal/corpus` imports `internal/curl`. It passes today
   because the guard covers only `internal/operation` and `internal/validate`; the `corpus → curl`
   edge is unguarded, so it would silently drag the executor into the twin at Phase 6.
2. `internal/corpus` builds and its tests run without importing `internal/curl` at all.

**Adversarial — what does hostile or malformed input do here?**
None — this touches no external input. It is a structural constraint. The adversarial question for
a *guard* is whether it can pass vacuously, and the file is better than the finding implies:
`boundary_test.go:58-61` already requires each package's own import path to appear in its own
`go list -deps` output, so a wrong package path fails loudly. **`internal/twin`'s absence is
deliberate and documented** at `boundary_test.go:28-31` (*"the twin lands in Phase 6. It is named
here anyway…"*) — do not "fix" it as an oversight.

The real vacuity gap is the other list: `DESIGN.md:285-286` names `operation`, `validate` **and
`gen`** as shared, but `shared` (`:16-19`) holds only the first two. `gen` was deleted, so the
forward-declaration discipline is applied to `forbidden` and not to `shared`. Make the *rule*
carry that distinction explicitly — every package named in a rule either exists, or is marked
not-yet-existing with the phase it arrives in — so the two lists cannot drift apart again, and a
typo cannot silently disable a rule.

**Green — minimal implementation:**
1. `NewEntry` (`internal/corpus/entry.go:151`) takes an `Observed{Status int; Headers
   map[string][]string; Body []byte; TimingMS int64}` declared in `corpus`. `cmd/talaria` fills it
   from `*curl.Response`. The dependency is **data-only** — `entry.go:170-177` reads exactly
   `Status`, `Headers` (twice: redacted, and for `contentType`), `Body` and `TimingMS`, and invokes
   no `curl` behaviour — so this is a shallow change. The single import to delete is
   `entry.go:24`. `curl.Response`'s `Headers` is `http.Header`, which is already
   `map[string][]string`, and `EntryResponse.Headers` (`entry.go:88`) is already the plain map
   type. `CLAUDE.md` states the rule: *"`corpus` may not import `curl` either: it takes a local
   observation struct, not a `*curl.Response`."*
2. Restructure `boundary_test.go`'s two flat slices into a table of (package, forbidden-imports)
   rules, keeping the existing two rules and adding `internal/corpus` → `internal/curl`,
   `internal/twin`. `internal/corpus` cannot simply join `shared`, because `corpus` is itself in
   the current forbidden list and the self-check would fail.

**Verify:** `go test ./...`

**Why:** `CLAUDE.md` requires that a package which must not cross a boundary is added to
`boundary_test.go` **in the same commit**. The rule was written down and never enforced.

---

### Task 10: Close the two holes in the canary gate

**Depends on:** Task 3 (the exit-5 path it scans), Task 2 (the withheld-credential path)

**Fixes findings:** 19, 20

**Test files:**
- `internal/canary/canary_test.go` (modify) — a validation-failure stage and a profile mechanism
- `internal/canary/testdata/canary.yaml` (modify) — an operation returning a schema-violating body

**Implementation files:** none. This task is entirely test-side — it is the gate that should have
caught several of the other findings.

**Red — write failing tests:**
1. A stage calling an operation whose response violates its schema, with a credential set and
   `--fail-on-error`, exits **4**, and neither the `validation.errors[]` messages on stdout nor
   the failure text on stderr contains the canary. The `stages` table (`canary_test.go:388-438`,
   seven entries) has no exit-4 case.

   **The comment to delete is at `canary_test.go:385-387`, not `:427`** (`:427` is the `curl exec`
   stage's `args` closure — leave it alone). It reads: *"A response-validation failure is not here
   because response validation is not built yet (plan tasks 26 and 27). Whoever adds
   --fail-on-error adds the case…"*. Its premise is **false against current `main`**:
   `validate.Response` (`internal/validate/validate.go:133`), the `--fail-on-error` flag
   (`cmd/talaria/call.go:215`) and the exit-4 return (`call.go:411`,`:414`) all exist and are
   already driven end to end at `internal/e2e/e2e_test.go:639`,`:744`,`:844`. So this is a stale
   waiver over a real hole, not a placeholder for unbuilt work.

   **There is no `run --report junit` comment in `internal/canary` — do not go looking for one.**
   The only `--report` mention is `canary_test.go:473`, about a different test, and `run` no longer
   exists as a command at all. The plan's earlier claim that a second comment needs deleting was
   wrong; delete one comment.
2. A `mechanism` whose `env` sets an arbitrarily-named variable and whose harness writes
   `profiles: {p: {auth: {bearerAuth: "${MY_TOKEN}"}}}`, driven through the same
   `call`/`history`/`replay` sequence as the env-var mechanisms. Every one of the five existing
   mechanisms sets a `TALARIA_AUTH_*` variable; the profile-reference path — one of the two
   credential sources DESIGN.md §5 names — is never leak-scanned.

**Adversarial — what does hostile or malformed input do here?**
This *is* the adversarial suite, so the question is whether it can pass vacuously.
1. The existing anti-vacuity guards (`mech.received(...)` must be true, stdout must be non-empty)
   must apply to the new mechanism too — a profile mechanism that silently failed to authenticate
   would leak nothing and pass. Assert the server actually received the credential.
2. The new validation stage must assert the canary is absent **and** that the validation error
   text is non-empty — a stage whose error message is blank scans nothing.
3. Confirm the profile file is written at the mode `config.Load` requires (0600); a mechanism that
   fails to load its profile would exercise the unauthenticated path instead.

**Green — minimal implementation:**
1. Add an operation to `testdata/canary.yaml` whose declared response schema the test server's
   body violates. **The fixture has no response schemas at all today** — all eight operations
   declare only `responses: "200"/"201": description: OK`, with no `content:` and no `schema:`.
   Model the new one on `internal/e2e/testdata/e2e-api.yaml:90` (`getBroken`), which is the
   working example of exactly this. Note `canary.yaml:8` points the server at
   `https://api.invalid/v1` deliberately, so the new stage must pass `--base-url` at the local
   `newServer` like every other stage does.
2. Add the stage to `stages` (`:388`) and the mechanism to `mechanisms` (`:99-151`).
3. Delete the one stale comment at `:385-387`.

For the profile mechanism, note the resolution path exists and is simply unscanned: `envRef`
(`internal/config/auth.go:314`) matches `${VAR}`/`$VAR` inside a profile `auth:` entry
(`config.Auth`, `internal/config/config.go:67`), and `grep '${' internal/canary/canary_test.go`
returns nothing today. The only profile-`auth:` use in the file is `:613`, which asserts a
**literal** value is refused with exit 2 — a different case, and one you must not break.

**Verify:** `go test ./internal/canary/...` then `go test ./...`

**Why:** DESIGN.md §5a: *"Test explicitly — error paths are where redaction bugs live."* The gate
that is supposed to make redaction regressions fail the build has no case for the error path the
design names, and never exercises one of the two credential sources.

---

### Task 11: Compaction — tasks 7–10

**Depends on:** Task 10

**Deliverable is negative.** No new behaviour, no new files, no new tests beyond ones that
replace several others. Green before and after; net-negative diff.

1. Read `.ralph/refactor-backlog.md` and drain it. Delete each entry as you resolve it; leave
   anything you deliberately did not do, with one line on why.
2. Read every file touched since Task 6.
3. Consolidate: tasks 7 and 8 both added size caps and buffer hygiene — one place for each
   constant, named and documented, not a literal per call site. Task 9 changed a struct that
   several tests construct — check the test helpers converged rather than each growing its own.
4. Move logic out of `cmd/talaria` and measure the non-comment line count before and after.
5. Delete superseded helpers and every comment asserting a property no test enforces.
6. Record the patterns in `CLAUDE.md`.

**Verify:** `go test ./...` green, lint clean, net line delta in the commit message.

---

### Task 12: Make `--output tsv` structurally valid

**Depends on:** none

**Fixes finding:** 8

**Test files:**
- `internal/output/render_test.go` (modify) — cell escaping in both renderers

**Implementation files:**
- `internal/output/render.go` (modify) — `tsvRenderer.Render`, `prettyRenderer.Render`

**Red — write failing tests:**
1. Two rows whose cells contain `\n` and `\t` render as exactly **two** lines with the **same**
   field count. Today `strings.Join(row, "\t")` is unescaped: one operation whose summary is
   `"line one\nline two\twith tab"` turns two operations into three rows with differing column
   counts, so `cut -f3` silently returns garbage with no way for the caller to detect it.
2. The pretty renderer's tabwriter alignment survives the same cells.
3. A cell with no special characters is byte-identical to today's output — this must not churn
   every existing tsv assertion.

**Adversarial — what does hostile or malformed input do here?**
Every cell that matters is spec-derived free text or recorded data, and the spec is untrusted
input. There are **seven** producers, not the three the finding names — enumerate them before you
choose an escaping scheme:
`list.go:104` (`op.Summary`), `search.go:66` (`r.Summary`/`r.Name`/`r.Where`),
`history.go:288-296` (recorded path and operation id), `history.go:331-346` (`history show`
headers and `bodyLine`, which returns `body.Data` **verbatim** at `:356-358` — the largest
unconstrained cell), `call.go:456-458` (the rendered curl command), `auth.go:104`, `uses.go:56`.
Plus `describe.go:192` (`describeRows`), which is a special case — see below.
1. `\r` alone, `\r\n`, and a lone `\n` — all three must be neutralised, not just `\n`.
2. A cell that is **only** a tab, and an empty cell — the field count must stay right.
3. A cell containing the escape sequence you chose (e.g. a literal `\t` two-character sequence) —
   the escaping must not be ambiguous with content that already looks escaped.
4. Terminal control characters and ANSI escapes in a summary, since pretty output goes to a TTY.
5. A multi-line cell in `history show`, whose cells are **request and response bodies** — the
   largest and least constrained cells in the tool.

**Green — minimal implementation:**
1. Escape or strip `\t`, `\r` and `\n` in every cell before joining, in one helper used by both
   the tsv renderer (`render.go:101-106`) and the pretty renderer (`render.go:121-126` — it joins
   with `"\t"` into a `tabwriter`, so it corrupts identically; this is not a tsv-only bug).
   Document the choice — escaping is reversible, stripping is not, and README promises *"bare
   tab-separated rows … for `cut` and `awk`"*.
2. **Two existing behaviours the helper must not break.** `describe.go:189-192` builds single-cell
   rows of multi-line prose and its comment asserts *"the tabwriter behind the pretty renderer
   passes a single-cell row through untouched"* — a blanket per-cell escaper changes `describe`
   output. Decide deliberately: exempt single-cell rows, or accept the change and update that
   comment and its tests (leaving the comment while breaking it violates `CLAUDE.md`).
   `fitSummaries` (`list.go:118+`, pretty only, `list.go:107`) computes column widths on
   *unescaped* cells, so escaping must run before it or the width budget drifts.

**Verify:** `go test ./...`

**Why:** The tool silently returns garbage to the exact consumer the format exists for, and there
is no way for that consumer to detect it.

---

### Task 13: Stop curl prompting on a TTY, and bound the version preflight

**Depends on:** none

**Fixes findings:** 10, 15

**Test files:**
- `internal/curl/config_test.go` (modify) — the basic-credential shape check
- `internal/curl/exec_test.go` (modify) — the preflight under a cancelled context

**Implementation files:**
- `internal/curl/config.go` (modify) — `document.auth`, the `user` directive
- `internal/curl/version.go` (modify) — `preflight` takes a context
- `internal/curl/exec.go` (modify) — pass the context to `preflight`

**Red — write failing tests:**
1. A resolved `TALARIA_AUTH_BASIC` containing no `:` is refused with
   `clierr.CredentialMissing` (exit 5) naming the variable, **without echoing the value**. Today
   curl reads the missing password from `/dev/tty` rather than the config pipe, so with stdout on
   a terminal the call blocks on an interactive prompt: 32 s wall clock, then a misleading
   *"curl outlived its 30s timeout"* that sends an agent looking for a slow API. DESIGN.md §3.1:
   *"Never prompt. Never page. No interactivity, ever."*
2. `user:` with an empty password (`user:`) is **accepted** — that is a valid credential and a
   different case from no colon at all.
3. `preflight` accepts a `context.Context` and honours it: with an already-cancelled context it
   returns promptly rather than running the subprocess. Every other curl in the tool is bounded by
   `execCtx` + `WaitDelay` + `Setpgid`; this one takes no context at all.
4. The `//nolint:noctx // phase-2a task 1` waiver is **removed** in this commit. It is at
   `internal/curl/version.go:45` (on the `exec.Command(path, "--version").Output()` line); `:42`
   is where its `TRACKED DEBT` comment begins. That comment does **not** contain the "remove this
   waiver in the commit that fixes it" sentence — that is the *spec* waiver at
   `internal/spec/source.go:121`, which Task 16 owns. Remove this one anyway: `golangci-lint` is
   the checker and an unused `nolint` is itself a finding.

**Adversarial — what does hostile or malformed input do here?**
`TALARIA_AUTH_BASIC` is human-supplied and `curl` on `PATH` is attacker-influenceable.
1. A basic value that is `:` alone, empty, or contains `\r\n` — the CRLF case must still hit the
   existing `checkSplit` gate, not the new colon check, or the error names the wrong problem.
2. A `curl` on `PATH` that is a **wrapper script that never exits** — the finding's motivating
   case. Assert the preflight gives up on its own short deadline (5 s) rather than wedging
   `talaria call` before it has done anything. A test can point at a script that sleeps.
3. `preflight` memoises with `sync.Once`. Assert a cancelled first call does not poison every
   later call in the same process with a cached failure — or, if it does, that the behaviour is
   deliberate and tested.

**Green — minimal implementation:**
1. In `document.auth` (`internal/curl/config.go:225-228`), reject a resolved basic credential
   containing no `:` before writing the `user` directive, matching the existing "its value is not
   echoed" convention. The only gate ahead of it today is `checkSplit` (`:221`), which checks CR/LF
   only; `internal/config/auth.go:35-36` merely *documents* the `user:password` shape and
   `resolve` (`config.go:394-409`) returns `Prefix() + value` unvalidated.
2. Change `preflight(path)` (`internal/curl/version.go:40`) to `preflight(ctx, path)` and use
   `exec.CommandContext` with a short independent deadline. `ExecuteWith` already holds one — it
   is called at `internal/curl/exec.go:62`, before the `execCtx`/`WaitDelay`/`Setpgid` machinery at
   `:82-95`, so the context is in scope at the call site.
3. Delete the waiver comment and the `//nolint` directive.

**Verify:** `go test ./internal/curl/...` then `go test ./...`

**Why:** Two ways for the tool to hang before it has sent anything, one of them reporting a cause
that is a lie. Both violate the "never interactive" principle an agent depends on.

---

### Task 14: Make Ctrl-C and SIGTERM work

**Depends on:** none

**Fixes findings:** 14, 17

**Test files:**
- `cmd/talaria/main_test.go` or a new `cmd/talaria/signal_test.go` (create/modify) — signal
  disposition
- `internal/request/body_test.go` (modify) — stdin read under a cancelled context
- `internal/corpus/store_test.go` (modify) — the lock deadline

**Implementation files:**
- `cmd/talaria/root.go` (modify) — restore default disposition after the first signal (`:206-207`)
- `internal/request/build.go`, `internal/request/body.go` (modify) — read stdin under the context.
  `Inputs` (`build.go:32-62`) has no context field and `Build(in Inputs)` (`:69`) takes none, so
  this is a signature change, not a plumbing tweak.
- `internal/corpus/lock_unix.go`, `internal/corpus/store.go` (modify) — bounded lock wait.
  `Store.Append` (`store.go:146`) and `lock` (`lock_unix.go:23`) take no context today.
- `cmd/talaria/call.go`, `cmd/talaria/history.go` (modify) — **threading a context into `Append`
  reaches both writers**: `recordCall` (`call.go:251`) and its call sites `call.go:177` and
  `history.go:225`. Budget for this; it is why the task looks smaller than it is.

**Red — write failing tests:**
1. After the first SIGINT, the **second** terminates the process. Today `signal.NotifyContext`
   installs a permanent `signal.Notify`; after the first signal its goroutine exits but the
   registration remains, so every later SIGINT/SIGTERM is delivered to a buffered channel and
   discarded — default termination is disabled for the whole run.
2. `--body -` with stdin a stalled pipe returns when the context is cancelled. Today
   `io.ReadAll(b.in.Stdin)` ignores the context: Ctrl-C does nothing and `kill -TERM` does
   nothing; only Ctrl-D or `kill -9` gets out.
3. `Append` against a lock another process holds fails with the existing *"cannot lock the
   history file"* error after a bounded wait, rather than blocking forever. `flock(2)` with
   `LOCK_EX` and no `LOCK_NB` is not interruptible by a signal either, because Go installs
   handlers with `SA_RESTART`.
4. A bounded lock wait costs a **warning line, not a silent loss**: `recordCall` already
   downgrades an `Append` failure to a stderr warning. Assert the warning appears and the call's
   own result is unaffected.

**Adversarial — what does hostile or malformed input do here?**
The stalled peer is the hostile input here: another process, or a filesystem, that never
progresses.
1. **A stale lock file on a mount that never answers** — the deadline must be wall-clock bounded,
   not dependent on the filesystem returning.
2. **A stdin that yields bytes forever** — combine with Task 7's bounds: a cancellable read that
   is still unbounded is a different way to die. Assert both hold together.
3. **Signal during each phase** — before curl spawns, while curl runs, and while the lock is
   held. Assert no orphaned curl process and no partially written history line in any of them.
4. **A test that hangs is a test that fails.** Give every one of these an explicit short timeout
   so a regression shows up as a failure, not as a suite that never returns. Note the race
   workflow runs the suite under `-race`, which is ~9× slower — pick deadlines that survive that.

**Green — minimal implementation:**
1. `cmd/talaria/root.go`: after `signal.NotifyContext`, add
   `go func(){ <-ctx.Done(); stop() }()` so the second signal gets the default disposition.
2. Read stdin under the context in `internal/request/body.go` (`b.in.Stdin` already comes from
   the command; thread the context through `Inputs`).
3. `internal/corpus/lock_unix.go`: loop on `LOCK_EX|LOCK_NB` with a short sleep against a bounded
   deadline and the caller's context, then return the existing error. Thread a context into
   `Store.Append`. Replace the comment at the top of `lock` justifying the unbounded wait — it
   argues a bounded wait *"would drop history the caller was told had been recorded"*, which is
   not true given `recordCall`'s warning.

**Verify:** `go test ./...`

**Why:** Finding 14 is the worst of the hang set: Ctrl-C and `kill -TERM` both do nothing. Under
systemd or CI a SIGTERM shutdown hangs until the SIGKILL timeout.

---

### Task 15: Stop the tool lying about history

**Depends on:** none

**Fixes findings:** 23, 24, 31

**Test files:**
- `internal/corpus/store_test.go` (modify) — `Append`'s truthfulness, `storedIDs`' errors
- `cmd/talaria/history_test.go` (modify) — the `timing_ms` view

**Implementation files:**
- `internal/corpus/store.go` (modify) — `Append`/`trim`, `storedIDs`
- `cmd/talaria/history.go` (modify) — `historyEntryView.TimingMS`

**Red — write failing tests:**
**Read this before writing the reproductions — all three obvious ones do not reproduce.** The
review's suggested repros were checked against the code and defeated by it. Each red test below
carries the repro that actually works. If yours goes green before your change, it is one of the
three below, not a sign the bug is absent.

1. When `trim` fails **after** the line is durably on disk, `Append` does not report failure for
   an entry that was written. Today `write` succeeds (`store.go:169`) and `return trim(path)`
   (`:173`) propagates the trim error, so `recordCall` prints *"warning: the call was not recorded
   in history"* — the opposite of what happened. An operator acting on that warning re-runs a
   mutating call.
   **Repro, corrected:** a read-only directory does **not** work — `write` unconditionally
   `os.Chmod(dir, dirMode)`s it back to 0700 for the owner (`store.go:268`) before writing. And
   `trim` only reaches `replace` when one source exceeds `maxPerSource = 1000` (`store.go:78`,
   `:313-320`), so a small-store test never enters the failing path. You need **>1000 entries of
   one source** plus a failure injected inside `replace` (`store.go:345`) — which most likely
   means introducing a seam. That seam is legitimate work for this task.
2. A genuine read failure in `storedIDs` — not `fs.ErrNotExist` — **aborts the append** rather
   than treating the store as empty. Today all errors collapse to `return nil` (`store.go:207-211`),
   every candidate id looks free, `uniqueID` returns an id an entry already holds, and `write`
   appends the duplicate. `Read` (`store.go:234-240`) already distinguishes `fs.ErrNotExist`
   properly — that is the precedent to copy.
   **Repro, corrected:** mode 0400 does **not** work — the owner can still read, so `os.ReadFile`
   succeeds and the failure never happens. Mode 0000 (as non-root) or a directory in place of the
   file fails the read, but then `write`'s `os.OpenFile` (`store.go:272`) fails too and `Append`
   errors for a different reason. The duplicate-id outcome needs a *transient* read failure, so
   inject it at a seam rather than through the filesystem.
   **And do not assert the overstated impact.** `history show` and `history replay` both resolve
   through the same `selectEntry` (`history.go:153` and `:196`), so against a static store they
   pick the **same** entry — an assertion that they disagree will not reproduce. The real defects
   are that the older duplicate becomes permanently unreachable by id, and that anything appending
   between the show and the replay changes which entry the id names. Assert those.
3. A call that rounds to 0 ms still shows `"timing_ms":0` in `history` list output.
   `historyEntryView.TimingMS` (`cmd/talaria/history.go:48`) is `int64,omitempty`, so a call
   against a local service is indistinguishable from no response observed. The stored
   `EntryResponse.TimingMS` (`internal/corpus/entry.go:90`) correctly has no `omitempty`, and
   `responseView.TimingMS` (`cmd/talaria/call.go:55`) is a third tagging of the same value — plain
   `int64`, no `omitempty`. The plan previously cited `runResult` as the precedent; **that type no
   longer exists** (`run` is gone). The live precedent is the test-side struct at
   `cmd/talaria/call_test.go:38-40`, whose own comment says *"A pointer so an absent timing_ms is
   distinguishable…"*. Converge all three views deliberately, or say in the commit why not.
4. An entry with no response still omits `timing_ms` entirely.

**Adversarial — what does hostile or malformed input do here?**
The history file is edited outside this process, and the filesystem is the other unreliable input.
1. **A full filesystem**: `write` succeeds for a small line, `replace`'s `CreateTemp` fails —
   reachable only past the 1000-entry cap, per red-test 1. Assert the warning matches reality.
2. **An `EIO`-shaped transient read failure** in `storedIDs`. Distinguish `fs.ErrNotExist` from
   every other error, as `Read` already does. (A mode change alone does not get you there — see
   red-test 2.)
3. **Two entries hand-edited to share an id.** Assert the ambiguity is reported rather than
   silently resolved to the newest — an agent replaying by id must not get a different request
   than it was shown.
4. Concurrent appends still keep every entry they acknowledged
   (`TestConcurrentAppendsKeepEveryEntryTheyAcknowledged` must stay green under `-race`).

**Green — minimal implementation:**
1. Return a distinguishable error from the trim stage, or report the trim failure separately and
   return nil once the line is on disk. The caller's warning must say what actually happened.
2. In `storedIDs`, distinguish `fs.ErrNotExist` from other errors and abort the append on a
   genuine read failure.
3. Make `historyEntryView.TimingMS` a `*int64` set only when `entry.Response != nil`, and fix the
   comment at `cmd/talaria/history.go:46-48` claiming it is *"omitted along with Status"*. The
   false part is "along with": `Status` also carries `omitempty` but a real status is never 0
   (`internal/curl/exec.go:154` rejects `http_code == 0`), so only `TimingMS` can be wrongly
   dropped.

**Verify:** `go test ./...`

**Why:** Both findings make the tool lie in the direction that costs the most: one says a
mutating call was not recorded when it was, the other lets replay send a different request than
the one shown.

---

### Task 16: Give the spec cache a TTL, a conditional revalidation and a bypass

**Depends on:** none

**Fixes finding:** 25

**Test files:**
- `internal/spec/source_test.go` (modify) — TTL, 304, `--refresh`, and the hostile cases below
- `cmd/talaria/flags_test.go` (modify) — `--refresh` is registered where every spec-taking
  command can reach it

**Implementation files:**
- `internal/spec/source.go` (modify) — cache metadata, conditional GET, context threading
- `cmd/talaria/root.go`, `cmd/talaria/list.go` (modify) — the `--refresh` flag, `loadSpec`

**Red — write failing tests:**
1. Inside 24 hours a URL-sourced spec is served from cache with **no network call** (assert the
   test server's request count).
2. Past 24 hours it is revalidated with a conditional GET carrying `If-None-Match` /
   `If-Modified-Since`; a **304** refreshes the timestamp without re-downloading, and the cached
   bytes are still what loads.
3. A **200** on revalidation replaces the cached bytes.
4. `--refresh` forces a fetch regardless of age.
5. The cache is written with the response's `ETag`/`Last-Modified` alongside the bytes. Today the
   cache is keyed on `sha256(url)` with no metadata at all and is served forever: every
   downstream stage works against a possibly-stale contract, most damagingly `validate`, where a
   schema violation the server never committed is indistinguishable from a real failure.
6. The `//nolint:noctx // phase-2a task 1` waiver at `internal/spec/source.go:122` is **removed**
   in this commit — threading a context through `Load` is what its comment says the fix requires.
   All six call sites already hold a `*cobra.Command`, so `cmd.Context()` is in scope; the change
   is confined to `loadSpec`/`loadIndex` in `cmd/talaria/list.go` and this package.

**Adversarial — what does hostile or malformed input do here?**
The cache directory is on disk and the server is remote; both are outside this process.
1. **A corrupt or truncated cache entry** — assert it is discarded and refetched, not parsed into
   a half-spec. The existing comment says parsing happens before caching for this reason; the
   *read* side has no equivalent guard.
2. **A hostile `ETag`** — a server-controlled string that goes back out in a request header. It
   must not be able to inject a header (CRLF), and must be length-bounded. This is the same class
   as the media-type vector in Task 4.
3. **A cache entry whose metadata is hand-edited** — a `Last-Modified` far in the future, a
   negative age, a clock that moved backwards. A stale-forever or refetch-every-time outcome are
   both wrong; assert which one you chose.
4. **A 304 with a body**, and a 304 for a cache entry that no longer exists on disk.
5. Combine with Task 7: a revalidation returning an oversized body must still hit the read cap.
6. Cache files must stay 0600 in a 0700 directory (`TestLoaderWritesCacheFilesPrivately`).

**Green — minimal implementation:**
1. Store the `ETag`/`Last-Modified`/fetch-time beside the cached bytes (a sidecar file or a small
   header prefix — pick one and say why in a comment). Today the cache is
   `os.UserCacheDir()/talaria/specs/<sha256-hex-of-url>` holding the **raw bytes only** — no
   metadata, no extension, no index — dir 0700, file 0600, written `CreateTemp`+`Chmod`+`Rename`
   (`internal/spec/source.go:143-156`, `:170-190`). Whatever you add keeps those modes.
2. Serve from cache inside 24h; past that issue a conditional GET; treat 304 as a timestamp
   refresh. Implement DESIGN.md §4's policy **as written** — it is settled, do not invent another.
3. **Add the read-side guard the adversarial section calls for — it is not free today.** `loadURL`
   (`source.go:86-90`) parses the cached bytes and *returns the parse error*; nothing refetches, so
   one corrupt cache file wedges that URL permanently. The existing comment at `source.go:97-98`
   explains that parsing happens before *writing* for exactly this reason; the read side has no
   equivalent. On a cache parse failure, discard the entry and refetch.
4. Add `--refresh` and thread `context.Context` through `Load` (`source.go:62`), `Loader.Load`
   (`:68`), `loadURL` (`:82`), `fetch` (`:111`) and `LoadFile` (`load.go:48`), using
   `http.NewRequestWithContext`. The six `loadSpec`/`loadIndex` call sites (`list.go:60`,
   `uses.go:36`, `describe.go:73`, `search.go:47`, `auth.go:59`, `call.go:120`) are all inside a
   `RunE`, so `cmd.Context()` is in scope, and `loadSpec`/`loadIndex` (`list.go:170`/`:161`)
   already take `cmd`. Remove the waiver at `source.go:122`.

**Verify:** `go test ./internal/spec/...` then `go test ./...`

**Why:** An unbounded cache means every downstream stage silently works against a contract the
server has moved off, and the design doc says so: *"a stale one makes `validate` report
violations the server never committed."*

---

### Task 17: Compaction — tasks 12–16

**Depends on:** Task 16

**Deliverable is negative.** No new behaviour, no new files, no new tests beyond ones that
replace several others. Green before and after; net-negative diff.

1. Read `.ralph/refactor-backlog.md` and drain it. Delete each entry as you resolve it; leave
   anything you deliberately did not do, with one line on why.
2. Read every file touched since Task 11.
3. Consolidate: tasks 13, 14 and 16 all threaded a `context.Context` through a package that did
   not take one — check the signatures converged on one shape rather than three. Tasks 7 and 16
   both bound a read of a remote body; one cap, one place.
4. Move logic out of `cmd/talaria` and measure the non-comment line count before and after.
   Tasks 14 and 16 both added flags and plumbing there.
5. Delete superseded helpers and every comment asserting a property no test enforces — including
   the two lock and cache comments tasks 14 and 16 were told to fix, if they were not.
6. Record the patterns in `CLAUDE.md`.

**Verify:** `go test ./...` green, lint clean, net line delta in the commit message.

---

### Task 18: Audit comments that assert an invariant no test enforces

**Depends on:** Task 17

**Fixes:** the design document's *"one task with no finding number"*, and the root cause behind
findings 19, 29, 30 and 31

**Test files:** wherever the surviving claims live — this task's output is tests *or* deletions.

**Implementation files:** the comments themselves, across `internal/` and `cmd/`.

This task has no red/green cycle of its own. Four findings shared one shape: **a comment asserts
a property the code does not have.** `config.go` claimed a buffer was zeroed; `boundary_test.go`
claimed to guard an import it did not; `canary_test.go` said *"whoever adds `--fail-on-error`
adds the case"* after it had been added. A finding-by-finding loop cannot catch these — there is
no line number to anchor on.

**Steps:**
1. Grep non-test Go files for comments asserting a guarantee: *never*, *always*, *cannot*,
   *is zeroed*, *must hold*, *guaranteed*, *validated upstream*, *callers must*, *by
   construction*, *so a … cannot*. About 147 comment lines match the broad pattern; the strongest
   candidates cluster in `internal/curl/exec.go` (capture-file mode and lifetime; the WaitDelay
   and PATH claims), `internal/corpus/entry.go` and `store.go` (the discarded `req.URL` error, the
   lock-discipline precondition, the atomic-replace claim), `internal/secret/redact.go` (the
   glob-cannot-fail and nil-receiver claims), `internal/curl/render.go` (the structural claim that
   nothing outside the package can reach `Resolve` — which `boundary_test.go` does **not**
   enforce), `internal/spec/source.go` (the concurrent-reader claim), and
   `internal/clierr/clierr.go` (marshalling cannot fail).
2. For each, decide: **write the test, or delete the claim.** Both are correct outcomes; leaving
   it is not. A structural claim (X cannot import Y, only Z can call W) belongs in
   `internal/e2e/boundary_test.go`, not in prose.
3. Pay particular attention to comments that justify a **discarded error** — `entry.URL, _ =
   req.URL(request.Redacted)` is justified by a claim that the call cannot fail. Either that is
   tested or the error is handled.
4. Record in `CLAUDE.md` that this rule is now enforced by audit, and note which claims you
   converted to tests so a later reviewer can find them.

**Adversarial — what does hostile or malformed input do here?**
Where a comment claims a *defence* — "this cannot be a regexp injection", "a symlink is never
followed", "a value cannot carry CRLF because the layer below re-checks it" — the test you write
must be the hostile one. Two of the claims found are mutually referential: `build.go` says CRLF
is rejected because `internal/curl` re-checks every pair as the last gate, and `internal/curl`
says the same in reverse. Assert **both** layers independently, by calling each directly with a
CRLF-bearing value; a defence that exists only in two comments pointing at each other is the
shape Task 4's finding took.

**Verify:** `go test ./...` and lint clean. Report the count of claims tested vs. claims deleted
in the commit message.

**Why:** `CLAUDE.md`: *"Never write a comment asserting a property no test enforces … Four review
findings were comments that documented an intention as if it were an invariant."* This is the
task that stops the fifth.

---

### Task 19: Stop re-reading the whole history file twice per append

**Depends on:** Task 15 (it rewrites the same functions)

**Fixes finding:** 16

**Cut this task first if the phase runs long.** The design document says so explicitly: `run` was
its pathological case and `run` is gone. Do not cut it silently — say so in the PR. **Task 20's
dependency on this task is ordering only** — if this is cut, Task 20 still runs.

**It also depends in practice on Tasks 7, 14 and 15**, which all rewrite these same functions:
Task 7 bounds the three `os.ReadFile`s, Task 14 threads a context into `Append`/`lock`, Task 15
changes `storedIDs`' error handling and `trim`'s failure reporting. Fold your single pass into
what they left; do not restore what they removed.

**Test files:**
- `internal/corpus/store_test.go` (modify) — one full parse per append, and the cap still holds

**Implementation files:**
- `internal/corpus/store.go` (modify) — `Append`, `uniqueID`/`storedIDs`, `trim`

**Red — write failing tests:**
1. Appending to a store already holding many entries parses the file **once**, not twice. Confirmed
   current behaviour, all inside the one `flock(LOCK_EX)` taken at `store.go:156` and held to the
   end: `uniqueID` (`:162`) → `storedIDs` (`:191`) → `os.ReadFile` (`:208`) + per-line `lineHead`
   (`:214-218`); then `write` (`:169`); then `trim` (`:173`) → `os.ReadFile` again (`:295`) +
   per-line `lineHead` (`:304-310`), **unconditionally** — `trim` returns early at `:318-320` only
   *after* the full parse. Two reads, two parses. Assert this at the observable level (a counter on
   a seam you introduce, or a bounded number of reads), not by timing.
2. `trim` runs only when the file could be over cap. Assert an append to a small store performs
   no trim scan.
3. The per-source cap still holds exactly: `TestAppendCapsEachSourceSeparately` and
   `TestAppendTrimsOldestFirst` stay green.

**Adversarial — what does hostile or malformed input do here?**
The optimisation's risk is that a cheap pre-check disagrees with the expensive truth.
1. **A file whose size suggests it is under cap but whose entry count is over** — many tiny
   entries. Assert the cap still holds, i.e. the size check is a lower bound that never skips a
   needed trim.
2. **Unparseable lines mixed in** — they are skipped by `Read` and dropped by `trim`, so a
   line-count heuristic and the real entry count differ. Assert the cap counts entries, not lines.
3. **Concurrent appends from two processes** while the single-pass version runs, under `-race`.
4. A hand-edited file with all entries under one source, and one with an unknown source value.

**Green — minimal implementation:**
1. Run `trim` only when the file could be over cap (an `os.Stat` size check or a cheaply
   maintained count), and fold `storedIDs`' scan and `trim`'s scan into a single pass.

**Verify:** `go test ./internal/corpus/...` then `go test ./...`

**Why:** Quadratic work under a cross-process exclusive lock, which compounds the lock contention
Task 14 bounds. Lowest priority in the phase.

---

### Task 20: Verify the phase end to end

**Depends on:** Task 19

**Fixes:** nothing new. This is the cross-component check that the pieces actually meet.

**Test files:**
- `internal/e2e/e2e_test.go` (modify) — one session exercising the phase's changes together
- `internal/canary/canary_test.go` (modify) — only if a surface is still unscanned

**Implementation files:** none expected. If this task needs production code, a earlier task was
incomplete — fix it there and say so.

**Red — write failing tests:**
1. **One session, spec → call → history → replay**, against a spec whose `servers[]` carries a
   **server variable** (Task 1), with a credential set (Task 3), a body from a file (Task 5), and
   `--base-url` pointing at the test server. Assert: the substituted host is what the allowed set
   was computed from; the credential reaches the server only when the host is allowed; the
   envelope carries `credentials_withheld` when it is not; the recorded entry replays through the
   spec to the **spec's** host; the replay emits a `validation` block.
2. Assert component N's output actually reaches component N+1 — the recorded entry's
   `operation_id` resolves in the spec, and the id `history show` displays is the id
   `history replay` re-issues (Task 15's duplicate-id fix).
3. Run the canary value through this whole session and scan every surface: stdout, stderr,
   history store, spec cache. The session-level scan is `TestNoStepOfTheWorkflowLeaksTheCredential`
   — extend it rather than writing a parallel one.

**Adversarial — what does hostile or malformed input do here?**
Every trust boundary this phase touched, in one place:
1. The spec is hostile (a server variable rewriting the authority, a CRLF media type), the history
   file is hostile (a hand-edited host, a `<redacted>` body, a duplicate id), and the flags are
   hostile (`--base-url` at a listener, `--allow-host` junk). Drive at least one of each through
   the full session and assert the failure is entry- or request-level with the documented exit
   code — never a panic, never a process-level death.
2. Assert the exit-code contract still holds across the whole spec
   (`TestTheExitCodeContractIsObservableAcrossOneSpec`), including the new exit-5 path from
   Task 3.

**Verify:** `go test ./...`, then the full lint command, then
`go test -race -count=1 ./...` — the race workflow gates the branch and this is the last chance
to see it fail cheaply.

**Why:** *"Do not assume component N's output reaches component N+1."* Tasks 1 and 2 are split
across four packages, and the phase's whole premise is that a green suite proved nothing last
time.

---

### Task 21: Compaction — tasks 18–20 and the phase

**Depends on:** Task 20

**Deliverable is negative**, and this is the phase-closing pass.

1. Read `.ralph/refactor-backlog.md` and drain it completely. Anything left must carry one line
   on why it was deliberately deferred — that list goes in the PR body.
2. Read every file touched in the phase. `git diff main...HEAD --stat` is the work list.
3. Consolidate across the whole phase, not just the last three tasks. The duplication that
   survives compaction is the duplication no single earlier compaction could see.
4. Measure `cmd/talaria`: it was 1,589 of 5,511 non-comment production lines (28%) at the start of
   the phase. Report the number now. If it grew, move logic down until it did not.
5. Delete every comment asserting a property no test enforces that Task 18 did not reach.
6. Confirm the phase's own hygiene: no `//nolint` waiver tagged `phase-2a task 1` survives; no
   `REVIEW_FINDINGS.md`-shaped file was created at the repo root; `internal/ci/workflow_test.go`
   still passes, meaning `.ralph/stack.json` and `.github/workflows/ci.yml` still agree.
7. Update `CLAUDE.md` with every pattern this phase established, and update the scope note if
   anything moved.

**Verify:** `go test ./...` green, `test -z "$(gofmt -l .)" && go vet ./... && golangci-lint run ./...`
clean, `go build ./...` clean, and the net line delta in the commit message.
