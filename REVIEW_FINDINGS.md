# Review findings — `ralph/design` vs `main`

Reviewers run: security, spec-compliance, concurrency, integration (`.ralph/stack.json`).
Spec: `docs/design/DESIGN.md`. Conventions: `README.md`, `AGENT.md`,
`docs/research/2026-08-02-design-review.md`.

Build, `gofmt`, `go vet` and `go test -race ./...` are all clean at HEAD.

33 findings: 6 CRIT, 17 WARN, 10 INFO.

## Finding 1: CR/LF in a header, cookie or method value smuggles a second request onto the wire
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/request/build.go:259
- **Description:** Nothing validates that a header value, cookie value or method is free of CR/LF. `escapeDirective` (internal/curl/config.go:393) escapes `\n`/`\r` so they cannot break out of a curl *config directive*, but curl un-escapes them back to literal CR/LF inside the value and writes them straight onto the wire. Header *names* are validated only on the `--header` path (`httpFieldName`, build.go:361); names from spec-declared header/cookie parameters (`located`, build.go:259), from profile headers (build.go:289) and from a replayed history entry are unvalidated. Reproduced against a listener — a single call emitted:

  ```
  GET /p HTTP/1.1
  Host: 127.0.0.1:19977
  X-Trace: ok

  DELETE /admin HTTP/1.1
  Host: 127.0.0.1
  X-Smug: yes
  ```

  A second, complete, attacker-chosen request goes out on the connection. The injected value can come from the *spec* (a required header param's `example`, consumed by `run`), so this is reachable with nothing but an untrusted API doc — which is the advertised workflow ("point it at any API's OpenAPI doc"). It defeats the `--allow-mutations` gate (DESIGN.md §3.5) entirely: a GET-only spec issues a DELETE. The same value also corrupts `--output tsv` row structure.
- **Suggested fix:** Reject any value containing `\r` or `\n` at bind time in `internal/request/build.go` (for `located`, `pairs` and profile headers) as a usage error, and re-check in `document.auth`/`document.cookies` as the last gate before the wire. Apply `isFieldName` to spec- and profile-supplied header names too, and validate `req.Method` against an HTTP token charset before writing the `request` directive.

## Finding 2: The emitted and `--dry-run` curl turns a GET with a body into a POST
- **Reviewer:** integration
- **Severity:** CRIT
- **File:** internal/curl/render.go:45
- **Description:** `Render` deliberately omits `-X` for GET, but `bodyArgs` (render.go:118) still appends `--data-raw`. curl's rule is that `-d`/`--data-raw` without `-X` implies POST. The config document always writes `request = "GET"` (internal/curl/config.go:149), so talaria really sends a GET-with-body while the command it prints is a POST with `Content-Type: application/x-www-form-urlencoded`. Reproduced: `call … getEcho --body '{"a":1}'` renders `curl -q -s --data-raw '{"a":1}' 'http://…/echo'`; the server saw `GET` from talaria and `POST` from the pasted line. An agent pasting `request.curl` into a bug report, or a human reproducing a 4xx, exercises a different method — and on a mutation-gated API a "read-only" GET reproduction silently becomes a write. This is the same class of bug commit 2d92e67 fixed for HEAD, left open for GET.
- **Suggested fix:** Emit `-X` whenever the command carries a body: change the GET guard to `case req.Method != "" && !(strings.EqualFold(req.Method, http.MethodGet) && req.Body == nil):`.

## Finding 3: `auth check` blesses a call that `call` and `run` then refuse with exit 5
- **Reviewer:** integration
- **Severity:** CRIT
- **File:** internal/config/auth.go:94
- **Description:** `Resolve` returns on the first requirement whose schemes are of a *supported type*, without consulting `Present()`. `satisfied` (cmd/talaria/auth.go:169) implements the opposite, correct rule: any one alternative being fully present is enough. The two components answer the same question differently, and `auth check` is the documented pre-flight ("The source follows the same resolution `call` uses", README.md:243). Reproduced with `security: [{keyA: []}, {keyB: []}]` and only `TALARIA_AUTH_APIKEY_KEYB` exported: `auth check` exits 0, `call` exits 5 with "no credential in $TALARIA_AUTH_APIKEY_KEYA", `run` exits 5 with every operation skipped, and `--dry-run` renders `-H "X-A: $TALARIA_AUTH_APIKEY_KEYA"`. An agent following AGENT.md's loop asks a human to export a variable the spec did not require, and has no way to discover that.
- **Suggested fix:** In `Resolve`'s loop, prefer the first requirement whose credentials are all `Present()`, falling back to the first supported one only if none is fully satisfiable (so the exit-5 message still names something actionable). Share one predicate with `cmd/talaria/auth.go`'s `satisfied` rather than duplicating the rule in two packages — the duplication is what let them drift.

## Finding 4: Ctrl-C during `run` fabricates failures for every untested operation and still exits 0
- **Reviewer:** concurrency
- **Severity:** CRIT
- **File:** cmd/talaria/run.go:123
- **Description:** The suite loop never consults `r.ctx`. Once the signal context is cancelled, `curl.ExecuteWith` returns immediately for every remaining operation and each is recorded as a genuine failure and appended to history. Reproduced — 30 GET operations against a 2s/request server, SIGINT at t=5s: server saw 3 hits, report said `{total: 30, passed: 2, failed: 28}` with `"reason": "the request was cancelled before it completed"` on all 28, history gained 30 entries, exit code **0**. Three harms: (a) a cancelled CI job reports 28 endpoints broken that were never asked, and with `--fail-on-error` exits 4 — a validation verdict for a suite that never ran, exactly the result run.go:186-188 says a smoke test "must never produce"; (b) without `--fail-on-error` a cancelled run is green; (c) the junk entries go in under `SourceRun` and are subject to `trim`'s per-source cap of 1000 (internal/corpus/store.go:31), so a cancelled run over a large spec evicts the previous run's real history — signal-triggered data destruction.
- **Suggested fix:** Break the loop when `r.ctx.Err() != nil` and report only what ran. In `verdict` (run.go:403) return a distinct error on cancellation so the run neither exits 0 nor masquerades as a validation failure, and skip `recordCall` when the failure is cancellation so history is not polluted.

## Finding 5: A credential-shaped literal query value leaks in cleartext through curl's stderr
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/curl/exec.go:277
- **Description:** `scrubber` builds its replacer only from pairs where `p.Value.IsSecret()` is true. A literal the user typed under a credential-shaped name — `--query api_key=…`, which `hide()` (internal/request/build.go:309) deliberately marks `Sensitive` and renders `<redacted>` on stdout, in history and in the emitted curl — is skipped, so nothing rewrites it out of curl's stderr. curl exit 3 echoes the full URL including the query string, and `requestFailed` embeds that text in the exit-1 JSON on stderr. Reproduced (a spec path containing `[` forces curl exit 3):

  ```
  {"schema":"talaria/v1","error":{"code":1,"message":"… curl: (3) bad range in URL position 26:\nhttp://127.0.0.1:19930/a[b?api_key=SUPERSECRETLITERAL\n ^"}}
  ```

  The same call with an `apiKey`-in-query *security scheme* correctly prints `<redacted:env:…>`, confirming the gap is exactly the literal path. DESIGN.md §5a: "Errors are built from the redacted representation, never the raw one. Test explicitly — error paths are where redaction bugs live."
- **Suggested fix:** Iterate on `p.Value.IsSensitive()` rather than `IsSecret()`; for a sensitive literal use `p.Value.Reveal()` as the needle and `secret.Placeholder` as the replacement, plus its `url.QueryEscape` form as the ref branch already does. Add a canary case for a literal `--query api_key=`.

## Finding 6: `history replay` resolves an arbitrary named env var and sends it to an arbitrary host
- **Reviewer:** security
- **Severity:** CRIT
- **File:** cmd/talaria/history.go:668
- **Description:** `replayValue` turns any stored string of the form `<redacted:env:NAME>` back into a live `SecretRef` (`secret.ParseRef`, internal/secret/secret.go:55, which accepts any non-empty name), and `internal/curl` resolves it at exec time. Both `NAME` and the destination (`entry.URL`) come from the store, a plain JSONL file in the state directory. Reproduced: a hand-written `history.jsonl` line with `"headers":{"X-Steal":"<redacted:env:MY_UNRELATED_SECRET>"}` and `"url":"http://127.0.0.1:19950/exfil"`, then `talaria history replay x1`, put `X-Steal: AWS_ROOT_KEY_ABC123` on the wire to that host. This makes the binary a general-purpose "resolve $ANY_VAR and send it to $ANY_URL" primitive, well outside the `TALARIA_AUTH_*` namespace the firewall is scoped to, and launders it through the one process the deployment trusts with credentials — a direct break of §5a's invariant under the deployment §5a itself names as fully realising the boundary ("running the agent without those vars in its environment — and letting the binary read them from a file it alone opens").
- **Suggested fix:** Do not accept an arbitrary ref name from the store. Restrict replay-resolvable refs to the `TALARIA_AUTH_*` convention plus names present in the selected profile's `auth:` map, and reject anything else with a usage error naming the variable it refused.

## Finding 7: curl inherits the full environment, contradicting the invariant stated in the code
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/curl/exec.go:41
- **Description:** The comment claims credentials are "not in the environment curl inherits (§5a)". `cmd.Env` is never set, so Go passes `os.Environ()` and every `TALARIA_AUTH_*` value is readable in `/proc/<curl-pid>/environ` for the lifetime of the call. The argv half of the claim is genuinely implemented; the environment half is not, and a reader will believe a property the code does not provide.
- **Suggested fix:** Set `cmd.Env` to a filtered environment (keep `PATH`, `HOME`, proxy and TLS variables; drop `TALARIA_AUTH_*` and anything a profile references), or narrow the comment to what is true.

## Finding 8: `redact.body-paths` is applied to the request body in history but not on stdout
- **Reviewer:** security
- **Severity:** WARN
- **File:** cmd/talaria/call.go:465
- **Description:** `corpus.Redactors.requestBody` (internal/corpus/entry.go:242) runs the configured JSON paths over the request body, so `access_token` is stripped from the permanent artifact. `callPayload` writes `view.Request.Body = string(req.Body.Data)` raw, and `curl.Render`'s `--data-raw` does the same. Observed: stdout `"body":"{\"access_token\":\"SUPERCANARY123\"}"`, history `"data":"{\"access_token\":\"<redacted>\"}"`. This is the exact inversion `buildRequest`'s own comment warns against — hiding a value in the permanent artifact but not on the stdout an agent reads has the firewall backwards. The canary suite does not inject through `--body`, so it does not catch this.
- **Suggested fix:** Apply the same `ResponseRedactor.Body` to `view.Request.Body` in `callPayload` and to the `--data-raw` word in internal/curl/render.go:128. Add a canary case that injects through `--body`.

## Finding 9: The spec cache never expires, revalidates, or verifies
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/spec/source.go:87
- **Description:** `loadURL` returns cached bytes whenever the file exists — no TTL, no ETag/Last-Modified revalidation, and no flag to force a refresh. `isURL` (source.go:79) also accepts plaintext `http://`. One on-path modification of a single `http://` spec fetch is therefore permanent: a poisoned `servers[0].url` (which is where every later call's resolved credential goes) and any injected header value from Finding 1 persist across every subsequent run, including runs made over a trusted network.
- **Suggested fix:** Stamp the cache entry with its fetch time and re-fetch past a TTL; send `If-None-Match`/`If-Modified-Since`; add `--refresh-spec`; warn once on stderr when a spec is fetched over plaintext `http://`.

## Finding 10: `search` is exact-substring matching, which DESIGN calls fuzzy
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** internal/operation/search.go:121
- **Description:** Matching is `strings.Contains(f.text, query)` on lower-cased text. DESIGN.md:169 annotates the command `# fuzzy find across the spec`, and the justifying paragraph (DESIGN.md:210-213) rests on the agent knowing "a *domain concept* … rather than an operationId". Verified: `search search.yaml invoice` → 5 hits; `search search.yaml invoce` → `{"results":[]}`, exit 0. A typo, a plural/singular mismatch or a synonym silently reports that the API has no such concept. README.md:120 documents the shipped substring behaviour, so DESIGN and README now contradict each other.
- **Suggested fix:** Either add an edit-distance/subsequence fallback ranked below exact matches, or amend DESIGN.md:169 to `# substring find across the spec`. This branch has already amended DESIGN.md for other resolved decisions; leaving this one is the inconsistency.

## Finding 11: The exit-code table reads as "`run` always exits 4"; shipped `run` needs `--fail-on-error`
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** docs/design/DESIGN.md:248
- **Description:** The table says exit 4 applies "only with `--fail-on-error` / `run`". `cmd/talaria/run.go:408` gates it on `failOnError && view.Summary.Failed > 0`. Verified: `run` over a spec with a schema-violating response exits 0 without the flag and 4 with it. The behaviour is defensible and matches DESIGN.md:196 and README.md:464, but an agent given only the exit-code table branches wrong.
- **Suggested fix:** Amend DESIGN.md:248 to "only with `--fail-on-error` (on `call` or `run`)".

## Finding 12: stderr mixes bare prose warnings with the JSON envelope
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** internal/secret/response.go:258
- **Description:** `QueryKeyWarner.Warn` and `warnUnreplayable` (cmd/talaria/history.go:687) both `fmt.Fprintf` unwrapped text to stderr. On a call that both warns and fails, stderr is a prose line followed by the JSON error object. AGENT.md:200 tells the model "Errors go to **stderr** as one line of JSON in the same envelope", and cmd/talaria/root.go:138-140 states the rule explicitly — "prose in front of it is what an agent parsing stderr would choke on". An agent doing `json.loads(stderr)` fails.
- **Suggested fix:** Emit warnings inside the versioned envelope (`{"schema":"talaria/v1","warning":{…}}`), or declare stderr newline-delimited JSON in AGENT.md and make the warnings JSON too.

## Finding 13: README claims a scaffolded command tree that does not exist
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** README.md:8
- **Description:** "the rest of the command tree is scaffolded." `talaria twin` returns `{"error":{"code":2,"message":"unknown command \"twin\" for \"talaria\""}}`; there is no `internal/twin` package and no `twin` command registered in cmd/talaria/root.go:92-100. Phases 5–8 being unbuilt is in scope and expected; the README asserting otherwise is not — an agent reading it will try `talaria twin serve` and get a usage error.
- **Suggested fix:** Replace with "Phases 5–8 (`twin serve`/`record`/`fault`) are designed but not implemented."

## Finding 14: "Every executed call carries a `validation` block" is false for `history replay`
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** cmd/talaria/history.go:225
- **Description:** Replay passes `nil` for the validation result (the comment at :223 correctly explains replay has no spec to check against), so the envelope omits the block. README.md:333 and AGENT.md:81 both say *every* executed call carries it. An agent that unconditionally reads `validation.body_valid` after a replay hits a missing key.
- **Suggested fix:** One clause in both docs: "except `history replay`, which re-sends a recorded request without a spec".

## Finding 15: The missing-path-parameter error contradicts itself, and the text reaches JUnit XML
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** internal/request/build.go:244
- **Description:** When a *declared, required* path param is unbound, both failures are emitted: `--param petId is required (path parameter); path "/pets/{petId}" has a placeholder the operation declares no parameter for`. The second clause is false — the operation does declare it. Verified verbatim in `call` stderr, in `run --report json` (`"reason"`), and in `<skipped message="…">` in the JUnit output. DESIGN.md §3.1 requires errors that say what failed and why.
- **Suggested fix:** In `binder.path`, emit the leftover-placeholder error only for placeholders with no matching declared param; suppress it when a required-param error was already recorded for that name.

## Finding 16: `run --tag <unknown>` exits 2 without `valid_alternatives`
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** cmd/talaria/run.go:217
- **Description:** `clierr.Usage("no operation matches %s", …)` carries no `.WithAlternatives(...)`, unlike every other unknown-name error — `run --operation bogus` lists all operation ids, and `describe`/`uses`/`--output`/`--kind` all list alternatives. DESIGN.md:246 requires exit 2 to come with "stderr JSON lists valid options", and the spec's tag set is enumerable at that point.
- **Suggested fix:** Collect the distinct tags (and ids) from the loaded document and attach them with `WithAlternatives`.

## Finding 17: 42 MB of agent transcripts and stale process artifacts are committed
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** .ralph/ralph-20260802-154350.log:1
- **Description:** That log (22.8 MB) and `.ralph/ralph-20260802-195025.log` (19.6 MB) account for roughly 403,000 of the diff's 433,282 inserted lines. `IMPLEMENTATION_PLAN.md:1` is titled "talaria — Review Fix Plan", is scoped to a completed round of findings, and points at `REVIEW_FINDINGS.md`, itself a process artifact. For a project whose premise is that transcripts are a leak channel (DESIGN.md §1: "It is in the context window, in the transcript, in the logs"), shipping full agent transcripts in the repo is also the wrong signal.
- **Suggested fix:** Add `.ralph/*.log` to `.gitignore` and drop the two logs from the branch; remove or relocate `REVIEW_FINDINGS.md` and `IMPLEMENTATION_PLAN.md` before the PR merges.

## Finding 18: The process-group kill has no already-reaped guard
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/curl/procgroup_unix.go:33
- **Description:** `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)` signals a bare pid with no check that the child is alive. `os/exec.Wait` calls `Process.Wait()` — reaping the child and freeing its pid — and only then receives from `ctxResult`, so `watchCtx` can invoke `Cancel` after the reap. Interleaving: curl exits and is reaped → SIGINT arrives (or the `MaxTime+2s` deadline fires) → `Cancel` runs → `Kill(-pid, SIGKILL)`. If the pid has been recycled as a process-group leader in that window, talaria SIGKILLs an unrelated group owned by the same uid. The stdlib's default `Cancel` is immune because `os.Process` tracks `statusDone` and returns `ErrProcessDone`; overriding it with a raw negated-pid kill discards that protection. The window is microseconds and pid wrap is slow, hence WARN, not CRIT.
- **Suggested fix:** Probe with `cmd.Process.Signal(syscall.Signal(0))` first and return its error (os/exec reads `ErrProcessDone` as "already finished") before issuing the group kill; fall back to `cmd.Process.Kill()` if the group kill fails.

## Finding 19: The curl version preflight is unbounded and ignores both `--timeout` and Ctrl-C
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/curl/version.go:42
- **Description:** `exec.Command(path, "--version").Output()` has no context, no `WaitDelay` and no timeout. It runs at internal/curl/exec.go:61, *before* `execCtx` is built at exec.go:81, so neither the `--timeout` bound nor the SIGINT context reaches it. A `curl` that is a shell wrapper, or a binary on a stalled NFS/autofs mount, wedges talaria indefinitely — and because of Finding 20 no catchable signal ends it. The only way out is SIGKILL, which is exactly the signal that strands a capture directory in `TMPDIR` for 24h (internal/curl/sweep.go:26).
- **Suggested fix:** Thread a context through `preflight` and wrap it in `context.WithTimeout(ctx, 5*time.Second)` with `cmd.WaitDelay = time.Second`; call `preflight(ctx, path)` from exec.go:61.

## Finding 20: SIGINT/SIGTERM are swallowed for the process lifetime with no escape hatch
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** cmd/talaria/root.go:207
- **Description:** `signal.NotifyContext` permanently diverts both signals into a context; nothing re-raises, and `stop()` runs only when `run` returns. Every path that blocks without watching that context becomes unkillable by any catchable signal, and a reflexive second Ctrl-C does nothing. Four such paths exist on this branch: the `run` suite loop (Finding 4), the unbounded preflight (Finding 19), `syscall.Flock(fd, LOCK_EX)` at internal/corpus/lock_unix.go:36 (no `LOCK_NB`, no deadline, no context), and `client.Get(url)` at internal/spec/source.go:117 (bounded by a 30s `fetchTimeout`, but uninterruptible). The user's remaining option is `kill -9`, the one signal that defeats every deferred cleanup this branch added.
- **Suggested fix:** Replace `NotifyContext` with an explicit `signal.Notify` handler that cancels the context on the first signal and calls `signal.Stop` so a second signal restores default disposition and kills the process outright.

## Finding 21: The non-unix corpus lock is a silent no-op, so `trim` can still destroy entries there
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/corpus/lock_other.go:11
- **Description:** `func lock(string) (func(), error) { return func() {}, nil }` under `//go:build !unix`, which excludes Windows, js and wasip1. On those platforms two concurrent processes both run the unserialised read-modify-write in `Append` (internal/corpus/store.go:115-126) and the loser's entry is erased by the winner's `replace()` rename at store.go:323 — the exact bug commit e0cceae was written to fix. `Append` returns `nil` in both, so the loss is never reported, contradicting the contract at store.go:94-95. CI is `ubuntu-latest` only and there is no release workflow yet, so this is latent — but DESIGN.md:459 promises "single static binaries per platform".
- **Suggested fix:** Implement it with `golang.org/x/sys/windows.LockFileEx`, or keep the dependency-free posture and make the no-op loud: return an error from `lock` on `!unix`, or have `Append` skip `trim` there so appends stay append-only and nothing can be destroyed.

## Finding 22: OpenAPI server variables are dropped, so templated `servers[0].url` fails every call
- **Reviewer:** integration
- **Severity:** WARN
- **File:** internal/request/build.go:178
- **Description:** `firstServer` returns `Servers[0].URL` verbatim and ignores `Servers[0].Variables`, whose `default` values are what make a templated server usable. `baseURL()` then rejects the templated URL at build.go:160. Reproduced with the common shape `servers: [{url: "https://{env}.example.com/{basePath}", variables: {env: {default: api}, basePath: {default: v1}}}]`: every `call` and every operation of `run` fails with `base URL "https://{env}.example.com/{basePath}" … is not an absolute http(s) URL`, and the message never mentions `--base-url`, the escape hatch. `{host}` templating is standard OpenAPI, not a malformed spec.
- **Suggested fix:** Substitute `{name}` → `Variables[name].Default` in `firstServer` before returning, leaving any unsubstituted placeholder to fail as it does today; append "; pass --base-url to override" to the build.go:160 message when the candidate came from the spec.

## Finding 23: The "`--dry-run` equals real" e2e assertion is a tautology
- **Reviewer:** integration
- **Severity:** WARN
- **File:** internal/e2e/e2e_test.go:454
- **Description:** The package doc claims the suite proves "the curl `--dry-run` shows is the curl `call` runs", but the assertion compares `dry.Request.Curl` to `called.Request.Curl` — both produced by the same `curl.Render` call on the same `*request.Request`. It can never fail. Nothing in the module executes a rendered command and compares it to what talaria actually sent (internal/curl/config_test.go:479 runs curl from the config document only). This is why Finding 2 survived three review cycles.
- **Suggested fix:** Add an e2e case that takes `request.curl` from `--dry-run`, runs it through `sh -c` against the same `httptest` server, and asserts the recorded method, path, headers and body match those of the real `call`. Parameterise over GET-with-body, HEAD, a cookie param and a form body.

## Finding 24: Re-marshalling a redacted response body HTML-escapes the whole document
- **Reviewer:** security
- **Severity:** INFO
- **File:** internal/secret/response.go:119
- **Description:** When any path matches, `json.Marshal` re-encodes with Go's default HTML escaping, so the placeholder becomes `"<redacted>"` and every unrelated `<`, `>`, `&` in the server's response is rewritten too (`"note":"a<b&c"` → `"a<b&c"`). Semantically equivalent, but it contradicts the file's stated goal of reporting what the server actually sent, and only on the redacted path — so two responses differing by one field render inconsistently.
- **Suggested fix:** Use a `json.Encoder` with `SetEscapeHTML(false)` instead of `json.Marshal`.

## Finding 25: `request.headers` carries a scheme prefix the DESIGN sketch does not show
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** internal/request/request.go:101
- **Description:** Returns `v.Prefix() + v.ref.String()`, producing `"Authorization": "Bearer <redacted:env:TALARIA_AUTH_BEARER>"`. DESIGN.md:233 shows it without the prefix. The shipped form is strictly more informative and leaks nothing; §5a:339 only requires `<redacted:env:NAME>`.
- **Suggested fix:** Update the sketch at DESIGN.md:233 so the doc and the envelope agree byte-for-byte.

## Finding 26: Request and response headers have different shapes in the same envelope
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** internal/curl/exec.go:27
- **Description:** Request headers serialise as `map[string]string`; response headers as `map[string][]string` (`http.Header`). An agent parsing "the headers of this envelope" needs two code paths.
- **Suggested fix:** Document the asymmetry in AGENT.md, or normalise response headers to single strings joined on `, `.

## Finding 27: `--output tsv` silently renders pretty text on row-less commands
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** README.md:477
- **Description:** README states `tsv` "prints bare tab-separated rows with no header line" for every command. Verified: `describe … --output tsv` and `call … --output tsv` emit the identical multi-line pretty rendering, with no tabs. DESIGN only requires tsv on `list` (DESIGN.md:167), so this is doc scope, not a missing feature.
- **Suggested fix:** Qualify README.md:471-477 — "`tsv` is a row format; commands with no row shape (`describe`, `call`, `version`) render their pretty form under it."

## Finding 28: DESIGN names viper for profile/env layering; it is not a dependency
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** docs/design/DESIGN.md:279
- **Description:** "spf13/cobra (+ viper for profile/env layering)". `go.mod` has no viper; layering is hand-rolled in internal/config/config.go over `gopkg.in/yaml.v3`. The result is simpler and satisfies §5 Auth, but the doc still prescribes viper and `.ralph/stack.json:3` still records it.
- **Suggested fix:** Amend DESIGN.md:279 and `.ralph/stack.json` to record the decision, as this branch did for the libopenapi-validator strictness question.

## Finding 29: `history replay` silently ignores the global `--base-url`
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** cmd/talaria/history.go:567
- **Description:** Replay rebuilds `BaseURL` from the recorded URL unconditionally; `--base-url` is registered persistently at cmd/talaria/root.go:73, so it parses and is discarded. Replaying a recorded staging call against localhost is the natural thing to try. The help text says "to the URL it went to", so the behaviour is discoverable, but a flag that accepts a value and does nothing is a trap.
- **Suggested fix:** Either honour `--base-url` on replay by re-pointing the recorded path and query at the new host, or reject it with a usage error naming why.

## Finding 30: The request body's media type is in the corpus but absent from `call`'s output
- **Reviewer:** integration
- **Severity:** INFO
- **File:** cmd/talaria/call.go:45
- **Description:** `requestView.Body` is a bare string; the media type appears only inside the rendered `curl` string. `corpus.Body` records `content_type`, so `history show --output json` exposes it and `call --output json` does not. An agent that wants to know what media type its generated body will be sent under has to regex the `curl` field.
- **Suggested fix:** Add `BodyContentType string \`json:"body_content_type,omitempty"\`` to `requestView`, populated from `req.Body.ContentType` in `callPayload` (call.go:464).

## Finding 31: Garbled missing-credential message in `run`
- **Reviewer:** integration
- **Severity:** INFO
- **File:** cmd/talaria/run.go:405
- **Description:** `pluralise(len(r.missing), "scheme")` is interpolated into a sentence that already reads "for security", producing `no credential for security 1 scheme tok (set $TALARIA_AUTH_BEARER)`. cmd/talaria/auth.go:155 phrases the same condition as `no credential for security scheme tok (…)`, and the run.go comment claims it uses "the same phrasing `auth check` uses". Also present at run.go:342, and already listed on `.ralph/pr_body.md:93`.
- **Suggested fix:** Drop the count — `"no credential for security scheme%s %s"` with `""`/`"s"` — or call the helper auth.go uses.

## Finding 32: `Read` takes no lock, so a concurrent `history` can miss the newest entry
- **Reviewer:** concurrency
- **Severity:** INFO
- **File:** internal/corpus/store.go:181
- **Description:** `Read` does a bare `os.ReadFile` while another process may be inside `write()`'s `f.Write` (store.go:233). A partial trailing line is skipped at store.go:202 and the newest entry is silently invisible. `replace`'s rename is atomic, so trim cannot tear a read — this is the append path only, and the skip is deliberate. It matters because `history show N` / `replay N` are index-based, so a reader racing an appender can index a different entry than the list it just printed. The id form added by 5aa07bf is the mitigation; the index form retains the hazard.
- **Suggested fix:** Take the same `lock(path)` in `Read` — it is cheap and already exists — or document that index selection is stable only under a quiescent store.

## Finding 33: `replace` renames without fsync, and its temp files are outside the sweep
- **Reviewer:** concurrency
- **Severity:** INFO
- **File:** internal/corpus/store.go:323
- **Description:** `replace` writes, closes and renames with no `tmp.Sync()` and no directory fsync. The comment at store.go:296-297 claims "a crash mid-trim leaves the old file intact" — true for a process crash, but on power loss the rename can reach disk before the data blocks, leaving a zero-length `history.jsonl`. Separately, a SIGKILL between `CreateTemp` (store.go:301) and `Rename` strands `.history-*` files in the state directory, which `curl.SweepStale` never collects — it scans only `os.TempDir()` (internal/curl/sweep.go:41).
- **Suggested fix:** `tmp.Sync()` before `tmp.Close()`, and either extend `SweepStale` to the state directory or have `Append` remove stale `.history-*` siblings while it already holds the lock.
