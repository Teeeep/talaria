# Review findings — branch `ralph/design` vs `main`

Reviewers run: security, spec-compliance, concurrency, integration (per `.ralph/stack.json`).
Baseline: `go build ./...`, `gofmt -l .`, `go vet ./...` and `go test -race ./...` all pass on
the tracked tree. Every finding below was reproduced by reading the code and running the built
binary or a probe test; nothing here is speculative.

Scope: Phases 1–4 of `docs/design/DESIGN.md` §7 (tasks 1–33). The twin (Phases 5–8) is
correctly absent and is not reported.

20 findings: 11 CRIT, 7 WARN, 2 INFO.

---

## Finding 1: A credential passed as a query parameter is never redacted
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/request/build.go:76
- **Description:** `Build` wraps `req.Headers` (line 77) and `req.Cookies` (line 78) in `hide()`,
  the §5a built-in name matcher, but leaves `req.Query` bare. Query is the one credential
  location where the firewall silently does not apply, and DESIGN.md §5a's leak-channel table
  names it explicitly: "Query-string API keys (`?api_key=`) → Symbolic in emitted curl,
  `<redacted>` in output URL fields." Reproduced with the built binary — an identically named
  header is redacted while the query key is not:
  `talaria call spec.yaml getPub --query api_key=SUPERSECRET123 --header X-Api-Key=SUPERSECRET456 --output json --dry-run`
  emits `"curl":"curl -s -H 'X-Api-Key: <redacted>' 'http://…/pub?api_key=SUPERSECRET123'"` and
  `"url":"http://…/pub?api_key=SUPERSECRET123"`. On a real call the same value is written verbatim
  into `$XDG_STATE_HOME/talaria/history.jsonl`, the artifact §5a calls "the highest-risk surface in
  the tool", where redaction is required at write time. It also reaches `validate.Input.URL` via
  `curl.URL(req)`, so a validation error message can quote it. This is a direct breach of the
  invariant the product exists to enforce.
- **Suggested fix:** `req.Query = hide(append(b.located(bound, inQuery), b.pairs(in.Query, "--query")...))`.
  Note the display consequence: `urlWord` (internal/curl/render.go:139) and `Request.QueryString`
  (internal/request/request.go:238) branch on `IsSecret()` rather than `IsSensitive()`, so a hidden
  literal currently renders as `%3Credacted%3E`; both should skip `url.QueryEscape` for a sensitive
  value. Add a `--query` case to `TestAUserSuppliedHeaderIsRedactedLikeASpecCredential` in
  internal/canary/canary_test.go — the canary suite has no `--query` counterpart today, which is why
  this shipped.

## Finding 2: Base URL scheme is never validated, so any spec can make curl speak gopher/file/dict
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/request/build.go:141
- **Description:** `baseURL` checks only that the URL parses and has a non-empty `Scheme` and
  `Host`. The scheme itself is never compared against anything, even though the error message on
  the very next line promises "is not an absolute http(s) URL". The value flows into
  `document.build`'s `url = "…"` directive, and internal/curl/config.go sets no `proto` /
  `proto-redir` restriction, so system curl will speak whatever protocol the scheme names.
  Confirmed against the built binary: `talaria call spec.yaml getPub --base-url gopher://127.0.0.1:18201
  --output json --dry-run` is accepted and emits `curl -s 'gopher://127.0.0.1:18201/pub'`. The
  candidate list in `baseURL` includes `firstServer(b.in.Doc)` — the spec's own `servers[0].url` —
  and the product's premise is that an agent points this at any OpenAPI doc it found, so that field
  is untrusted input. A spec with `servers: [{url: "gopher://127.0.0.1:18201"}]` and a path of
  `/_SET%20talaria%20pwned` made a plain `talaria call` (no `--base-url`, no `--allow-mutations`)
  deliver `SET talaria pwned\r\n` to a raw TCP listener — the classic Redis/memcached SSRF gadget
  with an attacker-controlled payload. It also bypasses the `--allow-mutations` rail entirely, since
  that rail gates the HTTP method and gopher has none. `file://` is likewise accepted and read.
- **Suggested fix:** In `baseURL`, reject any scheme other than `http` and `https`, making the check
  match the message it already prints. Defence in depth: emit `proto = "=http,https"` and
  `proto-redir = "=http,https"` directives in `document.build`. cmd/talaria/history.go:494 rebuilds
  `BaseURL` from a stored URL with the same missing check and needs the same guard.

## Finding 3: HEAD operations emit `-X HEAD` and hang forever
- **Reviewer:** integration
- **Severity:** CRIT
- **File:** internal/curl/config.go:78
- **Description:** `document.build` emits `request = "HEAD"`, which is `curl -X HEAD`, not `--head`.
  curl then waits for a body of `Content-Length` bytes that a compliant server never sends. `HEAD`
  is in `safeMethods` (internal/operation/operation.go:106), so it needs no `--allow-mutations` and
  `run` includes it by default. Reproduced against a local HTTP/1.1 server answering HEAD with
  `Content-Length: 42` and no body: `timeout 12 talaria call headspec.yaml headThing --base-url
  http://127.0.0.1:18878` exits 124 — hung. Against an HTTP/1.0 server that closes the connection it
  instead fails outright: `{"error":{"code":1,"message":"…curl exited 18: curl: (18) end of response
  with 42 bytes missing"}}`. Standalone `curl -X HEAD` reproduces both (exit 18 / hang) where
  `curl -I` returns instantly. Because there is no `max-time` (Finding 4), one HEAD operation in a
  spec makes `talaria run` never terminate and emit no report at all. No test in the repo ever
  executes a HEAD operation — operation_test.go:227 only asserts `IsMutation()==false`.
- **Suggested fix:** In `document.build`, emit the bare `head` directive when `req.Method == "HEAD"`
  instead of `request = "HEAD"`; curl also echoes the header block into the `output` file for `--head`,
  so route the body capture to `/dev/null` for that method. Mirror it in internal/curl/render.go:36
  as `-I`. Add a HEAD case to the executor tests.

## Finding 4: No timeout of any kind on the curl subprocess
- **Reviewer:** concurrency
- **Severity:** CRIT
- **File:** internal/curl/config.go:94
- **Description:** `document.build` emits only `url, request, header, cookie, data-raw, silent,
  show-error, write-out, output, dump-header` — no `max-time`, no `connect-timeout` — and
  internal/curl/exec.go:64 uses `exec.Command`, not `exec.CommandContext`. `grep -rn
  "Timeout|context\." --include=*.go` over non-test code matches only internal/spec/source.go, so
  there is no timeout anywhere in the call path and no flag to bound one. Verified: against an
  endpoint sleeping 90s, `Execute` was still blocked at 25s; against a server that never answers it
  blocks indefinitely. The impact is worst for `run` — the suite stalls on operation *k* of *n* and
  never emits its report, so CI or an agent gets no partial output at all. The design doc applies
  exactly this reasoning to the *spec fetch* (internal/spec/source.go:21 — "A spec that never arrives
  has to become an exit code rather than a hung process an agent cannot interpret") while leaving
  the higher-risk path unbounded.
- **Suggested fix:** Emit `connect-timeout` and `max-time` directives in `document.build`, defaulted
  and overridable via a `--timeout` flag. curl enforces these itself and exits 28, which `runFailure`
  already classifies correctly, so no Go-side plumbing is needed on the happy path. Pair with
  `exec.CommandContext` plus `cmd.WaitDelay` at a slightly larger deadline so a curl that ignores its
  own timeout is still reaped.

## Finding 5: `corpus.trim` is an unlocked read-modify-write that silently destroys history entries
- **Reviewer:** concurrency
- **Severity:** CRIT
- **File:** internal/corpus/store.go:179
- **Description:** `Append` (store.go:104) does `write()` — O_APPEND, atomic — then `trim()`, which
  `os.ReadFile`s the whole store, filters, and `replace()`s it via temp+rename. Nothing serialises
  the two halves. Any entry appended by another goroutine or another `talaria` process between the
  `ReadFile` at line 179 and the `Rename` at line 224 falls inside the overwritten snapshot and is
  lost. `Append` returns `nil` for those entries, so the "the call was not recorded in history"
  warning at cmd/talaria/call.go:250 never fires — the loss is completely silent. Proven with a probe
  test: 8 writers × 10 appends against a store seeded to `maxPerSource-1`, under `-race -count=3`,
  lost 57, 60 and 61 of 80 successfully-appended entries, every `Append` having returned nil. The
  entry count stays at exactly 1000, which is what hides this from a count-based test. Below the cap
  (`trim` returns early at store.go:203) 0 of 80 were lost, so the exposure is precisely the rewrite
  path. `run.go` is sequential so there is no in-process exposure today; the live exposure is two
  `talaria` processes sharing `history.jsonl` — an agent issuing parallel `call`s, or a `run`
  overlapping an interactive `call`. The cap is per-source and `run` alone can push `SourceRun` past
  1000 in one suite.
- **Suggested fix:** Take an exclusive `syscall.Flock` on the history file (or a sibling `.lock`) for
  the whole of `Append` — write *and* trim — released with `defer`. The temp+rename in `replace` is
  already correct for readers; only the unsynchronised snapshot is wrong.

## Finding 6: `run` sends its generated JSON body under the wrong `Content-Type`
- **Reviewer:** integration
- **Severity:** CRIT
- **File:** cmd/talaria/run.go:338
- **Description:** `gen.bodySchema` (internal/gen/fixtures.go:228) deliberately selects the *JSON*
  media type's schema to generate from, but that choice is never communicated onward: `run` passes
  only the bytes, and `binder.contentType` independently picks `rb.Content[0].ContentType`
  (internal/request/body.go:123). When the JSON media type is not first, the two disagree. Verified
  against a local echo server: a 3.0 spec declaring `application/xml` before `application/json`
  produced `Content-Type: application/xml` with body `{"name":"foxtrot-906"}`. It is worse on the
  Swagger 2.0 path, where it is unconditional — every converted `formData` operation gets
  `Content-Type: application/x-www-form-urlencoded` (or `multipart/form-data`) carrying a JSON body.
  talaria reported `outcome: passed` because the echo server accepts anything; a real API returns
  415/400 and the smoke test then reports a spec bug that is actually talaria's.
- **Suggested fix:** Have `gen.Data` carry the media type `bodySchema` selected, and have `run` pass
  it as an explicit `Content-Type` in `request.Inputs.Headers`. `binder.contentType` already lets a
  user-set header win, so nothing else changes.

## Finding 7: A non-UTF-8 request body is silently corrupted in the corpus and replayed corrupted
- **Reviewer:** integration
- **Severity:** CRIT
- **File:** internal/corpus/entry.go:216
- **Description:** `newBody` does `body.Data = string(data)`, and `Entry` is marshalled with
  `encoding/json`, which replaces every invalid byte with U+FFFD. `replayRequest` then does
  `Data: []byte(body.Data)` (cmd/talaria/history.go:511) with no guard — it checks `Truncated` and
  refuses, but has nothing to check for this. Verified: `talaria call … --body @bin.dat` (1024 bytes,
  `bytes(range(256))*4`) reached the server as exactly 1024 bytes; `talaria history replay N
  --allow-mutations` sent **2048 bytes**, silently, with no warning. internal/curl/config.go:192
  carries a whole temp-file path specifically so binary bodies survive exec, so one component handles
  them correctly and the next one mangles them. DESIGN.md §5 makes this store the twin's corpus, so a
  Phase 6+ twin would replay the corrupted bytes too.
- **Suggested fix:** Add an `encoding` field to `corpus.Body` and store the body base64-encoded when
  `!utf8.Valid(data)`. At minimum, set a `lossy` flag at write time and have `replayRequest` refuse it
  the way it refuses `Truncated`.

## Finding 8: The emitted/dry-run curl uses `--data-binary`, so a body starting with `@` reads a local file
- **Reviewer:** integration
- **Severity:** CRIT
- **File:** internal/curl/render.go:115
- **Description:** `Render` emits `--data-binary`, while the config document deliberately uses
  `data-raw` — internal/curl/config.go:169 carries the comment explaining exactly why: "`data` and
  `data-binary` read a value starting with `@` as a filename, and a JSON body legitimately can."
  The two paths therefore disagree for precisely that input. Verified: `--body @/tmp/atbody.txt`
  (file contents `@/etc/hostname`) really sent the literal `@/etc/hostname` on the wire, while the
  emitted command `curl -s -X POST … --data-binary '@/etc/hostname' 'http://…'`, pasted verbatim,
  sent the contents of the host's `/etc/hostname`. Two contracts break at once: DESIGN.md §3.4 says
  `--dry-run` prints "the exact curl command" and every call returns "a portable reproduction for bug
  reports" — here it is neither — and the reproduction command turns into a local-file read that
  exfiltrates to the API. Triggered by `--body @file` or `--body -` whose first byte is `@`
  (form/text/YAML payloads, webhook signatures).
- **Suggested fix:** Use `--data-raw` in `bodyArgs`, matching config.go, and add a render test with an
  `@`-leading body pinning the two paths together.

## Finding 9: An unknown command exits 1 instead of the documented usage code 2
- **Reviewer:** spec-compliance
- **Severity:** CRIT
- **File:** cmd/talaria/root.go:129
- **Description:** `exitCode` treats an unclassified error as a request failure (1). Cobra's
  "unknown command" error is never classified, so `talaria bogus` returns 1. Verified:
  `talaria bogus` → exit 1, stderr `{"schema":"talaria/v1","error":{"code":1,"message":"unknown
  command \"bogus\" for \"talaria\""}}`. DESIGN.md §4's exit table assigns 1 to "Request could not be
  completed (network, curl failure)" and 2 to "Usage error (unknown operation, missing required
  param)". Exit 1 is the code AGENT.md tells an agent is transient and worth retrying, so a typo'd
  command sends the agent into a retry loop. `history` and `list` classify their own unknown-argument
  errors as 2 correctly, so the contract is internally inconsistent as well as wrong.
- **Suggested fix:** Set `SilenceErrors`/`SilenceUsage` as already done and classify cobra's argument
  and unknown-command errors as `clierr.Usage` before they reach `exitCode` — either by wrapping in
  `root.SetFlagErrorFunc` plus a `RunE` on the root that returns `clierr.Usage`, or by mapping the
  unclassified-error default for the pre-`RunE` phase. Add a case to exitcode_test.go.

## Finding 10: `talaria version` ignores `--output` and emits no JSON envelope
- **Reviewer:** spec-compliance
- **Severity:** CRIT
- **File:** cmd/talaria/version.go:17
- **Description:** The command `fmt.Fprintf`s `talaria dev\n` unconditionally. Verified:
  `talaria version --output json` prints `talaria dev` and exits 0 — no `schema` field, no envelope,
  and the same when piped. DESIGN.md §3.1 requires "Machine-readable output everywhere: `--output
  json` on every command" and "Versioned output schema (`\"schema\": \"talaria/v1\"`) so agent prompts
  don't break"; AGENT.md:183 repeats the promise verbatim ("`--output json|pretty|tsv` on every
  command"). An agent that parses every command's stdout as the versioned envelope — which is what
  AGENT.md instructs — fails on the one command it would use to check compatibility.
- **Suggested fix:** Route `version` through `output.New(format, cmd.OutOrStdout())` with a
  `{"schema":"talaria/v1","version":"…"}` payload, keeping the bare string as the pretty rendering.
  Add a case to version_test.go asserting the JSON form.

## Finding 11: libopenapi writes structured error logs directly to `os.Stdout`, corrupting the envelope
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/spec/load.go:49
- **Description:** `libopenapi.NewDocument(data)` leaves `d.config == nil`, so `BuildV3Model` falls
  back to `datamodel.NewDocumentConfiguration()`, whose default `Logger` is
  `slog.New(slog.NewJSONHandler(os.Stdout, …))` (libopenapi@v0.38.7/datamodel/document_config.go:233).
  That writer is the real process stdout — it bypasses `cmd.OutOrStdout()`, the `talaria/v1` envelope,
  and every redaction path. Verified with a spec carrying an unresolvable remote `$ref`:
  `talaria list refspec.yaml --output json 2>/dev/null` puts **two** `{"time":…,"level":"ERROR",…}`
  documents on stdout and nothing else (exit 3). Unresolvable and remote `$ref`s are ordinary in real
  specs, so an agent parsing stdout as one JSON document breaks on them. It is also an unaudited
  third-party output channel in a tool whose entire invariant concerns what reaches stdout.
- **Suggested fix:** Use `libopenapi.NewDocumentWithConfiguration` with a configuration whose `Logger`
  writes to `io.Discard` (or to stderr, if the diagnostics are wanted). Verified while checking this:
  `AllowFileReferences` and `AllowRemoteReferences` both default to false, so a hostile spec's `$ref`
  cannot read local files or fetch remote URLs — keep it that way when passing the config.

---

## Finding 12: Killing `talaria` orphans the curl child and leaks the response capture directory
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/curl/exec.go:53
- **Description:** `cleanupCapture` (which `os.RemoveAll`s the 0700 `talaria-call-*` dir holding
  `body` and `headers`) and `cleanupConfig` (which removes `talaria-body-*` and zeroes the resolved
  credential buffer) are plain `defer`s. There is no signal handling anywhere in `cmd/talaria` and no
  context on the child. Verified: started a call against a hanging server, `kill -TERM` on the talaria
  pid — talaria died, `ps -o pid,ppid` showed the curl child reparented to PPID 1 and still running,
  and `/tmp/talaria-call-2594644524/` survived with its `body` and `headers` files, still being
  written to by the orphan. Consequences: the orphan keeps talking to the API after the operator
  killed the tool, and response headers — where `Set-Cookie` and refreshed tokens land — are left in
  TMPDIR indefinitely. `cleanupWith`'s credential zeroing (internal/curl/config.go:250) never runs
  either. Interactive Ctrl-C is partly mitigated by process-group signalling, but the temp-dir leak
  happens there too, and the pid-directed kill an agent harness issues is unmitigated. Given
  Finding 4, a hung `run` is exactly the thing a harness kills, so these accumulate.
- **Suggested fix:** `exec.CommandContext` with a context cancelled by `signal.NotifyContext(SIGINT,
  SIGTERM)` installed in cmd/talaria/root.go, plus `cmd.WaitDelay` so the child is killed and reaped;
  run both cleanup funcs from that signal path, not only from `defer`.

## Finding 13: The canary suite still skips the response-validation error path on a stale premise
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/canary/canary_test.go:424
- **Description:** The comment reads "A response-validation failure is not here because response
  validation is not built yet (plan tasks 26 and 27). Whoever adds `--fail-on-error` adds the case."
  `--fail-on-error` shipped on this same branch (cmd/talaria/call.go:213, cmd/talaria/run.go:147),
  `internal/validate` exists, and internal/e2e/e2e_test.go exercises the flag in four places. The
  comment is now false and `TestErrorPathsDoNotLeakTheCredential` still has no exit-4 stage, leaving
  DESIGN.md §5a's named leak channel "validation errors quoting the request" uncovered by the suite
  that §5a says "gates every release". The path traces clean today — `validateResponse` runs on the
  redacted `responseView` and on `curl.URL(req)` — *except* for Finding 1's query case, which this
  stage would have caught. A comment documenting a deliberate gap is fine; one documenting a gap on
  grounds that no longer hold is how a gate quietly stops covering something.
- **Suggested fix:** Add a `{name: "response validation", code: 4}` stage driving `call
  --fail-on-error` against a server returning a schema-violating body, and delete the stale comment.

## Finding 14: The missing-credential message is garbled: "no credential for security 1 scheme petKey"
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** cmd/talaria/run.go:325
- **Description:** `res.skip("no credential for security %s", pluralise(len(missing), "scheme")+" "+
  strings.Join(missing, ", "))` interpolates the count between the words "security" and "scheme",
  producing "no credential for security 1 scheme petKey". cmd/talaria/run.go:385 repeats the same
  construction for the exit-5 error. The string lands in the JSON `reason`, in the JUnit `<skipped>`
  element, and on stderr — all three of which AGENT.md instructs the agent to relay verbatim to a
  human, so the human is asked to act on a sentence that does not parse.
- **Suggested fix:** `"no credential for %s %s"` with `pluralise(n, "security scheme")` and a colon
  before the joined names, at both call sites.

## Finding 15: `history replay` skips the query-string credential warning
- **Reviewer:** integration
- **Severity:** WARN
- **File:** cmd/talaria/history.go:201
- **Description:** DESIGN.md §5a requires warning once when a credential travels in the query string,
  because it lands in the server's access log regardless of what talaria redacts. `call` and `run`
  both call `warnQueryCredentials`; `replay` re-issues the identical request and does not. Verified:
  `talaria call … getKeyed` printed the warning; `talaria history replay 1` printed nothing on stderr
  while the server logged `/keyed?api_key=qk`. An agent whose only path to a keyed endpoint is replay
  never learns the key is being logged.
- **Suggested fix:** Call `warnQueryCredentials(cmd.ErrOrStderr(), warner, req)` after `replayRequest`
  returns, with a warner constructed alongside the command as `call` does.

## Finding 16: A remote spec is cached forever with no expiry and no way to refresh
- **Reviewer:** integration
- **Severity:** WARN
- **File:** internal/spec/source.go:87
- **Description:** `loadURL` returns the cached bytes whenever the file exists. Nothing writes a
  timestamp, checks one, sends a conditional request, or exposes a `--refresh`/`--no-cache` flag.
  README.md:48 states the behaviour ("fetched once per URL") but neither it nor AGENT.md says how to
  invalidate it. Once an API publishes a changed `openapi.json`, every `list`/`describe`/`call`/`run`
  against that URL keeps binding to the stale operation model — new operations invisible, removed ones
  still callable, response contracts stale — until someone finds and deletes a SHA-256-named file
  under `$XDG_CACHE_HOME/talaria/specs`. That is precisely the failure mode the spec-centric pitch
  ("nothing to curate, nothing to drift", §2) exists to prevent, and an agent has no way to detect or
  fix it.
- **Suggested fix:** Add an mtime-based TTL (e.g. 1h) plus a `--refresh` flag on the root command that
  bypasses the cache read, and document both in README.md and AGENT.md.

## Finding 17: `run` on its own never exits 4, contradicting the DESIGN.md exit table
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** cmd/talaria/run.go:388
- **Description:** `verdict` returns `clierr.Validation` only when `failOnError` is set, so a bare
  `talaria run` over an API returning undocumented statuses or schema-violating bodies exits 0.
  DESIGN.md §4 assigns code 4 to "Validation failure (response violates spec) — only with
  `--fail-on-error` / `run`", which reads as `run` exiting 4 in its own right. README.md:418 documents
  the implemented behaviour instead ("Failing operations exit 0 unless you pass `--fail-on-error`,
  which makes them exit 4") while README.md's exit-code table repeats the DESIGN.md wording. This is
  recorded as WARN rather than CRIT because the DESIGN.md phrasing is genuinely ambiguous — it can be
  read as "in the context of the `run` command" — and the implemented behaviour is coherent and
  consistently documented in README prose. It still needs a decision rather than a drift.
- **Suggested fix:** Pick one. Either make `run` exit 4 on validation failure regardless of
  `--fail-on-error`, or amend DESIGN.md §4's exit table and README.md's table to say code 4 requires
  `--fail-on-error` in both `call` and `run`.

## Finding 18: `talaria auth` with no subcommand prints help to stdout and exits 0
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** cmd/talaria/auth.go:31
- **Description:** The `auth` parent command has no `RunE` and no `Args` constraint, so cobra's
  default help handler writes usage text to stdout and returns nil. An agent that mistypes `talaria
  auth` instead of `talaria auth check` receives non-JSON on stdout and a success code, so it has no
  signal that anything went wrong — the same class of contract break as Finding 10, in a command whose
  whole purpose is telling an agent whether credentials are in place.
- **Suggested fix:** Give the parent a `RunE` returning `clierr.Usage` naming the valid subcommands, or
  set `Args: cobra.NoArgs` with `RunE` that errors. Both put the message in the envelope on stderr with
  exit 2.

---

## Finding 19: The `authorization` redaction pattern is exact-match, missing common spellings
- **Reviewer:** security
- **Severity:** INFO
- **File:** internal/secret/redact.go:20
- **Description:** `compileGlob` anchors a pattern containing no `*` to the whole header name, so
  `authorization` matches only that exact header. Verified: `--header 'X-Authorization=Bearer SEKRIT1'
  --header 'X-Auth=SEKRIT2' --header 'Authentication=SEKRIT3'` all render unredacted in the `headers`
  block. This matches DESIGN.md §5a's list literally, so it is a faithful implementation of the design
  rather than a bug, and `redact.headers` in the config file is the sanctioned escape hatch — hence
  INFO. But the built-in list is explicitly the non-configurable floor, and `*auth*` would subsume
  `authorization`, `proxy-authorization`, `x-authorization`, `x-auth-token` and `authentication` at the
  cost of a few harmless false positives.
- **Suggested fix:** Replace `authorization` and `proxy-authorization` with a single `*auth*` pattern,
  and note the widening in the DESIGN.md §5a table so doc and code stay in step.

## Finding 20: `exclusiveMaximum` is carried by the strict-3.0 fixture but never asserted
- **Reviewer:** spec-compliance
- **Severity:** INFO
- **File:** internal/validate/validate_test.go:277
- **Description:** The DESIGN.md edit on this branch claims the strict-3.0 fixture is "pinned … so a
  library upgrade that re-imposed 3.1 semantics would fail the suite", naming four constructs. Three
  are asserted; `exclusiveMaximum` is present in internal/validate/testdata/strict-3.0.yaml but no test
  drives a value above the bound. I confirmed by hand that the library does translate it correctly
  (`id: 100` is rejected), so the claim is true — it is just not held true by the suite.
- **Suggested fix:** Add an `id: 100` case to the exclusive-bound test so all four constructs named in
  DESIGN.md §8 are actually pinned.
