# Review — `phase-2a-remediation` vs `main` (cycle 2)

Four reviewers (security, spec-compliance, concurrency, integration) over the full branch diff
(93 files, +15053/-1983). `go build`, `go vet`, `gofmt -l .` and `go test -race ./...` are all
green with finding 1 live — the standing warning in CLAUDE.md holds for a third cycle.

Branch is mid-flight: tasks 1–17 and 25–29 are complete, 18–24 and 30–33 not started. Cycle-1
findings 5, 8, 9, 10 and 11 map to unstarted tasks 30–33 and are recorded in finding 10 as
remaining scope, not re-derived as new defects.

Finding 1 was found independently by all four reviewers and wire-verified five separate ways.

## Finding 1: A history entry over `maxEntryBytes` is written, reported as recorded, and is invisible to every reader
- **Reviewer:** security, spec-compliance, concurrency, integration (all four, independently)
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/store.go:126
- **Description:** `lines()` (`internal/corpus/file.go:289`) drops any stored line over
  `maxEntryBytes` (256 KiB). `Append` applies no counterpart bound to the line it *writes*: it
  marshals the entry, calls `write`, and returns `nil`. So `recordCall` prints nothing, `call`
  exits 0, and `Store.Read`, `storedIDs`, `trim`, `history`, `history show` and `history replay`
  all skip the entry forever.

  The length filter in `lines()` is **new on this branch** (`git show main:internal/corpus/store.go`
  — `lines` has no length check), so the identical entry read back correctly on `main`. This
  branch made talaria write entries it will never read.

  Wire-verified against the built binary, four independent trigger routes, none of them requiring
  a malicious peer:

  ```
  # 1. ~200 KB of ordinary response headers (curl's own ceiling is 300 KB)
  $ talaria call spec.yaml listPets --output json
  call exit=0            stderr: (empty — no "the call was not recorded" warning)
  $ awk '{print length($0)}' $XDG_STATE_HOME/talaria/history.jsonl
  278240
  $ talaria history --output json
  {"schema":"talaria/v1","entries":[]}
  $ talaria history show 1
  {"error":{"code":2,"message":"no history entry 1: the history is empty"}}
  ```

  - **2.** One response body of `MaxBody` (64 KiB) of `0x01` — valid UTF-8, so it takes the
    non-base64 branch — becomes a 393,693-byte line, because `encoding/json` expands a C0 byte
    6× as `\u0001`. `newBody` caps *before* the encoding, so the cap does not bound the line.
  - **3.** Three `--query` values of 100 KB each — ordinary user input, no server involved —
    give a 300,291-byte line.
  - **4.** Two bodies at `MaxBody` base64'd already cost ~175 KB of the 256 KB before a single
    header is added, so a binary request/response pair reaches it with ordinary headers.

  Then it is silently deleted: `trim` builds its rewrite from `lines(data)`, which already
  excluded the oversize line, so the next retention pass drops it. Verified — seeded a store with
  67 MB of padding plus one oversize entry, ran one `talaria call`, and `grep -c 'X-Pad-000'`
  went 1 → 0 with no diagnostic.

  This breaks README.md:474 verbatim: *"an entry talaria reported as recorded is one you will
  find in the file."* It contradicts `Append`'s own doc comment — *"what this returns is the truth
  about the line it wrote"*, *"a nil return mean[s] the entry is in the store under an id nothing
  else holds"* — which is the unenforced-invariant class the house rules forbid, in the file that
  wrote the rule down. Three further consequences: `storedIDs` never sees the id, silently
  retiring `uniqueID`'s collision check; the entry never counts toward `maxPerSource`; and it
  breaks the arithmetic `maxKeptBytes` rests on, since `Append` no longer writes a line bounded by
  `maxEntryBytes`, so `trim leaves maxKeptBytes` + `Append writes one line` ≠ `maxStoreBytes`.

  It is the exact inverse of the hazard commit `8efc8dd` was written to close. That commit stopped
  talaria reporting "the call was not recorded" for an entry that was on disk; it now reports
  success for an entry no reader will ever see — and re-running a mutating call is the same
  consequence, reached from the other side.
  `TestTheReadBoundAdmitsWhatTrimLeavesPlusOneMaximalLine` pins the three constants against each
  other, but nothing pins a *written* line to the middle one.
- **Suggested fix:** bound the line where it is produced, in `Append`, after `json.Marshal` and
  before `write`. Preferred: shrink the entry until it fits — cut the bodies further and set the
  `Truncated` field that already exists to say so, and cap the two unbounded contributors
  (`EntryResponse.Headers`, `EntryRequest.Headers`) the way `newBody` caps a body — so a
  verbose-but-legitimate server still gets its metadata recorded, which is what history is for.
  Failing that, refuse the line and return an error so `recordCall`'s stderr warning fires; silent
  loss is the one outcome that must not survive. The bound must be applied to the **encoded**
  line, not to `MaxBody`, because the expansion happens in the JSON encoding. Regression test: a
  response body of `MaxBody` bytes of `0x01` must come back from `Store.Read` after `Append`
  returned nil — assert `Read` returns it *or* `Append` returned an error, never both nil.

## Finding 2: `document.auth` and `resolve` concatenate the resolved credential into Go strings nothing can zero
- **Reviewer:** security
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 1 findings 3 and 4 (partial fix)
- **File:** internal/curl/config.go:253
- **Description:** Commit `071b26d` rewrote `directive` to write its four pieces separately, rewrote
  `document.cookies` to join into `d.b` a piece at a time, added `grow` to clear abandoned arrays,
  and wrote the rule into CLAUDE.md: *"`directive` writes its four pieces separately rather than
  concatenating: a concatenation, and the copy `escapeDirective` makes when it must, are strings
  nothing can zero"* and *"Never introduce another accumulator for credential-bearing text without
  both properties."*

  The fix landed on the rare path and not on the common one. `document.auth` — the primary
  credential path, one function above `cookies` in the same file — still does:

  ```go
  d.directive("header", h.Name+": "+value)          // internal/curl/config.go:253
  ```

  That concatenation allocates a fresh Go string holding `Authorization: Bearer <token>` in full.
  `escapeDirective` returns its argument untouched when nothing needs escaping — which
  `TestEscapingAValueNeedingNoEscapeDoesNotCopyIt` guarantees, and a credential needs no escaping —
  so `write` copies *out of* that array and never touches it again. Neither `grow`, `discard` nor
  `cleanupWith` can reach it. One frame up, `resolve` does the same:
  `return v.Prefix() + value` (`internal/curl/firewall.go:131`) holds `Bearer <token>`.

  Verified with a scratch test holding `unsafe.StringData` across a real `buildDocument` +
  `cleanup()`:

  ```
  RESIDUE: the `h.Name+": "+value` array still reads "Authorization: Bearer canary-4f8c1e-do-not-leak"
  RESIDUE: resolve's Prefix()+value array still reads "Bearer canary-4f8c1e-do-not-leak"
  ```

  with `owned(doc)` and the returned `config` both correctly zeroed in the same run — `owned(doc)`
  is blind to this for the same reason it was blind to the abandoned arrays in cycle 1.
  `cookies` needs `type: apiKey, in: cookie` to run at all; `auth` runs on every bearer, basic and
  apiKey-in-header call talaria makes.

  CLAUDE.md's scoping sentence concedes `os.Getenv`'s immutable string and `escapeDirective`'s
  forced copy. It does not concede these two, and both are avoidable — so the claim *"what is
  scrubbed is the arrays this package allocated plus the copy it returns"* is still false on the
  primary path, one cycle after it was asserted as fixed.

  **What the previous fix missed:** it treated `directive`'s *internals* and `cookies` as the
  whole surface, and did not look at what `auth` hands `directive` in the first place. The
  approach is right; it was applied to two of four sites and then documented as complete. This is
  marked a repeat because the same claim has now failed review twice, and cycle 1 finding 3
  explicitly offered narrowing the claim as the honest alternative to chasing it — that choice was
  never made, it was just re-asserted more broadly.
- **Suggested fix:** give `document` a `pair(name, sep, value string)` — or inline the `write`
  calls — so `auth` emits `header`, ` = "`, `escapeDirective(h.Name)`, `: `,
  `escapeDirective(value)`, `"\n` as separate pieces, exactly the shape `cookies` already uses; it
  needs no new machinery. For `resolve`, either return `(prefix, value)` and let the writer emit
  them separately, or state the residue explicitly. Extend the zeroing suite the way
  `TestBuildZeroesTheArrayItAbandonsWhenTheBufferGrows` did: hold `unsafe.StringData` of the
  concatenation across the build and assert it is unreadable. **If the complete claim is judged not
  worth chasing, take cycle 1 finding 3's alternative instead and narrow CLAUDE.md and the
  `firewall.go` comments to what is actually enforced** — but make that choice explicitly rather
  than restating the broad claim a third time.

## Finding 3: The pretty and TSV renderers escape the emitted curl command, so the default TTY output is not the command talaria ran
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/output/render.go:133
- **Description:** `call`'s curl line is a one-cell table row (`cmd/talaria/call.go:443`), and both
  renderers now put every cell through `escapeCell`, which rewrites `\` as `\\`. Pretty is the
  default when stdout is a terminal — i.e. the human who is going to paste it.

  ```
  $ talaria call post.yaml login --allow-mutations --dry-run --output pretty --body '{"a":"line\nbreak"}'
  curl -q -s -X POST -H 'Content-Type: application/json' --data-raw '{"a":"line\\nbreak"}' 'https://api.example.com/login'

  $ ... --output json | jq -r .request.curl
  curl -q -s -X POST -H 'Content-Type: application/json' --data-raw '{"a":"line\nbreak"}' 'https://api.example.com/login'
  ```

  Pasting the pretty line sends `line\\nbreak`; the JSON field carries the correct `line\nbreak`.
  Same for headers (`--header 'X-P=C:\Users\x'` prints `-H 'X-P: C:\\Users\\x'`) and for tsv. Two
  surfaces of the same field disagree — the shape `48c4bf3` was written to close for the body.

  This breaks DESIGN.md §3.4's *"`--dry-run` prints the exact curl command"* and its *"a portable
  reproduction for bug reports, docs, and scripts"*. New on this branch: `git show
  main:internal/output/render.go` has no `escapeRow`. The escaping itself is right for
  `list`/`history show` cells; the mistake is routing a whole pre-formatted line through a *cell*
  escaper. README.md:503-509 documents the escaping as applying to text that is "spec-derived or
  read back from the history file" — the curl line is neither. Scope is `call` only:
  `history show` renders no curl row.
- **Suggested fix:** give `output.Payload` a way to carry a line that is a line rather than a
  one-column table (e.g. `Lines []string`, printed verbatim by both renderers), and have
  `callPayload` put the request line and the curl line there. Do not escape at the call site —
  CLAUDE.md forbids it — and do not exempt single-column rows generically, since `history show`
  has one-column cells that must stay escaped. Test: the pretty `curl` line is byte-identical to
  the JSON `curl` field for a body containing a backslash.

## Finding 4: A FIFO at the history path blocks `os.Open` with the append lock held, and the new comment says it cannot
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/file.go:265
- **Description:** `readStore`'s comment (`file.go:258-262`) justifies bounding the bytes actually
  read rather than trusting `os.Stat` with: *"a store that is a symlink to /dev/zero or a FIFO
  stats as empty and reads forever, and this file is user-writable by design."* `tail` repeats it
  (`file.go:225-227`) and CLAUDE.md records it as a rule. The `/dev/zero` half is true. The FIFO
  half is not: `os.Open` on a FIFO with no writer blocks in `open(2)` before any bound can apply.

  Verified against the built binary — and the process is not merely slow, it is unkillable by the
  ordinary signals:

  ```
  $ mkfifo $XDG_STATE_HOME/talaria/history.jsonl
  $ timeout 5 talaria history --output json
  ... still running many minutes later
  ```

  `timeout` sends SIGTERM at 5 s; `signalContext` catches it, and the process is blocked in
  `open(2)` where no context reaches, so the first signal is swallowed and only the second
  (default disposition) or SIGKILL ends it. The `Append` path is worse than the `Read` path:
  `trim` → `tail` → `os.Open` runs *inside* the critical section (`store.go:111`, after `lock` at
  105), so one wedged process pins the sibling `.lock` and every concurrent `talaria call` on the
  machine burns its full 60 s `lockTimeout` behind it. `write`'s `os.OpenFile` has the same
  property.

  The hang itself predates the branch (`main` used `os.ReadFile`, which opens the same way). What
  is new is the **claim**: `file.go` is a new file, both comments are new, and CLAUDE.md now
  states the FIFO case as handled. That is "never write a comment asserting a property no test
  enforces" broken in the same commit that wrote the rule down — the class four cycle-1 findings
  already belonged to. (One reviewer recorded this case as sound; that was wrong, and its own
  leftover hung process was still on the process table at the end of the run.)
- **Suggested fix:** open with `syscall.O_NONBLOCK` (`os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)`)
  in `readStore` and `tail`, and reject a non-regular store with an entry-level error naming the
  path, exactly as the over-bound case does. Add the FIFO to the hostile-input tests beside the
  `/dev/zero` one. If the fix is judged out of scope, narrow both comments and CLAUDE.md to what
  is enforced — the bound covers a device that reads forever, not a path that blocks on open.

## Finding 5: `request.body` inlines a `--body @file` / `--body -` body on stdout while `request.curl` beside it references it
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** design
- **Repeat-of:** none
- **File:** cmd/talaria/call.go:435
- **Description:** `9e8e4e0` established that a body the caller did not type is referenced, not
  printed: `curl.bodyDirective` emits `--data-binary @path` / `--data-binary @-`, and
  `internal/curl/render.go:126` gives the reason — *"a body read from a file or from stdin was
  written by someone other than whoever reads this command… §3 principle 0 puts stdout first among
  the surfaces such a value must not reach."* But `callPayload` sets `view.Request.Body =
  string(shown.Body.Data)` for every origin. The only filter is `secret.ResponseRedactor.Body`,
  whose built-in list is `access_token`, `refresh_token`, `id_token` plus the user's
  `redact.body-paths`; anything else goes to stdout verbatim, in the field beside the one that
  deliberately withholds it.

  ```
  $ talaria call post.yaml login --allow-mutations --dry-run --output json --body @/tmp/secret-body.json
  {"curl": "curl … --data-binary '@/tmp/secret-body.json' 'https://api.example.com/login'",
   "body": "{\"client_secret\":\"FILE-CANARY-SECRET-42\",\"grant_type\":\"client_credentials\"}"}

  $ printf '{"client_secret":"STDIN-CANARY-99"}' | talaria call … --body -
  {"curl": "curl … --data-binary '@-' 'https://api.example.com/login'",
   "body": "{\"client_secret\":\"STDIN-CANARY-99\"}"}
  ```

  One JSON object, two contradictory answers about whether the agent may see this body. The canary
  gate cannot see it: `TestABodyFileSecretReachesNoOutputSurface` puts its canary under
  `refresh_token`, which the built-in path list rewrites. Move the canary to `client_secret` and
  the suite goes red today.

  **The missing decision:** DESIGN.md §3.4's normative sentence names only the emitted curl, and
  CLAUDE.md's rule is *"redacted where it is displayed, and referenced where it was not typed"* —
  which as written assigns redaction to the display field and referencing to the curl, i.e. it
  describes today's behaviour. But §3 principle 0, the reason given for the curl rule, applies
  identically to `request.body`. A fix must choose between **(Y1)** referencing in `request.body`
  too — gate on `shown.Body.Origin == request.BodyArgv` and emit `"@/path"` / `"@-"` or omit the
  field, which is an envelope contract change needing DESIGN.md §4 and AGENT.md amended in the
  same commit — and **(Y2)** keeping the inline body and narrowing the curl-side rule and its
  comment, conceding that stdout shows a referenced body anyway. DESIGN.md does not decide, and
  guessing here churns.
- **Suggested fix:** make the choice in DESIGN.md first, as §5a source 4 was decided for cycle-1
  finding 7. If Y1: gate `view.Request.Body` on the origin, amend DESIGN.md §4's payload sketch
  and AGENT.md, and repoint the canary case at a field the built-in path list does not cover so
  the gate can fire. If Y2: narrow `render.go:126`'s comment and the CLAUDE.md rule, and still
  repoint the canary.

## Finding 6: `abandon`'s wait has no second way out, so a lock never granted retains a goroutine and a descriptor for the process's life
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/lock_unix.go:131
- **Description:** `waitForLock` gives up correctly — both `ctx` and `lockTimeout` work, verified
  with `-race -count=40` — and hands the descriptor to `abandon`, which is right: after five
  give-ups against a held lock, five descriptors are open and all five close within milliseconds
  of the holder releasing (`TestALockGrantedAfterTheWaitGaveUpIsReleased` covers that).

  What `abandon` lacks is the property CLAUDE.md requires of every other wait on this branch:
  `<-taken` blocks unconditionally, so against a holder that *never* releases — the wedged talaria
  and stale NFS mount `lockTimeout`'s own comment names as its reason for existing — the goroutine
  and the fd are retained until the process exits. For the CLI that is one per invocation and
  harmless. For the twin server this package is explicitly kept importable by (`entry.go:13`, and
  the `corpus → curl`/`config` boundary rules), it is one leaked goroutine and one leaked
  descriptor per contended append that timed out.
- **Suggested fix:** either give `abandon` a second bound (`select` on `taken` against a generous
  timer, accepting that a lock granted after it is held until exit — no worse than today), or
  state the limitation in the comment and scope the CLAUDE.md rule to the CLI's process lifetime,
  so the twin work does not inherit it as settled.

## Finding 7: An `apiKey` scheme's `name:` reaches the emitted curl with no CRLF gate
- **Reviewer:** security
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/request/build.go:442
- **Description:** **Pre-existing on `main`** (`git show main:internal/request/build.go:498` —
  `binder.credentials` attached the pair with no name check there either), so it is not this
  branch's defect. Recorded because it is the one remaining instance of the class this branch
  closed for media types, one gate short, and because `binder.credentials` is a function the
  branch rewrote.

  `credentialFor` (`internal/config/auth.go:433`) takes `cred.Name` straight from `scheme.Name` in
  the spec. `binder.credentials` appends it as a `Pair` with no `isFieldName` and no
  `SplitsRequest` check — unlike `binder.located`, `binder.pairs` and `binder.headers`, which all
  check. The only gate is `document.auth`'s `checkSplit` at exec time, and `--dry-run` never
  builds a document, so the two disagree:

  ```
  $ talaria call crlf_name.yaml getPets --dry-run --output json
  {"request":{"curl":"curl -q -s -H \"X-Key\r\nX-Injected: evil: $TALARIA_AUTH_APIKEY_APIKEYAUTH\" …"}}   exit 0
  $ talaria call crlf_name.yaml getPets --output json
  {"error":{"code":2,"message":"header \"X-Key\\r\\nX-Injected: evil\" carries a carriage return or newline …"}}  exit 2
  ```

  Pasting that reproduction (curl 8.14.1, captured on a raw TCP listener) sends two headers, the
  second attacker-named and carrying the credential. No shell escape is possible
  (`escapeInDoubleQuotes` covers `` \ " ` $ ``) and the credential stays on the allowed host,
  which is why this is INFO. The pretty renderer is safe — `escapeCell` escapes the CR/LF — so it
  is `--output json` only. It is precisely the hazard `bodyArgs`' own comment names for media
  types: *"a single-quoted word spans a raw newline happily, so printing it would hand the reader
  the injection the call itself refuses to make."*
- **Suggested fix:** check the credential name where it is bound, as every other header source is:
  in `binder.credentials`, `if cred.In == inHeader && !isFieldName(cred.Name)` → `b.fail`, and
  `SplitsRequest(cred.Name, "")` → `b.fail`, before appending the pair. That makes it exit 2 where
  a hostile spec is read and keeps `--dry-run` and `call` agreeing.

## Finding 8: `redact.body-paths` rewrites the request body shown in `request.curl`, and README documents it as response-only
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** README.md:260
- **Description:** `48c4bf3`'s `displayRequest` puts `req.Body.Data` through
  `redactors.Response.Body` before both `request.body` and `curl.Render`'s `--data-raw`. That is
  the right fix, but README.md:260-261 still says *"`body-paths` are dotted JSON paths into a
  **response** body, on top of the built-in `access_token`, `refresh_token` and `id_token`."*
  Nothing in README or AGENT.md says a configured path also rewrites what the emitted curl shows
  for a `--body` literal, or that the command therefore no longer reproduces the call:

  ```
  $ talaria call post.yaml login --allow-mutations --dry-run --body '{"access_token":"SUPERSECRET"}'
  curl … --data-raw '{"access_token":"\u003credacted\u003e"}' …
  ```

  README:338-342 makes exactly this point for credential-shaped *headers* ("the emitted command
  for a literal credential is deliberately not copy-pasteable") and stops short of the body. This
  is the cycle-1-finding-9 pattern: a contract change whose shipped-doc half did not land.
- **Suggested fix:** one sentence in README.md's redaction section saying `body-paths` apply to a
  request body too, on the display surfaces only, and one in "Making a call" extending the
  "deliberately not copy-pasteable" note to a redacted body. Same for AGENT.md's `--body` bullet.

## Finding 9: `auth check` exits 0 where `call` exits 2 on a spec whose `paths:` key is not a path
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/request/server.go:36
- **Description:** `b58b043` added two gates: `binder.path` refuses a non-`/` path template with
  exit 2, and `Destination`/`Withholds` join `in.Op.Path` so the host check sees the real
  destination. `Destination` deliberately does not apply `isPathTemplate`, so on the hostile
  fixture the two commands differ:

  ```
  $ talaria auth check hostile.yaml --output json
  {"schemes":[{"scheme":"bearerAuth","source":"env:TALARIA_AUTH_BEARER","present":true,"withheld":true}]}   exit=0
  $ talaria call hostile.yaml steal --dry-run
  exit=2
  ```

  The direction is safe — `withheld:true` correctly says the credential would not be sent — and
  the split is about spec validity rather than credentials, so it does not breach DESIGN.md:337 as
  written. Recorded because CLAUDE.md states the rule as "`auth check` and `call` may not
  disagree" without qualification, and the matrix in `cmd/talaria/auth_test.go` covers only the
  unsupported-scheme axis, not the new path/host axis. An agent that pre-flights with `auth
  check`, sees exit 0, then gets exit 2 from `call` had no way to predict it.
- **Suggested fix:** either add a row to `TestAuthCheckAndCallAgreeOnUnsupportedSchemes` recording
  this cell as deliberate, or have `destinationWithholds` report a spec whose path template
  `binder.path` would refuse, so `auth check` exits 2 too. Whichever, say which in CLAUDE.md's
  agreement rule.

## Finding 10: Remaining in-scope work (not a defect — recorded so it is not mistaken for one)
- **Reviewer:** security, spec-compliance, concurrency, integration
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** tasks.json:1
- **Description:** Tasks 18–24 and 30–33 have not started. Cycle-1 findings 5, 8, 9, 10 and 11 map
  to tasks 30–33 and were confirmed still live in the tree by all four reviewers, one line each,
  rather than re-derived:
  - **Cycle-1 finding 5** (task 30) — `go list -deps ./internal/corpus` still resolves
    `internal/config` via `internal/request`; `internal/e2e/boundary_test.go`'s `corpus` entry
    still forbids `executorAndTwin` only. Confirmed: **no new prose boundary rule was added
    without a table entry on this branch** — the fix commits added host/path/buffer rules, none of
    them import rules.
  - **Cycle-1 finding 8** (task 31) — `internal/spec/source.go:137` is still `client.Get(url)`;
    `Load`/`loadURL`/`fetch` still take no `ctx`.
  - **Cycle-1 finding 9** (task 32) — `grep withheld AGENT.md README.md docs/design/DESIGN.md`
    returns only `call`'s `credentials_withheld`; AGENT.md:137 still shows the three-field `auth
    check` object and says "`present` is a lookup".
  - **Cycle-1 finding 10** (task 32) — DESIGN.md:170 still says `--body @file` emits `--data
    @file`; the code emits `--data-binary`.
  - **Cycle-1 finding 11** (task 33) — `cmd/talaria/root_test.go:202` is still
    `time.Sleep(500 * time.Millisecond)`, and `curl.SweepStale()` is still above `signalContext()`
    at `cmd/talaria/root.go:212`.

  Also still open as recorded scope: cycle-1 finding 31 (`cmd/talaria/history.go:43`
  `TimingMS int64,omitempty`) belongs to task 19; the spec cache TTL / `--refresh` and the
  unbounded `os.ReadFile` of the cache (`internal/spec/source.go:103`) belong to task 20; the
  one-pass-per-append work is task 21.

  One item outside this branch's diff, for a later task: `readResponse`
  (`internal/curl/exec.go:145`) still `os.ReadFile`s the response body with no bound, and curl is
  spawned without `--max-filesize` — the last unbounded read of attacker-controlled bytes, and the
  upstream of finding 1. Pre-existing on `main`, so not a finding here.
- **Suggested fix:** none — continue the task list.

## Verified sound (recorded so cycle 3 does not re-derive it)

- **Cycle-1 finding 1 (hostile path key) is closed, wire-verified by three reviewers.** Two raw-TCP
  listeners, spec server `127.0.0.1:28081`, path key `"@127.0.0.1:28082/steal"`, no flags: exit 2
  from `binder.path`, attacker listener received nothing. `binder.credentials` asks
  `BaseURL + Path`, the string the executor uses. Allowed-host delivery, off-set `--base-url`
  withholding (exit 0 + `credentials_withheld` + one stderr line) all behave as §5a states.
- **Cycle-1 finding 2 (body in the emitted curl) is closed for an argv body.** `displayRequest` is
  the single redacted source; `callPayload` builds `Curl`, `URL`, `Headers`, `Cookies` and `Body`
  from it. Verified on all three body origins. (Finding 5 above is the separate origin question.)
- **Cycle-1 finding 7 (profile `base-url`) is closed, and the design decision was recorded.**
  DESIGN.md is amended to v0.6 with §5a source 4 and a rationale, rather than the code guessing.
  README.md:205's example run verbatim now sends `Authorization: Bearer $STAGING_TOKEN` to the
  profile's own base URL. All three negative edges hold: an unselected profile contributes
  nothing, `--base-url` to a third host is still withheld with a profile active, and no profile
  still withholds.
- **Cycle-1 finding 6 / task 29 is closed.** A 70 MB store makes `history` exit 1 with "move it
  aside"; one `talaria call` then repairs it via `tail` (70,000,350 → 1,049,423 bytes). The state
  is no longer absorbing. `storedIDs` propagates its error, so an unreadable store no longer
  retires `uniqueID`'s collision check. `Append` writes last, so the "not recorded" warning for an
  entry on disk is gone.
- **Task 17 (lock deadline) works end to end.** With `flock` held by another process, `talaria
  call` completed the request, took SIGINT during the lock wait, and exited 0 with `warning: the
  call was not recorded in history: … context canceled`. The uncontended `LOCK_NB` precedes the
  context check, so `recordCall` after Ctrl-C still records.
- **Concurrent appends do not lose or duplicate entries.** 10 processes × 40 appends: 400/400
  lines, 0 unparseable, 0 duplicate ids; 60 in-process goroutines the same. `trim → storedIDs →
  write` is entirely inside one `flock` critical section, the lock is on the never-renamed sibling
  so `replace`'s rename cannot orphan it, and `replace` is `CreateTemp` + explicit `Chmod(0600)` +
  same-directory `Rename`.
- **`call` → history → `history show` → `history replay` is byte-identical on the wire**, including
  a path param needing escaping (`a/b` → `/pets/a%2Fb`), a query param and a header, with the
  stored `Authorization` dropped and re-resolved. An entry whose recorded URL carries
  `<redacted:env:…>` round-trips and replays correctly.
- **`auth check` vs `call` agree on every cell driven:** unsupported+absent → 5/5,
  unsupported+brought-token → 0/0, undeclared → 2/2, `apiKey in: nowhere` → 5/5, off-set
  `--base-url` → 0/0 with `withheld:true` and `credentials_withheld`. The hostile-path-key cell is
  finding 9.
- **`history replay` refuses an off-set stored host** with exit 2 naming `--allow-host`, while
  `call` withholds and runs. The deliberate asymmetry holds and both use `allowedHosts`.
- **`newRedactors` is built exactly twice in the tree** — once in each of `call`'s and `history
  replay`'s `RunE` — and threaded into binder, view and store. No surface builds its own.
- **Buffer zeroing partially holds:** `grow` does clear the abandoned array, `discard` clears the
  full capacity on both paths, `cookies` joins into `d.b` piece by piece, `configEscape`'s
  pass-through holds. The two concatenations in finding 2 are the only accumulators left uncovered.
- **`preflightCache`/`preflightMu`:** every access on every path is under the mutex, and only a
  verdict the binary produced is cached. `Cmd.Wait` drains the copy goroutine even on the
  `WaitDelay` path, so `boundedBuffer` has no race.
- **Timeout composition:** `preflightTimeout` (5 s) is its own context outside `execCtx`; `execCtx`
  is `MaxTime + killGrace`; `WaitDelay = killGrace` on both exec sites; `lockTimeout` is consulted
  only after the non-blocking attempt. None swallows another. Worst case for one `call` is ~97 s,
  each segment individually escapable — except the FIFO open in finding 4.
- **Environment parity:** the only `TALARIA_TEST_*` variable is `signalChildEnv`, confined to
  `root_test.go`; no `testing.Testing()` branch; the four production `os.Getenv` reads are
  `TALARIA_SPEC`, `TALARIA_HISTORY`, `XDG_STATE_HOME`, `XDG_CONFIG_HOME`. Missing `curl` → exit 1
  before any request.
- **`escapeCell` holds where it belongs:** a response body containing a real tab, LF and CRLF
  renders as exactly 4 TSV lines with escapes intact. Finding 3 is the one place it is applied to
  something that is not a cell.
- `Entry.Replay` drops built-in credential names and redaction markers, refuses a truncated body,
  and hands the body over as stdin; `Body.Bytes` checks encoded length before the decode.
- `requestFailed`'s scrubber covers both raw and `QueryEscape`d forms of every sensitive value
  before curl's stderr is quoted. Spec loading keeps `AllowFileReferences`/`AllowRemoteReferences`
  false, bounds redirect hops and scheme, and bounds the fetch on bytes actually read.
- File modes 0700/0600 explicitly `Chmod`'d. No credential-shaped strings in the diff or
  `testdata/`. `.ralph/stack.json`'s three command fields still match `ci.yml`'s `run:` lines, and
  tasks 25–33 carry `kind: "fix"`, so `loop.sh`'s fix-round bounding works as intended.

## Build and test output

```
$ go build ./...        (no output, exit 0)
$ go vet ./...          (no output, exit 0)
$ gofmt -l .            (no output)
$ go test -race ./...   ok — all 14 packages
```

Green, with finding 1 live and found independently by four reviewers. The standing warning in
CLAUDE.md holds for a third cycle.
