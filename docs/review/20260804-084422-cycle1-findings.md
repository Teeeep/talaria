# Review — `phase-2a-remediation` vs `main` (cycle 3)

Four reviewers (security, spec-compliance, concurrency, integration) over the full branch diff
(95 files, +17304/−2026). `go build ./...`, `go vet ./...`, `gofmt -l .` and `go test -race ./...`
are all green with finding 1 live — the standing warning in CLAUDE.md holds for a fourth cycle.

**The two cycle-2 fixes both hold.** Commit `e281200` closes cycle-2 finding 1: all four trigger
routes were re-run against the built binary and each now either records a readable entry or is
refused with the stderr warning; the `maxKeptBytes = maxStoreBytes − (maxEntryBytes+1)` arithmetic
is exact at the boundary. Commit `d9d20fe` closes cycle-2 finding 2: `document.auth` writes six
pieces, `resolveParts` hands back the environment's own string uncopied, and the two new tests
(pointer identity, zero allocations over `credentialShapes`) are non-vacuous. Finding 1 below is a
*new* defect in `e281200`'s own truncation logic — it is not a repeat of what that commit fixed.

Branch is mid-flight: tasks 1–17, 25–35 complete; 18–24 and 36–42 not started. Cycle-2 findings
3–9 map to unstarted tasks 36–42 and are recorded in finding 14 as remaining scope, not
re-derived as new defects.

## Finding 1: `halveBodies` destroys the request body to make room for an oversized response, and the entry is then permanently un-replayable
- **Reviewer:** integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/file.go:337
- **Description:** `halveBodies` cuts *both* bodies on every pass, whichever one caused the
  overage:

  ```go
  if body := halfOf(e.Request.Body); body != nil { e.Request.Body = body; cut = true }
  if e.Response != nil { if body := halfOf(e.Response.Body); body != nil { … } }
  ```

  `Entry.Replay` refuses any entry whose *request* body is `Truncated`
  (`internal/corpus/replay.go:165`), so cutting the request body is strictly more destructive than
  cutting the response body — and it is cut first and unconditionally.

  Proven with a scratch test in `internal/corpus` (25-byte request body, 64 KiB of `0x01` as the
  response body — valid UTF-8, so it takes the text branch and `encoding/json` expands it 6× to a
  393 KB line):

  ```
  line=196929 bytes
  stored request body="{\"name\":\"rex"   truncated=true
  stored response body len=32768          truncated=true
  ```

  The final line is 196,929 bytes against a 262,144 bound — **one halving of the response alone
  was enough to fit**, so the request body was cut for nothing. Wire-verified independently
  against the built binary by the integration reviewer: `call` exits 0, `Append` returns nil, so
  `recordCall` prints **no stderr warning**; `history replay <id>` then exits 2 forever; and
  `history show` in pretty and TSV prints `{"name":"rex` with no marker (finding 7). Only
  `--output json` carries `"truncated": true`.

  Reachable with no hostile peer: any response body with a few tens of KB of C0 bytes, or two
  base64 bodies near `MaxBody`, and the user's own request body is destroyed. This is the
  capability §5a and README are about — `history replay` is how a recorded mutating call is
  reproduced — disabled silently by a *response* the caller did not control.

  The existing fixtures cannot catch it: `oversizeEntry` (`internal/corpus/file_test.go:290`) puts
  `MaxBody` in both bodies, and `TestAppendRecordsABodyWhoseJSONEncodingExpandsPastTheLineBound`
  (`internal/corpus/store_test.go:973`) builds a request with no body at all. The asymmetric case
  — small request body, oversized response — has no test.
- **Suggested fix:** cut what history can most afford to lose *in order*. Exhaust the response
  body first; touch the request body only when the response body has nothing left to give.
  `halveBodies` still returns true as long as either step cut something, so `encodeLine`'s loop and
  its refusal are unchanged. Regression test in `internal/corpus/file_test.go`: a small request
  body plus an escape-heavy `MaxBody` response must round-trip with
  `got.Request.Body.Data` intact and `got.Request.Body.Truncated == false`, and the entry must
  still `Replay` without error.

## Finding 2: `Entry.Replay` ignores `HeadersTruncated`, so a capped entry replays into a different request with nothing on any channel
- **Reviewer:** security, spec-compliance, integration (three, independently)
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/replay.go:84
- **Description:** `capHeaders`/`capMultiHeaders` (`internal/corpus/entry.go:315,337`, new in
  `e281200`) drop headers past `maxHeaderBytes` (8 KiB) and set `HeadersTruncated`, whose own doc
  comment says it exists *"so a reader never mistakes a capped set for the whole of it"*.
  `Entry.Replay` — the one reader that acts on an entry — never consults it.
  `out.headers(declared, e.Request.Headers)` binds the capped map, nothing lands in
  `Replayable.Dropped`, so `warnUnreplayable` never fires and `history replay` exits 0 having sent
  a request the original did not.

  Wire-verified: `call … --header "X-Big=<9000 bytes>" --header "Z-Small=keepme"` recorded
  `headers_truncated: true` with `X-Big` gone; `history replay <id>` exited **0**, warned only
  about `Authorization`, and the server received `Z-Small` and no `X-Big`.

  This is the identical argument the body rule makes twelve lines below at
  `internal/corpus/replay.go:165` — *"replaying it would send something the original did not"* —
  applied to one of the two capped fields and not the other. It breaks README.md:478 verbatim:
  *"Fields that could not be reproduced are reported on stderr rather than silently omitted."* And
  AGENT.md:272 tells agents *"Do not conclude a header was absent from a request whose entry says
  `headers_truncated`"* — while `history replay`, the tool's own re-run, draws exactly that
  conclusion mechanically. `maxHeaderBytes` is 8 KiB over the whole map, so ordinary `--header` use
  reaches it; a dropped idempotency or tenant header on an `--allow-mutations` replay is the bad
  case.
- **Suggested fix:** in `Entry.Replay`, treat `e.Request.HeadersTruncated` the way `body` treats
  `Body.Truncated`. Refusal (`clierr.Usage`, naming `maxHeaderBytes`) is the consistent choice
  given the body rule's own reasoning; if a partial replay is judged more useful, append to
  `Replayable.Dropped` so `warnUnreplayable` fires. Whichever, say which in README's replay bullet
  list, which currently enumerates every other refusal. Regression test: an entry with
  `HeadersTruncated: true` must produce either an error or a non-empty `Dropped`, never both empty.

## Finding 3: `auth check` reports a malformed `TALARIA_AUTH_BASIC` as satisfied where `call` exits 5
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** same class as cycle-2 finding 9 (task 40), different cell
- **File:** internal/curl/firewall.go:62
- **Description:** `basicPair` is new on this branch (task 16) and refuses a basic credential with
  no `user:` prefix at exec time with `clierr.CredentialMissing`. `auth check`'s verdict is
  `cred.Present()` (`cmd/talaria/auth.go:137`) — `os.LookupEnv` plus a non-empty test — so it never
  sees the shape. Wire-verified:

  ```
  $ TALARIA_AUTH_BASIC=nopassword talaria auth check basic.yaml --output json
  {"schemes":[{"scheme":"basicAuth","source":"env:TALARIA_AUTH_BASIC","present":true}]}   exit=0
  $ TALARIA_AUTH_BASIC=nopassword talaria call basic.yaml listPets --output json
  {"error":{"code":5,"message":"$TALARIA_AUTH_BASIC must be user:password; …"}}           exit=5
  ```

  DESIGN.md:346 is unqualified: *"**`auth check` never reports a scheme satisfied when the call
  would refuse it.** The two agree by construction, or `auth check` is worthless to an agent."*
  This is the direction it forbids. CLAUDE.md's matching rule — *"the verdict lives in
  `internal/config` … Never re-derive 'can this spec be called' in the command layer"* — is broken
  by a second verdict living in `internal/curl`.
  `TestAuthCheckAndCallAgreeOnUnsupportedSchemes` cannot catch it: it drives `call --dry-run`,
  which builds no config document, so `basicPair` never runs. CLAUDE.md already concedes the
  `--dry-run` half; it says nothing about `auth check`, which carries the stronger written promise.
- **Suggested fix:** move the shape test into `internal/config` so both commands read one verdict
  — a `Credential.Satisfied()` applying the `user:password` rule for `KindBasic` — and add a
  non-`--dry-run` row to the matrix in `cmd/talaria/auth_test.go`. `SecretRef.Present` already
  reads the value without printing it, so this does not breach *"never prints values"*;
  AGENT.md:141's looser *"`present` is a lookup; the value is never read"* needs the same-commit
  amendment. If the divergence is judged deliberate, record the cell in the matrix test and qualify
  CLAUDE.md's agreement rule, as cycle-2 finding 9 proposed for its own cell.

## Finding 4: The spec cache read is an uninterruptible `open(2)`, and the branch's new comment asserts it is not a wait
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none (sibling of cycle-2 finding 4 / task 37 — different package, different claim)
- **File:** internal/spec/source.go:112
- **Description:** Commit `6661045` made the whole spec-load path context-aware and deliberately
  left the cache read outside it, with the reason written into the code
  (`internal/spec/source.go:107-109`) and into CLAUDE.md:306:

  ```go
  // Deliberately not gated on ctx: reading the cache is not a wait, and the
  // answer is already on disk.
  if data, err := os.ReadFile(cachePath); err == nil {
  ```

  `os.ReadFile` opens `O_RDONLY`. On a FIFO with no writer that blocks in `open(2)` before any read
  — so *"reading the cache is not a wait"* is false, and the `LOCK_NB` analogy does not hold:
  `LOCK_NB` is non-blocking by construction, an open on a path this process did not create is not.

  Proven against the built binary with a FIFO planted at the cache path
  (`$XDG_CACHE_HOME/talaria/specs/<sha256(url)>`): `timeout -s INT 5 talaria list --spec http://…`
  was still running 30s later, blocked in `openat(2)` (`wchan: wait_for_partner`). The second way
  out works — a second SIGINT kills it via the restored default disposition — but everything that
  sends *one* signal never gets the process back: `timeout`, systemd's `TimeoutStopSec`, CI job
  cancellation, a supervisor's `kill`. That is the class `fetchTimeout`'s own comment says must
  become an exit code rather than a hung process an agent cannot interpret.

  The `os.ReadFile` itself is pre-existing (`git show main:internal/spec/source.go:86`); what is
  new on this branch is the ctx contract this line is the single documented exception to, plus the
  sentence claiming the exception is safe — the "comment asserting a property no test enforces"
  class, in the same commit that extended the rule.
- **Suggested fix:** open with `os.OpenFile(cachePath, os.O_RDONLY|syscall.O_NONBLOCK, 0)`, `Stat`
  the descriptor, treat a non-regular file as a cache miss (fall through to the fetch — the cache
  is an optimisation and every other failure there is already swallowed), then read under
  `io.LimitReader(f, maxSpecBytes+1)`, which closes task 20's unbounded-cache-read half in the same
  pass. Test: a FIFO at the cache path makes `Load` fall through rather than block, asserted from a
  goroutine against a timer the way `internal/corpus/lock_test.go` does. If deferred to task 20,
  narrow the comment at `internal/spec/source.go:107` and CLAUDE.md:306 to what is enforced — the
  exception is safe for a regular file, not for an arbitrary path.

## Finding 5: An over-bound history file exits 1, the code AGENT.md tells an agent to retry
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/file.go:277
- **Description:** `readStore`'s size refusal is new on this branch (task 9). It returns a bare
  `fmt.Errorf`, so `clierr.From`'s unclassified fallback makes it `CodeRequestFailed`:

  ```
  $ talaria history --output json          # 80 MB store
  {"error":{"code":1,"message":"the history file at …/history.jsonl is larger than the 67108864 bytes talaria will read; move it aside"}}   exit=1
  ```

  `talaria history` makes no request. DESIGN.md:279 defines exit 1 as *"Request could not be
  completed (network, curl failure)"*, and AGENT.md:227 turns that into instructions the agent will
  follow and cannot benefit from: *"check the host and `--base-url`; retrying once is reasonable."*
  Retrying never helps; the fix is in the message and is local. It is also inconsistent inside
  `internal/corpus` itself — an unreadable stored *entry* is already `clierr.Usage` → exit 2
  (`internal/corpus/entry.go:169,176`, and every refusal in `replay.go`); only the unreadable
  *store* falls through. `Store`'s "deliberately unclassified" comment reasons about the `Append`
  path, where `recordCall` turns the error into a warning; it does not cover the `Read` path, which
  reaches `RunE` directly. `tail`'s refusal (`internal/corpus/file.go:241`) has the same shape.
- **Suggested fix:** wrap both refusals in `clierr.Usage`, matching `Body.Bytes` in the same
  package. Keep the differing wording the CLAUDE.md rule requires ("move it aside" versus an I/O
  error).

## Finding 6: The referenced-body rule ships in DESIGN.md §3.4 and in neither README.md nor AGENT.md
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** partially cycle-1 finding 10 (task 32 fixed the DESIGN half only)
- **File:** README.md:332
- **Description:** Commit `9e8e4e0` made the emitted curl reference a body the caller did not type,
  and DESIGN.md:170-176 is normative: *"**A request body is referenced, never inlined, unless the
  caller typed it into argv:** `--body @file` emits `--data-binary @file` and `--body -` emits
  `--data-binary @-`"*. `grep -n 'data-binary' README.md AGENT.md` returns nothing.
  README.md:332-335 still describes `--body` as three interchangeable ways to supply bytes and says
  nothing about what the emitted command shows; README.md:337-347 — the paragraph whose whole
  subject is what the emitted command references — stops at credentials. AGENT.md:58 has the same
  gap in one bullet.

  This is the house rule *"A field an agent branches on is named in all three shipped documents …
  in the same commit as the code"*. `request.curl` is a field agents copy and run, and its
  runnability now depends on the body's origin: the `--data-binary '@/tmp/…'` form fails once the
  temp file is gone, and the `--data-binary '@-'` form hangs on stdin. Neither shipped document
  warns of either, and README.md:383's *"The `curl` field is identical either way"* reads as a
  promise that it is always the command that ran.
- **Suggested fix:** one sentence in README's `--body` paragraph and one in AGENT.md's `--body`
  bullet: a file- or stdin-supplied body is referenced as `--data-binary @path` / `@-` rather than
  inlined, because it may carry a credential the agent never saw, so reproducing the call needs the
  file (or the same stdin). Land it with task 42, whose fix changes the `request.body` half of the
  same rule.

## Finding 7: `history show` prints a truncated body as if it were whole
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/history.go:277
- **Description:** `bodyLine` returns `body.Data` verbatim for a non-base64 body and never consults
  `Body.Truncated`, while the header path four lines above (`cmd/talaria/history.go:251`) prints
  `<headers truncated: the entry kept what fit>`. `bodyLine` itself is unchanged from `main`, but
  this branch added both the header marker beside it and `halveBodies`, which makes a truncated
  body reachable for bodies well under `MaxBody` (finding 1). Verified: `history show <id> --output
  tsv` on a finding-1 entry printed `{"name":"rex` as the body row with no marker; pretty prints
  the same. Only `--output json` carries `"truncated": true`, so the two default surfaces state a
  body the call never sent.
- **Suggested fix:** append a marker in `bodyLine` when `body.Truncated` — `… (N bytes kept,
  truncated)` or the `<…>` form the header row already uses — so pretty and TSV say what JSON says.

## Finding 8: The truncated-body refusal names `MaxBody` rather than where the cut actually happened
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/replay.go:165
- **Description:** The message is hard-coded to `MaxBody`. When `halveBodies` did the cutting the
  number is wrong and misleadingly so. Verified: replaying a finding-1 entry (12 bytes stored, 25
  bytes originally sent) printed *"the recorded request body was truncated at 65536 bytes, so
  replaying it would send something the original did not"*. An operator reads that as "you sent a
  64 KB body" when they sent 25.
- **Suggested fix:** report the bytes actually kept (`len(body.Data)`) rather than the constant —
  "was truncated to N bytes". Fixing finding 1 makes this path rarer but does not remove it.

## Finding 9: `config.(*Profile).ReferencesEnv` is dead code whose comment claims a live security role
- **Reviewer:** security, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/config/auth.go:467
- **Description:** On `main` its one production caller was `cmd/talaria/history.go:733`
  (`return prof.ReferencesEnv(ref.Name)`), part of replay's old env-name allowlist. Commit
  `8cf9d1a` re-derived replay from the spec, so nothing stored is resolved and the caller went with
  it. The function survives, exported, referenced only by `internal/config/auth_test.go`, and its
  comment (`internal/config/auth.go:456-466`) still asserts the removed behaviour: *"It exists for
  `history replay`, which reads a variable's *name* out of a file on disk and would otherwise
  resolve whatever that file asked for. The set of names a replay may resolve is the
  TALARIA_AUTH_\* convention plus this."* Both halves are now false. A reader auditing the replay
  trust boundary from `internal/config` is told an allowlist exists that does not — the "comment
  asserting a property no test enforces" class CLAUDE.md forbids, in the package that owns the auth
  verdict.
- **Suggested fix:** delete `ReferencesEnv` and its two tests (`TestReferencesEnvAnswersForTheProfilesAuthMap`,
  `TestReferencesEnvOfNoProfileIsFalse`) — the mechanism it guarded was replaced, not relocated. If
  it is being kept for phase 2b, rewrite the comment to say it has no caller and why it is retained.

## Finding 10: `Append` trims before it encodes, so an entry `encodeLine` refuses can still cost an older one
- **Reviewer:** security
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/store.go:111
- **Description:** The critical section is `trim` → `storedIDs` → `encodeLine` → `write`
  (`internal/corpus/store.go:111-126`). `trim` counts the line the caller is about to write, so at
  `maxPerSource` it evicts the oldest entry of that source and rewrites the file. `encodeLine` —
  new in `e281200` — can then refuse, and `Append` returns without writing: one old entry deleted
  to make room for one that never arrived, reported only as *"the call was not recorded in
  history"*. Narrow (needs 1000 entries of one source *and* an unencodable entry) and the entry
  lost is the one retention would have dropped next, hence INFO. Recorded because it is the one
  seam where the deliberate write-last ordering — *"a retention pass afterwards can only report a
  failure for an entry already on disk"* — now has a *pre*-write pass that mutates the store for a
  write that may not happen.
- **Suggested fix:** reorder to `storedIDs` → `uniqueID` → `encodeLine` → `trim` → `write`, with
  `trim` still counting the incoming source.

## Finding 11: CLAUDE.md's rule "nothing that writes a directive may call `resolve`" is falsified by `resolve`'s only call site
- **Reviewer:** security
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/curl/firewall.go:157
- **Description:** Commit `d9d20fe` added the rule to CLAUDE.md and to `resolve`'s doc comment. The
  package's single call is `url, err := req.URL(resolve)` in `document.build`
  (`internal/curl/config.go:153`), whose very next line is `d.directive("url", url)` — so the one
  function that writes directives is the one that calls `resolve`, and for an `apiKey in: query`
  scheme the string it hands `directive` is the joined credential. The *behaviour* is fine and the
  residue is conceded two paragraphs earlier (`QueryString`'s own `strings.Builder` has the same
  one, with no seam at which to avoid it); it is the rule that is unenforceable as written, and
  nothing tests it. It matters because this is the sentence the next person adding a writer will
  read to decide whether their path is covered.
- **Suggested fix:** narrow the sentence to what is true and checkable — *"only `document.build`'s
  URL directive may call it, and only because `QueryString` has already made the copy; no new
  writer may"* — or add the test the rule implies (assert `resolve` has exactly one caller, or that
  `auth`/`cookies`/`directive` reach only `resolveParts`).

## Finding 12: `TestPreflightIsCancelledWithTheCallersContext` orders itself with a sleep
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/curl/exec_test.go:428
- **Description:** New on this branch (`7e3f281`): `go func() { done <- preflightWith(...) }()`
  then `time.Sleep(50 * time.Millisecond)` then `cancel()`. It does not flake — `runVersion` checks
  `ctx.Err()` before `runCtx.Err()`, so the assertion holds whether the cancellation lands before
  or after the process starts — the cost is 50ms and the shape. Recorded because CLAUDE.md bans the
  shape outright and commit `8dc6738` just removed the equivalent sleep from
  `cmd/talaria/root_test.go` in favour of `awaitStdinDrain`; leaving one behind is how the rule
  erodes.
- **Suggested fix:** have `fakeCurl` write a marker file or print a line the test scans (the
  `awaitLine` shape), then cancel — so the assertion is that a *running* subprocess is cancelled,
  which is the subject.

## Finding 13: Stale number in a test comment — the lock deadline is a minute, not five seconds
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/record_test.go:46
- **Description:** *"// Cancelled, so the bounded wait ends at once rather than in five seconds."*
  `lockTimeout` is `time.Minute` (`internal/corpus/lock_unix.go:26`), and the reasoning for the
  minute is written out at length there. Left over from an earlier value; the number is the point
  of the sentence.
- **Suggested fix:** say "a minute", or name `lockTimeout` rather than a literal.

## Finding 14: Remaining in-scope work and out-of-scope observations (not defects — recorded so they are not mistaken for one)
- **Reviewer:** all four
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** tasks.json:1
- **Description:** 28 of 42 tasks are done. Still open, and confirmed still live in the tree, so
  they are **not** re-derived as new findings above:
  - Cycle-2 findings 3–9 → tasks 36–42: the pretty/TSV renderers escape the emitted curl (36); a
    FIFO at the history path blocks `os.Open` inside the append critical section (37);
    `request.body` inlines a `--body @file` body while `request.curl` references it (42);
    `abandon`'s wait has no second way out (38); an `apiKey` scheme's `name:` has no CRLF gate
    (39); README's `redact.body-paths` wording (41); `auth check` vs `call` on a hostile path key
    (40).
  - Tasks 18–24: refactor passes, spec-cache TTL and `--refresh` (including the unbounded
    `os.ReadFile` of the cache at `internal/spec/source.go:112` — see finding 4, which shares the
    fix site), one pass over the history file per append, the comment/invariant audit, and the
    end-to-end credential-firewall test.

  Two observations **pre-existing on `main`** and therefore out of this branch's scope, recorded so
  cycle 4 does not spend a probe on them:
  - `internal/curl/exec.go:138` — with a query string over ~64 KiB, curl 8.14.1's
    `--write-out '%{json}'` emits `"urle.query":,`, which is invalid JSON, so a call that reached
    the server and got a response surfaces as `exit 1: cannot parse curl's response metadata`.
    Belongs with whoever picks up the `readResponse` bound.
  - `internal/curl/render.go:243` — `urlWord` skips `url.QueryEscape` for sensitive values so the
    `$NAME` reference stays shell-expandable, while the executed request percent-encodes the
    resolved value; a credential containing `&`, `=`, `#` or a space produces a reproduction that
    sends a different request. Identical code on `main`.
- **Suggested fix:** none — this entry is a scope record, not a defect.

---

## Verified sound, so cycle 4 does not re-derive it

- **Credential confinement holds end to end.** `resolveParts` returns the environment's own string
  uncopied (pointer identity verified); `document.auth` writes six pieces; `basicPair` uses
  `strings.Cut`, which aliases; `grow` clears every array it abandons; `discard()` clears the whole
  capacity on both paths. No canary reached any surface in ad-hoc runs across query-apiKey,
  cookie-apiKey and history. No credential-shaped strings in the diff or in `testdata/`.
- **§5a host binding is correct on every edge wire-tested:** off-set `--base-url` → withheld with
  the right `Key()`; profile `base-url` as source 4; profile `allow_hosts`; `example.com` vs
  `notexample.com` prefix confusion refused; bare `--allow-host` = any port; the zero `HostSet` and
  an unparseable target both fail closed; `binder.credentials` asks about `BaseURL + Path`, the
  exact string `Request.URL` builds.
- **Every new envelope key is in all three shipped documents.** Diffing `json:` tags against `main`
  gives exactly four additions — `credentials_withheld`, `supported`, `withheld`,
  `headers_truncated` — and all four appear in AGENT.md, README.md and DESIGN.md.
- **Exit codes 2/3/4/5 and the `valid_alternatives` shape match README.md:544 verbatim**;
  `supported` appears only when false and carries no `source`, per DESIGN.md:344.
- **Cross-command agreement:** `call`, `auth check` and `history replay` gave identical verdicts for
  an off-set `--base-url` (withheld + exit 0 / `withheld:true` + exit 0 / exit 2) and identical
  exit-2 messages for a malformed `--allow-host`; the credential was confirmed *absent* from the
  server's view when withheld. Finding 3 is the one cell they disagree on.
- **No data races, no goroutine leaks, no fd leaks.** The only shared mutable package state is
  `preflightCache`, every access under `preflightMu`. Three production goroutines, all with
  buffered channels so no send can strand. Every `Open`/`OpenFile`/`CreateTemp` closed on every
  error path. Both exec sites get `exec.CommandContext` + `WaitDelay` + `isolate`.
- **`signalContext()` is leak-free on the normal path too** (`stop` calls `cancel` internally), and
  commit `8dc6738`'s `awaitStdinDrain` closes the `SweepStale` window the 500ms sleep had.
- **Commit `6661045` is complete apart from finding 4:** the ctx rides on the request so it covers
  each redirect hop and the body read; `fetchFailed` discriminates on `ctx.Err()`, so Ctrl-C is
  exit 1 and an elapsed `fetchTimeout` is exit 3; `loadSpec` is the single seam.
- **Ordering under cancellation is intact** — `Append`'s `LOCK_EX|LOCK_NB` attempt precedes every
  context check, so a Ctrl-C'd `recordCall` still writes when the lock is free.
- **The boundary guard is complete:** no package was added on this branch, the table covers every
  package CLAUDE.md constrains, `corpus → config` is still indirect-only, and
  `TestTheDirectImportRuleReadsDirectImportsOnly` is non-vacuous.
- **Cell escaping is self-consistent:** one row in is one line out for a 64 KiB C0-byte body in TSV
  and pretty, and `fitSummaries` measures post-escape width.
- **No OOM-shaped generators** — every hostile-input generator is capped at a small multiple of its
  bound. **No test-only branches in production code** (`TALARIA_TEST_*` appears only in
  `root_test.go`).
- **Nothing reintroduces `run`, `internal/gen` or the JUnit report.** The stale `run` mentions in
  README and AGENT are all present in `git show main:` and are pre-existing.
