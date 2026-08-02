# Review findings — `ralph/design` vs `main`

Four reviewers (security, spec-compliance, concurrency, integration) over the full branch diff
(127 files, ~27k insertions). Baseline: `go build ./...`, `gofmt -l .`, `go vet ./...` clean;
`go test ./...` fully green; `go test -race ./...` clean, including a cross-process corpus
stress test (8 processes × 40 appends over the trim path, zero lost entries).

All eleven CRIT findings from the previous cycle were re-tested and hold. The previous cycle's
WARN/INFO findings were never actioned and its findings file was deleted, so the ones still
live are carried forward below (findings 21, 22, 23, 25, 32, 34).

Every finding below marked CONFIRMED was reproduced by executing the real binary against a real
server with real curl. Findings 1, 2, 3 and 8 were additionally re-verified independently.

---

## Finding 1: curl reads `~/.curlrc` on every call — one writable file exfiltrates every resolved credential
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/curl/config.go:103
- **Description:** argv is `{"curl", "-K", "-"}` with no `-q`/`--disable`, so curl parses its default config file (`$CURL_HOME/.curlrc`, else `$HOME/.curlrc`) *before* the `-K -` document that carries the resolved secret. Every directive there applies to the credential-bearing request: `trace-ascii = /path` writes the full `Authorization: Bearer …` to an attacker-chosen file, `proxy = http://…` ships it to an attacker-controlled host. This defeats the entire firewall, and it defeats it precisely in the deployment §5a calls "fully realised" — an agent with *no* credential env vars but write access to `$HOME` plants one file, waits for one `talaria call`, and reads the token out. §5a's "threats not covered" concedes only an agent that can *read* env vars or the profile file; writing one file is a strictly weaker capability, so this is inside the boundary the design claims. CONFIRMED: with `.curlrc` containing `trace-ascii`, stdout correctly showed `<redacted:env:TALARIA_AUTH_BEARER>` while the trace file (mode 0644) contained the plaintext `CANARY-SECRET-abc123`. Re-running the same config document through `curl -q -K -` produced no trace file.
- **Suggested fix:** Make argv `{"curl", "-q", "-K", "-"}` — curl requires `-q` to be the first parameter. Set `cmd.Env` explicitly (finding 27) so `CURL_HOME` cannot redirect the lookup either. Add a canary case that plants a `.curlrc` with `trace-ascii` in the harness `HOME` and scans the resulting file; `canary_test.go`'s harness already sets `HOME` to a temp dir.

## Finding 2: A malformed `--header`/`--body` flag echoes the credential verbatim into the structured error
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/request/build.go:311
- **Description:** `b.fail("%s %q is not name=value", flag, raw)` puts the whole rejected argument, value included, into the exit-2 JSON on stderr. The shell has already expanded it, so `--header "Authorization: Bearer $TOKEN"` prints the real token. This is the highest-probability leak in the tool because the colon form is exactly what AGENT.md tells the agent to write (finding 9). The `--body` variant at build.go:53 is worse in one respect: on a repeated flag it joins *every* body value into the message, and request bodies routinely carry `client_secret`/`password`. CONFIRMED: `--header 'Authorization: Bearer sk-live-REALSECRET123'` → `{"code":2,"message":"cannot build a request for getPet: --header \"Authorization: Bearer sk-live-REALSECRET123\" is not name=value"}`; `--body '{"password":"hunter2"}' --body '@/etc/hostname'` echoes both bodies.
- **Suggested fix:** Never echo the value half of a rejected `name=value` flag. Report only the part before the first `=`/`:`, or a positional index, with the value elided: `--header 1 is not name=value (got "Authorization: …")`. For `--body`, report the count and the *kind* of each value (literal / `@file` / `-`), never the bytes. Add a canary case: a malformed flag carrying the canary, scanned across every output format.

## Finding 3: Credentials in a URL's userinfo reach stdout, the emitted curl, and the permanent history file in cleartext
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/request/build.go:144
- **Description:** `baseURL` validates scheme and host but keeps `parsed.User`. `http://user:pass@host` is a valid absolute http(s) URL, so `--base-url`, a profile's `base-url`, **and a spec's `servers[0].url` — untrusted input** — can carry a credential in the authority. Nothing strips or redacts it: the password is printed in `request.url`, in the copy-pasteable `request.curl` (internal/curl/render.go:146), and appended to `history.jsonl` (internal/corpus/entry.go:154), which §5a calls "a permanent artifact — the highest-risk surface in the tool". There is no way to get it back out short of deleting the store. CONFIRMED: `--base-url 'http://admin:s3cr3t@127.0.0.1:8898'` printed `"curl":"curl -s 'http://admin:s3cr3t@…/pets/1'"`, the same in `request.url`, and `s3cr3t` matched in a freshly-created `history.jsonl`.
- **Suggested fix:** In `baseURL`, if `parsed.User != nil`, reject with a message pointing at `TALARIA_AUTH_BASIC` (preferred — talaria already has a basic-auth path that keeps the value symbolic), or strip the userinfo and re-inject it as a `request.Secret` so every display form renders `<redacted>`. Apply the same check in `replayRequest` (cmd/talaria/history.go:511), which re-parses a stored URL.

## Finding 4: `redact.headers` from the config file never reaches the displayed request or the emitted curl
- **Reviewer:** spec-compliance
- **Severity:** CRIT
- **File:** internal/request/build.go:80
- **Description:** DESIGN.md:339 requires `request.headers` in JSON output to be redacted against the built-in list "**+ user-extensible**", and says it "Applies to pretty output too"; README.md:209 states `headers` globs are "applied to request and response headers alike". In reality `request.Build` marks header/query/cookie values sensitive using a hard-coded `var builtin *secret.Redactor` (nil = built-ins only), and `cfg.Redact.Headers` is only ever passed to `corpus.Redactors` (cmd/talaria/call.go:239). The result is exactly inverted: with `redact: {headers: ["x-session-*"]}` the *history entry* stores `"X-Session-Id": "<redacted>"` while stdout's `request.headers` and `request.curl` both print `X-Session-Id: zzz` in the clear — the permanent artifact is protected and the surface the agent reads is not. Response headers are redacted correctly, so the gap is specifically the request side. No test at any level covers `redact.headers`; every existing test and the canary suite exercise only `redact.body-paths`. CONFIRMED.
- **Suggested fix:** Thread the extra patterns into `request.Inputs` and have `hide()` take a `*secret.Redactor` built from `secret.NewRedactor(cfg.Redact.Headers...)`. Add a canary case that configures `redact.headers` and greps stdout, dry-run and pretty for the value.

## Finding 5: Built-in response-body redaction silently fails on array-shaped bodies; the secret is written permanently to history
- **Reviewer:** integration
- **Severity:** CRIT
- **File:** internal/secret/response.go:155
- **Description:** `decodeObject` rejects any body not starting with `{`, and `redactPath` (response.go:190) only descends into `map[string]any`. So `ResponseRedactor.Body` returns the body **unchanged** for a top-level JSON array, and never descends into arrays nested under an object — two extremely common shapes. The built-in `access_token`/`refresh_token`/`id_token` paths and any user-configured `redact.body-paths` both silently do nothing. Because §5a mandates redaction at *write* time, the un-redacted value lands in `history.jsonl`. CONFIRMED: object body redacted correctly, while `[{"access_token":"TOPSECRET-ARR"}]` and `{"items":[{"access_token":"TOPSECRET-NEST"}]}` both leaked to the store. The unit test at internal/secret/response_test.go:127 (`TestBodySkipsNonJSONBodies`) **asserts the leak as correct behaviour**, listing `[{"access_token":"x"}]` alongside `"not json at all"`.
- **Suggested fix:** Make `decodeObject` decode into `any` (accept `[` as well as `{`), and make `redactPath` fan out across `[]any` elements at each segment, so `items.access_token` applies to every element of `items`. Retarget `TestBodySkipsNonJSONBodies` and add an array case to the canary suite so this is gated.

## Finding 6: Killing talaria orphans curl, leaks an unredacted response capture, and lets the request complete post-mortem
- **Reviewer:** concurrency
- **Severity:** CRIT
- **File:** internal/curl/exec.go:69
- **Description:** All cleanup is `defer`-based and nothing anywhere installs a signal handler (`grep -rn "os/signal\|signal.Notify"` over the tree returns nothing; cmd/talaria/main.go:8 is `os.Exit(run(...))`). An unhandled signal terminates the process immediately, so no defer runs. Three consequences, all confirmed against the real binary: curl is reparented to `ppid=1` and keeps running; the request **completes 8s after talaria is dead**, so Ctrl-C on an `--allow-mutations` POST does not cancel the write and — because `recordCall` never ran — the call is absent from history entirely, breaking its stated contract; and the capture directory survives indefinitely holding the *raw, unredacted* response (`Set-Cookie: session=abc123`, `{"access_token":"SUPER-SECRET-RESPONSE-VALUE"}`), exactly the material §5a's leak table says must be redacted. The same applies to the `talaria-body-*` request-body temp file (internal/curl/config.go:291). This is the previous cycle's WARN 12; the evidence shows it is worse than "leaks a directory".
- **Suggested fix:** Install `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` in cmd/talaria/root.go and thread that ctx into `curl.ExecuteWith` in place of `context.Background()` at exec.go:77, so cancellation kills curl via `CommandContext` and the deferred cleanups run normally. Set `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` with a `cmd.Cancel` that signals `-pgid` so anything curl spawned dies too. For SIGKILL, sweep stale `talaria-call-*`/`talaria-body-*` under `TMPDIR` at start-up.

## Finding 7: `history` indices are positional and shift on every write, so `replay <n>` re-issues the wrong request
- **Reviewer:** integration
- **Severity:** CRIT
- **File:** cmd/talaria/history.go:271
- **Description:** The index is `len(entries) - position`, recomputed on every read, and `corpus.Entry` carries no stable identifier at all. `call`, `run` **and `replay` itself** all append, so every index in a previously-printed listing silently shifts. The type comment at history.go:28 reasons carefully about filtering not renumbering, but not about appending. CONFIRMED: with index 1 = POST `createPetForm` and index 2 = GET `getPet`, `history replay 2` correctly replayed the GET and appended an entry; the immediately following `history replay 1` — intended to hit the POST — replayed the **GET again**. Two replays in a row is ordinary agent behaviour, so this misfires in normal use. The e2e suite cannot catch it: it replays index 1 exactly once, immediately after listing.
- **Suggested fix:** Give `corpus.Entry` a stable id (monotonic counter, or the RFC3339Nano timestamp) written at append time; have `history` print it alongside the positional index and have `show`/`replay` accept it.

## Finding 8: `talaria auth` and `talaria auth <typo>` exit 0 with help on stdout
- **Reviewer:** spec-compliance
- **Severity:** CRIT
- **File:** cmd/talaria/auth.go:30
- **Description:** `newAuthCmd` sets no `Args`/`RunE`, so `talaria auth` → exit 0 and `talaria auth bogus` → exit 0, both dumping cobra help to **stdout**. DESIGN.md §4 assigns 2 to usage errors with "stderr JSON lists valid options", AGENT.md:209 tells the agent code 2 covers "unknown command", and §3.1 makes deterministic exit codes the thing agents branch on. The previous fix (f747586) installed `unknownCommand` on the root only, so `talaria bogus` correctly exits 2 while the `auth` group does not — an agent that types `talaria auth chekc` gets exit 0 and unparseable text where `--output json` was promised. CONFIRMED independently.
- **Suggested fix:** Give `newAuthCmd` `Args: unknownCommand` plus `RunE: cmd.Help`, mirroring root — better, a shared `groupCommand()` helper so any future subcommand group inherits it. Assert `talaria auth bogus` → 2 in `exitcode_test.go`.

## Finding 9: AGENT.md and README document `--header 'Name: value'`; the binary only accepts `name=value`, and a near-miss silently corrupts the header
- **Reviewer:** spec-compliance
- **Severity:** CRIT
- **File:** AGENT.md:54
- **Description:** DESIGN.md §4 specifies `--header X-Foo=bar` and the code implements that, but AGENT.md's headline `call` example is `--header 'X-Trace: abc'`, which exits 2. AGENT.md:169 and README.md:274 repeat the colon form. §3.7 makes AGENT.md a first-class deliverable and this is the one line an LLM copies verbatim — and per finding 2, the resulting error echoes whatever secret the agent put there. Worse, the colon form is not always rejected: `--header 'X-Trace: abc=1'` is *accepted*, cut at the first `=`, and produces the malformed wire header `X-Trace: abc: 1` with a JSON `headers` key of `"X-Trace: abc"` — silent corruption rather than an error. `agentdoc_test.go` checks command names, exit codes and env vars but never flag argument syntax, so this drifts unpoliced. CONFIRMED.
- **Suggested fix:** Correct the three doc lines to `name=value`. Consider also accepting the curl-style `Name: value` in `binder.pairs` for headers (splitting on `:` when no `=` precedes it), since it is what every agent will reach for. Either way, reject a name containing `:` or whitespace, and extend `agentdoc_test.go` to execute the doc's fenced `talaria …` invocations against a fixture spec with `--dry-run`.

---

## Finding 10: No `globoff` — a glob in a spec-supplied URL path turns one authenticated call into N
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/curl/config.go:132
- **Description:** curl interprets `{a,b,c}` and `[1-100]` in a URL as glob ranges unless `-g`/`globoff` is given. `IsHTTPScheme` + `url.Parse` block globs in the *host* but not in the *path*, so a hostile or merely awkward `servers[0].url` of `http://host/{a,b,c}` makes curl issue three requests, each carrying the resolved credential, to three paths the user never asked for. talaria then gets three `%{json}` documents and dies with a misleading `cannot parse curl's response metadata` (exit 1), so the operator cannot tell what happened. It also breaks legitimate URLs containing `[`/`]`. CONFIRMED: exit 1 with `invalid character '{' after top-level value`; the equivalent document fed to `curl -K -` directly showed three completed 200s.
- **Suggested fix:** Emit `globoff` as a flag directive in `document.build` and `-g` in `curl.Render` so the URL is always literal. Optionally add a `build.go` guard rejecting a base URL whose path contains `{`, `}`, `[` or `]` for a clean exit 2.

## Finding 11: Request bodies are recorded with only the three RFC 6749 *response* token fields redacted
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/corpus/entry.go:232
- **Description:** `builtinBodyPaths` (internal/secret/response.go:17) is `access_token`, `refresh_token`, `id_token` — the fields of a token *response*. A token *request* body carries `client_secret`, `password`, `assertion`, `client_assertion`, `code_verifier`, none of which are redacted, so they land in `history.jsonl` verbatim and forever. §5a's "threats explicitly not covered" concedes only *response* bodies; a request body is data talaria itself assembled and chose to record, so this is inside the claimed boundary. CONFIRMED: `--body '{"client_secret":"SUPER-SECRET-XYZ","password":"hunter2"}'` → both values present in `history.jsonl`.
- **Suggested fix:** Give the request side its own built-in path list (`client_secret`, `client_assertion`, `password`, `passwd`, `secret`, `assertion`, `code_verifier`) applied by `Redactors.requestBody`, or at minimum a `redact.request-body-paths` config key. The cost is only to the recording — `request.Body.Data` still goes on the wire unchanged.

## Finding 12: The built-in sensitive-name list misses common credential spellings, including `Authentication`
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/secret/redact.go:19
- **Description:** The list is exactly §5a's table, matched case-insensitively — that part is correct. But `Authentication` (an exact-match miss against `authorization`), `*password*`, `*credential*`, `*signature*`, `*session*` and bare `x-auth` all fall through, on requests *and* responses. Since `hide()` and the corpus both key off this list, such a header is printed on stdout, emitted in the copy-pasteable curl, and appended to the permanent store. CONFIRMED: `X-Password`, `Authentication` and `X-Signature` all appear unredacted in `request.headers`, `request.curl` and `history.jsonl`. Supersedes the previous cycle's INFO 19, which covered only the `authorization` exact-match.
- **Suggested fix:** Extend `builtinPatterns` with `authentication`, `*auth`, `*password*`, `*passwd*`, `*credential*`, `*signature*`, `*session*`. The list is add-only by design so widening it cannot weaken anything; the only cost is over-redacting a benign `X-Session-Id`, which is the right way to be wrong here.

## Finding 13: `scrubber` misses name-matched sensitive literals, so curl's stderr is only scrubbed for structural secrets
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/curl/exec.go:265
- **Description:** The replacer that rewrites credential values out of curl's stderr is built only from pairs where `IsSecret()` is true — i.e. values backed by a `SecretRef`. A value the *name matcher* caught (`--query api_key=…`, `--header X-Api-Key=…`, a profile header) is `IsSensitive()` but not `IsSecret()`, has no ref, and is invisible to the scrubber. If curl echoes the request URL or a header into its error text, that literal reaches stderr unscrubbed — exactly the asymmetry §5a warns about ("error paths are where redaction bugs live"). Reasoned-only: the code path is confirmed, but no curl message was found that demonstrates the echo on common failure paths.
- **Suggested fix:** In `scrubber`, also add `(p.Value.Reveal(), secret.Placeholder)` pairs for values where `IsSensitive() && !IsSecret()`, plus their `url.QueryEscape` form, mirroring the existing ref branch.

## Finding 14: `scrubber` corrupts curl's error text when the credential is a short or common string
- **Reviewer:** integration
- **Severity:** WARN
- **File:** internal/curl/exec.go:257
- **Description:** The same `strings.Replacer` has no minimum length or word-boundary guard, so any occurrence of the credential *as a substring of an ordinary word* is rewritten, destroying the last diagnostic an operator has when a call fails. CONFIRMED with `TALARIA_AUTH_BEARER=t`: `"curl: (7) Failed <redacted:env:TALARIA_AUTH_BEARER>o connec<redacted:env:TALARIA_AUTH_BEARER> <redacted:env:TALARIA_AUTH_BEARER>o 127.0.0.1 por<redacted:env:TALARIA_AUTH_BEARER> 1 af<redacted:env:TALARIA_AUTH_BEARER>er 0 ms"`. The corrupted text also flows into `runResult.Reason` → the JSON report → the JUnit `<failure message=…>`. Note this is the opposite defect to finding 13 on the same function; fix both together.
- **Suggested fix:** Skip values shorter than a floor (8 chars is reasonable for anything worth calling a credential). If a short credential must be scrubbed, drop curl's detail entirely rather than emit mangled text — return the generic `curl exited %d` form.

## Finding 15: The curl-timeout fix does not cover the preflight subprocess; `curl --version` can hang talaria forever
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/curl/version.go:42
- **Description:** Commit 828ade8 bounded `ExecuteWith` but not the version preflight, which runs *before* it on every call path. `preflight` uses a plain `exec.Command(path, "--version").Output()` with no context, no deadline and no `WaitDelay`. If the curl binary is wedged (hung NFS mount, a shim on `PATH`, a slow plugin load) talaria hangs indefinitely producing no output and `--timeout` has no effect — contradicting the invariant at config.go:24 that "a wedged one becomes an exit code rather than a process an agent cannot interpret". `talaria run` is the worst case: the whole suite hangs before operation 1. CONFIRMED with a shim `curl` that sleeps on `--version`: `--timeout 3` ran until an external `timeout 20` killed it.
- **Suggested fix:** Use `exec.CommandContext` with a short deadline plus `cmd.WaitDelay`, and classify the deadline as `clierr.RequestFailed`. Secondary, same site: `preflightOnce`/`preflightErr` are package globals keyed on nothing, so the result is memoised across *different* resolved paths, undercutting the comment at exec.go:80 claiming "a PATH change mid-run cannot swap the binary between the preflight and the call" — key the cache on `path` or drop the comment.

## Finding 16: `replace` is not crash-safe: no fsync before the rename, so a crash mid-trim can destroy the whole history file
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/corpus/store.go:265
- **Description:** The comment at store.go:242 claims the swap is atomic and "a crash mid-trim leaves the old file intact". `rename(2)` is atomic with respect to *concurrent readers* — true, and what the lock+rename design needs — but it is not crash-safe: the rename metadata can reach disk before the temp file's data. After a power loss, `history.jsonl` can be the new inode with zero or partial contents, losing up to `maxPerSource` (1000) entries per source. `.Sync()` appears nowhere in the repo. Since `trim` runs on essentially every append once a source is at cap, the exposure window is every write, on the file the design calls the highest-risk artifact in the tool. Reasoned-only: a real reproduction needs a `dm-flakey`/power-cut harness; the absent `Sync()` and the overclaiming comment are both confirmed by reading.
- **Suggested fix:** In `replace`, `tmp.Sync()` before `tmp.Close()`, and after `os.Rename` open the parent directory and `Sync()` it so the directory entry is durable. Then the comment is true as written.

## Finding 17: CI never runs the race detector, so the concurrency tests guarding the corpus fix cannot catch a regression
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** .github/workflows/ci.yml:38
- **Description:** The branch added `TestConcurrentAppendsKeepEveryEntryTheyAcknowledged` (internal/corpus/store_test.go:288) specifically to lock in the `trim` fix, and several packages use `t.Parallel()`. Without `-race` none of that detects an unsynchronised write; a future change that drops the flock or shares a buffer across the parallel canary/e2e tests goes green. This directly protects the fix the previous cycle just landed. Note the race build is not free: `-race` took ~124s on `internal/corpus` alone, driven by the `maxPerSource-1` seeding loop.
- **Suggested fix:** Change the Test step to `go test -race ./...`, or add a second step running `-race` on the concurrency-bearing packages (`./internal/corpus/... ./internal/curl/... ./internal/e2e/...`) if full-suite timing is a concern. `internal/ci/workflow_test.go` asserts this file matches `.ralph/stack.json`, so update both together.

## Finding 18: `--report junit` drops the per-field validation errors `internal/validate` computed
- **Reviewer:** integration
- **Severity:** WARN
- **File:** cmd/talaria/run.go:553
- **Description:** `validate.Result.Errors` carries `{message, reason, field}` per violation and reaches the JSON report and the exit code correctly, but `junitSuite` copies only `res.Reason` (a *count*, from run.go:429) into `output.Case.Failure`. Since `--report` selects exactly one renderer, a CI job that asked for JUnit has no other artifact — the actionable data is computed, then discarded at the last boundary. internal/output/junit.go:36 states the opposite intent: "the reason is the whole value of the report". CONFIRMED: `<failure message="the response violates the spec: 2 validation errors"></failure>` while the JSON report for the same run carries `missing property 'name'` and `got number, want string` at `$.id`.
- **Suggested fix:** Add a `Detail string` to `output.Case`, emit it as the `<failure>` element's character data (`xml:",chardata"` — `xml.MarshalIndent` already escapes it), and fill it in `junitSuite` by joining `res.Validation.Errors` as `field: reason` lines.

## Finding 19: Response redaction and response validation collide, producing spurious validation failures and exit 4
- **Reviewer:** integration
- **Severity:** WARN
- **File:** cmd/talaria/call.go:390
- **Description:** `validationInput` is built from the redacted view, so a redacted field is validated as the literal string `<redacted>`. call.go:348 acknowledges the tradeoff and argues "a spurious error is visible and correctable", but nothing on any surface marks the error as talaria-induced, and it is not opt-in: the built-in list already redacts `access_token`/`refresh_token`/`id_token`, so any spec that types one of those as non-string, or constrains it with `pattern`/`maxLength`, fails out of the box. CONFIRMED with a spec typing `expires_in: integer` and config redacting it: `{"reason":"got string, want integer","field":"$.expires_in"}`, `call --fail-on-error` exit 4 and `run --fail-on-error` exit 4, against a perfectly spec-compliant server response.
- **Suggested fix:** Validate against the un-redacted bytes and pass the resulting `Error` list through the scrubbing that already exists for curl's stderr — or, cheaper and sufficient, have `redactResponse` return the set of paths it rewrote and drop/annotate any `validate.Error` whose `Field` is one of them.

## Finding 20: `internal/corpus` depends on `internal/curl`, which will block the Phase 6 twin from using the store
- **Reviewer:** integration
- **Severity:** WARN
- **File:** internal/corpus/entry.go:24
- **Description:** §5 says corpus "backs both `history` and the twin, so it is defined in Phase 2". `internal/validate` handles the identical requirement correctly — validate.go:5 explicitly refuses `http.Response` or anything from `internal/curl` and takes plain data. `corpus` does the opposite: its only entry-construction API is `NewEntry(source, req, resp *curl.Response, red)`, keyed to the outbound executor, so the twin's record path (which will hold an `http.Response`) must synthesise a fake `curl.Response`. `boundary_test.go` cannot catch this because corpus sits in the `forbidden` list rather than `shared`, despite the design assigning it the same dual-consumer role. `corpus` also transitively imports `internal/config` via `request`, contradicting its own package doc at entry.go:12 ("deliberately does not import internal/config") — confirmed with `go list -deps`.
- **Suggested fix:** Give `corpus` a plain-data input mirroring `validate.Input` (status, headers, body, timing) and have `cmd/talaria` do the `curl.Response` → that-type conversion, exactly as it already does for `validate.Input` at call.go:390. Then move `internal/corpus` into `boundary_test.go`'s `shared` list.

## Finding 21: `history replay` issues query-string credentials without the §5a warning
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** cmd/talaria/history.go:209
- **Description:** DESIGN.md:346 requires "Query-string API keys (`?api_key=`) … still leak into *server* logs — warn once on stderr", and AGENT.md:181 tells the agent "talaria warns once on stderr. Report the warning." `warnQueryCredentials` is called from cmd/talaria/call.go:156 and cmd/talaria/run.go:360 but not from replay, the third request-issuing path. CONFIRMED: replaying a recorded call whose credential rides in the query string resolves and sends the key with empty stderr and exit 0. Carried forward from the previous cycle (WARN 15), unaddressed.
- **Suggested fix:** Call `warnQueryCredentials(cmd.ErrOrStderr(), secret.NewQueryKeyWarner(), req)` immediately after `replayRequest` at history.go:205; add a test asserting stderr carries the warning on replay.

## Finding 22: Garbled missing-credential message in `run`: "no credential for security 1 scheme bearerAuth"
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** cmd/talaria/run.go:336
- **Description:** `pluralise(n, "scheme")` returns `"1 scheme"`, interpolated *after* the word "security", producing `no credential for security 1 scheme bearerAuth (set $TALARIA_AUTH_BEARER)` and `no credential for security 2 schemes bearerAuth (…), petKey (…)`. §3.1 requires errors that say what failed and why, and exit code 5's whole purpose is a sentence a human can act on. cmd/talaria/auth.go:152 gets the same message right — the previous cycle's fix (WARN 14) covered `auth` but not `run`'s two call sites (run.go:336 and run.go:396). CONFIRMED independently.
- **Suggested fix:** `clierr.CredentialMissing("no credential for %s %s", pluralise(len(r.missing), "security scheme"), strings.Join(r.missing, ", "))` at both sites.

## Finding 23: The remote-spec cache never expires and has no refresh escape hatch
- **Reviewer:** spec-compliance, integration
- **Severity:** WARN
- **File:** internal/spec/source.go:86
- **Description:** `loadURL` returns the cached bytes unconditionally whenever the file is readable — no TTL, no `If-None-Match`/`If-Modified-Since`, no `--refresh`/`--no-cache` flag anywhere in the CLI, and no location an agent can point a human at. This undercuts §3.3's pitch ("The spec *is* the collection … nothing to curate, nothing to drift"): a spec fetched once drifts silently forever, and the failure mode is `talaria call` producing 404s against an endpoint the current spec renamed. CONFIRMED: after replacing the upstream file (curl confirms it now serves `opTwo`), talaria still returned `opOne`; `talaria --help | grep -iE 'cache|refresh'` returns nothing. The code comment at source.go:98 states the situation plainly without recording a design decision. Carried forward from the previous cycle (WARN 16).
- **Suggested fix:** Store the `ETag`/`Last-Modified` alongside the body and issue a conditional GET, falling back to the cache on any network error; add a `--refresh` persistent flag on the root command and document both in README and AGENT.md.

## Finding 24: The emitted curl and `request.body` are lossy for non-UTF-8 request bodies
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** internal/curl/render.go:114
- **Description:** §3.4 promises "Every executed call returns its curl equivalent: a portable reproduction". `bodyArgs` always renders `--data-raw string(req.Body.Data)`, so for a body with invalid UTF-8 the wire path correctly stages a 0600 temp file and sends the bytes verbatim (internal/curl/config.go:256) while the *rendered* curl and `request.body` embed the bytes as a Go string — JSON encoding then replaces each invalid byte with U+FFFD. CONFIRMED: `--data-raw '�PNG\r\n\n…'`. Pasting the emitted command sends different bytes than talaria did. Commit 146dd1c fixed exactly this class for the corpus; the render path has the same defect. Secondary: a 2 MB body is inlined into `request.curl` in full, dropping 2 MB into the agent's context — the opposite of §3.2.
- **Suggested fix:** In `bodyArgs`, when `!utf8.Valid(data)` or the body exceeds a display threshold, emit `--data-binary @<path>` with a placeholder path and a sibling `body_encoding: base64` field, mirroring what `corpus` already does; note the substitution in AGENT.md's Calling section.

## Finding 25: The canary suite still omits the response-validation error path on a premise that is now stale
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/canary/canary_test.go:423
- **Description:** `TestErrorPathsDoNotLeakTheCredential` carries the comment "A response-validation failure is not here because response validation is not built yet (plan tasks 26 and 27). Whoever adds `--fail-on-error` adds the case." Response validation and `--fail-on-error` both shipped on this branch (Phase 3), so the premise no longer holds and the case was never added. §5a makes this suite a release gate over "every output surface", and finding 19 shows validation errors quote response field values — the exact material this test exists to check. Carried forward from the previous cycle (WARN 13). CONFIRMED by reading the live comment against the shipped features.
- **Suggested fix:** Add the response-validation stage to the `stages` table: a spec whose response schema the server violates, a credential present throughout, `--fail-on-error` for exit 4, scanning stdout, stderr, the report and the history store. Delete the stale comment.

---

## Finding 26: `lock_other.go` silently disables all locking on non-unix, reintroducing the trim data-loss bug
- **Reviewer:** concurrency
- **Severity:** INFO
- **File:** internal/corpus/lock_other.go:11
- **Description:** On `!unix` builds `lock` returns a no-op, so `Append`'s read-modify-write runs unserialised and two concurrent processes reproduce the destroyed-entries bug commit e0cceae fixed — with no warning, and with `Append` still returning `nil` for entries that were dropped. INFO rather than WARN only because CI builds `ubuntu-latest` only and there is no `GOOS=windows` target anywhere in the repo, so nothing ships that binary today. The comment is honest about it.
- **Suggested fix:** Add a `//go:build windows` implementation using `LockFileEx` via `golang.org/x/sys/windows`, or make the non-unix `lock` return an error so `Append` fails loudly rather than losing data silently. At minimum record the constraint where releases are configured.

## Finding 27: `curl` inherits talaria's whole environment, contradicting the comment that says it does not
- **Reviewer:** security
- **Severity:** INFO
- **File:** internal/curl/exec.go:40
- **Description:** The doc comment asserts credential values are "not in the environment curl inherits (§5a)", but `cmd.Env` is never assigned, so curl inherits `os.Environ()` in full — every `TALARIA_AUTH_*` var, plus `http_proxy`, `CURL_CA_BUNDLE`, `CURL_HOME`, `SSLKEYLOGFILE`. Not itself an escalation (a same-uid process can read `/proc/self/environ` anyway), but the comment states a property the code does not implement, and closing it is the natural companion to finding 1: a curl launched with a curated `cmd.Env` cannot have its config path, CA bundle or proxy redirected by the ambient environment.
- **Suggested fix:** Either set `cmd.Env` to a minimal allowlist and keep the comment, or correct the comment to say the guarantee is about argv only.

## Finding 28: `run --report tsv` appends a prose summary line to otherwise machine-readable output
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** cmd/talaria/run.go:539
- **Description:** README.md:456 states "`tsv` prints bare tab-separated rows with no header line, for `cut` and `awk`", but `run --report tsv` emits a trailing single-column `5 operations: 1 passed, 2 failed, 2 skipped`, which `awk -F'\t'` reads as a bogus record. Same class, smaller: `describe --output tsv` emits the pretty prose block as single-cell rows. CONFIRMED.
- **Suggested fix:** Drop the summary row from the TSV renderer and keep it in `pretty` only, or state the exception in README.

## Finding 29: README's status line claims a scaffolded command tree that does not exist
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** README.md:6
- **Description:** "…`talaria history` and `talaria run` work so far; **the rest of the command tree is scaffolded**." Nothing else is scaffolded — `twin` and its subcommands are not registered at all (`talaria twin` → exit 2, unknown command). AGENT.md:102 handles this correctly ("designed but not yet shipped"). CONFIRMED.
- **Suggested fix:** Replace with "the twin (Phases 5–8) is designed but not yet implemented".

## Finding 30: DESIGN.md calls `search` "fuzzy find"; it is case-insensitive substring matching
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** docs/design/DESIGN.md:169
- **Description:** `internal/operation/search.go:121` uses `strings.Contains`. `talaria search … invocie` (one transposition) returns nothing; `invoic` returns the full set. README.md:122 is accurate ("It matches a substring, case-insensitively") — only the design doc's inline comment overstates, and §4's own rationale for `search` is an agent that knows a domain concept but not the operationId, where a typo is plausible. CONFIRMED.
- **Suggested fix:** Amend §4's comment to "substring find across the spec", or record fuzzy matching as a deliberate deferral in §8.

## Finding 31: An unrecognised `openapi:` major version loads as an empty spec and exits 0
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** internal/spec/load.go:1
- **Description:** §4 scopes support to "Swagger 2.0 and OpenAPI 3.0/3.1/3.2", but there is no version gate before extraction: a document declaring `openapi: 4.0.0` produces `{"schema":"talaria/v1","operations":[]}` and exit 0, so an agent reads "this API has no operations" rather than exit 3, "I cannot parse this spec". 3.0/3.1/3.2 and 2.0 all work correctly. CONFIRMED.
- **Suggested fix:** Reject an `openapi:` major other than 3 (and `swagger: "2.0"`) in `spec.Load` with `clierr.SpecLoad`, naming the supported versions.

## Finding 32: `exclusiveMaximum` is carried by the strict-3.0 fixture but never asserted
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** internal/validate/testdata/strict-3.0.yaml:40
- **Description:** §8 records the libopenapi-validator 3.0 strictness question as resolved and "pinned by an adversarial 3.0 fixture carrying all four constructs, so a library upgrade that re-imposed 3.1 semantics would fail the suite". The fixture does carry all four, but internal/validate/validate_test.go asserts only `exclusiveMinimum` (line 287); nothing exercises the boolean `exclusiveMaximum` rewrite, so that quarter of the guarantee is unpinned. Carried forward from the previous cycle (INFO 20). CONFIRMED by reading fixture against test.
- **Suggested fix:** Add a case driving a value that violates only `exclusiveMaximum`, asserting `BodyValid == false`.

## Finding 33: JUnit reports 0.000s for the operation that consumed the entire run
- **Reviewer:** integration
- **Severity:** INFO
- **File:** cmd/talaria/run.go:574
- **Description:** `seconds()` returns 0 when `TimingMS == nil`, and a timed-out operation has no timing, so it is reported as taking no time. CONFIRMED: a `--timeout 2` run with 2.11s wall clock emitted `<testcase name="getSlow" time="0.000">` and `<testsuite … time="0.002">`. The comment reasons about operations that "never reached a server" — correct for a skip, wrong for a timeout, which is precisely the case a CI timing report exists to surface.
- **Suggested fix:** Record elapsed wall time in `runner.execute` around `curl.ExecuteWith` and use it when curl returned no timing.

## Finding 34: Repeated request headers are comma-folded through the corpus, so replay sends one header where the original sent two
- **Reviewer:** integration
- **Severity:** INFO
- **File:** internal/corpus/entry.go:71
- **Description:** `EntryRequest.Headers` is `map[string]string`, joined with `", "` at entry.go:191 and restored as a single pair at cmd/talaria/history.go:583. CONFIRMED: an original sending `X-Multi: one` + `X-Multi: two` replays as a single `X-Multi: one, two`. RFC 9110 field-value folding makes this equivalent for most headers and the code comments say it is deliberate, but it is a real deviation from "replay reissues the same request", with no warning. `EntryResponse.Headers` is already `map[string][]string`.
- **Suggested fix:** If fidelity matters, change `EntryRequest.Headers` to `map[string][]string` and have `replayPairs` emit one `request.Pair` per value. Otherwise note the folding in AGENT.md.

## Finding 35: DESIGN.md's exit-4 row is ambiguous about whether bare `run` should exit 4
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** docs/design/DESIGN.md:248
- **Description:** The table reads "4 | Validation failure (response violates spec) — only with `--fail-on-error` / `run`", which can be read as "the flag, or `run`" (bare `run` exits 4) or "in the `--fail-on-error` and `run` contexts" (the flag is always required). The code implements the latter: `runner.verdict` (cmd/talaria/run.go:399) gates exit 4 on `failOnError`, and a run with 5 failed operations exits 0. CONFIRMED. The previous cycle recorded this as a WARN contradiction; the behaviour is defensible and the doc is the ambiguous part, so it is recorded here as a wording fix rather than a code bug.
- **Suggested fix:** Reword to "only with `--fail-on-error` (on `call` or `run`)" if the current behaviour is intended, and state in README/AGENT.md that bare `run` reports failures without failing the process.
