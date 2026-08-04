# Review findings — phase-2a-experiment (cycle 3)

Base: `main`. Diff: `git diff main...HEAD` — 13 commits, tasks 1–6 of 21 in `tasks.json`.
Reviewers: security, spec-compliance, concurrency, integration. Every finding below was reproduced
against the built binary and, where it touches the wire, against a raw-socket capture listener.

**Verified fixed — do not re-litigate.** Every headline limb of prior findings 11, 1, 2, 3, 22, 4,
21, 5 and 6 holds at wire level: server-variable substitution works; an off-spec `--base-url`
withholds the scheme-resolved credential (confirmed absent from the bytes the server received),
reports `credentials_withheld` and one stderr line, all three every time; `--allow-host` and profile
`allow_hosts:` both put it back; `allow_hosts` parses under `KnownFields(true)`; replay re-derives
through `loadSpec → Lookup → request.Build → config.Resolve`, emits a `validation` block and exits 2
on a missing operationId, a drifted method, an off-set stored host, a `file://` scheme, userinfo, a
CRLF media type and a placeholder-bearing body; `replayableEnv`/`ReferencesEnv` are absent from the
tree; the oauth2 matrix matches DESIGN.md §5 row for row; `--body @file` renders `--data '@path'`
and `--body -` renders `--data '@-'`, both genuinely canary-gated; `newInvocation` is built once per
RunE in all four request-touching RunEs. `go build`, `go vet`, `gofmt`, `golangci-lint` and
`go test -race -count=1 ./...` are all clean on the tree as committed.

Below is what those fixes did not close, plus five regressions they introduced.

## Finding 1: A stored history header supplies the credential a replayed request authenticates with
- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** cycle 2 findings 2 and 3
- **File:** internal/replay/replay.go:287
- **Description:** `Inputs.headers` forwards every recorded header into `request.Build` as a
  `--header` flag, filtered only by `isRedacted`, which recognises just the two shapes talaria
  itself writes (`<redacted>`, `<redacted:env:NAME>`). A history file that was hand-edited, copied
  from another machine or written by another project — DESIGN.md §5a's stated threat model, restated
  in CLAUDE.md as "the spec is untrusted input, and so is the history file" — can therefore put a
  literal `Authorization: Bearer …` on the request. `binder.credentials` appends the re-resolved
  credential *second*, and HTTP semantics (`r.Header.Get`, most proxies and frameworks) take the
  first. DESIGN.md §5a's replay table, row 2, reads *"Credentials — **Never taken from the
  entry.**"* The file decides which credential authenticates the call. Because `hide()` marks any
  `Authorization`-named pair sensitive, the envelope renders the pair as
  `"<redacted>, Bearer <redacted:env:TALARIA_AUTH_BEARER>"`, so the caller cannot detect it. The
  duplicate `Authorization` is also a request-smuggling desync primitive across a proxy chain.
  The earlier fix re-derived host, path, params and body but left the header set a raw pass-through,
  so the one row of §5a's table that names credentials is still unmet.
- **Evidence:** recorded a call, edited `request.headers` to `{"Authorization":"Bearer
  ATTACKER-TOKEN"}`, replayed with `TALARIA_AUTH_BEARER=CANARY-REAL`. Raw socket capture:
  ```
  GET /thing HTTP/1.1
  Authorization: Bearer ATTACKER-TOKEN
  Authorization: Bearer CANARY-REAL
  ```
  Exit 0, stderr empty.
- **Suggested fix:** In `headers`, drop any recorded header whose *name* is credential-shaped by the
  same rule `hide`/`secret.Redactor` already applies (`Authorization`, `Cookie`,
  `Proxy-Authorization`, `*api*key*`, `*token*`, `*secret*`), redacted-looking or not, and
  `warnUnreplayable` on it. A credential position in an entry is a position, not a value; the
  existing check tests the value's *shape*, which is exactly what an editor controls.

## Finding 2: A credential talaria marks sensitive but did not resolve from a scheme is sent to any host
- **Reviewer:** spec-compliance, security
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** cycle 2 finding 1
- **File:** internal/request/build.go:541
- **Description:** `binder.credentials` populates `Withheld` by iterating `b.in.Creds` only. A value
  talaria itself classifies as a credential by §5a's built-in name list and hides on every display
  surface via `hide()` (`build.go:381`) is not in that set, so it is transmitted to a host the spec
  does not declare with no `credentials_withheld` entry and no stderr line. The highest-risk source
  is a profile's `headers:` map — human-authored, never seen by the agent, exactly the asymmetry §1
  exists to protect — and `--base-url` is agent-controlled and overrides the profile's own
  `base-url`. DESIGN.md §5a: "When `--base-url` points outside that set the request still runs, but
  **every credential is withheld**". Worse than silent: the tool prints
  `warning: bearerAuth withheld from …`, telling the operator the credential was protected, while a
  second one on the same request was not; the envelope shows only `<redacted>`, so they cannot see
  what left.
- **Evidence:** profile `prod` with `headers: {X-Api-Token: PROFILE-TOKEN-CANARY}` and no
  `allow_hosts`; spec declares `https://api.example.com`; agent passes
  `--base-url http://127.0.0.1:9098`.
  ```
  stderr : warning: bearerAuth withheld from 127.0.0.1:9098 …
  stdout : "headers":{"X-Api-Token":"<redacted>"}   (no second credentials_withheld entry)
  wire   : X-Api-Token: PROFILE-TOKEN-CANARY
  ```
- **Suggested fix:** When `!allowed.Allows(req.BaseURL)`, also withhold every `Pair` in
  `req.Headers`/`req.Query`/`req.Cookies` whose `Value.IsSensitive()` is true, recording each in
  `Withheld` with its own reason. That follows §5a's literal words ("every credential") applied to
  the tool's own definition of a credential; no policy needs inventing. If the team instead intends
  the host rule to cover scheme-resolved credentials only, DESIGN.md §5a must say so, because as
  written it says the opposite.

## Finding 3: `history replay` routes declared header and cookie parameters by transport, so it drops or refuses them
- **Reviewer:** concurrency, spec-compliance, security, integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none — regression introduced by task 2's rewrite
- **File:** internal/replay/replay.go:95
- **Description:** `Build` routes recorded values into `request.Inputs` by transport — path and query
  into `Params`, headers into `Headers`, cookies nowhere — while `request.Build` binds declared
  parameters *by declaration*: `binder.params()` reads only `Inputs.Params`, and `req.Cookies` comes
  only from `b.located(bound, inCookie)` (`internal/request/build.go:91`). There is no
  `Inputs.Cookies` field. Three failures follow from the one mistake.
  (a) **A required header or cookie parameter makes replay impossible.** `binder.params()`'s
  required check never sees the value, so replaying an entry talaria itself just wrote dies at exit
  2 and nothing is sent. `history replay` has no `--param` flag, so there is no workaround. For
  cookies it is permanent: `corpus.Redactors.cookies` (`internal/corpus/entry.go:215`) stores every
  non-secret cookie value as `<redacted>` unconditionally, so no recorded cookie can ever come back.
  (b) A declared, non-redacted cookie in a hand-edited or foreign entry takes neither branch of the
  loop at `replay.go:293-297`: no binding, no warning, no envelope field. The replay is a different
  request than the one `history show` displays and the caller has nothing to detect it by.
  (c) The comment at `replay.go:291-292` asserts these cookies are "bound below by name, alongside
  the path and query parameters". No such binding exists anywhere in the package — the CLAUDE.md
  rule against comments asserting properties no test enforces.
  DESIGN.md §5a's table says params are "Re-bound through the normal request-construction path"; for
  two of the four parameter locations they are not.
- **Evidence:** verified end to end with the shipped binary.
  ```
  $ talaria call req.yaml getThing --param X-Tenant=acme     # 200, stored {"X-Tenant":"acme"}
  $ talaria history replay 1 --spec req.yaml
  {"error":{"code":2,"message":"cannot build a request for getThing: --param X-Tenant is required (header parameter)"}}

  $ talaria call ck.yaml getThing --param session=s3ss10n    # 200, stored {"session":"<redacted>"}
  $ talaria history replay 1 --spec ck.yaml
  warning: the recorded cookie "session" held a redacted value …
  {"error":{"code":2,"message":"cannot build a request for getThing: --param session is required (cookie parameter)"}}
  ```
  For (b), a throwaway test against `internal/request/testdata/request.yaml` (`getPet`,
  `flavour in: cookie`): rebuilt request `Cookies = []request.Pair(nil)`, stderr empty, no error.
  An optional header parameter is unaffected and replays correctly.
- **Suggested fix:** Route every recorded value whose name `declaredParams` maps to `header` or
  `cookie` into `Params` as `name=value`, exactly as declared query parameters already are at
  `replay.go:259`; keep `Headers` for the undeclared remainder, and warn only for what genuinely
  cannot come back. Note that (a)'s cookie half also needs a decision about
  `corpus.Redactors.cookies`, which makes a declared cookie parameter unrecoverable by design.

## Finding 4: A spec's `securitySchemes.name` reaches the emitted curl unchecked — `--dry-run` exits 0 with a CRLF-injected reproduction
- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** cycle 2 finding 5
- **File:** internal/request/build.go:550
- **Description:** `binder.credentials` builds `Pair{Name: cred.Name}` where `cred.Name` is
  `components.securitySchemes.<x>.name` (`internal/config/auth.go:380`) — untrusted spec text.
  `binder.located` runs `isFieldName` + `SplitsRequest` on a declared *header parameter's* name;
  `binder.credentials` runs neither. `curl.Render` was hardened in this branch for
  `Body.ContentType` (`render.go:50`) but not for header names. Only `checkSplit` in the config
  document catches it, at exec time. So `call` exits 2 but `call --dry-run` exits 0 and prints a
  "portable reproduction" (DESIGN.md §3.4) carrying a raw CRLF inside a double-quoted,
  shell-expanding word. Pasted, it delivers the real API key under an attacker-chosen header name
  and can terminate the header block. This is CLAUDE.md's own rule — "bind time, the config
  document, and the emitted curl" — applied to one spec-derived header value and not the second one.
  **Note:** this reproduces on `main` too, so it is left-unfixed rather than introduced. It is in
  scope because task 4 claimed the three-surface sweep and codified it as a house rule.
- **Evidence:** spec with `name: "X-Key: v\r\nX-Injected: pwned"`,
  `TALARIA_AUTH_APIKEY_EVIL=REAL-KEY-CANARY`:
  ```
  $ talaria call inject.yaml getThing --dry-run --output json     # exit 0, stderr empty
  "curl":"curl -q -s -H \"X-Key: v<CR><LF>X-Injected: pwned: $TALARIA_AUTH_APIKEY_EVIL\" 'http://…'"
  $ talaria call inject.yaml getThing                             # exit 2, checkSplit catches it
  ```
- **Suggested fix:** Apply `isFieldName` + `SplitsRequest` to `cred.Name` in `binder.credentials`
  and fail exit 2 as `located` does, and have `curl.Render` return `""` for a request carrying a
  splitting header name, as it already does for `Body.ContentType`.

## Finding 5: `auth check` reports a scheme satisfied for a `--base-url` that `call` refuses outright
- **Reviewer:** integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** cycle 2 finding 1 (limb)
- **File:** internal/request/hosts.go:109
- **Description:** `Target` discards `ResolveBaseURL`'s error and returns `""`, and
  `credentialsWithheld` (`cmd/talaria/auth.go:114`) reads `target == ""` as "no host to withhold
  from" and reports the plain result. A `--base-url` that is not http(s), or that carries userinfo,
  therefore makes `auth check` print `present:true` with no `withheld` and exit 0, while `call` with
  the identical flags exits 2. DESIGN.md §5 states the contract as non-negotiable: *"**`auth check`
  never reports a scheme satisfied when the call would refuse it.** The two agree by construction,
  or `auth check` is worthless to an agent."* `Target`'s doc comment justifies the swallow for the
  "spec declares no server" case only; it also swallows malformed input.
- **Evidence:**
  ```
  $ talaria auth check one.yaml --base-url 'ftp://evil.example.com' --output json
  {"schemes":[{"scheme":"bearerAuth","source":"env:TALARIA_AUTH_BEARER","supported":true,"present":true}]}   exit=0
  $ talaria call one.yaml getThing --base-url 'ftp://evil.example.com' --dry-run
  {"error":{"code":2,"message":"… base URL \"ftp://evil.example.com\" from --base-url is not an absolute http(s) URL"}}   exit=2
  ```
  Same divergence for `http://u:p@evil.example.com` (userinfo).
- **Suggested fix:** Have `credentialsWithheld` call `request.ResolveBaseURL` directly and return the
  error, distinguishing only the "no base URL at all" sentinel — `Target`'s stated purpose — from a
  malformed one. Add the case beside `TestAuthCheckReportsACredentialWithheldFromAnOffSpecHost`.

## Finding 6: A server variable with no `default` substitutes to the empty string instead of omitting the server
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 2 finding 11 (partial)
- **File:** internal/spec/servers.go:64
- **Description:** `substitute` writes `defaults[name] = v.Default` for every declared variable,
  including one whose `default` is absent (`""`). The URL substitutes "successfully" to a nonsense
  authority instead of being dropped, contradicting the function's own doc comment ("A server whose
  URL cannot be fully substituted is omitted rather than returned half-done"), CLAUDE.md ("omits any
  server it cannot fully substitute") and the plan's finding-11 wording. The bogus host then enters
  the allowed credential set *and* becomes `servers[0]`, so the call runs against it with the
  credential attached. `default` is REQUIRED in the OpenAPI Server Variable Object, so its absence is
  malformed input — which the spec is by premise.
- **Evidence:** `servers: [{url: "https://{sub}.example.com/v1", variables: {sub: {}}}]`
  ```
  $ talaria call sv.yaml listPets --dry-run --output json      # exit 0
  "curl":"curl -q -s -H \"Authorization: Bearer $TALARIA_AUTH_BEARER\" 'https://.example.com/v1/pets'"
  ```
- **Suggested fix:** Skip a variable whose `Default == ""` when building the lookup, so the
  leftover-placeholder check at `servers.go:93` drops the server.

## Finding 7: Two schemes that both map to `Authorization` emit two `Authorization` headers where `main` refused the call
- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none — regression introduced by task 3
- **File:** internal/config/auth.go:374
- **Description:** `credentialFor` now resolves every out-of-scope scheme to
  `{In: header, Name: "Authorization", Ref: EnvBearer}`. When one security requirement names such a
  scheme *together with* an `http bearer` scheme (they apply as an AND), `credentials()` returns two
  credentials with the same name and location and `binder.credentials` appends both. RFC 9110 allows
  one `Authorization`; servers reject the request or read only the first. On `main` this exited 2
  with a clear message, so the branch turned a clean refusal into a malformed request. `Schemes`'
  new `checkEnvCollisions` only inspects `KindAPIKey`, so it does not catch this.
- **Evidence:** requirement `- {oauthA: [], bearerAuth: []}`, `TALARIA_AUTH_BEARER=tok`:
  ```
  branch: "curl":"curl -q -s -H \"Authorization: Bearer $T…\" -H \"Authorization: Bearer $T…\" '…'"   exit 0
  main  : {"error":{"code":2,"message":"no usable security scheme for getThing: scheme oauthA is of unsupported type oauth2"}}
  ```
- **Suggested fix:** Collapse credentials that resolve to the same `In`+`Name`+`Ref` into one, or
  extend `checkEnvCollisions` to cover the bearer fallback and report the requirement as unusable.

## Finding 8: `auth check` exits 2 on an env-var collision `call` never sees
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none — regression introduced by task 3
- **File:** internal/config/auth.go:228
- **Description:** `Schemes` runs the new `checkEnvCollisions` over **every** scheme the document
  declares; `declaredCredentials` (`auth.go:344`) runs it over only the schemes the operation names.
  A spec with two schemes that `envSuffix` flattens onto one variable but that are used by
  *different* operations makes `auth check` exit 2 while every individual `call` succeeds.
  DESIGN.md §5's clause is "the two agree by construction"; this breaks it in the direction the
  sentence does not name, and hands an agent a hard stop on a spec that works.
  `internal/config/testdata/collide.yaml` only covers the case where one operation names both, where
  the two do agree.
- **Evidence:** schemes `key-a` and `key.a` on operations `opOne` and `opTwo` respectively:
  ```
  $ talaria auth check collide.yaml   → exit 2 'security schemes "key-a" and "key.a" both read $TALARIA_AUTH_APIKEY_KEY_A'
  $ talaria call collide.yaml opOne --dry-run   → exit 0, -H "X-A: $TALARIA_AUTH_APIKEY_KEY_A"
  ```
- **Suggested fix:** Report the collision per scheme in the `auth check` payload as a third state
  beside `supported`/`present`, or restrict `Schemes`' check to pairs that co-occur in some
  operation's requirement.

## Finding 9: `call` reports `credentials_withheld` for a credential that is not set, contradicting `auth check`
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/request/build.go:541
- **Description:** `binder.credentials` appends a `Withheld` for every entry in `b.in.Creds` without
  checking `cred.Present()`. `authPayload` (`cmd/talaria/auth.go:129`) does the opposite, and its
  comment states the rule `call` violates: *"A credential that is not set is not withheld: there is
  nothing to withhold, and reporting both would send a reader to `--allow-host` when what they need
  is to export the variable."* At the same invocation the two commands give contradictory diagnoses,
  and `call`'s is the misleading one.
- **Evidence:** `TALARIA_AUTH_BEARER` unset, off-spec `--base-url`:
  ```
  $ talaria auth check one.yaml --base-url http://127.0.0.1:9098
  {"schemes":[{"scheme":"bearerAuth","present":false}]}     # no "withheld"
  {"error":{"code":5,"message":"no credential for security scheme bearerAuth (set $TALARIA_AUTH_BEARER)"}}   exit=5
  $ talaria call one.yaml getThing --base-url http://127.0.0.1:9098 --dry-run
  warning: bearerAuth withheld from 127.0.0.1:9098 … pass --allow-host …
  "credentials_withheld":[{"scheme":"bearerAuth", …}]   exit=0
  ```
- **Suggested fix:** Guard the `Withheld` append on `cred.Present()` — `request` already imports
  `config`, and `Credential.Present()` reads no value. Assert the two commands agree in
  `cmd/talaria/call_test.go`.

## Finding 10: The allowed host set ignores the URL scheme, so an agent can downgrade TLS and keep the credential
- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** design
- **Repeat-of:** none — introduced by this branch's new `hosts.go`
- **File:** internal/request/hosts.go:86
- **Description:** `canonicalHost` reduces an authority to host + non-default port and discards the
  scheme, so `https://api.example.com` and `http://api.example.com` are the same set member. A spec
  that declares only the HTTPS server therefore admits a plaintext base URL as "a host the spec
  declares". Anyone who can set `--base-url` — the agent, in this product's threat model — can strip
  TLS from the request carrying the credential, and an on-path attacker reads it. No warning, no
  `credentials_withheld`.
  DESIGN.md §5a defines the set purely in terms of *hosts*, and `--allow-host` takes a bare host with
  no scheme. The missing decision is **whether a credential binds to an authority or to an origin
  (scheme + authority)**, and if the latter, what a scheme-less `--allow-host localhost:9000` means.
  A fix invents that policy.
- **Evidence:** `servers: [{url: "https://api.example.com/v1"}]`, `TALARIA_AUTH_BEARER=CANARY`:
  ```
  $ talaria call tls.yaml listPets --dry-run --base-url http://api.example.com
  "curl":"curl -q -s -H \"Authorization: Bearer $TALARIA_AUTH_BEARER\" 'http://api.example.com/pets'"   exit=0
  ```
- **Suggested fix:** Decide origin-binding vs authority-binding in DESIGN.md §5a first. The narrowest
  code answer once decided is to withhold on an https→http downgrade specifically.

## Finding 11: Server-variable `enum` alternatives are not in the allowed host set, and the message denies the spec declares them
- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** design (for the set); none (for the message)
- **Repeat-of:** cycle 2 finding 11 (partial)
- **File:** internal/spec/servers.go:58
- **Description:** `substitute` expands only each variable's `default`, so a spec whose `region`
  variable enumerates `[eu, us]` yields one host. Calling the spec's *own* `us` server withholds
  every credential and the stderr line says "it is not a host the spec declares" — which is false;
  the spec declares it in the enum. It fails closed, so it is not a leak, but it makes the
  multi-region specs that motivated finding 11 uncallable-with-credentials without `--allow-host`,
  and the message points the reader at the wrong mistake. DESIGN.md §5a says "after server-variable
  substitution" without stating **whether an enum member counts as declared**; a fix must choose, and
  must bound the cross-product if it says yes.
- **Evidence:** `variables: {region: {default: eu, enum: [eu, us]}}`:
  ```
  $ talaria call enum.yaml listPets --dry-run --base-url https://us.api.example.com/v1
  warning: bearerAuth withheld from us.api.example.com: it is not a host the spec declares; …
  ```
- **Suggested fix:** Decide the enum question in DESIGN.md. Independently of it, reword
  `warnWithheldCredentials` (`cmd/talaria/call.go:305`) to "not among the hosts the spec resolves to";
  `request.WithheldReason` is a fixed machine-readable constant and should stay as it is.

## Finding 12: Path- and operation-level `servers[]` are not in the allowed host set
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 2 finding 1 (limb)
- **File:** internal/spec/servers.go:29
- **Description:** `Servers` reads only `doc.Model.Servers`. OpenAPI puts `servers[]` on the Path
  Item and Operation objects too, and those *override* the root for that operation. DESIGN.md §5a
  defines the set as "every host in the spec's `servers[]`, after server-variable substitution",
  which those are. A credential is therefore withheld from a host the spec genuinely declares, and
  the caller has to pass `--allow-host` to reach the spec's own documented server. It fails closed,
  hence WARN.
- **Evidence:** operation-level `servers: [{url: "https://ops.example.com"}]`:
  ```
  $ talaria call sv.yaml listPets --base-url https://ops.example.com --dry-run
  "credentials_withheld":[{"scheme":"bearerAuth","reason":"host not in spec servers[]","host":"ops.example.com"}]
  ```
- **Suggested fix:** Have `spec.Servers`, or a sibling `AllowedHosts` calls, union the root,
  path-item and operation `servers[]`. `firstServer` should keep reading the root only.

## Finding 13: A `--body @file` credential still reaches the agent on stdout, defeating §3.4's reason for referencing the file
- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** design
- **Repeat-of:** cycle 2 finding 6 (residual)
- **File:** cmd/talaria/call.go:442
- **Description:** Task 5 correctly made `request.curl` reference a file or stdin body
  (`--data '@path'`) on the stated grounds that "a body read from a file or stdin may carry a
  credential the agent never saw" (DESIGN.md §3.4). The `request.body` field in the same envelope
  then prints those exact bytes, minus the three built-in JSON paths (`access_token`,
  `refresh_token`, `id_token`). A `client_secret`, a password or a private key in a `--body @file` is
  published to the agent on stdout by the field immediately after the one that withheld it. Parity
  with history is genuinely achieved — both surfaces redact the same three paths — so finding 6 as
  written is closed; what remains is that §3.4's protection is nullified one field away. DESIGN.md
  never says **whether `request.body` should be elided, referenced or fully printed for a non-argv
  body**; a fix must choose between (a) omitting it for `BodyFile`/`BodyStdin`, (b) replacing it with
  the source reference, and (c) leaving it and widening the built-in path list.
- **Evidence:**
  ```
  $ echo '{"client_secret":"SUPERSECRET"}' > body.json
  $ talaria call post.yaml makeThing --allow-mutations --body @body.json --dry-run --output json
  "curl":"… --data '@body.json' …","body":"{\"client_secret\":\"SUPERSECRET\"}\n"
  ```
- **Suggested fix:** Record the decision in DESIGN.md §3.4, then implement it. (b) keeps the envelope
  self-describing and matches what `request.curl` already does.

## Finding 14: An edited history entry injects arbitrary non-credential headers into a replay
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** design
- **Repeat-of:** none
- **File:** internal/replay/replay.go:276
- **Description:** Beyond the credential case in finding 1, `headers` forwards *every* recorded header
  name unfiltered. `request.Build` blocks CRLF and non-token names, but a well-formed header passes:
  `Host` can be overridden on a request that still carries the spec's credential to a spec-declared
  host (vhost/routing confusion at the target), and any attacker-chosen name matching §5a's
  credential-name list is marked sensitive by `hide()`, so its value renders `<redacted>` in the
  envelope, the emitted curl and the new history entry — the operator running the replay cannot see
  what was sent. CLAUDE.md and DESIGN.md §5a's table say the entry supplies "operation, params and
  body"; headers are in neither list, and DESIGN.md never states **which headers a replay may take
  from an entry**. Narrowing it changes what `replay` reproduces, so it needs a written rule.
- **Evidence:** entry edited to `{"Host":"evil.example.net","X-Api-Key":"…"}` → `Host` displayed
  verbatim and forwarded, `X-Api-Key` displayed `<redacted>` and forwarded, stderr empty.
- **Suggested fix:** Decide the rule in DESIGN.md §5a's replay table. The natural one is: restrict
  forwarded headers to names the operation declares as header parameters plus a small explicit
  allowlist (`Content-Type`, `Accept`), and warn for the rest.

## Finding 15: `history replay` omits the query-string credential warning `call` emits for the identical request
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/history.go:227
- **Description:** `secret.QueryKeyWarner` is §5a's leak-channel disclosure for an API key that lands
  in the server's access log ("These still leak into *server* logs — warn once on stderr"). The
  replay RunE calls `warnWithheldCredentials` but not `warnQueryCredentials`; `newCallCmd` calls both
  (`cmd/talaria/call.go:158-159`). The replay path re-resolves the same credential into the same
  query parameter and puts it on the wire, so the stderr a caller sees for a replay is not the stderr
  they see for the call it reproduces.
- **Evidence:** spec with `securitySchemes: {qKey: {type: apiKey, in: query, name: api_key}}` —
  `call` prints the "recorded in server access logs" warning, `history replay` prints only the
  dropped-field notice, and the wire dump shows `GET /pets?api_key=…` on both.
- **Suggested fix:** Build the `secret.QueryKeyWarner` beside the replay command the way `newCallCmd`
  does and call `warnQueryCredentials` next to the existing `warnWithheldCredentials`.

## Finding 16: A replay can ship an empty `request.curl` at exit 0 with no explanation
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none — new with task 4
- **File:** internal/curl/render.go:50
- **Description:** `Render` returns `""` when `Body.ContentType` would split the request. On the
  replay path `document.body` (`internal/curl/config.go:289`) only reaches its `checkSplit` when the
  request carries no `Content-Type` *header*; an entry that carries both a `Content-Type` header and
  a splitting `Body.ContentType` therefore executes successfully and reaches `callPayload`, where
  `Render` returns `""`. The envelope ships `"curl":""` — a documented field of DESIGN.md §4's output
  shape — at exit 0, with nothing on stderr saying why. `Render`'s stated rationale ("printing one
  with the Content-Type quietly dropped would hand the reader a runnable request nobody made") argues
  for refusing, but the caller is given no signal at all.
- **Evidence:** entry hand-edited to `headers {"Content-Type":"application/json"}` and
  `body.content_type "application/json\r\nX-Injected: yes"`:
  ```
  $ talaria history replay 1 --spec pb.yaml --allow-mutations --output json
  {"dry_run":false,"request":{"curl":"","method":"POST", …},"response":{"status":200, …}}   exit=0
  ```
  The wire request itself was clean — no injected header.
- **Suggested fix:** Emit a `clierr.Warnf` when `Render` returns `""`, and make `document.body`'s
  `checkSplit` unconditional rather than gated on `hasHeader`, so the two surfaces refuse together.

## Finding 17: The unreplayable-field warning states a reason that is false
- **Reviewer:** concurrency, security
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/replay/replay.go:294
- **Description:** The cookie loop routes an *undeclared* cookie — an ordinary plain value — into the
  same `warnUnreplayable` branch as a redacted one, so the operator is told the value "held a
  redacted value, which history does not store" when in fact the operation simply declares no such
  parameter. It points them at the wrong fix. CLAUDE.md's warning rule is that a warning is something
  the caller should know; one that misstates the cause is worse than none.
- **Evidence:**
  ```
  stderr = warning: the recorded cookie "tracking" held a redacted value, which history does not store; replaying without it
  ```
  for an entry whose `tracking` cookie held the plain string `t1`.
- **Suggested fix:** Split the two conditions and give the undeclared case its own message, e.g. "the
  operation declares no cookie parameter %q, and a cookie has no flag to come back through".

## Finding 18: Replay re-serialises stored pairs through `name=value`, so a name containing `=` re-splits at the wrong point
- **Reviewer:** concurrency, security
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/replay/replay.go:259
- **Description:** `query` and `headers` percent-decode each recorded pair and re-join it as
  `name+"="+value` for `binder.params`/`binder.pairs`, which cut at the **first** `=`
  (`internal/request/build.go:213`, `:404`). A stored name containing `=` therefore comes apart in a
  different place, and the replayed request carries a different parameter name and a different value
  than the one recorded — silently, from a file the design calls untrusted. `history replay` is
  advertised as re-issuing what `history show` displayed.
- **Evidence:** stored `?a%3Db=c` (name `a=b`, value `c`) replayed as `?a=b%3Dc` (name `a`, value
  `b=c`); stored header `{"X=Y":"v"}` replayed as wire header `X: Y=v`.
- **Suggested fix:** Pass structured pairs into `request.Inputs` rather than re-encoding through the
  CLI's flag syntax, or reject a stored name containing `=` as a malformed entry.

## Finding 19: Three comments and CLAUDE.md justify the `Render` media-type guard with a `history replay --dry-run` that does not exist
- **Reviewer:** spec-compliance, concurrency, security, integration
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/curl/render.go:49
- **Description:** `newHistoryReplayCmd` (`cmd/talaria/history.go:161`) registers only
  `--allow-mutations`; `grep -n "dry-run\|dryRun\|DryRun" cmd/talaria/history.go` returns nothing.
  The guard is worth keeping, but its stated reachability is false: on `call --dry-run` the media
  type has already passed `isMediaType`, and the genuinely reachable path is the one in finding 16,
  which nobody identified. The same fictitious surface is repeated in `internal/curl/config.go:290`,
  `internal/curl/render_test.go:479` and CLAUDE.md:100-105, so the "three surfaces, one value" house
  rule is taught with an example that does not exist — the exact pattern this phase's own unnumbered
  task exists to drain. `render_test.go` passes because it constructs a `request.Request` literal, so
  the test is green over a path the binary does not take.
- **Suggested fix:** Restate the justification as the finding-16 path (a stored `Body.ContentType`
  that `BuildConfig` skipped because a `Content-Type` header was also set), or add `--dry-run` to
  `history replay` if it was intended.

## Finding 20: Two stale cross-references, one naming a mechanism that no longer exists
- **Reviewer:** spec-compliance, integration
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** CLAUDE.md:75
- **Description:** (a) CLAUDE.md sends the reader to `hostFlags(cmd)` in `cmd/talaria/call.go` as the
  single reader of `--base-url`/`--allow-host`. `hostFlags` was introduced in `d1548bf` and folded
  into `newInvocation` (`cmd/talaria/root.go:139`) by the compaction commit `e2e4a9d`;
  `grep -rn hostFlags --include=*.go .` returns nothing. CLAUDE.md:127 correctly describes the
  replacement, so two paragraphs of the house rules now name two different mechanisms for the same
  thing. (b) Both new `//nolint` waivers (`internal/spec/source.go:117`,
  `internal/curl/version.go:42`) read `TRACKED DEBT (phase-2a task 1)` and one adds "this is cycle-2
  finding 8". Task 1 is server-variable substitution, already landed without touching them; finding 8
  is `--output tsv`. The real owner is task 13. The waiver instructs "Remove this waiver in the commit
  that fixes it" and names a commit that has already shipped.
- **Suggested fix:** Replace `hostFlags(cmd)` in `cmd/talaria/call.go` with `newInvocation(cmd)` in
  `cmd/talaria/root.go`, and retarget both waivers at task 13.

## Finding 21: `internal/replay` ships with no tests while its package doc claims each refusal has one
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/replay/replay.go:11
- **Description:** `go test ./...` prints
  `? github.com/Teeeep/talaria/internal/replay [no test files]`. The package doc says it lives here
  "because it is a whole transformation … every refusal below is a decision with a test", and
  `Build`'s doc repeats it. Coverage exists only indirectly through `cmd/talaria/history_test.go` —
  through the command tree the extraction was meant to escape — which is precisely why findings 1, 3,
  14, 17 and 18 all survived into this cycle. CLAUDE.md: never write a comment asserting a property
  no test enforces.
- **Suggested fix:** Add `internal/replay/replay_test.go` covering each refusal and each dropped field
  directly, and let the command test keep only the wiring case.

## Finding 22: `auth check`'s `withheld` field and the undeclared-scheme exit 2 ship in README/AGENT.md but not in DESIGN.md
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/auth.go:41
- **Description:** DESIGN.md §4's `auth check` example and §5's mandated shape never mention
  `"withheld"`, and §4's exit-code table maps 2 to "usage error (unknown operation, missing required
  param)" with no entry for "the spec requires a scheme `components.securitySchemes` does not
  declare" (`internal/config/auth.go:186`, `cmd/talaria/auth.go:231`). Both behaviours are correct and
  both are documented as contract in README.md and AGENT.md. DESIGN.md is the stated source of truth
  for output shape, and CLAUDE.md's rule is "a new failure mode maps to an existing code **or the
  design doc changes** — never both silently."
- **Suggested fix:** Add `withheld` to §4's `auth check` line and §5a's paragraph, and one line to §4
  or §5 naming the undeclared-scheme case and the code it takes.

## Finding 23: `--allow-host host:443` also admits `http://host:80`
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** design
- **Repeat-of:** none
- **File:** internal/request/hosts.go:221
- **Description:** An `--allow-host` value carries no scheme, so `isDefaultPort`'s empty-scheme branch
  treats both 80 and 443 as default and canonicalises them away. A human who allows `example.com:443`
  has also allowed plain-HTTP `example.com:80`. The behaviour is deliberate and commented at
  `hosts.go:179-181`, but it widens a default-deny set and is the same missing decision as finding 10
  — authority or origin — seen from the flag's side.
- **Evidence:** `--allow-host example.com:443` allows `http://example.com`, `http://example.com:80`
  and `https://example.com`.
- **Suggested fix:** Resolve alongside finding 10; if authority-binding is kept, say so in DESIGN.md
  §5a rather than only in a code comment.

## Finding 24: Two style drifts from CLAUDE.md in code the branch touched
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/request/hosts.go:96
- **Description:** (a) `sort.Strings` over a hand-built key slice remains at
  `internal/request/hosts.go:96`, `internal/config/auth.go:215` and `:270`, and
  `cmd/talaria/auth.go:229`, while CLAUDE.md's "reach for the standard library first:
  `slices.Sorted(maps.Keys(m))`" was applied in `cmd/talaria/history.go` and
  `internal/replay/replay.go` in the same branch. Inconsistent, not incorrect.
  (b) `internal/secret/response.go:3-11` puts the new `internal/clierr` import inside the stdlib
  group (`io`, `github.com/Teeeep/…`, `strings`, `sync`). `gofmt` and the configured linter set both
  pass, so nothing catches it.
- **Suggested fix:** Apply `slices.Sorted(maps.Keys(m))` at the four sites; move the `clierr` import
  into its own group.
