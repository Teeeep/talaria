# Review findings — phase-2a-experiment (cycle 5)

Base: `main`. Diff: `git diff main...HEAD` — 27 commits, tasks 1–6 of 21 in `tasks.json` done,
7–21 queued. Reviewers: security, spec-compliance, concurrency, integration, all run in parallel
against the full branch diff.

**No production code has changed since cycle 4.** `git diff 0b89537..HEAD -- cmd internal` is
empty; the only commits since are `4466dd6` (`.ralph/loop.sh`) and `2b3c90e` (deletes
`REVIEW_ESCALATION.md`). Three consequences:

1. **No fix cycle has run.** A carried finding is therefore `Repeat-of: none` — a repeat means a
   previous fix was *attempted and failed*, and none was. The two exceptions are Findings 22 and
   23, where `c05ba96` did attempt a fix and partly failed; that lineage is preserved.
2. **Every finding below was re-derived this cycle**, at a raw-TCP capture listener or in the
   curl config document, against binaries built from both `HEAD` and `main`. None is carried on a
   previous cycle's word, and two of cycle 4's claims are corrected (Findings 6 and 21).
3. **Exactly one CRIT is a branch regression** — Finding 1, created by `f8ef1d3` (task 5). Every
   other CRIT was reproduced on a `main`-built binary and behaves identically there.

**One CRIT is new this cycle** and was missed by four previous reviews: Finding 10 — the emitted
curl inlines a body that is not valid UTF-8, and `encoding/json` rewrites every invalid byte to
U+FFFD, so a replayed binary body pastes as different bytes. Three WARNs are new: Finding 27
(replay silently retargets a recorded call at a different **path prefix**, not just a different
host), Finding 24 (repeated request headers reach the wire comma-joined on replay) and Finding 32
(CLAUDE.md's `corpus` may-not-import-`curl` rule is false in the tree *and* the guard it names does
not assert it). Finding 5 gained a second limb: an `apiKey` scheme with no `name` sends the request
unauthenticated while all three surfaces claim the credential went out.

**Dropped from cycle 4.** Its Finding 11 ("an entry talaria just wrote is unreplayable at exit 2,
naming a flag `history replay` does not have") **did not reproduce**: `--base-url` and
`--allow-host` are persistent root flags (`cmd/talaria/root.go:76,83`) and are listed under
`history replay --help`, and a round trip of an entry talaria wrote replays at exit 0. The live
defects in that entry's replay are Findings 22 and 25. It should not be carried a fourth time.

**Verified sound — do not re-litigate.** `SecretRef` discipline holds: `.Resolve()` has two
non-test call sites, both in `internal/curl`, and a five-scheme canary sweep across `call`,
`--dry-run`, `auth check`, `history`, `history show`, `history replay` and `--output json` put zero
canary bytes on any stdout, stderr or `history.jsonl` while all five arrived at the listener —
every CRIT below is a *destination* or *pasted-command* bug, not a redaction bug. No credential is
ever an argv element; `-q` is first, so a planted `~/.curlrc` cannot add `trace-ascii` or `proxy`.
Redirects are never followed. `canonicalHost` normalises case, default port, trailing root dot and
IPv6 brackets and nothing else, `HostSet.Allows` is the only comparison in the tree, and
`api.example.com.attacker.com` is correctly not a match. `--base-url` cannot itself bypass the host
set: `http://good\@evil.com` and `http://good.com\@evil.com` are refused at exit 2, and `%40`/`%00`
forms canonicalise to a host that is withheld. A hostile spec cannot choose which environment
variable is read. NUL and CRLF in a spec `paths` key are refused by curl's own URL parser. Emitted
shell quoting is sound apart from Finding 5 — a spec-controlled `$(...)`, backtick or quote is
inert in both the single- and double-quoted branches. Basic auth stays symbolic and never prompts.
The spec cache is sha256-named under 0700 with 0600 files, written temp-then-rename; no traversal.
DESIGN.md §5's oauth2/openIdConnect/mutualTLS matrix matches row for row, including the
bring-your-own-token clause, and `auth check` agrees with `call` across it. The withheld-credential
contract is implemented on both surfaces and in both directions, `allow_hosts:` parses under
`KnownFields(true)`, and `replayableEnv` is absent from the tree. The `call → history → replay →
wire` round trip is byte-identical for a path parameter containing a space and an encoded slash, a
declared header parameter, a repeated declared query parameter, an empty value, a non-ASCII path
parameter, a percent-encoded query name and a `@file` body; a 63-byte body carrying CRLF, a tab,
doubled backslashes, an unescaped quote and a vertical tab survives `escapeDirective` → curl →
the wire unchanged on all three of call, replay and paste. `internal/e2e/e2e_test.go:960` is a
real wire-level assertion and meets the phase doc's hardest acceptance bar. CI parity holds:
`internal/ci/workflow_test.go` binds `ci.yml` to `.ralph/stack.json` and `go.mod`, and
`TestRaceWorkflowRunsTheDetector` enforces `-race -count=1`. No drift into phase-2b/2c scope.

**The concurrency reviewer re-derived all six of cycle 4's claims from scratch and confirmed every
one**: `go test -race -count=1 ./...` clean across 15 packages; no `go func` in production code at
all (two `sync.Once`, and `secret.NewRedactor` allocates rather than aliasing the one package-level
mutable value); 40 concurrent appends → 40 distinct well-formed entries; 990 seeded entries + 36
concurrent appends + 72 concurrent reads → exactly 1000 lines, no torn reads, no temp left behind;
30 concurrent processes into an empty spec cache → one 0600 file, no temps; SIGINT and SIGTERM
mid-request leave no orphan curl, no body temp and no capture directory. Finding 36 is the signal
that claim does not cover.

**Build, lint and suite are green** — `go build ./...`, `gofmt -l`, `go vet ./...`,
`golangci-lint run ./...` (0 issues), `go test ./...` (15 packages), `go test -race`. Per CLAUDE.md
that is evidence of nothing: every finding below was found by a listener or by reading.

**Scope rule used.** A defect whose entire remedy is a queued, not-yet-started task (7–21) is not
listed as a finding — it is normal mid-phase state, not something the branch broke. Those are
recorded at the end under "Live, but already scoped to a queued task", with any new evidence this
cycle produced.

---

## Finding 1: The emitted `--dry-run` curl for a `@file` or stdin body sends different bytes than talaria itself sends
- **Reviewer:** security, spec-compliance, integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `f8ef1d3` (task 5) — verified: `git show main:internal/curl/render.go` emits `--data-raw` inline for every body and is byte-identical to what it sends
- **Repeat-of:** none
- **File:** internal/curl/render.go:165
- **Description:** DESIGN.md:158 states "`--dry-run` prints the exact curl command" and DESIGN.md:163 says `--body @file` emits `--data @file`. The two are incompatible: `curl --data @path` strips every CR and LF from the file — `internal/curl/config.go:314` already documents exactly that as the reason the *executor* refuses `data`. Reproduced independently by two reviewers against curl 8.14.1 with the same 36-byte pretty-printed JSON file:
  ```
  talaria call --body @body.json   Content-Length: 36   {\n  "name": "rex",\n  "sig": "abc"\n}\n
  the emitted curl, pasted         Content-Length: 32   {  "name": "rex",  "sig": "abc"}
  ```
  Exit 0, stderr empty, and `request.body` in the same envelope shows the unstripped text, so nothing on stdout contradicts it. A signed payload, NDJSON or anything whitespace-significant reproduces as a *different request* and the agent that pasted the command cannot tell. `--body -` renders `--data '@-'`, which on paste reads the reader's own terminal. AGENT.md:172-174 records the divergence honestly, so AGENT.md and DESIGN.md v0.5 now contradict each other with no design-doc amendment. **This is the one CRIT this branch's own work created.**
- **Suggested fix:** Emit `--data-binary` for both `BodyFile` and `BodyStdin` — still a *reference*, so §3.4's actual requirement (the bytes are not printed) holds, and it converges with what the executor already does. Amend DESIGN.md §3.4:163 in the same commit. `internal/curl/render_test.go`'s `TestRenderReferencesABodyTheReaderWasNeverShown` and `internal/e2e/e2e_test.go:643` both pin the defect in and must change with the fix. Fix Finding 10 in the same commit — converging only `BodyFile`/`BodyStdin` leaves the inline limb.

## Finding 2: An operation path that does not begin with `/` sends the resolved credential to a host of the spec's choosing, and nothing is withheld
- **Reviewer:** security, spec-compliance
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — a `main`-built binary produces the identical envelope and the identical wire capture. `main` had no host binding at all; the hole in the *new control* is this branch's to close
- **Repeat-of:** none
- **File:** internal/request/build.go:546
- **Description:** The host check tests `req.BaseURL`, but the request goes to `req.BaseURL + req.Path` (`internal/request/request.go:323`), and `req.Path` is the spec's `paths` map key, validated only for leftover `{}` (`build.go:271`). A hostile spec declares a legitimate server so `AllowedHosts` is satisfied, then names its path so concatenation rewrites the authority. Reproduced at the wire:
  ```
  servers: [{"url":"http://127.0.0.1"}]        # port 80, the only allowed host
  paths:   {"@127.0.0.1:42999/steal": {...}}

  envelope: "url":"http://127.0.0.1@127.0.0.1:42999/steal"   credentials_withheld: absent
  stderr:   (empty)                                          exit 0
  :42999:   GET /steal / Host: 127.0.0.1:42999 / Authorization: Bearer CANARY-BEARER-999
  ```
  The `?` variant also reaches the wire (`paths: {"/thing?injected=1"}` → `GET /thing?injected=1`); `#`, `/../../admin` and `\` all pass through. CR/LF and the bare-`:port` form are refused by curl's URL parser, so those two limbs are closed. This defeats §5a's "credentials bind to hosts" using nothing but the spec — the input §1 promises is safe to point the tool at.
- **Suggested fix:** (1) In `binder.path`, refuse an `op.Path` that does not begin with `/` or that contains `?`, `#` or `@`. (2) Run `HostSet.Allows` on the **assembled** URL rather than on `BaseURL`, so any future path-shaped input is covered by the check itself. Add a hostile-path case to `internal/canary` asserting the canary never reaches a second listener.

## Finding 3: A base URL carrying a fragment or a query string silently discards the operation's path, and the credential still goes out
- **Reviewer:** security, spec-compliance, integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — `git blame -L 176,186 internal/request/build.go` is entirely the initial commit; a `main`-built binary produces the identical envelope and wire
- **Repeat-of:** none
- **File:** internal/request/build.go:183
- **Description:** `ResolveBaseURL` validates userinfo, scheme and host, then returns the raw string with only a trailing `/` trimmed; `Request.URL` is pure concatenation. Reproduced at the wire:
  ```
  --base-url 'http://127.0.0.1:43112/v1#frag'   spec path /pets/{petId}, petId=42
    envelope: "url":"http://127.0.0.1:43112/v1#frag/pets/42"   credentials_withheld: absent
    stderr:   (empty)                                          exit 0
    wire:     GET /v1 HTTP/1.1 / Authorization: Bearer CANARY-FRAG
  --base-url 'http://127.0.0.1:43112/v1?x=1'
    wire:     GET /v1?x=1/pets/42 HTTP/1.1      <- the path is now query text
  ```
  The request went to an endpoint the caller never named, the bearer token went with it, and every surface reports the URL it did *not* send. It is also reachable from the spec alone: `servers: [{url: "http://host/v1#"}]` with no `--base-url` does the same, and CLAUDE.md names the spec as untrusted input. `internal/spec/servers.go:26-28`'s doc comment claims `binder.baseURL` applies the same checks a hand-typed `--base-url` gets — true, and those checks do not cover this.
- **Suggested fix:** In `ResolveBaseURL`, reject a candidate whose parsed `RawQuery` or `Fragment` is non-empty, with the same exit-2 shape as the other refusals. That one seam covers `--base-url`, the profile and `spec.Servers` together.

## Finding 4: A query-string credential is emitted unescaped in `request.curl`, so the pasted command authenticates with a different value and can inject a parameter
- **Reviewer:** security, spec-compliance, integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — `git blame -L 191,214 internal/curl/render.go` is entirely the initial commit and the branch does not touch `urlWord`; the `main`-built binary emits the byte-identical word
- **Repeat-of:** none
- **File:** internal/curl/render.go:207
- **Description:** `Request.QueryString` (`internal/request/request.go:358`) percent-encodes a resolved credential before it goes on the wire, deliberately. `urlWord` does not: for any `IsSensitive()` value it writes `render(q.Value)` straight into the word with no `url.QueryEscape`, on the assumption (comment at render.go:201-205) that the rendered text is only a display placeholder. For a `SecretRef` it is `$TALARIA_AUTH_APIKEY_QUERYKEY`, and the shell expands it to the *raw* value at paste time. Both wires captured with the variable set to `ab+cd/ef==&injected=1`:
  ```
  emitted:              curl -q -s "http://127.0.0.1:43113/thing?api_key=$TALARIA_AUTH_APIKEY_QUERYKEY"
  talaria's own wire:   GET /thing?api_key=ab%2Bcd%2Fef%3D%3D%26injected%3D1 HTTP/1.1
  emitted curl's wire:  GET /thing?api_key=ab+cd/ef==&injected=1              HTTP/1.1
  ```
  Two silent divergences at exit 0: `+` decodes server-side as a space, so a base64 key authenticates with a different value, and `&` splits the value into a second query parameter talaria never sent. Same DESIGN.md §3.4 contract as Finding 1; a fix cycle that converges only the body path will leave this.
- **Suggested fix:** Do not put a raw `$VAR` in the query string. For the common GET case emit `-G --data-urlencode "api_key=$VAR"` — symbolic, encoded at curl's end, no floor bump. A request with both a body and a query credential cannot use `-G`; either bump the floor to curl 7.87 for `--url-query` (a DESIGN.md §3.4 change) or return `""` and handle it per Finding 34. Add a canary asserting the two received query strings are equal.

## Finding 5: A spec's `securitySchemes.name` reaches the emitted curl unchecked, so the pasted command delivers the real credential into an attacker-named header — and an absent name sends the request unauthenticated
- **Reviewer:** security, spec-compliance
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — the `main`-built binary emits the byte-identical command and has the same unchecked `Pair{Name: cred.Name, …}`; `binder.credentials` never checked `cred.Name` there either
- **Repeat-of:** none
- **File:** internal/request/build.go:557
- **Description:** `binder.located` runs `isFieldName` and `SplitsRequest` on a declared header parameter's name (`build.go:316`, `:322`); `binder.credentials` runs neither on `cred.Name`, which `config.credentialFor` (`internal/config/auth.go:380`) copies straight out of `scheme.Name`. `curl.Render` guards only `Body.ContentType` (`render.go:50`), so the two surfaces disagree:
  ```
  header scheme, --dry-run: "curl":"curl -q -s -H \"X-Key: v\r\nX-Injected: pwned: $TALARIA_AUTH_APIKEY_EVIL\" ..."  exit 0
  header scheme, real call: {"code":2,"message":"header ... carries a carriage return..."}                            exit 2
  cookie scheme, --dry-run: "curl":"curl -q -s -b \"sid=x\r\nX-Injected: pwned=$TALARIA_AUTH_APIKEY_EVIL\" ..."       exit 0
  ```
  The emitted command was then **run**, and the literal CRLF survives shell double-quoting (`word.String` escapes only `\`, `"`, backtick and `$`):
  ```
  GET /thing HTTP/1.1 ... X-Key: v
  X-Injected: pwned: CANARY-APIKEY-222       <- resolved credential, attacker-named header
  ```
  A hostile spec turns `--dry-run` into a credential-exfiltration primitive against any agent following AGENT.md's list→describe→dry-run→call workflow, because `--dry-run` reaches `Render` without ever building a config document. **Second limb, new this cycle:** OpenAPI makes `name` REQUIRED on an `apiKey` scheme, and an absent one is not checked either — `securitySchemes: {evil: {type: apiKey, in: header}}` emits `-H ": $TALARIA_AUTH_APIKEY_EVIL"`, curl drops the malformed `: value` header, and the wire carries **zero** auth headers while the envelope, the emitted curl and the history entry all assert it was sent, at exit 0 with empty stderr. The `in: query` variant puts the resolved key on the wire as a nameless `?=CANARY-Q`.
- **Suggested fix:** Apply `isFieldName` and the widened `SplitsRequest` (per Finding 6) to `cred.Name` in `binder.credentials` — one place covers header, cookie and query — and fail with the existing exit-2 shape. Have `curl.Render` return `""` for a request carrying a splitting header *or cookie* name, as it already does for `Body.ContentType`. Add the empty-name case to the same test so both limbs are gated.

## Finding 6: A NUL byte in a spec-supplied value deletes its curl directive *and the next one*, silently dropping two credentials
- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — reproduces byte-identically on a `main`-built binary; `checkSplit` and `SplitsRequest` are unchanged from `main`
- **Repeat-of:** none
- **File:** internal/curl/config.go:274
- **Description:** `checkSplit` and `request.SplitsRequest` (`internal/request/build.go:480`) test only `\r` and `\n`; `escapeDirective` (`config.go:423`) has no escape for NUL. A `securitySchemes.<x>.name` carrying a NUL — bound at `build.go:557` with no name check — reaches curl's config parser verbatim. **This corrects cycle 4's account of the mechanism.** With three schemes (`bearerAuth`, `evil` carrying the NUL, `zzz` declared after it), the config document captured from a PATH-shim curl contains all three `header` directives and the wire carries **only the first**:
  ```
  envelope: three headers, all three claimed sent, exit 0, stderr EMPTY
  config:   header = "Authorization: Bearer B999"
            header = "X-Key\0 junk: EVIL222"
            header = "X-After: AFTER333"
  wire:     GET /thing ... Authorization: Bearer B999      <- and nothing else
  ```
  So the NUL consumes its own directive **and the following one** — two credentials silently dropped and a silent downgrade to partially-unauthenticated, while every surface asserts three headers went out. (Cycle 4 reported "ZERO auth headers"; that was fixture ordering — the directive *before* the NUL survives.) A NUL in a spec *path* key does not reach the wire: curl rejects the URL at exit 3.
- **Suggested fix:** Widen the wire-safety predicate to reject any rune `< 0x20` or `== 0x7f` in both `request.SplitsRequest` and `curl.checkSplit`, and apply it at all three surfaces CLAUDE.md names. Add a NUL-in-scheme-name canary — the existing set has no case where the credential is *dropped* rather than printed.

## Finding 7: A stored history header or query parameter supplies the credential the replayed request authenticates with
- **Reviewer:** security, spec-compliance
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — a `main`-built binary replaying the same hand-edited entry produces `Authorization: Bearer ATTACKER-TOKEN` and `?api_key=ATTACKER-QUERY-KEY` on the wire
- **Repeat-of:** none
- **File:** internal/replay/replay.go:293
- **Description:** `Inputs.headers` forwards every recorded header as a `--header` flag, filtered only by `isRedacted` — i.e. only by *value*, never by name — and `Inputs.query` (`replay.go:256`, `:273`) applies the identical value-only filter. Both limbs reproduced from one hand-edited entry:
  ```
  stdout: "url":"http://127.0.0.1:43103/thing?api_key=<redacted>"
          "headers":{"Authorization":"<redacted>, Bearer <redacted:env:TALARIA_AUTH_BEARER>"}
  wire:   POST /thing?api_key=ATTACKER-QUERY-KEY HTTP/1.1
          Authorization: Bearer ATTACKER-TOKEN     <- from the file, first, so it wins
          X-Whatever: injected
          Authorization: Bearer CANARY-REAL-333
  exit 0, stderr empty
  ```
  For the query limb the redaction machinery actively conceals it: the envelope renders `<redacted>` over the attacker's value. DESIGN.md §5a's replay table row 2: "Credentials — **Never taken from the entry**." CLAUDE.md: "a stored credential position is dropped, not read." Both violated on two surfaces.
- **Suggested fix:** Drop any recorded header **or query parameter** whose *name* is credential-shaped by the same `secret` rule `hide()` uses, and `warnUnreplayable` on it. Name-based is the only rule that works — the value the store wrote is a placeholder, the value an attacker writes is not.

## Finding 8: `--body @file` and `--body -` publish the file's bytes to stdout and into the permanent history store
- **Reviewer:** security, spec-compliance, integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — the `request.body` field predates the branch and a `main`-built binary leaks the same value (worse: `main` also inlined it into `request.curl`); `f8ef1d3` (task 5) fixed only the `curl.Render` half
- **Repeat-of:** none
- **File:** cmd/talaria/call.go:447
- **Description:** CLAUDE.md states both rules for a file body and that "each one alone still leaks". Only one landed. Reproduced with a key deliberately outside `builtinBodyPaths` (`access_token`/`refresh_token`/`id_token`):
  ```
  $ talaria call post.yaml makeThing --allow-mutations --dry-run --body @sec.json
  "curl":"... --data '@sec.json' ..."                                     <- §3.4 honoured
  "body":"{\"client_secret\":\"SECRET-CANARY-98765\",\"password\":\"hunter2\"}\n"
  $ grep -c SECRET-CANARY-98765 $XDG_STATE_HOME/talaria/history.jsonl
  1
  ```
  `client_secret` is the canonical OAuth2 client-credentials secret and is not in the built-in list; neither is `password`. The `--body -` limb reproduces identically. Arbitrary local file contents cross into the agent's channel via a path the agent itself supplied, and land verbatim in the store §5a calls "the highest-risk surface in the tool".
- **Suggested fix:** For `BodyFile` and `BodyStdin`, have `callPayload`'s `request.body` carry the same `@path` / `@-` reference `request.curl` does, and have `corpus.Redactors` store the reference rather than the bytes. Extending `builtinBodyPaths` is the weaker option — the list can never be complete, which is the property that produced this finding. Fix Finding 31 (the canary) in the same commit or this stays untested.

## Finding 9: A profile-supplied credential is sent to any host, while stderr and the envelope claim a different credential was protected
- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — a `main`-built binary sends the same profile headers anywhere. (`d1548bf` added the *false reassurance*: `main` prints no `credentials_withheld` and no withholding warning at all)
- **Repeat-of:** none
- **File:** internal/request/build.go:548
- **Description:** `binder.credentials` populates `req.Withheld` from `b.in.Creds` only. A profile `headers:` value that talaria *itself* classifies as a credential — `hide()` (`build.go:388`) marks it sensitive and every display surface prints `<redacted>` — is never considered by the host check. Reproduced at the wire with a 0600 profile:
  ```
  stderr: warning: bearerAuth withheld from 127.0.0.1:43102: it is not a host the spec declares
  stdout: "headers":{"Authorization":"<redacted>","X-Api-Token":"<redacted>"},
          "credentials_withheld":[{"scheme":"bearerAuth","reason":"host not in spec servers[]",...}]
  wire:   Authorization: Bearer PROFILE_BEARER_LEAK
          X-Api-Token: PROFILE_SECRET_TOKEN
  ```
  The profile header is literally named `Authorization` — the boundary reports itself as holding for `bearerAuth` while an `Authorization` header from the same invocation goes to the attacker's host. That false reassurance is what makes this CRIT rather than a missing check.
- **Suggested fix:** Withhold on `Value.IsSensitive()` rather than on membership of `b.in.Creds`. `Withheld` needs a second form for a value classified by name rather than by scheme, and `warnWithheldCredentials` (`cmd/talaria/call.go:294`) must render both. Scope note: §5a plus §5's listing of profiles as a credential source settles the **profile** limb — fix that. The `--header Authorization=…` limb reproduces identically but is the caller's own per-call choice and is not settled by the doc; record that question in §5a rather than deciding it here.

## Finding 10: The emitted curl inlines a body that is not valid UTF-8, so the pasted command sends different bytes and the envelope hides it
- **Reviewer:** integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Introduced-by:** `none` — `git blame -L 163,171 internal/curl/render.go` shows line 170 is the initial commit, and a `main`-built binary reproduces it via `--body @binfile`
- **Repeat-of:** none
- **File:** internal/curl/render.go:170
- **Description:** **New this cycle, and not fixed by Finding 1's fix.** Finding 1 covers `BodyFile`/`BodyStdin`, which `bodyArgs` *references*. Line 170 is the fall-through — an inlined `--data-raw` — and it inlines `string(req.Body.Data)` with no UTF-8 or NUL guard. `requestView.Curl` is then a Go `string` marshalled by `encoding/json`, which rewrites every invalid byte to U+FFFD; NUL survives into the JSON as `\u0000` and dies in any shell that pastes it. Reached at HEAD by two live paths: `history replay` of any recorded binary body (`corpus` base64s it faithfully, `replay.body()` decodes it faithfully, and `Body.Source` is empty so `Render` inlines it), and `call --body <invalid-UTF-8 argv>`. Recorded body `\x00\x01\xff\xfeAB\n` (7 bytes), replayed:
  ```
  talaria's own wire:    Content-Length: 7    b'\x00\x01\xff\xfeAB\n'
  envelope request.curl: ... --data-raw '\u0000\u0001<U+FFFD><U+FFFD>AB\n' ...
  the emitted curl, pasted:
                         Content-Length: 10   b'\x01\xef\xbf\xbd\xef\xbf\xbdAB\n'
  ```
  Two independent corruptions in one field, exit 0, stderr empty, and `request.body` shows the same mangled text so nothing on stdout contradicts it. The argv limb alone reproduces too: `--body $'A\xff\xfeB'` sends 4 bytes, the emitted command sends 6.
- **Suggested fix:** In `bodyArgs`, fall back to a *reference* rather than an inline when `!utf8.Valid(req.Body.Data)` or the body contains a NUL — the same predicate `curl.inlinable` (`internal/curl/config.go:325`) already uses to decide the executor's own temp-file path, so the two surfaces share one rule instead of two. A replayed binary body has no path to point at, so the honest answer there is Finding 34's explicit empty-render condition. Fix with Finding 1, and make the canary Finding 31 asks for assert *executed bytes == pasted bytes*, which catches all three limbs at once.

---

## Finding 11: `auth check` exits 2 with no report at all for a spec whose `servers[]` is relative, and `firstServer` never reaches the usable second server
- **Reviewer:** security, spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `c05ba96` for the `auth check` limb — verified by building both trees against the same fixture; `git show d1548bf:internal/request/hosts.go` shows `Target` swallowed *every* `ResolveBaseURL` error and `c05ba96` tightened it to `ErrNoBaseURL` only, overshooting the case it was fixing. The `firstServer` limb reproduces on `main`
- **Repeat-of:** none
- **File:** internal/request/hosts.go:114
- **Description:** `credentialsWithheld` (`cmd/talaria/auth.go:118`) calls `request.Target`, which converts only `ErrNoBaseURL` into "no target" and returns every other failure. Relative `servers[].url` values (`/v1`, `/`) are common, especially in Swagger 2.0 conversions carrying only a `basePath`. Both binaries, same fixture:
  ```
  main:   auth check rel.yaml --output json -> {"schema":"talaria/v1","schemes":[{...,"present":true}]}  exit 0
  branch: auth check rel.yaml --output json -> code 2: base URL "/v1" from the spec's servers[0].url
          is not an absolute http(s) URL                                                                exit 2
  ```
  The report is not printed at all, contradicting `newAuthCheckCmd`'s own Long text (`auth.go:97-99`) and README:307 ("The report is printed either way"). DESIGN.md §4 describes `auth check` as a question about the environment, which needs no resolvable target. **Second limb:** `ResolveBaseURL`'s doc comment (`build.go:148-149`) states "A spec whose server URL is relative … counts as no server", which the code does not do — `firstServer` (`build.go:202`) returns `spec.Servers(doc)[0]` unconditionally, so `servers: [{url: /v1}, {url: https://ok.example.com}]` exits 2 on both binaries and the usable second server is never reached. `AllowedHosts` already drops hostless URLs, so the two readers of `spec.Servers` disagree about the same list, and per CLAUDE.md that comment asserts a property no test enforces.
- **Suggested fix:** Make `firstServer` skip URLs `hostOf` cannot resolve, so a relative server really does "count as no server" and the next absolute one wins — one change fixes both limbs and aligns it with `AllowedHosts`. Add a test asserting the second server is chosen, and one asserting `auth check` on a relative-only spec still prints its report.

## Finding 12: `auth check` exits 2 on a spec `call` runs at exit 0, because the collision check is document-wide on one path and operation-scoped on the other
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d2cad54` (task 3) — `checkEnvCollisions` is entirely new there
- **Repeat-of:** none
- **File:** internal/config/auth.go:228
- **Description:** `Schemes` runs `checkEnvCollisions` over every scheme the document declares; `Resolve` runs it only over the schemes the *operation* names (`auth.go:344`). Reproduced with schemes `key-a` and `key.a` used by different operations: `call collide.yaml opA --dry-run` → exit 0 with `X-A: $TALARIA_AUTH_APIKEY_KEY_A` on the wire; `auth check collide.yaml` → `{"error":{"code":2,"message":"security schemes \"key-a\" and \"key.a\" both read $TALARIA_AUTH_APIKEY_KEY_A…"}}` exit 2, again with no report. DESIGN.md §5: "`auth check` never reports a scheme satisfied when the call would refuse it. **The two agree by construction, or `auth check` is worthless to an agent.**" Agreeing in only one direction is not the whole contract — an agent uses `auth check` to decide whether calling is worth attempting.
- **Suggested fix:** Run the same validation set on both paths, or downgrade the `auth check`-only failures to a reported condition in the payload rather than a non-zero exit that suppresses the report.

## Finding 13: Two schemes that both map to `Authorization` emit two `Authorization` headers where `main` refused the call
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d2cad54` (task 3)
- **Repeat-of:** none
- **File:** internal/config/auth.go:353
- **Description:** Reproduced at the wire with `security: [{oauth2: [], bearerAuth: []}]` — both resolve through `credentialFor` to `In: header, Name: "Authorization"`, `credentials()` returns both, and `binder.credentials` appends both. The listener received two `Authorization: Bearer …` lines. `main` refused the combination. Which one the server honours is server-dependent, so the resulting authentication is nondeterministic; with a profile `auth:` entry redirecting one scheme, the two values differ.
- **Suggested fix:** Refuse the combination with the existing collision error, or define and document a precedence rule. Either way the wire must carry exactly one `Authorization`.

## Finding 14: `credentials_withheld` names schemes whose credential was never set, contradicting `auth check`'s reading of the same word
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/request/build.go:549
- **Description:** `binder.credentials` appends a `Withheld` for every `cred` in `b.in.Creds` when the host is off-set, without checking `cred.Ref.Present()`. `config.Resolve` returns an `Incomplete` alternative's credentials as a fallback, so an unset credential is in that slice. `cmd/talaria/auth.go:131-136` reasons the opposite way for the identical situation, in a comment: "A credential that is not set is not withheld: there is nothing to withhold, and reporting both would send a reader to `--allow-host` when what they need is to export the variable." The two surfaces disagree about what `withheld` means, and `call` gives the agent exactly the misdirection `auth check` was written to avoid — CLAUDE.md's "one name for one thing".
- **Suggested fix:** Gate the append on `cred.Ref.Present()`, matching `authPayload`.

## Finding 15: `spec.Servers` reads only root-level `servers[]`, so a path- or operation-level server is neither callable nor allowed
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `301338b` (task 1) — `internal/spec/servers.go` is new there
- **Repeat-of:** none
- **File:** internal/spec/servers.go:35
- **Description:** OpenAPI 3.x lets a Path Item and an Operation each override `servers[]`; DESIGN.md §5a defines the allowed set as "every host in the spec's `servers[]`" — the spec's servers, not the document root's. Reproduced with a spec declaring both: `call oplevel.yaml listPets --dry-run` → exit 2 "the spec declares no server, so pass --base-url…" (false; it declares two), and `--base-url https://op.example.com` → "warning: bearerAuth withheld from op.example.com: it is not a host the spec declares" for a host the document itself names. Both messages state something false about the document, and the remedy offered sends the human to re-declare what the spec already declares. README:210 repeats the incomplete claim.
- **Suggested fix:** Have `Servers` walk `doc.Model.Servers`, every `PathItem.Servers` and every `Operation.Servers`, substituting each the same way, and return the deduplicated union for the host set. `firstServer` should keep using the root list only.

## Finding 16: A server variable with no `default` substitutes to the empty string instead of omitting the server
- **Reviewer:** security, spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `301338b` (task 1) — a `main`-built binary exits 2 on the same fixture (`base URL "https://{region}.api.example.com/v1" … is not an absolute http(s) URL`), i.e. `main` refused where the branch proceeds
- **Repeat-of:** none
- **File:** internal/spec/servers.go:64
- **Description:** `substitute` records `defaults[name] = v.Default` unconditionally, so a variable declared with no `default` (or `default: ""`) substitutes to nothing rather than failing the server:
  ```
  servers: [{url: "https://{region}.api.example.com/v1", variables: {region: {}}}]
  call srvvar.yaml listPets --dry-run
    -> "url":"https://.api.example.com/v1/pets"   exit 0
  ```
  That authority — `.api.example.com`, which names nothing — becomes both the default target *and* a member of the allowed host set. `Servers`' own doc comment (`servers.go:22-23`) says "Callers treat every URL returned here as a usable base URL, so a template must never reach one", which this violates; per CLAUDE.md that comment asserts a property no test enforces. OpenAPI makes `default` REQUIRED on a server variable, so an absent one is malformed input. The sibling case is handled correctly: `enum: [eu, us]` with no `default` omits the server.
- **Suggested fix:** Treat an empty `Default` as an unsubstitutable variable and omit the server, alongside the existing enum check. Add the case beside the enum case in `internal/spec/servers_test.go`.

## Finding 17: Server-variable `enum` alternatives are not in the allowed host set, and the message denies the spec declares them
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `301338b` (task 1)
- **Repeat-of:** none
- **File:** internal/spec/servers.go:57
- **Description:** Only the `default` value of an enumerated server variable enters the host set. Reproduced with `variables: {region: {default: eu, enum: [eu, us]}}`: `--base-url https://us.api.example.com` → "warning: bearerAuth withheld from us.api.example.com: it is not a host the spec declares", `credentials_withheld` populated, credential dropped. The spec does declare it. Fail-safe, but the message is false and the remedy makes the human re-declare a host the document names.
- **Suggested fix:** Expand each enumerated variable across its `enum` values when building the allowed set (not when choosing the default target), bounded by `maxServerURL` and a cap on the cross-product.

## Finding 18: The allowed host set omits the active profile's own `base-url`, so DESIGN.md §6's worked example silently withholds every credential
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** design — DESIGN.md §5a lists three sources for the allowed set (spec `servers[]` ∪ `--allow-host` ∪ profile `allow_hosts:`) and never states whether the selected profile's own `base-url` host is a fourth. A fix must choose between (Y1) adding it and (Y2) keeping the rule and amending §6's worked example plus the stderr warning
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/request/hosts.go:44
- **Description:** The code implements §5a's three sources exactly, and the consequence is that a profile setting only `base-url` withholds from its own target. Reproduced at the wire: `profiles: {staging: {base-url: http://127.0.0.1:41991}}` + `call other.yaml getPet --profile staging` → the listener received no `Authorization`, `credentials_withheld` populated, stderr "pass --allow-host 127.0.0.1:41991". DESIGN.md §6's worked loop ends with `talaria call spec.yaml listUsers --profile staging  # final verification`, which under this rule gets a 401. A profile is a 0600 file a human wrote — the same trust level as `--allow-host`, which §5a admits without question. The stderr warning also never mentions `allow_hosts:`, which is the profile-shaped remedy.
- **Suggested fix:** Y1 is one line plus a test and makes §6's example work as written; Y2 is doc-only. Either way, add `allow_hosts:` to the warning text.

## Finding 19: The allowed host set ignores the URL scheme, so an agent can downgrade TLS and keep the credential
- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** design — DESIGN.md §5a defines the allowed set as "every host in the spec's `servers[]`" but never states whether a scheme downgrade is the same host. A fix must choose between (Y1) making the set scheme-aware so an http target against an https-only server is withheld, and (Y2) keeping the set host-only and emitting a `clierr.Warnf` naming the downgrade. Y2 preserves the local-twin ergonomics §5a is explicit about
- **Introduced-by:** `none` — `main` had no host binding, so it sent the credential over http regardless
- **Repeat-of:** none
- **File:** internal/request/hosts.go:142
- **Description:** `hostOf` reduces a URL to `canonicalHost(parsed.Host, parsed.Scheme)`; the scheme decides which port is default and is then discarded, so `https://api.example.com` and `http://api.example.com` are one member. Reproduced: spec declares `https://127.0.0.1:43115` only, `--base-url http://127.0.0.1:43115` → the wire carried `Authorization: Bearer PROD-TOKEN` in cleartext, `credentials_withheld` absent, stderr empty, exit 0. Held at WARN rather than CRIT because §5a's rule is literally host-scoped, the credential still reaches only a host the spec declares, and a real HTTPS-only API refuses the plaintext connection — but nothing warns, which is the part that should not ship.
- **Suggested fix:** See Blocked-by.

## Finding 20: The `Host` header is settable from `--header` and from a stored entry, and it decides who receives the credential without passing the host check
- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** design — §5a says a credential goes only to "a host the spec declares" but never states whether "host" means the connect target or the `Host` header. A fix must choose between (Y1) rejecting `Host` as a settable header and dropping it from a replayed entry, and (Y2) running a supplied `Host` value through `HostSet.Allows` alongside the base URL. The **replay limb is not design-blocked** — §5a's replay table already answers it — and can be closed now under the existing text
- **Introduced-by:** `none` — reproduces on a `main`-built binary with the identical emitted curl
- **Repeat-of:** none
- **File:** internal/request/build.go:337
- **Description:** `binder.headers` accepts `Host` like any other field name, and `internal/replay`'s `headers()` forwards a stored one. Reproduced at the wire on the flag limb: `call hosthdr.yaml getThing --header 'Host=evil.internal'` → `GET /thing HTTP/1.1 / Host: evil.internal / Authorization: Bearer PROD-TOKEN`, `credentials_withheld` absent, stderr empty, exit 0. `AllowedHosts` compares the *connect* target; the `Host` header is what a shared frontend (CDN, ingress, nginx vhost, API gateway) actually routes on — the normal deployment for the `api.example.com`-shaped hosts a spec declares. So the agent, or an edited history file, can move a production credential to a different backend behind the same frontend. WARN rather than CRIT because delivery still requires the attacker to control a vhost behind a host the spec declares.
- **Suggested fix:** See Blocked-by; Y1 is the smaller change and matches "there is no `--show-secrets` flag" in spirit.

## Finding 21: A media type `call` refuses at bind time is put on the wire by `history replay` — three surfaces, two predicates
- **Reviewer:** security, spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `e47176c` (task 4) — for the *inconsistency*, which is what this finding is. **Correcting cycle 4's implication that the wire behaviour is new:** it reproduces byte-identically on a `main`-built binary, because `main` had no `isMediaType` at all and accepted `content: {"NOT A MEDIA TYPE ": …}` at bind time, where the branch exits 2
- **Repeat-of:** none
- **File:** internal/replay/replay.go:226
- **Description:** Task 4's three surfaces do not apply the same predicate. Bind time uses `isMediaType` (`internal/request/body.go:189` — type/subtype tokens, no control characters in parameters); the config document (`internal/curl/config.go:293`) and `curl.Render` (`internal/curl/render.go:50`) use only `request.SplitsRequest`, i.e. CR/LF. `replay.body()` sets `Body.ContentType` straight from the stored entry and never reaches `binder.contentType`. Both limbs reproduced at the wire: `"content_type": "application/json\x0Bevil"` → `Content-Type: application/json<VT>evil`, and `"NOT A MEDIA TYPE "` → `Content-Type: NOT A MEDIA TYPE `, both exit 0 with no warning, and `curl.Render` emitted matching `-H` words so all three surfaces agree on the wrong predicate. A vertical tab in a header value is where request-smuggling differentials between a proxy and an origin live. CLAUDE.md's worked example — written by this branch — says "Three surfaces, one value"; this is three surfaces, two values. Distinct from Finding 6: widening `checkSplit` to reject control runes still admits `NOT A MEDIA TYPE `.
- **Suggested fix:** Export `isMediaType` from `internal/request` and apply it in `checkSplit`'s Content-Type branch and in `curl.Render`'s guard, so an entry-sourced value meets the same predicate as a spec-sourced one.

## Finding 22: `history replay` reorders the query string, and the fix that claimed to preserve order asserts in a comment that it does
- **Reviewer:** spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — `main` re-sent the stored URL and preserved order exactly
- **Repeat-of:** cycle 2 finding 1 — `c05ba96` attempted this fix and closed only the value-loss half, leaving the ordering half and adding a comment asserting the property it does not have
- **File:** internal/replay/replay.go:267
- **Description:** `query()` returns declared and undeclared parameters as two slices; `request.Build` then renders `located(bound, inQuery)` first and `pairs(in.Query…)` after, so declared parameters are hoisted ahead of undeclared ones. Reproduced at the wire by two reviewers: `call --query 'a=b=c' --query zz=1 --query tag=x` sent `?a=b%3Dc&zz=1&tag=x`; `history replay 1` sent `?tag=x&a=b%3Dc&zz=1`, exit 0, nothing on stderr. The comment at `replay.go:265-266` says "Order is preserved, so the bytes on the wire are the bytes that were recorded" — the property it does not have. Order is semantically significant for signed URLs and for APIs that read the first occurrence of a repeated key. Repeated and empty declared values do round-trip byte-identically.
- **Suggested fix:** Preserve the recorded pair order verbatim instead of rebuilding declared-first, and replace the comment with a test asserting the replayed query string equals the recorded one byte for byte. **Note the `Repeat-of`: the previous approach — patching the value loss and documenting the ordering as fixed — failed. Do not re-apply it.**

## Finding 23: `history replay` drops a declared cookie from *every* entry the store writes, sending a different request than the one recorded
- **Reviewer:** spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` for the drop — a `main`-built binary produces the identical missing cookie. `c05ba96` added the declared-cookie branch and its test, which this makes dead code
- **Repeat-of:** cycle 2 finding 7 — `c05ba96` attempted a fix by adding a declared-cookie branch that no entry the store produces can reach, and a test using an entry shape the store cannot produce
- **File:** internal/replay/replay.go:314
- **Description:** `corpus.Redactors.cookies` (`internal/corpus/entry.go:215-227`) writes `secret.Placeholder` for **every** non-secret cookie value, so an ordinary declared cookie parameter is always stored as `<redacted>`. Reproduced end to end: `call cook.yaml getThing --param sid=abc --param XT=hdr` put `Cookie: sid=abc` on the wire and stored `"cookies":{"sid":"<redacted>"}`; `history replay cook.yaml 1` warned "the recorded cookie \"sid\" held a redacted value … replaying without it" and the listener received **no Cookie header at all**, at exit 0. The replay is therefore never the request `history show` displays, for any cookie-carrying operation. `c05ba96`'s branch at `replay.go:314-322` is unreachable from any entry the store produces, reporting coverage that does not exist.
- **Suggested fix:** Stop blanket-redacting cookie *values* in `corpus` — apply the same name-based `Redactor.IsSensitive` rule headers get — so the declared-cookie branch becomes reachable and a replay reproduces the recorded request. Otherwise delete the branch and its test and say in the docs that cookies are never replayable. **Note the `Repeat-of`: adding a branch without checking it is reachable from a real entry is the approach that already failed once.**

## Finding 24: Repeated request headers reach the wire as one comma-joined header on replay
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` — a `main`-built binary produces the identical joined header on the same fixture
- **Repeat-of:** none
- **File:** internal/corpus/entry.go:200
- **Description:** **New this cycle.** `EntryRequest.Headers` is a `map[string]string` and `Redactors.headers` folds repetitions with `existing + ", " + value`; `internal/replay/replay.go:291` reads that map back and emits one `--header` per key. Reproduced:
  ```
  call spec2.yaml cookieOp --param sid=abc123 --header X-Multi=one --header X-Multi=two
    wire: Cookie: sid=abc123 / X-Multi: one / X-Multi: two
  history replay 1
    wire: X-Multi: one, two          <- one header, joined
  ```
  Comma-folding is only equivalent for `#list`-grammar fields, which an arbitrary header is not. `history show` displays the folded form too, so the store and the replay agree with each other and neither agrees with what was sent — the recorded artifact cannot answer "what did I call" for this shape. The corpus doc comment (`internal/corpus/entry.go:77`) states the folding is "the way HTTP joins repeated field values", which is a claim about the store; nothing states that replay inherits it. The asymmetry is the tell: `EntryResponse.Headers` already uses ordered pairs (`entry.go:84-88`) for exactly this reason.
- **Suggested fix:** Either make `EntryRequest.Headers` ordered pairs (`[]corpus.Pair`), matching the response side, so repetition survives the round trip — or keep the map and have `replay` warn through `clierr.Warnf` for any value containing `", "` it cannot prove was one header.

## Finding 25: The replay warning about a dropped credential states the opposite of what goes on the wire
- **Reviewer:** spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/replay/replay.go:416
- **Description:** Reproduced at the wire on two limbs. `history replay local.yaml 1` printed `warning: the recorded header "Authorization" held a redacted value, which history does not store; replaying without it` while the listener received `Authorization: Bearer CANARY-BEARER-777` — `config.Resolve` re-resolved it a few lines later. The query limb prints the same inverted line while `?api_key=KEYVAL` goes out. A warning that contradicts the wire is worse than no warning: an operator reading it concludes the request went out unauthenticated. CLAUDE.md's own definition of a warning is "something the caller should know".
- **Suggested fix:** Say what happens — the stored placeholder was dropped and the credential was re-resolved from the environment. Assert the wording against a wire capture, not against the string alone.

## Finding 26: `history replay` silently retargets a recorded call at a different declared server, with the credential, while its comment says it refuses to
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — `main` re-sent the stored URL and could not retarget
- **Repeat-of:** none
- **File:** internal/replay/replay.go:188
- **Description:** `target()` returns the resolved target unconditionally when `allowed.Allows(in.Entry.URL)` is true, with nothing on stderr. So a call recorded against declared server A replays against declared server B, credential attached. The function's own comment (`replay.go:169-172`) says "Silently retargeting a recorded call at a different host would make `replay` mean something other than 'do that again'." DESIGN.md:407 covers only the outside-the-set case; a stored host *inside* the set but different from the resolved target is described accurately by neither the doc nor the comment.
- **Suggested fix:** `clierr.Warnf` when the resolved target differs from the recorded host, naming both, and correct the comment to describe what the code does.

## Finding 27: `history replay` silently retargets a recorded call at a different path prefix, with the credential, while `history show` displays the path it will not send
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — verified against a `main`-built binary on the same fixture and store: `main` re-sent the recorded URL and replayed `/v1/pets/42` both times
- **Repeat-of:** none
- **File:** internal/replay/replay.go:342
- **Description:** **New this cycle**, and a distinct limb from Finding 26, which is about the *host*. `replay.target` (`:173`) compares only `request.Host(stored)` against `request.Host(target)`, so any base-URL **path prefix** difference passes unnoticed; `pathParams` then discards everything before the operation's own template segments by design (`got = got[len(got)-len(want):]`). The recorded prefix is dropped and the current one substituted, silently:
  ```
  call ... --base-url http://127.0.0.1:41999/v1 --param petId=42
    wire: GET /v1/pets/42        Authorization: Bearer CANARY-BEARER-1
  history show 1  ->  "url":"http://127.0.0.1:41999/v1/pets/42"
  history replay 1 (spec server http://127.0.0.1:41999/v2)
    wire: GET /v2/pets/42        Authorization: Bearer CANARY-BEARER-1     exit 0
  ```
  With no `--base-url` at all the same store replays as `GET /pets/42`. `/v1` → `/v2` is the ordinary shape of an API version bump, and a replay that silently moves to it sends the production credential to an endpoint the caller never named while `history show` — the command an agent uses to decide what to replay — displays the other one.
- **Suggested fix:** Compare the recorded base against the resolved one on the whole prefix, not just the host: derive the recorded prefix as `stored.EscapedPath()` minus the template tail `pathParams` already computes, and `clierr.Warnf` naming both when it differs. Warning rather than refusal keeps the local-twin ergonomics §5a is explicit about. Add a replay test asserting the wire path equals the recorded one for a `/v1` base URL — the assertion that fails at HEAD.

## Finding 28: `history replay` omits the §5a query-string warning `call` fires for the identical request
- **Reviewer:** spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` — `main`'s `history replay` also emits no such warning
- **Repeat-of:** none
- **File:** cmd/talaria/history.go:227
- **Description:** DESIGN.md §5a's leak-channel table requires, for query-string API keys, that talaria "warn once on stderr" — the key lands in server access logs whatever redaction does. `cmd/talaria/call.go:158` does this via `warnQueryCredentials`; the replay RunE calls `warnWithheldCredentials` on the line beside it and never the query warner, while `replay.Build` re-resolves the credential through `config.Resolve` and `request.Build` puts it in exactly the same place. Verified back to back on one store: `call` printed the warning, the replay of that same entry printed only Finding 25's inverted line while putting the same key in the same place on the wire.
- **Suggested fix:** Hoist both onto a single `warnRequest(stderr, warner, req)` called by every command that sends a `*request.Request`, so a future sender inherits both — the same shape CLAUDE.md prescribes for the host flags.

## Finding 29: Replay re-serialises stored pairs through `name=value`, so an operation declaring two names differing only by an `=` replays the wrong parameter
- **Reviewer:** spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/replay/replay.go:269
- **Description:** `query()`, `headers()` and the cookie branch all build `name+"="+value` strings that `request.Build` re-parses with `strings.Cut(raw, "=")`. **Narrowing cycle 4's impact claim, which was too broad:** a name containing `=` cannot be bound from the CLI at all, and a hand-edited entry whose operation declares only `a=b` fails loudly (`getThing declares no parameter "a"`, exit 2, nothing sent). The silent shape is narrower and real: an operation declaring **both** `a` and `a=b` records `a=b`=`VAL1` and replays `GET /thing?a=b%3DVAL1`, i.e. `a`=`b=VAL1`, at exit 0 with empty stderr. A name containing `&` round-trips byte-identically and is not affected.
- **Suggested fix:** Carry the pairs as structured `(name, value)` through to `request.Build` instead of flattening. Add a case with two parameters whose names differ only by an `=`.

## Finding 30: Replay drops a path parameter's recorded value when the operation declares the same name in two locations
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — `main` re-sent the stored URL and was correct
- **Repeat-of:** none
- **File:** internal/replay/replay.go:97
- **Description:** `Build` concatenates path params, declared query params and declared header params into one `Params` slice, and `binder.params` keys them into a single `map[string]string`. An operation declaring `id` in both `path` and `query` therefore loses the recorded path value to the query one, and the replay targets a different resource than the one recorded. Silent.
- **Suggested fix:** Key the recorded values by (name, location) rather than by name alone, and fail that entry loudly rather than dropping a value.

## Finding 31: The canary CLAUDE.md names as the gate on the file-body rule passes for the wrong reason
- **Reviewer:** security, spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `f8ef1d3` (task 5) — `git log main...HEAD -- internal/canary/canary_test.go` shows that commit adding the test
- **Repeat-of:** none
- **File:** internal/canary/canary_test.go:824
- **Description:** `TestARequestBodyFromAFileReachesNoSurfaceButTheWire` writes its canary into a field named `refresh_token`, which is in `secret.builtinBodyPaths` (`internal/secret/response.go:18`). The body-redaction limb therefore hides the value regardless of what `curl.Render` and `request.body` do — the test's own doc comment says "each one alone would still leak", but the fixture makes the leaking limb unobservable. Finding 8 is exactly the same shape with `client_secret` instead and reproduces at exit 0. CLAUDE.md names this test as the gate on both halves of the rule; it currently gates neither, and the `--body -` limb has no case at all. It also has no case that executes the request, executes the emitted curl, and compares the two — which is what would have caught Findings 1, 4 and 10.
- **Suggested fix:** Use a field name outside `builtinBodyPaths` (`client_secret`) so the assertion depends on the reference, not on path redaction; add a stdin case; and add an executed-bytes-versus-pasted-bytes assertion. Fix with Finding 8, not after it.

## Finding 32: CLAUDE.md's `corpus` may-not-import-`curl` boundary is false in the tree, and the import-graph guard it names does not assert it
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` — `git show main:CLAUDE.md` carries the same sentence at line 47, `git show main:internal/corpus/entry.go` already imports `internal/curl`, and `git show main:internal/e2e/boundary_test.go` has the same two-element `shared` list
- **Repeat-of:** none
- **File:** internal/e2e/boundary_test.go:32
- **Description:** **New this cycle.** CLAUDE.md:51-53 states the rule and names its enforcement: *"`corpus` may not import `curl` either: it takes a local observation struct, not a `*curl.Response`. `internal/e2e/boundary_test.go` asserts this against the real import graph."* All three clauses are false. `go list -deps ./internal/corpus` returns `github.com/Teeeep/talaria/internal/curl` (verified this cycle); `NewEntry` (`internal/corpus/entry.go:151`) takes a `*curl.Response`; and `boundary_test.go` checks only `shared = {operation, validate}` against `forbidden` — `corpus` appears only *in* `forbidden`, never as a subject. The suite is green. It matters because the next autonomous run reads CLAUDE.md as the specification of the boundaries and will believe both that the rule holds and that a violation would fail the suite. CLAUDE.md's own rules in the same file: "if you add a package that must not cross a boundary, add it there in the same commit" and "Never write a comment asserting a property no test enforces."
- **Suggested fix:** Decide which is true and make the other match, in one commit. Either add `internal/corpus` as a second subject in `boundary_test.go` with `internal/curl` forbidden and change `NewEntry` to take a local observation struct (the shape CLAUDE.md already describes, and what the twin will need anyway) — or drop the sentence from CLAUDE.md. Do not leave the sentence with no guard; that is the state that survived to `main` and through four review cycles.

## Finding 33: A lone `--allow-host=` is silently dropped instead of reported, and the test that proves otherwise never goes through the CLI
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — `--allow-host` and `allowedHost` are both new there
- **Repeat-of:** none
- **File:** internal/request/hosts.go:164
- **Description:** `AllowedHosts`' doc comment (`hosts.go:41-43`) argues explicitly that a silently dropped entry "looks exactly like a credential that was withheld for a good reason". Reproduced at the CLI: `--allow-host=` alone → exit 0, nothing on stderr; `--allow-host= --allow-host='x y'` → exit 2 with both reported. Cause: `cmd/talaria/root.go:158` reads the flag through `GetStringArray`, which round-trips through the flag's CSV string form; `stringArrayValue.String()` on `[""]` is `"[]"` and the conversion reads an empty payload as `[]string{}`. `TestAllowHostRejectsJunk` passes because it calls `AllowedHosts` and `request.Build` directly and never the command tree — the "green suite is not evidence" pattern in this branch's own conventions.
- **Suggested fix:** Read the flag through `pflag.SliceValue.GetSlice()`. Add a CLI-level case to `cmd/talaria/call_test.go`; the unit test cannot reach this.

## Finding 34: `history replay` can ship an empty `request.curl` at exit 0, and the condition is about to become more reachable
- **Reviewer:** spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `e47176c` (task 4)
- **Repeat-of:** none
- **File:** internal/curl/render.go:50
- **Description:** `Render` returns `""` for a request whose `Body.ContentType` would split the header block, while `document.body` skips `checkSplit` entirely when a `Content-Type` header is already present — so the request succeeds and the envelope carries `{"request":{"curl":"", …},"response":{"status":200}}` at exit 0 with an empty stderr. Reproduced with a hand-written entry carrying `request.headers["Content-Type"]` plus a CRLF `body.content_type`. DESIGN.md §3.4 promises "every executed call returns its curl equivalent". Findings 4, 5 and 10 all propose returning `""` for further shapes, so this is about to become more reachable, not less.
- **Suggested fix:** Make an empty render an explicit reported condition — a `clierr.Warnf` naming why no reproduction could be emitted, plus an envelope field an agent can branch on. Decide it once, before the other three fixes add callers.

## Finding 35: A response body with invalid UTF-8 is embedded raw in `--output json`, producing an envelope a strict JSON parser rejects
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` — `git blame -L 75,88 cmd/talaria/call.go` is entirely the initial commit
- **Repeat-of:** none
- **File:** cmd/talaria/call.go:81
- **Description:** Reproduced against a listener returning `Content-Type: application/json` and `{"a":"\xff\xfe"}`: the raw `0xff` lands in stdout, exit 0, and parsing the whole envelope fails with `'utf-8' codec can't decode byte 0xff in position 423`. Every consumer of `--output json` is an agent that parses it; an envelope that cannot be parsed is indistinguishable from a crash, and the exit code says success.
- **Suggested fix:** Detect invalid UTF-8 in the passthrough branch and fall back to the string-encoded form the non-JSON path already uses, so the envelope is always parseable.

## Finding 36: SIGHUP kills talaria mid-request with no cleanup, orphaning curl and leaving the request body and the raw unredacted response in TMPDIR
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` — `git show main:cmd/talaria/root.go` carries the identical `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`, and a binary built from `main` reproduces the leak byte for byte
- **Repeat-of:** none
- **File:** cmd/talaria/root.go:308
- **Description:** `run` traps exactly two signals. Every other catchable terminating signal — SIGHUP above all, which is what a closed terminal, a dropped ssh session, a `systemd` stop with `KillSignal=SIGHUP` or `timeout -s HUP` sends — takes its default action and the process dies where it stands. That is precisely the failure the doc comment eight lines above (`root.go:297-302`) says the signal context prevents. Reproduced side by side against a listener that sleeps 6s, with a binary body so the temp-file path is taken:
  ```
  SIGTERM  exit=1   {"code":1,"message":"the request was cancelled before it completed"}
                    leftovers: none                 orphan curl: none
  SIGHUP   exit=129 stderr (empty)
                    leftovers: /tmp/talaria-body-*  (0600, the request body)
                               /tmp/talaria-call-*/ (0700 dir)
                    orphan curl PPid: 1
  ```
  The orphan then completed the aborted `POST` and wrote the response into the leaked directory (`body`, 0600, containing the unredacted server response). Nothing reclaims either artifact for 24 hours — `SweepStale`'s `staleGrace` is `24 * time.Hour` (`internal/curl/sweep.go:26`), observed walking straight past both. Two consequences: a mutation the caller cancelled reaches the server anyway with no record on stdout or in history, and a response body §5a treats as sensitive sits in a shared TMPDIR. Not covered by any queued task — task 14 is "Make Ctrl-C and SIGTERM work" and the cycle-4 hang findings are all about blocking calls ignoring the context, an orthogonal defect. `grep -rin 'sighup\|hangup'` over the repo returns nothing.
- **Suggested fix:** Add `syscall.SIGHUP` (and, if the exit-code contract can take it, `syscall.SIGQUIT`) to the `signal.NotifyContext` call. One argument; the existing cancellation path does the rest, as the SIGTERM column shows. Add an e2e case that sends SIGHUP to a call against a slow listener and asserts no `/tmp/talaria-*` leftovers and no surviving curl.

## Finding 37: A semantically broken spec maps to exit 2, which DESIGN.md §4 defines as a usage error
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** design — DESIGN.md §4 defines 2 as "Usage error (unknown operation, missing required param)" and 3 as "Spec parse/load error", and never says which code a spec that parses but cannot be used gets. A fix must choose between widening 3 and widening 2
- **Introduced-by:** `d2cad54` (task 3) — the branch's `Resolve` splits its terminal error into exit 5 / exit 2 where `main`'s was a single `clierr.Usage`, and `checkEnvCollisions` is entirely new
- **Repeat-of:** none
- **File:** internal/config/auth.go:186
- **Description:** Three failure modes land on exit 2 — an undeclared scheme reached from `call`, the same from `auth check` (`cmd/talaria/auth.go:234`), and `checkEnvCollisions`' refusal (`auth.go:281`). All three describe a defect in the *document*, not in the invocation, and none emits `valid_alternatives`. AGENT.md:198 tells an agent exit 2 means "fix the invocation using `valid_alternatives` and the message" — advice no agent can act on for a spec it did not write. CLAUDE.md was updated with the new rule and README:174-176 documents the collision case, but DESIGN.md §4 was not amended. CLAUDE.md's own rule is "A new failure mode maps to an existing code or the design doc changes — **never both silently**"; here a new failure mode was mapped onto a code whose published meaning does not cover it *and* the conventions file was changed.
- **Suggested fix:** Decide the code in DESIGN.md §4, then either amend §4 and AGENT.md's table together, or move `unsupportedReason`'s undeclared branch and `checkEnvCollisions` to `clierr.SpecLoad`. `cmd/talaria/agentdoc_test.go` already cross-checks the AGENT.md table against `clierr.Code`, so the doc edit stays honest cheaply.

## Finding 38: DESIGN.md §4's CLI surface still spells `history replay <n>`, which the branch made unrunnable
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2) — `git show main:cmd/talaria/history.go:167` reads `Use: "replay <id|n>"`; HEAD reads `replay [spec] <id|n>`. `git diff main...HEAD -- docs/design/DESIGN.md` is empty, so the code moved and the spec did not
- **Repeat-of:** none
- **File:** docs/design/DESIGN.md:207
- **Description:** §4's CLI-surface block deliberately marks which commands take a spec — `list [spec]`, `describe [spec]`, `call [spec]`, `auth check [spec]` — and spells replay as `talaria history replay <n>`, with no `[spec]`. Since task 2 made replay re-derive through the spec, that invocation no longer works: `talaria history replay 3 --output json` → `{"error":{"code":2,"message":"no spec given: pass one as the positional argument, with --spec, or in $TALARIA_SPEC"}}`, exit 2. DESIGN.md v0.5's own header announces "`history replay` resolves nothing from a stored entry" — the change reached the changelog and never reached §4. README and AGENT.md were both updated to `history replay [spec] <id|n>`; the specification alone was not, so the three shipped documents disagree and DESIGN.md is the one CLAUDE.md calls "the source of truth for scope and behaviour".
- **Suggested fix:** Amend DESIGN.md §4:207 to `talaria history replay [spec] <n>` and note that replay takes `--base-url`/`--allow-host`. §5a's replay table already describes the mechanism; only the synopsis is stale.

## Finding 39: AGENT.md documents a `--source run` filter value that exits 2, and tells the agent `run` records history
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Introduced-by:** `none` — `git show main:AGENT.md` carries both lines unchanged and the branch did not touch them
- **Repeat-of:** none
- **File:** AGENT.md:42
- **Description:** `talaria run` was cut on 2026-08-03 (DESIGN.md §7, CLAUDE.md's scope note). AGENT.md:42 still documents `--source call|run|replay` and AGENT.md:215 still tells the agent that "`call`, `run` and `replay` all record". Verified: `talaria history --source run --output json` → `{"error":{"code":2,"message":"no history source \"run\"","valid_alternatives":["call","replay"]}}`, exit 2. AGENT.md is a first-class deliverable (DESIGN.md §3.7) written for the one reader who cannot check the source when the manual is wrong, and it *has* a gate — `cmd/talaria/agentdoc_test.go` cross-checks exit codes, command names, env-var names and fenced invocations — but nothing reads a flag's *argument* in a table cell, which is why this passes. The same drift lives in README:23, README:435-438 and in comments at `internal/curl/config.go:149`, `:179`, `cmd/talaria/call.go:221`, `cmd/talaria/history.go:398` and `internal/clierr/clierr.go:36`.
- **Suggested fix:** Drop `run` from AGENT.md:42 and :215, from README:23 and README:435-438, and from the five comments — one commit. Extend `agentdoc_test.go` to check enumerated flag values in the command table against the code's own valid-alternatives list, which is what would have caught it.

---

## Finding 40: CLAUDE.md names `hostFlags(cmd)` in `cmd/talaria/call.go` as the single reader of the host flags; no such function exists
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `e2e4a9d` (task 6) — `d1548bf` introduced `hostFlags`, task 6 folded it into `newInvocation` (`IMPLEMENTATION_PLAN.md:682` records the fold) without updating the CLAUDE.md sentence naming it
- **Repeat-of:** none
- **File:** CLAUDE.md:75
- **Description:** `grep -rn hostFlags --include=*.go .` returns nothing (verified this cycle). CLAUDE.md:75 says "every command that reports on or sends to a host reads both through `hostFlags(cmd)` in `cmd/talaria/call.go`, so `auth check` cannot drift from what `call` does" — the mechanism is now `newInvocation(cmd)` in `cmd/talaria/root.go:139`, which CLAUDE.md:127-134 describes correctly a few paragraphs later. The conventions file is what the next autonomous run reads first, and it points at a symbol that does not exist. (Reported in cycles 1–2 and dropped by cycle 4; still true.)
- **Suggested fix:** Replace the reference with `newInvocation(cmd)` in `cmd/talaria/root.go`, and fold the sentence into the "One `invocation` per RunE" paragraph so there is one statement of the rule.

## Finding 41: Three cross-references justify a guard by `history replay --dry-run`, a surface that does not exist
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `e47176c` (task 4) for the code comments; `e2e4a9d` (task 6) for the CLAUDE.md copy
- **Repeat-of:** none
- **File:** internal/curl/render.go:49
- **Description:** `curl.Render`'s Content-Type guard is justified by "`history replay --dry-run` takes that media type from the history file"; `internal/curl/config.go:290` and CLAUDE.md's worked example repeat it. `talaria history replay --help` registers only `--allow-mutations` plus the persistent root flags — there is no `--dry-run`. The guard itself is correct and worth keeping; the stated reason is not.
- **Suggested fix:** Reword to the real reason — `Render` is called from `callPayload` on a `Request` that `internal/request` did not build, and must not emit a command `BuildConfig` would refuse. Fix the CLAUDE.md copy in the same commit.

## Finding 42: The "no base URL" usage error prints its sentence twice
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `c05ba96` — `git log -S ErrNoBaseURL -- internal/request/build.go` returns only that commit; `main:internal/request/build.go:167` emitted it once
- **Repeat-of:** none
- **File:** internal/request/build.go:186
- **Description:** `ErrNoBaseURL` already carries the full sentence and line 186 wraps it with the same sentence again: `{"code":2,"message":"cannot build a request for listPets: no base URL: the spec declares no server, so pass --base-url or set one in a profile: the spec declares no server, so pass --base-url or set one in a profile"}`. Cosmetic — the code and `valid_alternatives` survive the wrap.
- **Suggested fix:** `return "", ErrNoBaseURL`.

## Finding 43: `--allow-host '*.example.com'` is accepted as a member that can never match
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/request/hosts.go:156
- **Description:** Reproduced — `call other.yaml getPet --allow-host '*.example.com' --dry-run` accepts the value, and `HostSet.Allows` compares canonical hosts exactly, so it never matches anything. Fail-safe, but the human believes they allowed a host and the resulting withhold message never mentions the wildcard.
- **Suggested fix:** Reject a value containing `*` at `allowedHost`, so the failure is at the flag rather than at the request.

## Finding 44: `internal/replay`'s package doc claims every refusal path has a test
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `d1548bf` (task 2)
- **Repeat-of:** none
- **File:** internal/replay/replay.go:11
- **Description:** "every refusal below is a decision with a test" — Findings 23 and 26 are two that are not, and Finding 23's test asserts against an entry shape the store cannot produce. CLAUDE.md: "Never write a comment asserting a property no test enforces." Task 18's scope, but the claim was written by task 2 and is live now.
- **Suggested fix:** Add the missing cases or drop the claim.

## Finding 45: `//nolint` waivers name task 1, which is complete, while the debt belongs to tasks 13 and 14
- **Reviewer:** spec-compliance, concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `e2e4a9d` (task 6) for the source waivers; `89ad251` for `.golangci.yml`
- **Repeat-of:** none
- **File:** internal/spec/source.go:117
- **Description:** `internal/spec/source.go:117-122` and `internal/curl/version.go:42-45` both carry `// TRACKED DEBT (phase-2a task 1)` with "Remove this waiver in the commit that fixes it", but task 1 is "Substitute server variables and expose the spec's server URLs" and is `"done": true` in `tasks.json`. The spec-fetch context belongs to task 14 and the version preflight to task 13. An audit grepping for the waiver's task number finds a completed task and either removes a live waiver or leaves the debt untracked. `.golangci.yml:71-76` additionally describes "the two noctx waivers in internal/spec and internal/curl" as exclusions when the file contains none.
- **Suggested fix:** Renumber to `phase-2a task 14` and `phase-2a task 13` in both the prose and the directives, and correct the `.golangci.yml` comment to name the `//nolint` sites.

## Finding 46: `--dry-run` exits 0 and emits a command for a request `call` refuses at exit 2 when the resolved credential value carries CRLF
- **Reviewer:** integration
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `none` — reproduces on a `main`-built binary
- **Repeat-of:** none
- **File:** internal/curl/render.go:50
- **Description:** `Render` guards only `Body.ContentType`, and never sees a credential *value* at all — it is symbolic by design. `BuildConfig`'s `checkSplit` sees the resolved value and refuses. With `TALARIA_AUTH_BEARER=$'good\r\nX-Injected: pwned'`, `--dry-run` exits 0 emitting `-H "Authorization: Bearer $TALARIA_AUTH_BEARER"` while the real call exits 2. Held at INFO because the value is operator-supplied, not spec- or entry-supplied — no privilege boundary is crossed, unlike Finding 5's name limb, which is the same divergence driven by a hostile spec. It still breaks AGENT.md's `list → describe → dry-run → call` pre-flight for an agent whose operator exported a malformed variable.
- **Suggested fix:** Nothing at `Render` — it cannot resolve, and should not. Either have the binder record that a credential's *shape* was not checkable symbolically, or accept the divergence and say so in DESIGN.md §3.4 beside the body-source rules.

## Finding 47: `isolate`'s `Cancel` sends SIGKILL to a raw pgid without the stdlib's already-reaped guard
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `none` — `diff <(git show main:internal/curl/procgroup_unix.go) internal/curl/procgroup_unix.go` is empty; `git log` bottoms out at the initial commit
- **Repeat-of:** none
- **File:** internal/curl/procgroup_unix.go:33
- **Description:** `exec.Cmd.Cancel` runs on the `watchCtx` goroutine, which races `Wait`'s call to `Process.Wait()`. The stdlib's default (`c.Process.Kill()`) is guarded — it returns `os.ErrProcessDone` once the child is reaped. `isolate` replaces it with a bare `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)`, which has no such guard: once the child is reaped its pid — and therefore its pgid — is back in the kernel's free pool. A harness mirroring `isolate` and `ExecuteWith` exactly measured the window: **Cancel ran with the child already reaped in 85 of 5000 cancelled runs (1.7%)**. What did *not* reproduce is the harmful outcome — delivering SIGKILL to an unrelated group requires pid reuse inside that window, and `pid_max` here is 4194304. Hence INFO: the exposure is quantified, the consequence is not. The fallback limb is fine (on `ESRCH` it falls through to the guarded `Process.Kill()`), and the comment at `:34-36` describes only that case, so this is a gap rather than a false comment.
- **Suggested fix:** Guard the raw kill the way the stdlib does — check `cmd.Process.Signal(syscall.Signal(0))` for `os.ErrProcessDone` first and return that error unchanged (`watchCtx` already handles it), or capture the pid at `Start` time and skip the group kill once `cmd.ProcessState != nil`. Two lines, and it keeps the group-kill behaviour the comment justifies.

## Finding 48: `.golangci.yml`'s errcheck exclusion permits exactly the write-path dropped `Close` its own comment says it does not
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `89ad251` — the file does not exist on `main`
- **Repeat-of:** none
- **File:** .golangci.yml:37
- **Description:** Still live at HEAD, re-confirmed this cycle. The exclusion's comment argues that a dropped error on a *write* path is the cache-poisoning class and is not waived, but the pattern as written admits it.
- **Suggested fix:** Narrow the pattern to read paths, or drop the claim from the comment.

## Finding 49: `--timeout` has no upper bound, and the comment says it does
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `none` — `git show main:internal/curl/config.go` carries `withDefaults` and its comment unchanged
- **Repeat-of:** none
- **File:** internal/curl/config.go:53
- **Description:** Re-measured this cycle: `--timeout 2e10` still overflows into the silent 30s fallback rather than being refused. The phase doc defers the overflow itself (cycle-1 finding 28); the unenforced comment is the part live here.
- **Suggested fix:** Bound the flag at parse time, or drop the claim from the comment.

## Finding 50: `preflight`'s `sync.Once` is keyed on nothing, and its stated reason names a command that was removed
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `none` — the `var` block and its comment are unchanged from `main`
- **Repeat-of:** none
- **File:** internal/curl/version.go:41
- **Description:** The `sync.Once` caches the version preflight across every call in the process, justified by a comment naming `talaria run`, which was cut on 2026-08-03. With `run` gone there is one request per process, so the `Once` guards nothing it was written to guard. Harmless today; the reason is stale.
- **Suggested fix:** Reword or remove, with task 13 (the version preflight's bound), which touches the same lines.

## Finding 51: A comment in the spec cache describes a race that cannot occur
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `89ad251`
- **Repeat-of:** none
- **File:** internal/spec/source.go:174
- **Description:** `defer os.Remove(tmp.Name()) //nolint:errcheck // Best effort: the rename below usually wins the race with it.` The deferred `os.Remove` runs strictly *after* the `os.Rename` in the same function, so it is a guaranteed `ENOENT`, not a race that the rename "usually" wins. Re-confirmed this cycle alongside the 30-process cache stress, which left one correct file and no temps.
- **Suggested fix:** Reword to say the remove is a no-op on the success path and cleans up only when the rename failed.

## Finding 52: Two archived findings files carry raw NUL bytes, so git treats them as binary
- **Reviewer:** integration
- **Severity:** INFO
- **Blocked-by:** none
- **Introduced-by:** `ebeb244` for the first; `0b89537` for the second
- **Repeat-of:** none
- **File:** docs/review/20260804-184701-cycle1-findings.md:77
- **Description:** `file docs/review/*.md` reports `data` for `20260804-184701-cycle1-findings.md` and `20260804-195027-cycle1-findings.md`; the rest are `UTF-8 text`. `git diff` renders them as `Bin 0 -> 42194 bytes` and plain `grep` refuses them without `-a` — which is why `.ralph/loop.sh` comments that `-a` on every read of `REVIEW_FINDINGS.md` is "load-bearing, not defensive". A NUL in a findings file is a review artifact escaping into the repo, and it makes the archive undiffable.
- **Suggested fix:** Strip control bytes when writing a findings file, and scrub the two archived ones. Consider adding a NUL check to `.github/workflows/hygiene.yml`, which already gates tracked-file shape.

---

## Live, but already scoped to a queued task — not counted as findings

These reproduce at HEAD and are real, but their entire remedy is a task in `tasks.json` that has
not started. They are normal mid-phase state, not something the branch broke. Listed so the
evidence gathered this cycle reaches the task that will fix it.

- **Task 7 — a network-fetched spec is read with an unbounded `io.ReadAll`** (`internal/spec/source.go:132`). CRIT-grade in isolation: `Loader.fetch` bounds the fetch in *time* and not at all in size, and `grep -rn "MaxBytesReader|LimitReader|maxSpec" internal/` finds nothing. **New measurement this cycle:** a listener serving an 800 MB `/spec.yaml` drove peak RSS to **5,473,868 kB — 5.47 GB, ~6.8× amplification** (the sniff and the YAML parser each re-allocate). On the 8 GB host CLAUDE.md documents, a ~1.2 GB response is a kernel OOM kill: exit 137, which is in no exit-code contract, so an agent cannot classify it and a retry loop repeats the kill. Reproduces identically on `main`. Suggested bound: `io.LimitReader(resp.Body, maxSpec+1)` with a stated const beside `fetchTimeout`, the same bound applied to `LoadFile` via `os.Stat`, and the ceiling recorded in DESIGN.md.
- **Tasks 13, 14, 19 — the hang set.** Re-confirmed live and unchanged from `main`: with the history lock held by another process, `talaria call` **completed the request on the wire**, then blocked in `flock` forever — deaf to SIGINT *and* SIGTERM (`SA_RESTART`), state `S`, requiring `kill -9`, with stdout never written so the caller never learns the call went out. `internal/spec/source.go:122` and `internal/curl/version.go:45` remain uncancellable behind their `//nolint:noctx` waivers. `internal/corpus/lock_unix.go` and `internal/curl/exec.go` are byte-identical to `main`. Finding 36 (SIGHUP) is *not* part of this set and is reported above.
- **Task 16 — the spec cache is served forever** (`internal/spec/source.go:82`): no TTL, no conditional revalidation, no `--refresh`, contradicting DESIGN.md:235-239 outright.
- **Task 19 — `internal/corpus` takes 59.5s under `-race`** for 999 sequential seeded appends, each a full read-parse-trim. Not a race; the double-read-per-append this task removes.
