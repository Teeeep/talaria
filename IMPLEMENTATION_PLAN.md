# talaria — Review Fix Plan

Source of truth: [docs/design/DESIGN.md](docs/design/DESIGN.md) (v0.3). Stack: `.ralph/stack.json`.
Findings: [REVIEW_FINDINGS.md](REVIEW_FINDINGS.md) — do not edit that file.

## Scope

This plan replaces the previous review-fix plan, whose 9 tasks are all landed. It covers **only
the 9 CRIT findings** (1–9) from the current review of `ralph/design` vs `main`. The 16 WARN and
10 INFO findings are deliberately **not** planned: they are recorded for a human and do not block
the PR. Do not create work for them, and do not "fix them while you are in the file" — an
unrequested change in a reviewed diff costs another review cycle.

Still out of scope, unchanged: Phases 5–8 (`internal/twin`, the recording proxy, `talaria twin …`),
distribution and packaging, request chaining, and everything in DESIGN.md §9 Non-goals.

Six of the nine findings are credential leaks (1, 2, 3, 4, 5, and the capture-directory half of
6), which is the surface DESIGN.md §5a calls the product. In every one of them the existing canary
suite passed while the secret escaped, so each of those tasks ends with a canary case rather than
only a unit assertion.

## Commands (from `.ralph/stack.json` — do not invent others)

| Purpose | Command |
|---|---|
| All tests | `go test ./...` |
| One package | `go test ./<dir>/...` (expansion of `test_single_command`) |
| Build | `go build ./...` |
| Lint | `test -z "$(gofmt -l .)" && go vet ./...` |

Run the build and lint before every commit. `golangci-lint` is **not** installed. The tracked tree
passes `go build ./...`, `gofmt -l .`, `go vet ./...` and `go test -race ./...` today, so any
failure after a task is that task's doing.

## Rules for these tasks

- Every task adds or extends a test that **fails before the fix and passes after**. A CRIT
  finding that ships with no regression test comes back.
- Redaction changes go through the canary suite (`internal/canary`), which DESIGN.md §5a calls
  the release gate — extend it rather than only asserting in a unit test.
- Keep the existing comment style: these files explain *why*, not *what*. A fix that lands with
  no explanation of the invariant it restores is half the change.

## Task order

Tasks 1–5 are the credential-firewall breaches (§5a) and come first. Task 6 is the curl execution
contract. Task 7 is data integrity. Tasks 8–9 are the CLI/output contract. There are no
dependencies between tasks; the order is by severity of consequence. Tasks 2 and 9 both come from
finding #9 and are split by file — Task 2 owns the `binder.pairs` parser change, Task 9 owns the
docs — so run Task 2 first if you run them in the same session.

---

### Task 1: Stop curl reading `~/.curlrc` before the credential-bearing config

**Fixes findings:** #1

`argv` is `{"curl", "-K", "-"}`, so curl parses `$CURL_HOME/.curlrc` (else `$HOME/.curlrc`)
*before* the `-K -` document carrying the resolved secret. A `trace-ascii` or `proxy` directive
there captures the plaintext `Authorization` header. Writing one file under `$HOME` is a weaker
capability than the env-var read §5a concedes, so this is inside the claimed boundary.

**Files:**
- `internal/curl/config.go` (modify)
- `internal/curl/render.go` (modify)
- `internal/canary/canary_test.go` (modify)

**Steps:**
1. In `BuildConfigWith` (config.go:103), change `argv` to `{"curl", "-q", "-K", "-"}`. Both the
   early-return argv and the success argv come from this one variable, so the single edit covers
   the `req == nil` and `doc.build` error paths too. `-q` must be the first parameter — curl
   ignores it elsewhere.
2. Add a comment at that line recording *why* `-q` is load-bearing: without it a writable
   `.curlrc` reads the credential out of the `-K` document.
3. Mirror the flag in the rendered curl (`internal/curl/render.go`) so the copy-pasteable
   reproduction behaves the same as the call talaria made.
4. Add a canary stage in `canary_test.go` that writes a `.curlrc` containing
   `trace-ascii = <path under the harness temp dir>` into the harness `HOME` (already set at
   canary_test.go:188), runs a call carrying the canary, and asserts the trace file does not
   exist — and, if it does, that it does not contain the canary.

**Verify:** `go test ./internal/curl/... ./internal/canary/...`
- [x] With a `.curlrc` containing `trace-ascii` planted in `HOME`, a call carrying the canary
      writes no trace file and leaks nothing.
      (`TestAPlantedCurlrcCannotCaptureTheCredential`; it reproduced the leak before the fix.)
- [x] `BuildConfigWith` returns argv beginning `curl -q -K -` on the success path and on both
      error paths. (`TestBuildConfigDisablesTheDefaultCurlrcOnEveryPath`.)

The rendered curl now starts `curl -q -s`, so the `curl -s` examples in `README.md`, `AGENT.md`
and the `call` output sketch in DESIGN.md §"Output shape" were updated to match.

---

### Task 2: Never echo a rejected flag's value, and reject malformed header names

**Fixes findings:** #2, #9 (the silent-corruption half)

`b.fail("%s %q is not name=value", flag, raw)` puts the whole rejected argument into the exit-2
JSON on stderr, so `--header "Authorization: Bearer $TOKEN"` — the form AGENT.md currently
documents — prints the real token. The `--body` variant joins *every* body value into one
message, and request bodies routinely carry `client_secret`/`password`. Separately,
`--header 'X-Trace: abc=1'` is *accepted*, cut at the first `=`, and sends the malformed wire
header `X-Trace: abc: 1`.

**Files:**
- `internal/request/build.go` (modify)
- `internal/request/build_test.go` (modify)
- `internal/canary/canary_test.go` (modify)

**Steps:**
1. In `binder.pairs` (build.go:311), report the position and the name half only, never the value:
   `%s %d is not name=value` with the 1-based index, plus the text before the first `=` or `:`
   when there is one. Elide the value entirely — do not include a truncated prefix of it.
2. In the same function, reject a name containing `:`, whitespace, or any other character not
   valid in an HTTP field name, with a message naming the offending *name* only. This closes
   finding #9's silent corruption: `X-Trace: abc=1` becomes a clean exit 2 instead of a malformed
   header. Accepting the curl-style `Name: value` form is deliberately **not** done here —
   DESIGN.md §4 specifies `name=value`, and Task 9 fixes the docs to match.
3. In the `--body` failure path (the flag declared at build.go:53), report the count and the
   *kind* of each value (literal / `@file` / `-`), never the bytes.
4. Update any existing test asserting the old message text.
5. Add a canary stage: a malformed `--header` and a malformed `--body` each carrying the canary,
   scanned across stdout, stderr, every `--output` format, the report and the history store.

**Verify:** `go test ./internal/request/... ./internal/canary/...`
- [ ] `--header 'Authorization: Bearer <canary>'` exits 2 with the canary absent from every
      output surface.
- [ ] Repeated `--body` with two secret-bearing values reports two bodies by kind, no bytes.
- [ ] `--header 'X-Trace: abc=1'` exits 2 rather than sending `X-Trace: abc: 1`.

---

### Task 3: Reject credentials in a base URL's userinfo

**Fixes findings:** #3

`baseURL` validates scheme and host but keeps `parsed.User`, so `http://user:pass@host` from
`--base-url`, a profile, **or a spec's `servers[0].url` (untrusted input)** reaches `request.url`,
the emitted `request.curl` (render.go:146), and `history.jsonl` (entry.go:154) in cleartext — a
permanent artifact with no way to get the value back out short of deleting the store.

**Files:**
- `internal/request/build.go` (modify)
- `cmd/talaria/history.go` (modify)
- `internal/request/build_test.go` (modify)
- `internal/canary/canary_test.go` (modify)

**Steps:**
1. In `baseURL` (build.go:144), after the scheme/host check, reject `parsed.User != nil` via
   `b.fail`, naming the source (`--base-url`, the profile, or the spec's `servers[0].url`) and
   pointing at `TALARIA_AUTH_BASIC` as the supported path. Rejecting rather than stripping is the
   right call: talaria already has a basic-auth path that keeps the value symbolic, and silently
   dropping the userinfo would send an unauthenticated request the caller thinks is
   authenticated. The message must not echo the userinfo — report the host only.
2. Apply the same check in `replayRequest` (history.go:511), which re-parses a URL read off disk,
   alongside the existing `IsHTTPScheme` guard and for the reason its comment already states.
3. Add unit cases for all three sources plus the replay path.
4. Add a canary stage passing `--base-url http://user:<canary>@host` and scanning every surface.

**Verify:** `go test ./internal/request/... ./cmd/talaria/... ./internal/canary/...`
- [ ] `--base-url 'http://admin:s3cr3t@127.0.0.1:8898'` exits 2, and `s3cr3t` appears in no
      output and in no freshly-created `history.jsonl`.
- [ ] A spec whose `servers[0].url` carries userinfo is rejected the same way.

---

### Task 4: Apply `redact.headers` to the displayed request and the emitted curl

**Fixes findings:** #4

`hide()` hard-codes `var builtin *secret.Redactor` (nil = built-ins only) and `cfg.Redact.Headers`
only ever reaches `corpus.Redactors` (call.go:239). The result is inverted against DESIGN.md:339
and README.md:209: the *history entry* stores `<redacted>` while stdout's `request.headers` and
`request.curl` print the value in the clear — the permanent artifact is protected and the surface
the agent reads is not. No test at any level covers `redact.headers`.

**Files:**
- `internal/request/build.go` (modify)
- `cmd/talaria/call.go` (modify)
- `cmd/talaria/run.go` (modify)
- `internal/request/build_test.go` (modify)
- `internal/canary/canary_test.go` (modify)

**Steps:**
1. Add a `Redactor *secret.Redactor` field to `request.Inputs`, documented as the config file's
   user-extensible display patterns layered over the non-configurable built-in floor.
2. Change `hide()` (build.go:289) to take that `*secret.Redactor` and use it in place of the
   local `var builtin`. A nil value must keep meaning built-ins only, so every existing caller and
   test is unaffected — `secret.Redactor.IsSensitive` already has a nil receiver path.
3. Thread it at every `request.Build` call site (`cmd/talaria/call.go`, `cmd/talaria/run.go`, and
   any other) from `secret.NewRedactor(cfg.Redact.Headers...)`, the same construction
   `newRedactors` uses at call.go:239.
4. Confirm the query and cookie paths pick it up too — `hide` is shared by all three at
   build.go:78-80, which is what README.md:209 promises.
5. Add a canary stage that configures `redact.headers` with a glob and greps stdout, `--dry-run`
   and pretty output for the value.

**Verify:** `go test ./internal/request/... ./cmd/talaria/... ./internal/canary/...`
- [ ] With `redact: {headers: ["x-session-*"]}`, `X-Session-Id` renders `<redacted>` in
      `request.headers`, in `request.curl`, in pretty output and in `--dry-run`.
- [ ] Built-in-only behaviour is unchanged when the config sets no extra patterns.

---

### Task 5: Redact response bodies that are arrays or contain arrays

**Fixes findings:** #5

`decodeObject` rejects any body not starting with `{`, and `redactPath` only descends into
`map[string]any`. A top-level JSON array and an array nested under an object — two very common
shapes — are returned unchanged, so the built-in `access_token`/`refresh_token`/`id_token` paths
and any configured `redact.body-paths` silently do nothing and, because §5a mandates redaction at
write time, the value lands permanently in `history.jsonl`. `TestBodySkipsNonJSONBodies`
currently **asserts the leak as correct behaviour**.

**Files:**
- `internal/secret/response.go` (modify)
- `internal/secret/response_test.go` (modify)
- `internal/canary/canary_test.go` (modify)

**Steps:**
1. Rename/repurpose `decodeObject` (response.go:155) to decode into `any`, accepting a leading
   `[` as well as `{`. Keep both existing guards exactly as they are: `dec.UseNumber()` (so a
   large id is not rewritten through float64) and the trailing-token `io.EOF` check (so a body
   ending in junk is not re-encoded).
2. Change `redactPath` (response.go:190) to take `any` and fan out across `[]any` at each
   segment: applying `items.access_token` to every element of `items`, and a top-level
   `access_token` to every element of a top-level array. Report `true` if any element matched.
3. Update the encode side to round-trip a non-object root.
4. Retarget `TestBodySkipsNonJSONBodies` (response_test.go:127): `[{"access_token":"x"}]` moves
   out of the skip list into a new case asserting it *is* redacted; `"not json at all"` stays.
5. Add both array shapes to the canary suite so the store is gated on them.

**Verify:** `go test ./internal/secret/... ./internal/canary/...`
- [ ] `[{"access_token":"TOPSECRET-ARR"}]` and `{"items":[{"access_token":"TOPSECRET-NEST"}]}`
      are both redacted on stdout and in `history.jsonl`.
- [ ] Object bodies, non-JSON bodies and large integer ids behave exactly as before.

---

### Task 6: Handle signals so a killed talaria does not orphan curl or leak the raw capture

**Fixes findings:** #6

Nothing installs a signal handler and all cleanup is `defer`-based, so an unhandled signal runs
none of it. Confirmed: curl is reparented to `ppid=1` and the request **completes 8s after talaria
is dead** (so Ctrl-C on an `--allow-mutations` POST does not cancel the write, and `recordCall`
never runs, so the call is absent from history); and the capture directory survives holding the
*raw, unredacted* response, along with the `talaria-body-*` request-body temp file
(config.go:291).

**Files:**
- `cmd/talaria/root.go` (modify)
- `internal/curl/exec.go` (modify)
- `internal/curl/exec_test.go` (modify)

**Steps:**
1. In `cmd/talaria/root.go`, wrap execution in
   `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` and thread that
   context down to `curl.ExecuteWith`.
2. In `exec.go:77`, derive the timeout context from the passed-in ctx instead of
   `context.Background()`, so both cancellation sources kill curl through the existing
   `CommandContext` and every `defer` (capture cleanup, config cleanup) runs normally.
3. Set `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` with a `cmd.Cancel` that signals
   `-pgid`, so anything curl spawned dies with it. This is unix-only — put it behind the same
   build-tag split `internal/corpus` already uses for `lock`, with a no-op elsewhere, so the
   non-unix build keeps compiling.
4. For SIGKILL, which no handler can catch: sweep stale `talaria-call-*` and `talaria-body-*`
   directories under `TMPDIR` at start-up, removing only those older than a conservative age and
   only ones talaria itself named.
5. Test that a cancelled context terminates curl and removes both the capture directory and the
   `talaria-body-*` temp file.

**Verify:** `go test ./internal/curl/... ./cmd/talaria/...`
- [ ] Cancelling the context mid-call kills curl rather than orphaning it, and leaves no
      `talaria-call-*` or `talaria-body-*` behind.
- [ ] The un-signalled path is unchanged: `go test -race ./...` stays green.

---

### Task 7: Give history entries a stable id so `replay` cannot re-issue the wrong request

**Fixes findings:** #7

The index is `len(entries) - position` (history.go:271), recomputed on every read, and
`corpus.Entry` carries no stable identifier. `call`, `run` **and `replay` itself** all append, so
every index in a previously-printed listing silently shifts. Confirmed: `history replay 2` then
`history replay 1` replayed the *same* entry twice. Two replays in a row is ordinary agent
behaviour, and the e2e suite cannot catch it because it replays index 1 exactly once.

**Files:**
- `internal/corpus/entry.go` (modify)
- `internal/corpus/store.go` (modify)
- `cmd/talaria/history.go` (modify)
- `internal/corpus/store_test.go` (modify)
- `AGENT.md` (modify)

**Steps:**
1. Add a stable `ID string` to `corpus.Entry`, assigned at append time. Use the entry's
   RFC3339Nano timestamp: it needs no counter state in the file, is already monotonic per process,
   and survives the trim path — but make `Append` disambiguate a collision (two entries in the
   same nanosecond) rather than emit a duplicate id.
2. Tolerate entries already on disk with no id: an old store must keep listing and showing, so an
   empty id renders as such rather than erroring the whole read.
3. Have `history` list print the id alongside the positional index, in the JSON view and as a
   column in the table/TSV rows.
4. Have `show` and `replay` accept either: an id when the argument matches one, the positional
   index otherwise. Keep the positional form working — it is documented — but resolve the id
   first so an unambiguous id always wins.
5. Document the id as the stable handle in `AGENT.md`'s history section, and say plainly that
   positional indices shift on every write.
6. Test the exact confirmed sequence: list, replay one entry, then replay a *different* id and
   assert it replayed that one.

**Verify:** `go test ./internal/corpus/... ./cmd/talaria/...`
- [ ] Two consecutive `history replay <id>` calls with different ids replay different requests.
- [ ] A store written before this change still lists, shows and replays.

---

### Task 8: Make `talaria auth` and `talaria auth <typo>` exit 2

**Fixes findings:** #8

`newAuthCmd` (auth.go:30) sets no `Args`/`RunE`, so `talaria auth` and `talaria auth bogus` both
exit 0 with cobra help on **stdout**. DESIGN.md §4 assigns 2 to usage errors with "stderr JSON
lists valid options", AGENT.md:209 tells the agent code 2 covers "unknown command", and §3.1 makes
deterministic exit codes the thing agents branch on. Commit f747586 installed `unknownCommand` on
the root only.

**Files:**
- `cmd/talaria/root.go` (modify)
- `cmd/talaria/auth.go` (modify)
- `cmd/talaria/exitcode_test.go` (modify)

**Steps:**
1. Add a shared `groupCommand()` helper in `root.go` that sets `Args: unknownCommand` and
   `RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }`, mirroring what the
   root does at root.go:35 and root.go:42, so any future command group inherits the behaviour.
2. Apply it in `newAuthCmd`, and to any other parent-only command group registered on the root —
   check `history` and anything else with subcommands but no `RunE`.
3. Confirm help goes to stderr and the structured error is the usual exit-2 JSON envelope,
   consistent with what `talaria bogus` already produces.
4. Assert `talaria auth` → 2 and `talaria auth bogus` → 2 in `exitcode_test.go`.

**Verify:** `go test ./cmd/talaria/...`
- [ ] `talaria auth` and `talaria auth chekc` both exit 2 with a structured error on stderr.
- [ ] `talaria auth check <spec>` is unaffected.

---

### Task 9: Correct the documented `--header` syntax and gate the docs on it

**Fixes findings:** #9 (documentation half; the parser change is Task 2)

DESIGN.md §4 specifies `--header X-Foo=bar` and the code implements it, but AGENT.md's headline
`call` example is `--header 'X-Trace: abc'`, which exits 2 — and per finding #2 the resulting
error echoes whatever secret the agent put there. §3.7 makes AGENT.md a first-class deliverable
and this is the one line an LLM copies verbatim. `agentdoc_test.go` checks command names, exit
codes and env vars but never flag argument syntax, so this drifted unpoliced.

**Files:**
- `AGENT.md` (modify)
- `README.md` (modify)
- `cmd/talaria/agentdoc_test.go` (modify)

**Steps:**
1. Fix `AGENT.md:54` (`--header 'X-Trace: abc'` → `--header X-Trace=abc`) and `AGENT.md:169`
   (`--header 'X-Api-Key: sk-live-…'` → `--header X-Api-Key=sk-live-…`).
2. Fix `README.md:274` (`--header "X-Api-Key: sk-live-…"`) the same way. README.md:251, :259 and
   :421 mention `--header` without an argument and need no change — confirm before editing.
3. Extend `agentdoc_test.go` to extract the fenced `talaria …` invocations from AGENT.md and
   execute each against a fixture spec with `--dry-run`, asserting a non-usage exit. This is the
   part that stops the next drift; without it the doc fix is a one-off.
4. Skip or annotate any fenced invocation that legitimately cannot run under `--dry-run`, with the
   reason stated inline rather than by silently narrowing the extraction.

**Verify:** `go test ./cmd/talaria/...`
- [ ] Every `talaria …` invocation in AGENT.md runs against the fixture spec without a usage
      error.
- [ ] No `--header 'Name: value'` form remains in AGENT.md or README.md.
