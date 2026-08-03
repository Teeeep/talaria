# talaria — Review Fix Plan

Source of truth: [docs/design/DESIGN.md](docs/design/DESIGN.md) (v0.3). Stack: `.ralph/stack.json`.
Findings: [REVIEW_FINDINGS.md](REVIEW_FINDINGS.md) — do not edit that file.

## Scope

This plan replaces the previous review-fix plan, whose 9 tasks are all landed. It covers **only
the 6 CRIT findings** (1–6) from the current review of `ralph/design` vs `main`. The 17 WARN and
10 INFO findings are deliberately **not** planned: they are recorded for a human and do not block
the PR. Do not create work for them, and do not "fix them while you are in the file" — an
unrequested change in a reviewed diff costs another review cycle.

Still out of scope, unchanged: Phases 5–8 (`internal/twin`, the recording proxy, `talaria twin …`),
distribution and packaging, request chaining, and everything in DESIGN.md §9 Non-goals.

Every task lands with the regression test that proves its finding closed — a fix without one is
not done. Build, `gofmt`, `go vet` and `go test -race ./...` are clean at HEAD and must stay so.

---

### Task 1: Reject CR/LF in header, cookie and method values before they reach the wire

**Fixes findings:** #1

**Files:**
- `internal/request/build.go` (modify)
- `internal/curl/config.go` (modify)
- `internal/request/build_test.go` (modify)
- `internal/curl/config_test.go` (modify)

**Steps:**
1. In `internal/request/build.go`, add a value check rejecting any string containing `\r` or
   `\n` as a usage error via `b.fail`. Apply it on every path that produces a `Pair`:
   `located` (:252, spec-declared header/cookie/query params), `pairs` (:329, the
   `--header`/`--query`/`--cookie` flags) and the profile-header branch of `headers` (:287).
   Follow the existing convention at :322-326 — report by name and position, never quote the
   offending value back, since it may hold a credential.
2. Apply `isFieldName` to header *names* that do not go through `httpFieldName` today: the
   `inHeader` results of `located` and the profile header names in `headers`. Leave query and
   cookie parameter names unconstrained — only header names go on the wire as HTTP field names.
3. Validate `req.Method` against the HTTP token charset (`fieldNameChars`, :365, already holds
   it) before the request is built, rejecting anything else as a usage error.
4. In `internal/curl/config.go`, re-check for `\r`/`\n` in `document.auth` (:208) and
   `document.cookies` (:228) as the last gate before the config document is written, and check
   the method in the `request` directive (:149). Return an error rather than escaping:
   `escapeDirective` (:393) protects the *directive* syntax, but curl un-escapes the value back
   to literal CR/LF onto the wire.
5. Add regression tests: a spec-declared required header param whose bound value contains
   `\r\n` is rejected at bind time; a profile header with an invalid field name is rejected; a
   method containing a space is rejected.

**Verify:** `go test ./...`
- [x] A header, cookie or query value containing `\r` or `\n` — from a spec parameter, a profile
      header or a flag — is a usage error (exit 2), not a second request on the wire.
- [x] Spec- and profile-supplied header names are held to `isFieldName`, as `--header` names are.
- [x] No test asserts on the rejected value's content appearing in the error message.

---

### Task 2: Emit `-X GET` in the rendered curl when the request carries a body

**Fixes findings:** #2

**Files:**
- `internal/curl/render.go` (modify)
- `internal/curl/render_test.go` (modify)
- `internal/e2e/e2e_test.go` (modify)

**Steps:**
1. In `internal/curl/render.go:45`, change the GET guard to
   `case req.Method != "" && !(strings.EqualFold(req.Method, http.MethodGet) && req.Body == nil):`
   so `-X` is emitted whenever a GET carries a body. `--data-raw` without `-X` makes curl send
   POST, while the config document at `internal/curl/config.go:149` always writes
   `request = "GET"` — the printed command and the executed one differ today.
2. Update the comment above the case to say why a body changes the rule: naming GET is noise
   only when curl's default method is what actually goes out.
3. Add a render test asserting `-X GET` appears for a GET-with-body, and that a bodyless GET
   still renders without `-X`.
4. Replace the tautological assertion at `internal/e2e/e2e_test.go:454` — it compares two
   outputs of the same `curl.Render` call on the same `*request.Request`, so it can never fail,
   which is why this bug survived three review cycles. Instead run the `request.curl` string
   from `--dry-run` through `sh -c` against the same `httptest` server and assert the method,
   path, headers and body the server recorded match those of the real `call`. Parameterise over
   GET-with-body, HEAD, a cookie param and a form body.

**Verify:** `go test ./...`
- [ ] `call … --body '{"a":1}'` on a GET operation renders a command containing `-X GET`.
- [ ] The e2e case executes the rendered command and compares it against the server's record of
      the real call, and fails if the two methods diverge.

---

### Task 3: Make `auth check` and `call`/`run` agree on which requirement is chosen

**Fixes findings:** #3

**Files:**
- `internal/config/auth.go` (modify)
- `cmd/talaria/auth.go` (modify)
- `internal/config/auth_test.go` (modify)

**Steps:**
1. In `internal/config/auth.go`, export one predicate over a security requirement: given the
   requirement, the declared schemes and the profile, report whether talaria can supply it
   (`known`) and whether every credential is `Present()`. This is the rule `satisfied`
   (`cmd/talaria/auth.go:169`) already implements correctly.
2. Rewrite `Resolve` (:80) to make two passes over `op.Security`: return the first requirement
   that is both supported and fully present; if none is, fall back to the first supported
   requirement so the exit-5 message still names an actionable variable. Preserve the current
   early return for an empty requirement and the "no usable security scheme" error when nothing
   is supported.
3. Rewrite `satisfied` in `cmd/talaria/auth.go` to call the shared predicate instead of
   re-deriving it. The duplication across two packages is what let the two answers drift.
4. Add a test for the reproducing case: `security: [{keyA: []}, {keyB: []}]` with only
   `TALARIA_AUTH_APIKEY_KEYB` set resolves to `keyB`, and `Resolve` with neither set still
   returns the `keyA` credential so the error names a variable.

**Verify:** `go test ./...`
- [ ] With only the second alternative's credential exported, `Resolve` returns it and `call`
      succeeds where it previously exited 5.
- [ ] `auth check` and `Resolve` agree on every alternative arrangement the tests cover.
- [ ] With no credential set at all, the exit-5 message still names a concrete variable.

---

### Task 4: Stop `run` from fabricating failures and history when cancelled

**Fixes findings:** #4

**Files:**
- `cmd/talaria/run.go` (modify)
- `cmd/talaria/run_test.go` (modify)

**Steps:**
1. In the suite loop (`cmd/talaria/run.go:123`), break when the runner's context is cancelled so
   only operations that actually ran appear in the report. `runner.ctx` is already held (:245,
   set at :302); the loop never consults it.
2. In `runner.execute` (:320), when `curl.ExecuteWith` (:371) fails because the context was
   cancelled, skip `recordCall` (:374) — history must not gain entries for requests that were
   never made. Those entries land under `SourceRun` and are subject to `trim`'s 1000-per-source
   cap (`internal/corpus/store.go:31`), so a cancelled run over a large spec evicts the previous
   run's real history.
3. In `verdict` (:403), return a distinct error when the context was cancelled, ahead of the
   `failOnError` branch, so a cancelled run neither exits 0 nor reports a validation verdict for
   a suite that never ran. Use a non-zero exit distinct from 4 (validation) and 5 (credentials).
4. Add a test that cancels the runner's context partway through a multi-operation suite and
   asserts: the report contains only the operations that ran, no cancelled operation is counted
   as `failed`, no history entry was appended for them, and the exit is neither 0 nor the
   validation code.

**Verify:** `go test ./...`
- [ ] A run cancelled after N of M operations reports N results, not M.
- [ ] `--fail-on-error` on a cancelled run does not produce a validation failure verdict.
- [ ] The corpus gains no entry for an operation whose request was cancelled before it ran.

---

### Task 5: Scrub sensitive literals, not just resolved credentials, from curl's stderr

**Fixes findings:** #5

**Files:**
- `internal/curl/exec.go` (modify)
- `internal/curl/exec_test.go` (modify)

**Steps:**
1. In `scrubber` (`internal/curl/exec.go:269`), iterate on `p.Value.IsSensitive()` rather than
   `p.Value.IsSecret()`. A literal typed under a credential-shaped name — `--query api_key=…`,
   marked `Sensitive` by `hide` (`internal/request/build.go:309`) and rendered `<redacted>`
   everywhere else — is skipped today, so curl's own error text carries it in cleartext into the
   exit-1 JSON on stderr.
2. Split the two cases inside the loop: for a secret keep the current `Ref().Resolve()` →
   `Ref().String()` mapping; for a sensitive literal use `p.Value.Reveal()` as the needle and
   `secret.Placeholder` as the replacement. Skip an empty needle either way.
3. Keep the `url.QueryEscape` companion pair for both branches — a value echoed back inside a
   URL is percent-encoded, which the literal form walks straight past.
4. Add a canary test for a literal `--query api_key=` whose call provokes a curl error that
   echoes the URL (a spec path containing `[` forces curl exit 3), asserting the emitted error
   text contains the placeholder and not the literal.

**Verify:** `go test ./...`
- [x] A curl error echoing a URL that contains a literal `api_key=` value emits the placeholder
      on stderr, not the value.
- [x] The existing secret-ref scrubbing behaviour is unchanged.

---

### Task 6: Restrict `history replay` to credentials inside the auth namespace

**Fixes findings:** #6

**Files:**
- `cmd/talaria/history.go` (modify)
- `cmd/talaria/history_test.go` (modify)

**Steps:**
1. In `replayValue` (`cmd/talaria/history.go:655`), stop accepting an arbitrary ref name from
   the store. `secret.ParseRef` accepts any non-empty name, and both the name and the
   destination URL come from a plain JSONL file — which makes the binary a "resolve $ANY_VAR and
   send it to $ANY_URL" primitive, run from the one process the deployment trusts with
   credentials.
2. Add an allowlist predicate: accept a ref whose name is `config.EnvBearer` (auth.go:21),
   `config.EnvBasic` (:23), or carries the `config.EnvAPIKeyPrefix` prefix (:26), or is a value
   in the selected profile's `Auth` map (`internal/config/config.go:67`). Thread the profile
   into `replayValue` — its caller already resolves one.
3. Reject anything else with a usage error (exit 2) naming the variable it refused, so the
   failure is diagnosable. Do not fall through to `request.Literal(stored)`: that would put the
   literal string `<redacted:env:NAME>` on the wire.
4. Add tests: a stored `<redacted:env:TALARIA_AUTH_BEARER>` still replays; a stored
   `<redacted:env:AWS_SECRET_ACCESS_KEY>` is refused with the name in the message; a name
   present in the profile's `auth:` map is accepted.

**Verify:** `go test ./...`
- [ ] A hand-written history entry naming an env var outside the auth namespace is refused with
      exit 2 rather than resolved and sent.
- [ ] Replay of an entry recorded by a normal `call` is unaffected.
- [ ] The refusal message names the variable it refused.
