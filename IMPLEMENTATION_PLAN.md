# talaria — Review Fix Plan

Source of truth: [docs/design/DESIGN.md](docs/design/DESIGN.md) (v0.3). Stack: `.ralph/stack.json`.
Findings: [REVIEW_FINDINGS.md](REVIEW_FINDINGS.md) — do not edit that file.

## Scope

This plan replaces the Phase 1–4 build plan, which is complete (tasks 1–33 all `done`). It
covers **only the 11 CRIT findings** from the review of `ralph/design` vs `main`. The 7 WARN and
2 INFO findings are deliberately **not** planned: they are recorded for a human and do not block
the PR. Do not create work for them, and do not "fix them while you are in the file" — an
unrequested change in a reviewed diff costs another review cycle.

Still out of scope, unchanged from the build plan: Phases 5–8 (`internal/twin`, the recording
proxy, `talaria twin …`), distribution and packaging, request chaining, and everything in
DESIGN.md §9 Non-goals.

## Commands (from `.ralph/stack.json` — do not invent others)

| Purpose | Command |
|---|---|
| All tests | `go test ./...` |
| One package | `go test ./<dir>/...` (expansion of `test_single_command`) |
| Build | `go build ./...` |
| Lint | `test -z "$(gofmt -l .)" && go vet ./...` |

Run the build and lint before every commit. `golangci-lint` is **not** installed. The tracked
tree passes `go build ./...`, `gofmt -l .`, `go vet ./...` and `go test -race ./...` today, so
any failure after a task is that task's doing.

## Rules for these tasks

- Every task adds or extends a test that **fails before the fix and passes after**. A CRIT
  finding that ships with no regression test comes back.
- Redaction changes go through the canary suite (`internal/canary`), which DESIGN.md §5a calls
  the release gate — extend it rather than only asserting in a unit test.
- Keep the existing comment style: these files explain *why*, not *what*. A fix that lands with
  no explanation of the invariant it restores is half the change.

## Task order

Tasks 1–2 are the credential-firewall breaches (§5a) and come first. Tasks 3–4 are the curl
execution contract. Tasks 5–7 are data integrity. Tasks 8–9 are the CLI/output contract. There
are no dependencies between tasks; the order is by severity of consequence.

---

### Task 1: Redact credentials passed in the query string

**Fixes findings:** #1

**Files:**
- `internal/request/build.go` (modify)
- `internal/request/request.go` (modify)
- `internal/curl/render.go` (modify)
- `internal/canary/canary_test.go` (modify)
- `internal/request/build_test.go` (modify)

**Steps:**
1. In `Build` (build.go:76), wrap the query pairs in `hide()` exactly as headers and cookies
   already are:
   `req.Query = hide(append(b.located(bound, inQuery), b.pairs(in.Query, "--query")...))`.
   Query is the one credential location §5a names ("Query-string API keys (`?api_key=`)") that
   the built-in name matcher was never applied to.
2. In `urlWord` (render.go:139), branch on `q.Value.IsSensitive()` rather than
   `q.Value.IsSecret()`, so a hidden literal renders `<redacted>` instead of being
   percent-encoded into `%3Credacted%3E`. This path is display-only — `render` is `Symbolic` or
   `String`, never a resolving renderer — so it is safe to skip `url.QueryEscape` there.
3. In `Request.QueryString` (request.go:238), **do not** simply branch on `IsSensitive`: this
   function is shared with the wire path (`req.URL(resolve)` at curl/config.go:71), and skipping
   `url.QueryEscape` for a resolved credential would put unescaped bytes in the request URL.
   Skip the escape only when the rendered text is a placeholder rather than a value — i.e. when
   `p.Value.IsSensitive() && (value == p.Value.String() || value == p.Value.Symbolic())` —
   mirroring the comparison `word.credential` (render.go:201) already makes. Comment why.
4. Add a `--query` case to `TestAUserSuppliedHeaderIsRedactedLikeASpecCredential` in
   `internal/canary/canary_test.go`: pass the canary as `--query api_key=<canary>` and assert it
   appears on no surface the suite greps (json, pretty, tsv, dry-run, errors, history).
5. Add a builder test asserting `--query api_key=x` produces a `Pair` whose `Value.IsSensitive()`
   is true and whose `String()` is the placeholder, and a render test pinning the `?api_key=`
   form of both `curl.Render` and `curl.URL`.

**Verify:** `go test ./internal/request/... ./internal/curl/... ./internal/canary/...`
- [ ] `talaria call spec.yaml getPub --query api_key=SECRET --output json --dry-run` shows
      `<redacted>` (not `SECRET`, not `%3Credacted%3E`) in both the `curl` and `url` fields.
- [ ] A real call writes no query credential into `$XDG_STATE_HOME/talaria/history.jsonl`.
- [ ] The wire form is unchanged: the resolved value still reaches the server percent-encoded.

---

### Task 2: Restrict base URLs and curl to http and https

**Fixes findings:** #2

**Files:**
- `internal/request/build.go` (modify)
- `internal/curl/config.go` (modify)
- `cmd/talaria/history.go` (modify)
- `internal/request/build_test.go` (modify)
- `internal/curl/config_test.go` (modify)

**Steps:**
1. In `binder.baseURL` (build.go:141), reject any scheme other than `http` and `https`, making
   the check match the error message it already prints ("is not an absolute http(s) URL").
   Compare case-insensitively. This applies to all three candidate sources — `--base-url`, the
   profile, and `firstServer(b.in.Doc)` — because a spec's `servers[0].url` is untrusted input:
   the product's premise is that an agent points talaria at any doc it found.
2. In `document.build` (config.go), emit `proto = "=http,https"` and `proto-redir = "=http,https"`
   as defence in depth, so a scheme that slips past step 1 or arrives via a redirect still cannot
   make curl speak gopher, file, dict or smb.
3. In `replayRequest` (history.go:494), apply the same scheme check to the URL rebuilt from the
   stored entry, returning `clierr.Usage` naming the offending scheme.
4. Test: a spec whose `servers[0].url` is `gopher://127.0.0.1:1234` fails to build with a usage
   error; `--base-url file:///etc/passwd` likewise; `http` and `https` still build. Assert the
   two `proto` directives are present in the built config document.

**Verify:** `go test ./internal/request/... ./internal/curl/... ./cmd/talaria/...`
- [ ] `talaria call spec.yaml getPub --base-url gopher://127.0.0.1:18201 --dry-run` exits 2 with
      a usage error instead of emitting a gopher command.
- [ ] A spec declaring a `gopher://` server cannot deliver a payload to a raw TCP listener.

---

### Task 3: Emit correct curl for HEAD, and make the rendered body flag match the wire

**Fixes findings:** #3, #8

**Files:**
- `internal/curl/config.go` (modify)
- `internal/curl/render.go` (modify)
- `internal/curl/config_test.go` (modify)
- `internal/curl/render_test.go` (modify)

**Steps:**
1. In `document.build` (config.go:77), when `req.Method == "HEAD"` emit the bare `head` flag
   instead of `request = "HEAD"`. `-X HEAD` makes curl wait for a `Content-Length` body a
   compliant server never sends, so the call hangs (or exits 18 against HTTP/1.0), and because
   `HEAD` is in `safeMethods` a single HEAD operation makes `talaria run` never terminate.
2. With `head`, curl writes the header block to the output file, which would land in the response
   *body*. For `HEAD`, point the `output` directive at `os.DevNull` instead of
   `capture.BodyPath`; `dump-header` still captures the headers, and the staged (empty) body file
   keeps `readResponse` unchanged.
3. In `Render` (render.go:35), render `HEAD` as `-I` rather than `-X HEAD`, so the emitted
   command reproduces the call instead of hanging when pasted.
4. In `bodyArgs` (render.go:115), use `--data-raw` instead of `--data-binary`, matching the
   `data-raw` the config document deliberately uses (config.go:169 carries the reason: `data` and
   `data-binary` read a leading `@` as a filename, and a body legitimately can start with one).
   Without this, a `--body @file` whose contents start with `@` sends the literal text on the
   wire while the emitted command reads a local file and exfiltrates it.
5. Tests: a HEAD operation builds a config with `head` and no `request = "HEAD"`, renders `-I`,
   and — as an executor test against a local server answering HEAD with `Content-Length` and no
   body — completes rather than hanging. A render test with a body of `@/etc/hostname` pins
   `--data-raw` and pins the two paths together.

**Verify:** `go test ./internal/curl/...`
- [ ] `talaria call headspec.yaml headThing` against a server that answers HEAD returns promptly
      with a status instead of exiting 124/18.
- [ ] The emitted command for a body starting with `@` sends that text as data when pasted.

---

### Task 4: Bound the curl subprocess with connect and total timeouts

**Fixes findings:** #4

**Files:**
- `internal/curl/config.go` (modify)
- `internal/curl/exec.go` (modify)
- `cmd/talaria/call.go` (modify)
- `cmd/talaria/run.go` (modify)
- `internal/curl/exec_test.go` (modify)

**Steps:**
1. Add an exported options type to `internal/curl` carrying `ConnectTimeout` and `MaxTime`
   (`time.Duration`), with defaults (10s connect, 30s total). Thread it through `BuildConfig` and
   `Execute` by adding an `ExecuteWith(req, opts)` / `BuildConfigWith(req, capture, opts)` pair
   and keeping `Execute(req)` / `BuildConfig(req, capture)` as the default-options wrappers, so
   existing call sites and tests keep compiling.
2. In `document.build`, emit `connect-timeout` and `max-time` directives from those options.
   curl enforces them itself and exits 28, which `runFailure` already classifies as a request
   failure (exit 1), so the happy path needs no Go-side plumbing.
3. In `Execute`, use `exec.CommandContext` with a context whose deadline is `MaxTime` plus a
   small margin, and set `cmd.WaitDelay`, so a curl that ignores its own timeout is still killed
   and reaped rather than leaving talaria blocked forever.
4. Add a `--timeout` flag (seconds, total) to `call` and `run`, defaulting to the package
   default, and pass it through. `run` is the reason this is CRIT: without a bound, the suite
   stalls on operation *k* of *n* and emits no report at all, so an agent or CI gets nothing.
5. Test against a local server that accepts the connection and never responds: `ExecuteWith` with
   a short `MaxTime` returns a request-failure error within the timeout instead of blocking, and
   the built document contains both directives.

**Verify:** `go test ./internal/curl/... ./cmd/talaria/...`
- [ ] A call against a never-answering server fails with exit 1 inside the timeout.
- [ ] `talaria run` over a spec containing such an operation still emits its report.

---

### Task 5: Serialise history appends so trim cannot destroy entries

**Fixes findings:** #5

**Files:**
- `internal/corpus/store.go` (modify)
- `internal/corpus/lock_unix.go` (create)
- `internal/corpus/lock_other.go` (create)
- `internal/corpus/store_test.go` (modify)

**Steps:**
1. `Append` (store.go:104) does an atomic O_APPEND `write` and then `trim`, which reads the whole
   file, filters, and replaces it. Nothing serialises the two, so any entry appended between
   `trim`'s `os.ReadFile` (store.go:179) and its `Rename` (store.go:224) is silently dropped —
   `Append` returned nil for it, so the "not recorded in history" warning never fires.
2. Take an exclusive advisory lock for the whole of `Append` — write *and* trim — released with
   `defer`. Lock a sibling `history.jsonl.lock` file rather than the store itself, so the lock
   survives `replace`'s rename. Keep `Read` lock-free: `replace`'s temp+rename is already correct
   for readers.
3. Put the lock behind two small files so the package still builds everywhere:
   `lock_unix.go` (`//go:build unix`) using `syscall.Flock` with `LOCK_EX`, and `lock_other.go`
   (`//go:build !unix`) whose acquire is a no-op. Do not add a new module dependency.
4. Regression test: seed a store to `maxPerSource-1` entries for one source, then run N
   goroutines × M `Append`s, and assert every entry that returned nil is present afterwards.
   Run it under `-race`. Below the cap the trim path returns early (store.go:203), so the test
   must seed *over* the cap boundary to exercise the rewrite.

**Verify:** `go test -race -count=3 ./internal/corpus/...`
- [ ] The concurrency test loses zero entries where the pre-fix code lost ~60 of 80.
- [ ] Two `talaria` processes appending to the same `history.jsonl` both keep their entries.

---

### Task 6: Send `run`'s generated body under the media type it was generated from

**Fixes findings:** #6

**Files:**
- `internal/gen/fixtures.go` (modify)
- `cmd/talaria/run.go` (modify)
- `internal/gen/fixtures_test.go` (modify)
- `cmd/talaria/run_test.go` (modify)

**Steps:**
1. `bodySchema` (fixtures.go:223) deliberately picks the *JSON* media type to generate from, but
   that choice is never communicated onward: `run` passes only the bytes and
   `binder.contentType` (request/body.go:123) independently picks `rb.Content[0].ContentType`.
   When JSON is not first they disagree — and on the Swagger 2.0 path every converted `formData`
   operation sends a JSON body under `application/x-www-form-urlencoded`.
2. Have `bodySchema` return the media type alongside the schema, and add a `ContentType` field to
   `gen.Data` recording the media type the body was actually generated for. When the body came
   from a fixture and the operation declares no content, use `application/json` — `compact`
   already treats a fixture body as JSON.
3. In `runner.execute` (run.go:329), when `data.Body` is non-nil and `data.ContentType` is
   non-empty, add `Content-Type: <media type>` to the headers passed as `request.Inputs.Headers`
   — unless a fixture header already set one. `binder.contentType` already lets a user-set header
   win, so nothing else changes.
4. Tests: a 3.0 spec declaring `application/xml` before `application/json` produces a request
   with `Content-Type: application/json`; a converted Swagger 2.0 `formData` operation likewise;
   a fixture-supplied `Content-Type` still wins.

**Verify:** `go test ./internal/gen/... ./cmd/talaria/...`
- [ ] The request `run` builds carries a `Content-Type` matching the bytes in its body.
- [ ] A smoke run against a strict server no longer reports a spec bug that is talaria's.

---

### Task 7: Preserve non-UTF-8 request bodies in the corpus, and refuse lossy replays

**Fixes findings:** #7

**Files:**
- `internal/corpus/entry.go` (modify)
- `cmd/talaria/history.go` (modify)
- `internal/corpus/entry_test.go` (modify)
- `cmd/talaria/history_test.go` (modify)

**Steps:**
1. `newBody` (entry.go:206) does `body.Data = string(data)` and the entry is marshalled with
   `encoding/json`, which replaces every invalid byte with U+FFFD. `replayRequest`
   (history.go:511) then does `Data: []byte(body.Data)` with no guard, so a 1024-byte binary body
   replays as 2048 bytes, silently.
2. Add an `Encoding string \`json:"encoding,omitempty"\`` field to `corpus.Body`. In `newBody`,
   when `!utf8.Valid(data)` after truncation, store `base64.StdEncoding.EncodeToString(data)` in
   `Data` and set `Encoding` to `"base64"`. Leave valid UTF-8 exactly as it is today, so existing
   stores and their tests are unaffected and the common case stays readable.
3. In `replayRequest`, decode `"base64"` back to bytes before building `request.Body`. Refuse any
   `Encoding` value it does not recognise with a `clierr.Usage` error, the way it already refuses
   `Truncated` — a store written by a newer talaria must not be replayed as literal text.
4. Check every other reader of `Body.Data` (`history show`, and any display path) and render a
   base64 body without pretending it is text.
5. Tests: a body of `bytes(range(256))*4` round-trips byte-for-byte through
   `Entry` → JSON → `replayRequest`; an unknown `encoding` is refused; a UTF-8 body's stored form
   is unchanged from today.

**Verify:** `go test ./internal/corpus/... ./cmd/talaria/...`
- [ ] `talaria history replay N` sends exactly the bytes the original call sent.
- [ ] The corpus the twin will read (DESIGN.md §5) holds the real bytes, not U+FFFD.

---

### Task 8: Restore the CLI contract — usage exit code for unknown commands, JSON envelope for `version`

**Fixes findings:** #9, #10

**Files:**
- `cmd/talaria/root.go` (modify)
- `cmd/talaria/version.go` (modify)
- `cmd/talaria/exitcode_test.go` (modify)
- `cmd/talaria/version_test.go` (modify)

**Steps:**
1. `talaria bogus` exits 1, because cobra's "unknown command" error is never classified and
   `exitCode` treats an unclassified error as a request failure. DESIGN.md §4 assigns 2 to usage
   errors, and exit 1 is the code AGENT.md tells an agent is transient and worth retrying — so a
   typo sends the agent into a retry loop. Fix by setting `Args: usageArgs(cobra.NoArgs)` on the
   root command: `cobra.NoArgs` produces exactly the `unknown command %q for %q` message, and
   `usageArgs` already wraps it as `clierr.Usage`. Bare `talaria` still prints help and exits 0,
   because `ValidateArgs` passes on an empty argument list and cobra then returns `flag.ErrHelp`.
2. `version` (version.go:17) `fmt.Fprintf`s `talaria dev` unconditionally, ignoring `--output`.
   DESIGN.md §3.1 and AGENT.md:183 both promise `--output json` on *every* command with the
   `talaria/v1` envelope — and this is the one command an agent uses to check compatibility.
   Route it through `resolveFormat(cmd)` + `output.New(format, cmd.OutOrStdout()).Render(...)`
   with a payload of `{"version": version}`, exactly as `list` does. Keep the bare
   `talaria <version>` string as the pretty rendering, and give the payload a `Table` so `tsv`
   renders too.
3. Tests: `talaria bogus` exits 2 and its stderr envelope carries `"code":2`; `talaria version
   --output json` emits one object with `"schema":"talaria/v1"` and a `version` field;
   `--output pretty` is unchanged from today.

**Verify:** `go test ./cmd/talaria/...`
- [ ] `talaria bogus` → exit 2. `talaria` with no arguments → help, exit 0.
- [ ] Every command's stdout under `--output json` parses as the versioned envelope.

---

### Task 9: Stop libopenapi writing structured logs to stdout

**Fixes findings:** #11

**Files:**
- `internal/spec/load.go` (modify)
- `internal/spec/load_test.go` (modify)

**Steps:**
1. `libopenapi.NewDocument(data)` (load.go:49) leaves the document config nil, so `BuildV3Model`
   falls back to a default configuration whose logger is a JSON `slog` handler writing to the
   real `os.Stdout`. That bypasses `cmd.OutOrStdout()`, the `talaria/v1` envelope and every
   redaction path — a spec with an unresolvable `$ref` puts two `{"time":…,"level":"ERROR"}`
   documents on stdout and nothing else.
2. Switch to `libopenapi.NewDocumentWithConfiguration(data, cfg)` where `cfg` is a
   `datamodel.NewDocumentConfiguration()` with its `Logger` set to
   `slog.New(slog.NewJSONHandler(io.Discard, nil))`. stdout belongs to the envelope; diagnostics
   that must be kept go to stderr, not there.
3. Leave `AllowFileReferences` and `AllowRemoteReferences` at their `false` defaults when passing
   the configuration — that default is what stops a hostile spec's `$ref` reading local files or
   fetching URLs, and constructing the config explicitly is exactly where it could be lost.
4. Test: loading a spec with an unresolvable remote `$ref` writes nothing to the process's
   stdout. Capture `os.Stdout` around the call (swap in an `os.Pipe`) so the assertion covers the
   real file descriptor rather than a cobra writer, since that is the channel that leaked.

**Verify:** `go test ./internal/spec/... ./cmd/talaria/...`
- [ ] `talaria list refspec.yaml --output json 2>/dev/null` emits exactly one JSON document.
- [ ] File and remote `$ref` resolution is still disabled.
