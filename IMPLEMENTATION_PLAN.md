# Phase 2a — remediation: implementation plan

Design doc: `docs/plans/2026-08-03-phase-2a-remediation.md`
Fix detail: `docs/review/2026-08-03-review-findings.md` — **read the finding before writing the fix.**
Spec: `docs/design/DESIGN.md` v0.5. House rules: `CLAUDE.md`.

## The rule that governs every task here

**`go test ./...` is green right now with all 25 defects live** (re-confirmed 2026-08-03 at
`4eaee50`). A green suite is not an acceptance signal. A task is done when a test exists that
**fails before your change and passes after it**. If your new test passes before you touch the
implementation, it is testing the wrong thing — rewrite it.

## Verified state of the tree (2026-08-03, HEAD `4eaee50`)

Confirmed by reading the source, not assumed. `git diff 7069923..HEAD -- '*.go'` is **empty**, so
every file:line below still holds; the commits since are docs and loop prompts only.

- `internal/gen`, `cmd/talaria/run.go` and `internal/corpus/lock_other.go` **do not exist**
  (removed in `795b533`). Findings 7, 12 and 26 are moot. Finding 4's `run` half, finding 19's
  `--report junit` half and finding 16's `run` pathological case are all gone with them.
- `grep -rn "credentials_withheld\|allow-host\|allow_hosts\|AllowHosts\|Withheld" --include=*.go .`
  returns **zero hits**. The whole of §5a "Credentials bind to hosts" is unimplemented.
- `grep -rn "\.Variables" --include=*.go .` returns **zero hits**. Server-variable substitution
  does not exist, so the §5a host set cannot be computed correctly until Task 1 lands.
- `grep -rn "LimitReader\|CheckRedirect" --include=*.go .` returns **zero hits**.
- Several line numbers in the findings report have drifted. Corrections carried into the tasks
  below: the TSV join is `render.go:104` (not 108); the stale canary comment is
  `canary_test.go:385-387` (not 427); `NewEntry` is `entry.go:151` (not 24); `Body.Bytes` is
  `entry.go:119` (not 121); `signal.NotifyContext` is `root.go:206` (not 207); `satisfied`'s bug is
  `auth.go:188` (not 182). **Grep for the symbol, do not trust a line number.**
- `docs/plans/2026-08-02-phase-2-boundary-design.md:234` marks the cache TTL "applied in v0.5".
  That is false against this tree — none of it exists. Do not trust that row.
- `.ralph/refactor-backlog.md` does not exist yet. That is not an error; the build prompt creates
  it the first time an iteration has something to record.

### Signatures this phase builds on, read off the source

Copied here so no task has to re-derive them. Verified 2026-08-03; still grep before you edit.

```go
func Build(in Inputs) (*Request, error)                                  // request/build.go:69
type Inputs struct {                                                     // request/build.go:32-62
    Op operation.Operation; Doc *spec.Document; Profile *config.Profile
    Creds []config.Credential; BaseURL string
    Params, Query, Headers, Body []string   // "name=value" STRINGS, not pairs
    Redactor *secret.Redactor; Stdin io.Reader
}
type Request struct { OperationID, Method, BaseURL, Path string          // request/request.go:254-268
                      Query, Headers, Cookies []Pair; Body *Body }
func firstServer(doc *spec.Document) string                              // request/build.go:173
func (b *binder) baseURL() string                                        // request/build.go:134
func (b *binder) credentials(req *Request)                               // request/build.go:498
func (ix *Index) Lookup(id string) (Operation, error)                    // operation/index.go:111
func Resolve(op operation.Operation, doc *spec.Document, prof *Profile) ([]Credential, error)
                                                                         // config/auth.go:133
func Schemes(doc *spec.Document, prof *Profile) ([]Credential, error)     // config/auth.go:179
func loadSpec(cmd *cobra.Command, args []string) (*spec.Document, *operation.Index, error)
                                                                         // cmd/talaria/list.go:170
func callPayload(req *request.Request, resp *responseView, result *validate.Result) output.Payload
                                                                         // cmd/talaria/call.go:453
func validateResponse(w io.Writer, doc, req, view) *validate.Result      // cmd/talaria/call.go:366
```

`config.Credential` is `{Scheme, Kind, In, Name, Ref}` (auth.go:53-64) — **no `Supported` field**.
`Unsupported` is a `Coverage` enum constant (auth.go:73-87), not a credential state; unsupportedness
is encoded today as *absence from the `byName` map*. `config.Profile` (config.go:56-73) is
`{Name, BaseURL, Headers, Auth, History}` — no `AllowHosts`; `KnownFields(true)` is at config.go:134.
`cmd/talaria` has exactly **four** persistent flags — `--output` (root.go:58), `--spec` (:65),
`--profile` (:71), `--base-url` (:73). Neither `--allow-host` nor `--refresh` exists.

Neither `config.Resolve` nor `config.Schemes` takes a host or a base URL: credentials are resolved
today with **no knowledge of the destination**. That is the shape Task 2 and Task 3 change.

### What a stored history entry actually holds — read this before Task 3

`corpus.Entry` (entry.go:53): `ID, Timestamp, Source, OperationID, Method, URL, Request, Response`.
`EntryRequest` (entry.go:78) is `{Headers map[string]string, Cookies map[string]string, Body *Body}`.

Four consequences the replay rewrite must handle, none of them obvious:

1. **There is no query field.** The query survives only inside `URL`; `replayQuery`
   (history.go:615) walks `parsed.RawQuery` by hand precisely to keep its order.
2. **There is no path template and no base-URL field.** The entry holds the *concrete* path
   (`/pets/42`); `operation.Operation.Path` holds the template (`/pets/{petId}`). Recovering
   `petId=42` is a matching problem the task must solve, or `Build` fails "petId is required".
3. **Headers and cookies are maps** — order is already lost, and repeated names are joined
   HTTP-style.
4. **Stored credential values are the literal placeholder text** `<redacted:env:NAME>`. Feeding
   them back through `Inputs.Headers` sends the placeholder *and* duplicates the credential
   `binder.credentials` re-adds. They must be dropped, not replayed.

### Two behaviours that look like one rule and are not

DESIGN.md §5a:374-377 — `call` with an off-set `--base-url` **still runs**, withholds every
credential, exits 0. §5a:403-409's replay table — replay with an off-set *stored* host **refuses**,
exit 2. Same host set, deliberately different outcomes. Do not unify them.

## Order

Two hard constraints from the design doc, both encoded in the dependencies below:

1. **Task 1 (finding 11) before Task 3 (finding 1).** The allowed host set is defined as the
   spec's `servers[]` *after* server-variable substitution. Substitution does not exist. Build the
   host set on unsubstituted URLs and it passes a fixture test and is wrong in production.
2. **Findings 1, 2, 3 and 22 land together, in Task 3, in one commit.** They are one rule seen
   through four doors: *the destination of a request, and the credentials attached to it, are
   derived from the spec and the flags — never from stored or off-spec input.* The previous cycle
   split them and shipped a fix that narrowed *which* credential replay resolves while leaving
   *where it is sent* untouched; finding 2 is a repeat for exactly that reason. **Do not split
   Task 3.** Task 2 is separable only because it computes the host set without enforcing it.

Finding 21 is the documentation half of finding 4 and ships inside Task 4, or README contradicts
the code.

**That rule is general, and this plan applies it to three tasks, not one.** Finding 21 is not a
special case — it is what happens when a shipped document outlives the behaviour it describes. Three
tasks here change a user-facing contract, so each carries its own doc edits in its own commit:
Task 3 (README.md:387-399 and AGENT.md:199-207 document `replayableEnv`, which Task 3 deletes
outright; plus `--allow-host` and `credentials_withheld`, which nothing documents yet), Task 4
(finding 21 proper), and Task 20 (README:49-50's *"fetched once per URL"* is the forever-cache being
removed). No CI check enforces doc/code agreement — Task 24 step 8 is the only backstop, and a
backstop is not a substitute for shipping them together.

## Structure

24 tasks: 20 feature tasks in four blocks of five, each block closed by a refactor pass (tasks 6,
12, 18, 24). Task 24 is both the fourth block's pass and the end-of-phase pass. The loop's
`--review-every 6` puts the review checkpoint immediately after each refactor pass.

## Side effect worth knowing

Task 3 deletes `replayableEnv` entirely, which makes DESIGN.md §5a's replay-table row *"Env var
names — Not applicable; nothing in a stored entry is resolved"* true by construction. **Finding
18** (a documented conflict between §5a and README/AGENT.md over whether a profile's `auth:` map
may name an arbitrary variable) is therefore resolved as a side effect, without a design
amendment. It is not scoped as work here; Task 4's doc pass must not reintroduce the conflict.

---

### Task 1: Substitute server variables in `servers[].url`

**Depends on:** none

**Test files:**
- `internal/request/request_test.go` (modify) — substitution feeding `binder.baseURL`
- `internal/spec/load_test.go` (modify) — `testdata` spec carrying server variables

**Implementation files:**
- `internal/request/build.go` (modify) — `firstServer` (line 173) substitutes; add an exported
  `ServerURLs(doc *spec.Document) []string` returning every server URL after substitution
- `internal/spec/testdata/server-variables.yaml` (create) — fixture

**Red — write failing tests:**
1. A spec with `servers[0].url: "https://{region}.api.example.com/v1"` and
   `variables.region.default: eu` builds a request whose `BaseURL` is
   `https://eu.api.example.com/v1`. Today this fails with exit 2, *"base URL … is not an absolute
   http(s) URL"* (the check is `build.go:159`) — reproduce that failure first.
2. A variable whose `default` is not in its own `enum` is a spec error: `clierr.SpecLoad` or
   `clierr.Usage`, naming the variable, not a silent pass-through.
3. Multiple variables in one URL (`https://{region}.{env}.example.com`) all substitute.
4. `ServerURLs` returns one substituted entry per `servers[]` element, in spec order.
5. A `{placeholder}` with no matching entry in `variables` leaves the URL unusable and fails with
   a message naming the placeholder — not a request to a literal `{region}` host.

**Adversarial — what does hostile or malformed input do here?**
The spec is untrusted input; every string here is attacker-controlled.
1. A variable `default` of `evil.com/` or `evil.com#` — substitution must not let a variable value
   rewrite the *host* of a URL it was only meant to fill a segment of. Assert that a default
   containing `/`, `?`, `#`, `@` or `:` either fails or cannot move the authority. This is the
   same class as the `url.PathEscape` rule already enforced in `binder.path` (build.go:225).
2. A self-referential or recursive-looking default (`{region}` whose default is `{region}`) must
   terminate — substitute **once**, never to a fixed point.
3. A URL with 10,000 `{a}` placeholders must not blow up quadratically or allocate unboundedly.
4. A variable named `""` or containing `{`/`}`.
5. `variables` present but `default` empty — OpenAPI requires `default`; an absent one must fail
   loudly, not substitute the empty string into a host.

**Green — minimal implementation:**
1. In `internal/request/build.go`, add
   `substituteServer(url string, vars map[string]*v3high.ServerVariable) (string, error)` doing one
   left-to-right pass over `{name}` spans.
2. Validate each substituted value against `enum` when the variable declares one; reject a value
   that would introduce an authority separator.
3. `firstServer` (build.go:173-178, whose body returns `doc.Model.Servers[0].URL` verbatim today)
   returns the substituted URL; add `ServerURLs` for Task 2. `firstServer` has exactly one caller,
   `binder.baseURL` at build.go:142.
4. Report failures through `b.fail` (build.go:111) so they join the existing collected-problems
   error.

**Verify:** `go test ./internal/request/... ./internal/spec/...`

**Why:** §5a defines the allowed host set as the servers *after* substitution. Without this, Task 3
computes the host set from URLs containing literal `{region}` and is wrong the moment it meets a
real spec — and a large class of 3.x specs is uncallable without `--base-url` today.

---

### Task 2: Compute the allowed host set (no enforcement yet)

**Depends on:** Task 1

**Test files:**
- `internal/config/config_test.go` (modify) — `allow_hosts` parses under `KnownFields(true)`
- `internal/request/request_test.go` (modify) — host-set construction and matching
- `cmd/talaria/root_test.go` (create or modify) — `--allow-host` is registered and repeatable

**Implementation files:**
- `internal/config/config.go` (modify) — add `AllowHosts []string \`yaml:"allow_hosts"\`` to
  `Profile` (config.go:56-73)
- `internal/request/hosts.go` (create) — `HostSet` type,
  `NewHostSet(specURLs, allowFlags, profileHosts []string) HostSet`,
  `(HostSet) Allows(rawURL string) bool`, `(HostSet) Key(rawURL string) string`
- `cmd/talaria/root.go` (modify) — register `--allow-host` as a repeatable persistent flag beside
  `--base-url` (root.go:73)

**Red — write failing tests:**
1. A profile file containing `allow_hosts: ["localhost:9000"]` loads without error. **Today this
   is a parse error** because `Load` decodes with `KnownFields(true)` at config.go:134 and wraps
   the unknown key into `clierr.Usage` at config.go:137-139 — write that failing test first, it is
   the regression the finding names.
2. `NewHostSet` with spec server `https://api.example.com` allows `https://api.example.com/v1/pets`
   and rejects `http://127.0.0.1:8765/pets`.
3. Default ports normalise: spec server `https://api.example.com` allows
   `https://api.example.com:443/x`, and `http://x.com` allows `http://x.com:80/y`.
4. Host comparison is case-insensitive (`API.Example.COM` matches `api.example.com`) but path and
   scheme are not part of the key.
5. An `--allow-host` entry **with** a port matches only that port; an entry **without** a port
   matches any port on that host. Assert both directions explicitly — this is the rule a later
   agent will otherwise guess at.
6. `--allow-host` is repeatable and unions with profile `allow_hosts` and the spec servers.
7. `Key` renders `host:port` — the form §5a's `credentials_withheld[].host` shows
   (`"localhost:9000"`).

**Adversarial — what does hostile or malformed input do here?**
Spec servers are untrusted; the profile file is human-authored but read from disk; the flag is a
CLI string.
1. A spec server URL that does not parse, is relative, or has an empty host — it must contribute
   *nothing* to the set rather than a wildcard or an empty-string key that matches everything.
   **This is the dangerous failure mode: a malformed server must not open the set.**
2. A spec server with userinfo (`https://user:pass@api.example.com`) — the key is the host only;
   assert the credential does not become part of a set entry, and that nothing echoes it.
3. `--allow-host ""`, `--allow-host "*"`, `--allow-host "http://x.com"` (a URL where a host is
   wanted) — decide and assert: reject with exit 2 rather than silently admitting a wildcard.
4. An IPv6 literal (`[::1]:9000`) parses and matches correctly.
5. A trailing dot (`api.example.com.`) and a Unicode/punycode homograph — assert they do **not**
   match `api.example.com`; matching is byte-wise after lowercasing, never normalised.
6. A profile with 10,000 `allow_hosts` entries — lookup must not be linear per credential.

**Green — minimal implementation:**
1. Add `AllowHosts` to `config.Profile` with the `allow_hosts` YAML key.
2. Write `internal/request/hosts.go`: `HostSet` wraps a `map[string]bool` of `host:port` keys plus
   a `map[string]bool` of portless "any port" hosts. `Allows` parses, refuses non-http(s) and
   empty hosts, and checks both maps. `request.IsHTTPScheme` (request.go:205) and
   `request.Userinfo` (request.go:230) already exist — reuse them rather than re-parsing.
3. Register `root.PersistentFlags().StringArray("allow-host", nil, …)` in `newRootCmd`
   (root.go:25), so `call`, `auth check` and `history replay` all accept it.

**Adversarial note for the implementer:** `HostSet` must have no "empty set means allow
everything" behaviour. An empty set allows nothing. Task 3 depends on that.

**Verify:** `go test ./internal/config/... ./internal/request/... ./cmd/...`

**Why:** Separates the mechanical, edge-case-heavy work of *computing* the host set from the
security rule that *enforces* it, so Task 3 is about credential diversion and replay
re-derivation rather than URL parsing. Nothing enforces the set yet — Task 3 does, immediately
after.

---

### Task 3: Credentials bind to hosts, and replay re-derives — findings 1, 2, 3, 22

**Depends on:** Task 2

> **Do not split this task.** The design doc names splitting it as the known cause of the previous
> two failed cycles. One commit, one rule: *the destination of a request, and the credentials
> attached to it, are derived from the spec and the flags — never from stored or off-spec input.*

**Test files:**
- `cmd/talaria/call_test.go` (modify) — wire-level withholding on `call`
- `cmd/talaria/history_test.go` (modify) — replay re-derivation, refusals, validation block
- `internal/request/request_test.go` (modify) — `Build` diverts off-set credentials
- `cmd/talaria/auth_test.go` (modify) — `auth check` reports against the resolved host set
- `internal/e2e/e2e_test.go` (modify) — end-to-end withholding

**Implementation files:**
- `internal/request/request.go` (modify) — add `Withheld []Withheld` to `Request` (request.go:254-268);
  `Withheld{Scheme, Reason, Host string}`
- `internal/request/build.go` (modify) — `Inputs.Hosts HostSet`; `binder.credentials` (build.go:498)
  diverts instead of attaching at build.go:500-510
- `cmd/talaria/call.go` (modify) — render `credentials_withheld` in `callView` (call.go:30),
  stderr warning
- `cmd/talaria/history.go` (modify) — **rewrite the replay path**; delete `replayRequest` (554),
  `replayQuery` (615), `replayPairs` (651), `replayValue` (681), `replayableEnv` (721),
  `encodingPrefix` (739)
- `cmd/talaria/auth.go` (modify) — report against the host set
- `README.md` (modify) — lines 387-399 describe the behaviour this task deletes
- `AGENT.md` (modify) — lines 199-207 describe it too; line 61 documents `--base-url` with no
  mention that credentials are now withheld when it points off-spec

**Red — write failing tests:**

*Host binding (finding 1) — the assertion must be wire-level:*
1. Spec server `https://api.example.com`, `TALARIA_AUTH_BEARER` set to a canary, `--base-url`
   pointed at an `httptest` listener (use the existing `callServer` harness,
   `cmd/talaria/call_test.go:44-95` — its `recordedRequest` records `{Method, Path, Query, Header,
   Body}` and `call_test.go:174` already asserts on a received `Authorization` header, so the shape
   is proven. **It keeps only the *last* request**, so a test that makes two calls must read
   `received()` between them). Assert **the canary is absent from every header the server actually
   received**. This is the acceptance criterion the design doc names; an output-level assertion
   cannot catch this bug and is why the canary suite passes today.
2. The same call emits
   `credentials_withheld: [{"scheme":"bearerAuth","reason":"…","host":"127.0.0.1:PORT"}]` in the
   JSON envelope, and exactly one line on stderr naming the scheme and the host.
3. The call still **runs** and exits 0 — withholding, not refusing. §5a is explicit: pointing at a
   local twin is the common case and must not need a flag.
4. With `--allow-host 127.0.0.1` the same call **does** deliver the credential.
5. A `--base-url` inside the spec's `servers[]` delivers the credential and emits no
   `credentials_withheld` and no stderr line.
6. `auth check` against an off-set `--base-url` reports the scheme as not-going-to-be-sent, so
   "present" never means "will actually be sent" (§5a:389-390).

*Replay re-derivation (findings 2, 3, 22):*
7. A `history.jsonl` line hand-edited to `"url":"http://127.0.0.1:PORT/steal"` replays to the host
   the **spec/flags** name, not the stored one — and the canary never reaches the planted
   listener. Assert at the wire.
8. `history replay <id> --base-url https://elsewhere.example.com` actually retargets. Today the
   flag is accepted and silently discarded, which is worse than erroring.
9. An entry whose `operation_id` is no longer in the spec fails **that entry** with exit 2.
10. A replay emits a `validation` block, like `call` does.
11. An entry whose stored request body contains `secret.Placeholder` is refused with exit 2 rather
    than sending the literal string `<redacted>` to the API (finding 22 — verified today as
    `{"refresh_token":"<redacted>"}` arriving with empty stderr).
12. A stored host outside the currently allowed set refuses with exit 2 rather than retargeting
    (§5a's replay table, row "Target host").
13. **`replayableEnv` is absent from the tree.** Write a test that reads `cmd/talaria/*.go` and
    asserts the identifier does not appear. The design doc requires the symbol be *deleted*, not
    made unreachable — an unreachable version passes a behaviour test and comes back next cycle.
14. `history replay <id> --spec <path>` loads the spec the **flag** names. Assert the entry id is
    never mistaken for a spec ref: with `--spec` pointing at a valid spec and the id being `1`,
    replay succeeds rather than failing with "no spec given" or a load error naming `1`. Assert
    the `$TALARIA_SPEC` form works too. See the trap under Green step 5 — this is the assertion
    that catches it.
15. **The mutation gate reads the spec, not the entry.** An entry whose stored `"method"` is `GET`
    but whose `operation_id` resolves to a `DELETE` in the spec is **refused without
    `--allow-mutations`**. Today the gate is `(operation.Operation{Method: entry.Method}).IsMutation()`
    at `history.go:203` — built from the stored method, and it runs *before* any spec is loaded, so a
    one-word edit to a JSONL line turns a DELETE replay into an ungated one. Assert the inverse too:
    a stored `"method":"DELETE"` whose operation is a `GET` does **not** demand the flag, or the
    hostile file gets to make replay harder rather than easier.

**Adversarial — what does hostile or malformed input do here?**
The history file is untrusted input by §5a: *"a history entry is data, never instruction."*
Assume every field was written by an attacker.
1. An entry whose `operation_id` names a *different* operation than its stored method/path — the
   spec wins; assert the request uses the spec's method and path template, **and that the
   `--allow-mutations` gate is decided from the spec's method** (Red 15). The stored method is the
   attacker's field; the gate is the one place where believing it costs a write against a live API.
2. An entry whose stored path cannot be matched against the operation's path template — exit 2,
   not a request to a half-substituted path.
3. An entry with a stored header named `Authorization` holding a plausible-looking literal — it
   must be dropped, never sent. Credentials come only from `config.Resolve`.
4. An entry whose URL has userinfo, a non-http scheme (`file://`, `gopher://`), a CRLF in the
   path, or 10 MB of query string.
5. An entry whose `operation_id` is empty, absent, or 1 MB long.
6. A path-parameter value recovered from the stored path containing `../`, a CRLF, or a `%00` —
   it re-enters through `request.Build`, so assert `Build`'s existing escaping and
   `SplitsRequest` (build.go:451) gates actually catch it on this path too.
7. A spec whose `servers[]` is empty combined with no `--base-url`: replay fails cleanly, and
   critically **does not fall back to the stored URL**.

**Green — minimal implementation:**

*Host binding:*
1. Add `Withheld` to `internal/request`, and `Hosts HostSet` to `Inputs` (build.go:32-62).
2. In `binder.credentials` (build.go:498), for each credential test `b.in.Hosts.Allows(req.BaseURL)`.
   On failure append
   `Withheld{Scheme: cred.Scheme, Reason: "host not in spec servers[]", Host: hosts.Key(req.BaseURL)}`
   instead of the `Pair` built at build.go:500 and routed at build.go:504/506/508.
3. In `cmd/talaria/call.go`, build the `HostSet` from `request.ServerURLs(doc)` ∪ `--allow-host` ∪
   `prof.AllowHosts`, pass it into `Inputs` (`buildRequest`, call.go:270, calls `request.Build` at
   call.go:292), and render `req.Withheld` as `credentials_withheld` in `callView` plus one stderr
   line.
4. Do the same in `cmd/talaria/auth.go` so `auth check` agrees.

*Replay:*
5. Rewrite `newHistoryReplayCmd`'s `RunE` (`history.go:163`) to: `loadSpec` →
   `index.Lookup(entry.OperationID)` → rebuild `Inputs` → `config.Resolve` → `request.Build` with
   the same `HostSet`. **`Inputs` takes `[]string` of `name=value`, not pairs** — see "What a
   stored history entry actually holds" at the top of this plan before writing a line of this.
   Concretely:

   - **Path params.** The entry stores `/pets/42`; `op.Path` is `/pets/{petId}`. Split both on `/`,
     require equal segment counts, and for each `{name}` segment take the stored segment as
     `name=value` in `Inputs.Params`. A length mismatch, or a literal segment that differs, is
     exit 2 — never a request to a half-substituted path.
   - **Query.** Parse `entry.URL`'s `RawQuery` in order (as `replayQuery` does today at
     history.go:615), drop any pair whose value contains `secret.Placeholder`, and pass the rest as
     `name=value` in `Inputs.Query`. `binder.credentials` re-adds the real ones.
   - **Headers and cookies.** `EntryRequest.Headers`/`Cookies` are `map[string]string`. Drop every
     entry whose value contains `secret.Placeholder` and every header a credential would occupy;
     pass the remainder as `name=value`. Order is already lost in the store — do not pretend to
     preserve it.
   - **Body.** Refuse with exit 2 when the decoded body contains `secret.Placeholder` (finding 22).
     Otherwise pass it as an argv-style `--body` literal in `Inputs.Body`.

   **Trap — deleting the helpers deletes checks that exist nowhere else.** `replayRequest` and
   friends currently enforce userinfo rejection, non-http scheme rejection and truncated-body
   rejection (the truncation check is history.go:596-600). `binder.baseURL` covers userinfo
   (build.go:151) and scheme (build.go:159) **for the base URL only**. Re-assert the truncated-body
   refusal on the new path or it silently disappears — the adversarial list above assumes it is
   still there.

   **Trap — the stored URL folds the path prefix into `Path`.** `replayRequest` sets
   `BaseURL: parsed.Scheme + "://" + parsed.Host` (history.go:578) and puts everything else in
   `Path` (history.go:579), while `binder.baseURL` returns `scheme://host[/prefix]` from the spec.
   Matching the stored path against `op.Path` must account for the server prefix, or every spec
   with a `/v1` base fails.

   **Trap — call `loadSpec(cmd, nil)`, not `loadSpec(cmd, args)`.** `loadSpec`
   (`cmd/talaria/list.go:170`) takes `args[0]` as the spec ref (list.go:171-174), and `spec.Resolve`
   (`internal/spec/source.go:36`) gives that positional argument precedence over `--spec` **and**
   `$TALARIA_SPEC`. On `replay <id>` `args[0]` is the entry id, so threading `args` through — the
   obvious move, because every other command in the tree calls `loadSpec(cmd, args)` — makes
   `history replay 3` try to load a spec named `3` while silently ignoring the flag the caller
   set. `Args` stays `ExactArgs(1)`: the id is the positional argument, and the spec comes from
   `--spec` or `$TALARIA_SPEC` only. Replay with neither set now fails `clierr.Usage` ("no spec
   given"), which is a deliberate contract change — step 9's `Long` rewrite must say so.
6. Refuse with exit 2 when the stored host is outside the allowed set, before building.
7. Emit the `validation` block using the same path `call` uses. **Reuse `validateResponse`
   (`call.go:366`), not `validationInput` (`call.go:394`)** — `validateResponse` is the one that
   also runs `reportValidation`, which downgrades a validator error to a stderr warning instead of
   failing the call. `callPayload` (call.go:453) already renders the block from
   `callView.Validation` (`call.go:34`), so replay only has to stop passing `nil` as its third
   argument (`history.go:232`). Do **not** reach for `validateWith` (`call.go:380`) — it is dead
   code with zero callers and a doc comment describing the deleted `run`; Task 6 removes it.
8. **Move the mutation gate after `index.Lookup` and gate on the spec's operation.** It sits at
   `history.go:203` today — `(operation.Operation{Method: entry.Method}).IsMutation()`, built from
   the stored method, evaluated before any spec exists. It becomes `op.IsMutation()` on the
   operation `index.Lookup` returned. Keep the flag registration (`history.go:236`) and the message
   at `history.go:205` as they are; only the source of the method changes. This is the same rule as
   everything else in this task — the spec decides, the file does not — and it is the one instance
   where getting it wrong sends a real DELETE.
9. Delete `replayRequest`, `replayQuery`, `replayPairs`, `replayValue`, `replayableEnv` and
   `encodingPrefix`. Keep `warnUnreplayable` (history.go:746) for dropped fields. Delete the
   now-false comment at `history.go:230-231` ("replay reads a recorded request and needs no spec").
10. Update `newHistoryReplayCmd`'s `Long` text: replay now needs a spec.

*Documentation — this ships in the same commit, or README contradicts the code:*
11. **README.md:394-399 and AGENT.md:204-207 become wholly false and must be rewritten, not
    amended.** Both paragraphs document `replayableEnv` — *"the names replay will resolve are an
    allowlist … `TALARIA_AUTH_BEARER`, `TALARIA_AUTH_BASIC`, any `TALARIA_AUTH_APIKEY_*`, and the
    variables the selected profile's `auth:` map names"*. After this task **nothing in a stored
    entry is resolved at all**; credentials come from `config.Resolve` against the current spec and
    profile. The replacement states that, which is a simpler rule than the one it deletes. This is
    also what silently resolves finding 18 (see "Side effect worth knowing" at the top of this
    plan) — do not restate the old conflict in the new wording.
12. README.md:387-392 needs the rest of replay's new contract: it re-derives through the spec, so
    it now **requires** a spec (`--spec` or `$TALARIA_SPEC`; the positional argument is the entry
    id, per the trap in step 5); an entry whose `operation_id` is gone fails exit 2; a stored host
    outside the allowed set is refused exit 2 rather than retargeted; a stored body still carrying
    a redaction placeholder is refused; and a `validation` block is emitted as `call` does.
13. Document the **new user-facing surface** this task creates, which no passage covers today:
    `--allow-host` (registered in Task 2, meaningful only now), the `credentials_withheld` envelope
    array, and the one stderr line. AGENT.md:61 currently says only *"`--base-url` overrides the
    spec's server"* — an agent following that will point `--base-url` at a local twin, silently get
    no credential, and have nothing in the docs explaining why. That is the retry-loop-instead-of-
    asking-a-human failure Task 4's **Why** describes, arriving through a different door. Say
    plainly: off-spec host ⇒ credentials withheld, call still runs, exit 0, pass `--allow-host` to
    override. Add `--allow-host` beside `--base-url` in AGENT.md's flag list (line 61) and README's
    (line 174).
14. AGENT.md:177's exit-2 row enumerates its causes — add replay's new refusals to it.

**Verify:** `go test ./...`

**Why:** This is the phase. Everything else is hardening around it. A resolved production
credential is currently delivered to any host `--base-url` names, and to any host a line in a
JSONL file names — the two attacks §5a exists to prevent, both live, both verified empirically.

---

### Task 4: An unsupported security scheme is reported, not hidden — findings 4 and 21

**Depends on:** none

**Test files:**
- `internal/config/auth_test.go` (modify) — `Schemes` emits unsupported schemes
- `cmd/talaria/auth_test.go` (modify) — `auth check` exit 5 and report shape
- `cmd/talaria/call_test.go` (modify) — `call` exit 5 naming scheme and variable
- `internal/e2e/e2e_test.go` (modify) — `auth check` and `call` agree

**Implementation files:**
- `internal/config/auth.go` (modify) — `Supported bool` on `Credential` (auth.go:53-64); `Schemes`
  (auth.go:179) emits unsupported; `credentialFor` (auth.go:288) satisfies an unsupported scheme
  from `TALARIA_AUTH_BEARER`; `auth.go:167` becomes `clierr.CredentialMissing`
- `cmd/talaria/auth.go` (modify) — `authScheme.Supported` (auth.go:24); `satisfied` (auth.go:173)
  stops treating `config.Unsupported` as a no-op
- `README.md` (modify) — rewrite lines ~165-171 and ~254 (finding 21); exit-5 row at line 460 says
  only *"a required security scheme has no credential"*, which no longer covers "declared but
  unsupportable"
- `AGENT.md` (modify) — add unsupported-scheme behaviour under `## Credentials` (~line 108); same
  widening for the exit-5 row at line 180, whose advice (*"tell a human which variable to export"*)
  is the right action for an unsupported scheme too, but only if the report names it

**Red — write failing tests:**
1. On a spec whose only scheme is `oauth2`, `auth check` reports
   `{"scheme":"oauth2","supported":false,"present":false}` and exits **5**. Today: `{"schemes":[]}`
   and exit 0.
2. The same spec with `TALARIA_AUTH_BEARER` exported: `auth check` reports the scheme satisfied by
   that token and exits 0 — DESIGN.md §5's bring-your-own-token rule.
3. `call --dry-run` on that spec with no token exits **5** naming both the scheme and
   `TALARIA_AUTH_BEARER`. Today: exit 2, *"no usable security scheme"*.
4. `call` with `TALARIA_AUTH_BEARER` set actually sends the token.
5. **The agreement test, which is the point:** for a matrix of specs (`oauth2`, `openIdConnect`,
   `mutualTLS`, `apiKey` in an unsupported `in`) × token-set/token-unset, assert `auth check`'s
   verdict and `call`'s exit code never disagree. DESIGN.md:329-331 calls this non-negotiable:
   *"`auth check` never reports a scheme satisfied when the call would refuse it."*
6. A supported scheme still reports `supported:true` — no regression.

**Adversarial — what does hostile or malformed input do here?**
The security-scheme block comes from the spec, which is untrusted.
1. A scheme with an empty `type`, an unknown `type`, or a `type` 1 MB long — reported as
   unsupported, never a crash and never a panic on a nil `scheme`.
2. A scheme whose *name* contains CRLF or shell metacharacters — it reaches the exit-5 error
   message and `auth check` output. Assert it is quoted/escaped and cannot forge a second line of
   structured stderr JSON.
3. An `apiKey` scheme with `in: "path"` or `in: ""` — unsupported, reported, not silently dropped.
4. **`in: Header` with a capital H.** `schemeReason` (`auth.go:225`) tests the location with a
   plain `switch scheme.In { case InHeader, InQuery, InCookie:` at **auth.go:230-231** against the
   lowercase constants (`auth.go:44-46`), while the type test one line up uses
   `strings.EqualFold(scheme.Type, "apiKey")` (`auth.go:229`) and `isHTTP` (`auth.go:387-389`) uses
   `EqualFold` on both fields. So `in: "Header"` falls to `default` and is reported unsupported.
   Decide whether that is right and assert the decision either way.
5. A spec with 1,000 alternative security requirements — the report is bounded and the exit code
   deterministic.
6. `components.securitySchemes` present but null; a `security` entry naming a scheme that is not
   declared at all.

**Green — minimal implementation:**
1. Add `Supported bool` to `config.Credential` (five fields today and no way to express "declared
   but unsupported") and to `cmd/talaria`'s `authScheme` (`auth.go:24` — `Scheme`, `Source`,
   `Present`, same gap). Supportedness is currently encoded structurally, by *omission*: `Schemes`
   and `supportedCredentials` filter on `schemeReason(...) == ""` and leave unsupported schemes
   out, and `Covers` (auth.go:99) reads "absent from the map" as `Unsupported`.
2. `Schemes` (`auth.go:179`) stops filtering at **line 183** (`if schemeReason(name, scheme) == ""`);
   it emits rejected schemes with `Supported: false, present: false`. `supportedCredentials`
   (`auth.go:250`) is the second call site that pre-filters the same way — check it too.
3. **Trap — read this before changing `Schemes`.** `credentialFor` (`auth.go:288`) has **no
   supportedness check of its own**: its `default` branch assumes apiKey and reads `scheme.In` /
   `scheme.Name`. It is safe today only because both call sites pre-filter on `schemeReason`.
   Removing that filter without guarding `credentialFor` silently mints an apiKey credential for an
   OAuth2 scheme. Guard it explicitly.
4. `credentialFor` satisfies an unsupported requirement from `secret.Env(EnvBearer)` (auth.go:21)
   when set.
5. `internal/config/auth.go:167-168` changes from `clierr.Usage` (exit 2) to
   `clierr.CredentialMissing` (exit 5), naming the scheme and `TALARIA_AUTH_BEARER`. Note
   `cmd/talaria/auth.go:155,158` already reports the neighbouring condition with
   `CredentialMissing` — that inconsistency is the finding.
6. `satisfied` (`cmd/talaria/auth.go:173`) — **the bug is line 188, not line 182.** `case
   config.Unsupported:` at auth.go:182 is empty, so `usable` stays false, and `return !usable` at
   auth.go:188 then returns **true**: an operation whose only alternative is OAuth2 is reported
   satisfied and exits 0, while `call` on the same operation exits 2. Give `Unsupported` an
   explicit verdict rather than an empty case plus an inverted default, and update the comment at
   auth.go:165-168 that justifies the no-op.
7. `authPayload` (`cmd/talaria/auth.go:91`, populating at :97-101) iterates only the pre-filtered
   `creds`, so an unsupported scheme produces **no row at all** today. It must now produce one.
8. Rewrite README ~165-171 (*"bring your own token and let a `bearer` scheme carry it"* does not
   describe a reachable workaround — a spec declaring only `oauth2` has no bearer scheme, so say
   what actually happens) and ~254 (*"left out rather than reported missing"* is now forbidden).
   Add an AGENT.md subsection under `## Credentials`.

**The behaviour is already written down — copy it, do not re-derive it.** DESIGN.md:321-328 gives
the exact rule and the exact report shape (`{"scheme":"oauth2","supported":false,"present":false}`,
exit 5), and DESIGN.md:329-331 is the agreement clause. Two cautions: DESIGN.md:327 still reads
*"`call` and `run` exit 5"* — `run` was cut in v0.5, so implement the `call` half only and do not
reintroduce `run`; and the unsupported-scheme shape uses `supported`/`present`, **not** the
`source` field the §4 envelope sketch (DESIGN.md:217) shows, so `authScheme` needs a fourth field
rather than a repurposed one.

**Verify:** `go test ./...`

**Why:** `auth check` exists so an agent can diagnose a broken auth setup blind. A report that says
"nothing required" for a spec that cannot be called is worse than no report — it sends the agent
into a retry loop instead of to the human. Finding 21 ships here because README currently
documents the behaviour this task deletes.

---

### Task 5: A spec-supplied media type cannot inject headers — finding 5

**Depends on:** none

**Test files:**
- `internal/curl/config_test.go` (modify) — the `Content-Type` directive goes through `checkSplit`
- `internal/request/body_test.go` (modify) — a non-token media type fails at bind time
- `internal/canary/canary_test.go` (modify) — a hostile media type on the wire

**Implementation files:**
- `internal/curl/config.go` (modify) — `document.body` (`config.go:284`) calls `checkSplit`
  (`config.go:273`) before the `d.directive("header", "Content-Type: "+…)` at `config.go:289-291`
- `internal/request/body.go` (modify) — `binder.contentType` (~line 136) rejects a non-token media
  type
- `internal/curl/render.go` (modify) — the emitted curl's `-H "Content-Type: …"` at `render.go:129`

**Red — write failing tests:**
1. A spec whose `content:` key is `"application/json\r\nX-Injected: pwned"` fails at exit 2 in
   `request.Build`, naming the media type as malformed. Today it reaches the wire.
2. `BuildConfig` on a `Request` whose `Body.ContentType` carries a CR or LF returns an error and
   **no config document** — the same shape `checkSplit` already produces for headers and cookies.
   This is the second gate; both are needed because `history replay` builds a `Request` without
   going through `binder.contentType`.
3. Wire-level: against a capture socket, a hostile media type never produces a second header line.
   Verified in the finding as producing `Content-Type: application/json\r\nX-Injected: pwned\r\n`.
4. The emitted "portable reproduction" curl carries no raw newline inside a quoted argument.
5. Ordinary media types (`application/json`, `application/vnd.api+json`,
   `text/plain; charset=utf-8`) still pass. **Do not break the `; charset=` form** — a naive token
   check will.

**Adversarial — what does hostile or malformed input do here?**
`ContentType` originates at `internal/request/body.go` from a key in the spec's `content:` map:
untrusted by the product's own premise, and this is the one header path that skips `checkSplit`,
inside the component that holds resolved credentials.
1. A bare `\r`, a bare `\n`, and a **double CRLF** — the last terminates the header block and
   smuggles a second request on the same connection. Assert each is refused.
2. A NUL byte, and a media type that is entirely whitespace or empty.
3. A media type 1 MB long.
4. Non-ASCII / UTF-8 in the media type.
5. A media type that is a valid token but names a header (`application/json: x`).
6. The same value arriving via `history replay` rather than a spec — assert the `internal/curl`
   gate catches it even when the `internal/request` gate was never run.

**Green — minimal implementation:**
1. In `document.body` (`config.go:284`), call
   `checkSplit("header", "Content-Type", req.Body.ContentType)` and return its error before writing
   the directive at line 290. `checkSplit` has exactly two call sites today — `config.go:221`
   (headers) and `config.go:250` (cookies) — and line 290 is the only `d.directive("header", …)` in
   the file that bypasses it.
2. In `binder.contentType` (`body.go:136`), reject a media type that is not `type/subtype` with
   optional `; parameter=value` — allow the parameter form explicitly. Note the function has **no
   validation of any kind** today: `SplitsRequest` is called at `build.go:294`, `:336` and `:394`,
   and none of those sees `Body.ContentType`. The unguarded branch is the spec one
   (`rb.Content[0].ContentType`, body.go:148); the `--header` branch at body.go:137-141 is already
   CRLF-checked.
3. Apply the same guard on the render path so the emitted curl is safe to print. Both the config
   document (`config.go:289`) and the emitted curl (`render.go:129`) gate on the same `hasHeader`
   helper (`render.go:136`), so the two paths already agree — put the guard where they both reach
   it rather than writing it twice.

**Note while you are in `contentType`:** body.go:139 returns `h.Value.String()` — the *redacted*
display form — for a user-set `Content-Type`. That path is inert today because `config.go:289`'s
`hasHeader` check suppresses emission when the user set the header. Do not make it live by
accident; if you touch it, add a test.

**Verify:** `go test ./internal/curl/... ./internal/request/... ./internal/canary/...`

**Why:** Header injection inside the one component that holds resolved credentials. A double CRLF
lets a hostile spec smuggle a request on the connection carrying the token. The comment at
`config.go:265-272` already explains why `escapeDirective` is not the answer here — curl un-escapes
`\r\n` back to two bytes on the wire — and this path is the one that never asks.

---

### Task 6: Refactor pass — tasks 1–5

**Depends on:** Task 5

**Deliverable is negative.** No new behaviour, no new files, no new tests beyond ones that replace
several others. The suite is green before and after, and the diff is net-negative in lines.

1. Read `.ralph/refactor-backlog.md` first — build iterations record structural problems there as
   they hit them, because each one is visible only from inside the task that caused it. That file
   is the work list; this task drains it. Delete each entry as you resolve it, and leave anything
   you deliberately did not do, with one line on why. If the file does not exist, no iteration
   recorded anything — carry on, do not treat it as an error.
2. Read every file touched since the start of the phase.
3. Consolidate types and helpers that were duplicated because separate iterations could not see
   each other's work. Named suspects for this block: URL/host parsing added by tasks 1, 2 and 3
   (`substituteServer`, `HostSet`, and whatever the replay path grew) versus the existing
   `request.IsHTTPScheme` (request.go:205) and `request.Userinfo` (request.go:230); the
   media-type guard if Task 5 wrote it twice rather than once.
4. Move logic that accumulated in `cmd/talaria` down into the package that owns it. Task 3 is the
   largest command-layer change in the phase — host-set assembly and replay reconstruction both
   belong in `internal/request`, not in `history.go`. A thin entrypoint is the goal; measure it.
5. **Delete `validateWith` (`cmd/talaria/call.go:380`)** — zero callers, and its doc comment
   describes the deleted `run` command. While there, clear the other stale `run` references at
   `call.go:224`, `call.go:392`, `root.go:68`, `root.go:72-74`, `history.go:406`,
   `internal/canary/canary_test.go:473-475` (a doc block reasoning about a `--report` surface and
   about what `run` accepts) and `internal/output/output.go:49` (`parseFormat`'s comment explains a
   "rejected `--report`" case no caller can produce). Delete
   commented-out code and any comment asserting a property no test enforces — either write the test
   or delete the claim. Task 22 is the systematic sweep; do not pre-empt it, just clean what you
   touched.
6. Record every pattern you consolidated in `CLAUDE.md`, so later iterations follow it instead of
   re-inventing it. In particular the host-binding rule and where the host set is assembled.

**Verify:** `go test ./...` green, and report the net line delta in the commit message.

---

### Task 7: A request body carrying a secret does not print on stdout — finding 6

**Depends on:** Task 3

**Test files:**
- `cmd/talaria/call_redact_test.go` (modify) — request body redacted in the envelope
- `internal/curl/render_test.go` (modify) — emitted curl references rather than inlines
- `internal/canary/canary_test.go` (modify) — canary in a `--body @file` never reaches stdout

**Implementation files:**
- `internal/request/request.go` (modify) — `Body` gains its origin (`BodyArgv`, `BodyFile`,
  `BodyStdin` + path)
- `internal/request/body.go` (modify) — `binder.body` records which of the three sources `--body`
  named
- `cmd/talaria/call.go` (modify) — apply `redactors.Response.Body` to `view.Request.Body` in
  `callPayload`
- `internal/curl/render.go` (modify) — `bodyArgs` emits `--data @file` / `--data @-` /
  `--data-raw <inline>`

**Red — write failing tests:**
1. `--body '{"refresh_token":"CANARY"}'` written to a file, called with `--body @file`: stdout
   shows `{"refresh_token":"<redacted>"}` while today it shows the canary. The finding verified the
   exact asymmetry — history redacts it, stdout does not:
   `stdout: "body":"{\"refresh_token\":\"CANARY\"}"` vs
   `history: "data":"{\"refresh_token\":\"<redacted>\"}"`.
2. `--body @file` emits `--data @file` in the reproduction curl, not the file's contents.
3. `--body -` emits `--data @-`.
4. An argv-supplied `--body '{"a":1}'` is still **inlined**, because it is already in the agent's
   hands (DESIGN.md §3.4, lines 162-166).
5. The executed request is byte-identical in all three cases — only the *emitted* form differs.
   `internal/e2e/e2e_test.go:567` already proves emitted-equals-executed; keep that test passing.

**Adversarial — what does hostile or malformed input do here?**
The body comes from a file or stdin — a human or CI, not the agent — so the agent reading it off
stdout is the exact asymmetry §1 exists to prevent.
1. A file path containing a space, a quote, a newline, or a `$` — it lands inside a shell-quoted
   argument in the emitted curl. Assert it cannot break out of quoting.
2. A file path that is `-`, or begins with `@`, or is `/dev/stdin`.
3. A body that is invalid UTF-8, or 100 MB, or a single 10 MB JSON string.
4. A body whose *content* looks like a redaction placeholder (`<redacted>`) already — assert the
   redactor is idempotent and does not corrupt it further.
5. A binary body must not be mangled by redaction — assert the executed bytes are untouched;
   redaction applies only to the displayed copy.

**Green — minimal implementation:**
1. Add an origin to `request.Body`; `binder.body` sets it from which branch of `bodyData` ran.
2. Thread the redactor into `callPayload` (`call.go:453`) and apply `redactors.Response.Body` to
   `view.Request.Body` only — never to `req.Body.Data`. `callPayload` takes only
   `(req, resp, result)` today and never receives a redactor; the assignment it makes is
   `view.Request.Body = string(req.Body.Data)` at **call.go:464-466**, raw. `redactors` is built at
   `call.go:164` and used at 177 (history) and 182 (the *response* view), which is exactly the
   asymmetry. Note `newRedactors` is *also* called a second time inside `buildRequest`
   (`call.go:305`) for the same invocation — consolidating that is refactor-pass work, record it in
   `.ralph/refactor-backlog.md` rather than widening this task.
3. `bodyArgs` (`render.go:123`, emitting `--data-raw` at `render.go:133`) switches on origin. Update
   the comment at `render.go:117-122`, which explains why `--data-raw` is always used; that
   rationale now applies only to the argv case.
4. **Also fix the two false comments this exposes**, or Task 22 will find them:
   `call.go:449-452` claims the view is "built from the request's *redacted* representation
   throughout", which is false for `Body`; and `render.go:4-7` claims an emitted command "never
   calls `SecretRef.Resolve`" and is "useless to exfiltrate", which holds for headers, cookies and
   query but not for the body — `request.Body.Data` is raw `[]byte` and never becomes a
   `request.Value` at all.

**Note:** `document.body` in `internal/curl/config.go` already splits inline (`data-raw`, line 298)
from temp-file (`data-binary @path`, line 309) via `inlinable()`. The *executed* request must keep
that behaviour unchanged; only the *emitted* form changes. **No canary test currently injects a
credential into a request body** — that blind spot is why this shipped.

**Verify:** `go test ./...`

**Why:** `buildRequest` in `cmd/talaria/call.go` states the rule this violates verbatim: *"A
pattern that hides a value in the permanent artifact but not on the stdout an agent reads has the
firewall backwards."* §3 principle 0 names stdout first among the surfaces a credential must never
reach.

**Built 2026-08-03, with one divergence.** The emitted form is `--data-binary @file` /
`--data-binary @-`, not the `--data` this section named. `--data` strips newlines and carriage
returns out of a file, so the reproduction would send different bytes than the call — the exact
failure the rationale at `render.go:117-122` already described, and the reason the config document
uses `data-binary` for its temp file. Proven rather than asserted: a `--body @file` case carrying a
pretty-printed body was added to `TestThePreviewedCommandSendsWhatTheCallSends`
(`internal/e2e/e2e_test.go`), and it fails with `--data` on both the body and Content-Length.
One case the section's adversarial list raised needed a decision: `--body @-` names a file
literally called `-`, which curl reads as stdin after the `@`, so `curl.fileRef` emits `@./-` for
that path alone.

---

### Task 8: `internal/corpus` stops importing `internal/curl` — finding 30

**Depends on:** Task 3

**Test files:**
- `internal/e2e/boundary_test.go` (modify) — guard `corpus → curl` against the real import graph
- `internal/corpus/entry_test.go` (modify) — `NewEntry` takes a local observation struct

**Implementation files:**
- `internal/corpus/entry.go` (modify) — `NewEntry` (line 151) takes
  `Observed{Status, Headers, Body, TimingMS}` instead of `*curl.Response`; drop the
  `internal/curl` import
- `cmd/talaria/call.go` (`recordCall`, call.go:251), `cmd/talaria/history.go` (call site at
  history.go:225) — fill `Observed` from `curl.Response`

**Red — write failing tests:**
1. A test asserting `go list -deps ./internal/corpus` does not contain `internal/curl`. **It passes
   today only because nothing checks it** — `boundary_test.go`'s `shared` list (`:16-19`) is
   `{internal/operation, internal/validate}` and `corpus` appears only in the `forbidden` list
   (`:32-36`), never as a checked source. Write the failing guard first. The existing test
   (`TestSharedPackagesDoNotDependOnTheExecutorOrTheTwin`, `:45`) already shells out to
   `go list -deps`, so the check is transitive rather than a source-level import scan — reuse
   `dependenciesOf` (`:75-94`) and keep the anti-vacuity guard at `:58-61`, which fatals if a
   package is missing from its own dep closure.
2. `NewEntry` accepts an `Observed` and produces the same `Entry` the `*curl.Response` form did —
   port the existing round-trip assertions.
3. A nil observation (dry run, connection failure) still records an entry with no response, which
   `entry.go:48-50` documents as a supported case.

**Adversarial — what does hostile or malformed input do here?**
None new — this is a signature change, not a new input path. The existing hostile-input coverage
for `Body.Bytes` and redaction must keep passing unchanged; assert that explicitly rather than
assuming it.

**Green — minimal implementation:**
1. Define `Observed` in `internal/corpus`, mirroring the fields `NewEntry` actually reads.
2. Change `NewEntry`'s signature and delete the `internal/curl` import.
3. Fill `Observed` at the two `cmd/talaria` call sites.
4. Extend `boundary_test.go` so `corpus` has its own forbidden set including `internal/curl`. The
   test's loop iterates only `shared` (boundary_test.go:16-19); `corpus` appears solely as a
   forbidden *target* (:32-36), which is exactly why it passes today. Fix the prose too — it reads
   as though `corpus` is already constrained. **Do not re-add `internal/gen` to `shared`** even
   though DESIGN.md:285 still names it: the package was deleted, and a guard naming a package that
   does not exist proves nothing.

**Verify:** `go test ./...`

**Why:** §5 says *"`corpus` backs both `history` and the twin"* and *"Twin's serve side uses
`net/http` directly — curl is only for outbound calls."* Left alone this silently drags the
subprocess executor into the twin at Phase 6. CLAUDE.md requires the guard land in the same commit
as the fix.

---

### Task 9: History entries are size-bounded on read — finding 13

**Depends on:** Task 8

**Test files:**
- `internal/corpus/store_test.go` (modify) — oversized file and oversized line
- `internal/corpus/entry_test.go` (modify) — oversized decoded body

**Implementation files:**
- `internal/corpus/store.go` (modify) — bound the read in `Read` (line 181), `storedIDs` (160) and
  `trim` (247)
- `internal/corpus/entry.go` (modify) — `Body.Bytes` (line 119) caps the decode at `MaxBody`

**Red — write failing tests:**
1. A `history.jsonl` whose single line carries a 200 MB `data` field: `history`, `history show` and
   `history replay` each **skip that entry** and succeed on the rest. Today `os.ReadFile` is
   unbounded and the process dies. §5a requires *"a corrupt or hostile entry fails that entry,
   never the process."*
2. A base64 `data` that decodes to more than `MaxBody`: `Body.Bytes` (`entry.go:119-133`) returns
   an error rather than allocating. `MaxBody` (`entry.go:34`) is enforced only at **write** today,
   in `newBody` at `entry.go:258-261`, which truncates and sets `Truncated` rather than refusing.
3. A file larger than the whole-store bound is refused with a clear error, not an OOM.
4. Entries under the bound are unaffected — no regression in the existing store tests.

**Adversarial — what does hostile or malformed input do here?**
The history file is untrusted: it may have been written by another project, copied from another
machine, or hand-edited. It is also user-writable by design.
1. A single line of 500 MB with no newline — the reader must not buffer it whole looking for a
   delimiter.
2. A base64 payload that is a decompression-style amplifier (short stored, huge decoded) — bound
   the **decoded** size, not just the stored string.
3. A file of 10 million empty lines.
4. A file that is a symlink to `/dev/zero`, or a FIFO — assert the read does not block forever.
5. A line that is valid JSON but nests 10,000 levels deep.
6. Assert the **failure is entry-level**: an oversized entry in the middle of the file must not
   prevent the entries after it from being read.

**Green — minimal implementation:**
1. Add a package-level `maxStoreBytes` and read through an `io.LimitReader`, or `os.Stat` first and
   refuse past the cap with a clear error.
2. Scan lines with a bounded `bufio.Scanner` buffer; a line over cap is skipped like an unparseable
   one already is at `store.go:202`.
3. In `Body.Bytes`, check the encoded length against `MaxBody` before decoding, and the decoded
   length after.

**Verify:** `go test ./internal/corpus/... ./cmd/...`

**Why:** §5a's replay table requires bounded, type-checked reads. Type-checking is handled; size is
not, on any of three read paths. A copied history file currently turns every `history` command into
a process-level failure where the design mandates an entry-level one.

---

### Task 10: A remote spec read is bounded — finding 9

**Depends on:** none

**Test files:**
- `internal/spec/source_test.go` (modify) — oversized body, redirect behaviour

**Implementation files:**
- `internal/spec/source.go` (modify) — `fetch` (`source.go:111`) wraps `resp.Body` in
  `io.LimitReader`; the unbounded `io.ReadAll` is at `source.go:127`

**Red — write failing tests:**
1. An `httptest` server streaming past the cap: `Load` returns `clierr.SpecLoad` naming the limit,
   and the process survives. Today `io.ReadAll` is unbounded — `grep LimitReader` over the repo
   returns zero hits.
2. A spec at exactly the cap loads; one byte over does not.
3. An oversized fetch writes **nothing** to the cache. `writeCache` (`source.go:156`) has no size
   or count bound, and `TestLoaderDoesNotCacheFailedFetches` already asserts the failure case —
   extend it.
4. A 2 MB spec (the size DESIGN.md cites for a real `swagger.json`) still loads.

**Adversarial — what does hostile or malformed input do here?**
The spec URL is untrusted by the product's premise, and §1 sells pointing it at any API's doc.
1. A server that sends a truthful small `Content-Length` and then streams gigabytes — bound the
   **actual read**, never trust the header.
2. A server that never sends `Content-Length` (chunked).
3. **Redirects.** `client.Get` follows up to 10 by default, cross-scheme and cross-host, and the
   result is cached under the *original* URL's hash (`cachePath`, `source.go:138`, is
   `sha256(url)` only). Assert a redirect chain is bounded and that a redirect to a non-http(s)
   scheme is refused. `grep CheckRedirect` returns zero hits today.
4. A server that sends one byte per second for 30 seconds — `fetchTimeout` (`source.go:23`, applied
   at `source.go:114`) bounds the clock but not the volume; confirm both bounds exist
   independently.
5. A `Content-Encoding: gzip` body that decompresses far past the cap.

**Green — minimal implementation:**
1. Add `maxSpecBytes` (a few tens of MB — DESIGN.md cites a 2 MB real-world `swagger.json`).
2. `io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))`; over cap → `clierr.SpecLoad`.
3. Set `CheckRedirect` on the client to bound the chain and refuse non-http(s) hops.

**Verify:** `go test ./internal/spec/...`

**Why:** A hostile or compromised spec endpoint currently kills the process with no structured
error and no exit code an agent can branch on — and the bytes are written to a cache that never
expires.

---

### Task 11: The config document's credential buffer is actually zeroed — finding 29

**Depends on:** none

**Test files:**
- `internal/curl/config_test.go` (modify) — the builder's own buffer is zeroed, not just the copy

**Implementation files:**
- `internal/curl/config.go` (modify) — `document.b` becomes a `[]byte` the document owns;
  `BuildConfigWith` lines 120-121; `discard` (370); `cleanupWith` (382); doc comment lines 85-88

**Red — write failing tests:**
1. After `cleanup()`, the document's **own** backing buffer contains no byte of the canary — not
   only the returned `config` slice. `TestBuildConfigCleanupZeroesTheDocument`
   (`config_test.go:376`) asserts only the returned slice today, which is why this passes while the
   defect is live. Reach the builder's buffer through a test-only accessor or by restructuring so
   the buffer is addressable.
2. `discard()` (the failure path at `config.go:116`) also zeroes — a build that failed part-way
   through has already written the credential into the buffer.
3. `cleanup` remains safe to call more than once, and non-nil even when `BuildConfig` fails
   (`config.go:87-88` promises both).

**Adversarial — what does hostile or malformed input do here?**
None — this touches no external input. It is a memory-hygiene fix inside the one component §5a
designates as the sole holder of resolved secrets. Stated, not omitted.

**Out of scope but record it in `.ralph/refactor-backlog.md`:** `tempFile` (`config.go:327`) writes
body bytes to a 0600 file that `cleanup` (373) `os.Remove`s **without overwriting**. Same class of
defect, different medium, and not one of the 25 findings — do not fix it here, but write it down.

**Green — minimal implementation:**
1. Replace `strings.Builder` (`config.go:129`) with a `[]byte` field on `document`;
   `directive`/`flag` append to it.
2. `BuildConfigWith` copies out, then `clear()`s the document's slice rather than `Reset()`ing a
   `strings.Builder` (whose `Reset` sets `addr, buf = nil, nil` without zeroing — the defect).
   Note `doc.b.String()` at line 120 also produces an **immutable string** aliasing the builder's
   array, and the `[]byte(...)` conversion makes a second copy: today there are at least two
   un-zeroable copies on the heap, plus every intermediate array the builder abandoned as it grew.
   Appending into an owned `[]byte` is what removes them, not a tidier `Reset`.
3. `discard` (line 370) and `cleanupWith` (382) both `clear()`.
4. **The error path returns the wrong cleanup.** `config.go:117` returns `doc.cleanup` (temp files
   only) after `doc.discard()` at `:116`, never `cleanupWith` — and `discard` (`config.go:370`) is
   just `d.b.Reset()`, which zeroes nothing. A build that failed *after* writing a resolved
   credential into the buffer therefore scrubs nothing at all. Fix that too — it is the same defect
   on the path nobody tests.
5. **Either** make the comment at `config.go:85-88` true, **or** narrow it to what the code does.
   The current wording — *"the resolved values do not linger in a buffer the rest of the process
   can still reach"* — is precisely the CLAUDE.md-forbidden pattern of a comment asserting an
   invariant no test enforces.

**Verify:** `go test ./internal/curl/...`

**Why:** `strings.Builder.String()` aliases `b.buf` and the `[]byte` conversion copies, so
`cleanupWith` zeroes the copy while the original heap buffer — holding the resolved bearer token or
`user:password` — stays readable until the GC happens to reuse that memory.

---

### Task 12: Refactor pass — tasks 7–11

**Depends on:** Task 11

**Deliverable is negative.** No new behaviour, no new files, no new tests beyond ones that replace
several others. The suite is green before and after, and the diff is net-negative in lines.

1. Read and drain `.ralph/refactor-backlog.md`. Tasks 7 and 11 were explicitly told to record
   entries there (the double `newRedactors` construction at `call.go:164`/`call.go:305`, and
   `tempFile`'s un-overwritten 0600 body file at `config.go:327`) — resolve or restate each with a
   reason. An absent file means nothing was recorded, not an error.
2. Read every file touched since Task 6.
3. Consolidate types and helpers duplicated across this block. Named suspects: bounded-read helpers
   written separately in `internal/corpus` (Task 9) and `internal/spec` (Task 10) — they are the
   same shape and should be one; redaction plumbing added by Task 7 versus the existing
   `newRedactors`.
4. Move logic that accumulated in `cmd/talaria` down into the package that owns it. Task 7 and
   Task 8 both edited `call.go`. A thin entrypoint is the goal; measure it.
5. Delete commented-out code, superseded helpers, and any comment asserting a property no test
   enforces — either write the test or delete the claim. Task 22 is the systematic sweep; do not
   pre-empt it, just clean what you touched.
6. Record every pattern you consolidated in `CLAUDE.md`.

**Verify:** `go test ./...` green, and report the net line delta in the commit message.

---

### Task 13: The canary gate covers the validation error path and profile auth — findings 19 and 20

**Depends on:** Task 7

**Test files:**
- `internal/canary/canary_test.go` (modify) — a new stage and a new mechanism
- `internal/canary/surfaces.go` (modify, if a surface must be added)

**Implementation files:** none — this task is tests. If a leak is found, fix it and say so.

**Red — write failing tests:**
1. **Finding 19 — the exit-4 stage.** `TestErrorPathsDoNotLeakTheCredential`'s `stages` table
   (`canary_test.go:388-438`) has seven stages — spec load, operation lookup, parameter binding,
   mutation gate, body read, curl exec, history index — and **no validation-failure stage**. Add
   one: an operation returning a schema-violating body with a credential set, called with
   `--fail-on-error`, asserting exit 4 and scanning stdout, stderr and `validation.errors[]` for the
   canary. Then delete the stale comment at `canary_test.go:385-387` ("response validation is not
   built yet (plan tasks 26 and 27). Whoever adds `--fail-on-error` adds the case") — response
   validation **is** built: the flag is registered at `cmd/talaria/call.go:215-216`, the decision
   block is `call.go:189-196` and `callFailure` (`call.go:423`) returns `clierr.Validation`, exit 4.
   The `run --report junit` half of this finding is gone with `run`; do not reintroduce it.
2. **Finding 20 — the profile mechanism.** `mechanisms` (`canary_test.go:99-151`) has five entries
   — bearer, basic, apikey-header, apikey-query, apikey-cookie — and every one sets a
   `TALARIA_AUTH_*` variable. Add a mechanism whose `env` sets an **arbitrary** variable name and
   whose harness writes `profiles: {p: {auth: {bearerAuth: "${MY_TOKEN}"}}}`, driven through the
   same `call` / `history` / `history show` / `replay` sequence as the env-var mechanisms.
3. Both new cases must be run against **every** output surface `canary.Formats()` enumerates, so a
   fourth format inherits them automatically. `TestNoAuthMechanismLeaksIntoAnyOutputSurface`
   (`canary_test.go:332-379`) sweeps eight runs × two streams — `call --dry-run`, `call`,
   `auth check`, `history`, `history show 1`, `history replay 1`, `describe`, `list` — plus
   `h.written()` (`:258-274`), which walks the history and cache dirs. The config dir is
   deliberately excluded (`:254-257`); leave it that way.
4. **A third gap, found while surveying and worth closing here:** no canary test injects a
   credential into a request **body**. That is the blind spot behind finding 6 (Task 7). Add a
   mechanism or stage that puts the canary in a `--body @file` and scans every surface.

   **This one needs the harness widened first.** The canary package has its *own* recorder —
   `recordingServer`/`newServer` at `canary_test.go:283-323`, whose `recordedRequest` is
   `{Header, Query, Cookies}` with **no `Body`, no `Method`, no `Path`**. It cannot see a body
   today, so a body case asserts nothing until you add the field. Do not confuse it with
   `cmd/talaria/call_test.go:44-53`, which is a different `recordedRequest` in a different package
   that *does* capture `Body`.
5. **`needles()` has a dead branch.** `canary.Value` returns `label + "-" + hex(...)`, which is
   URL-safe, so `url.QueryEscape(value) == value` and the `percent` needle at `surfaces.go:149-151`
   is **never added for any canary this package generates**. A credential that reached a URL field
   percent-encoded would not be caught. Either make `Value` produce a value that needs escaping, or
   scan for the percent form unconditionally — and say which you chose.

**Adversarial — what does hostile or malformed input do here?**
This task *is* the adversarial suite; the hostile input is the canary itself, reaching surfaces it
must not.
1. The validation error path quotes the request — §5a names *"validation errors quoting the
   request"* as a leak channel and adds *"Test explicitly — error paths are where redaction bugs
   live."* Assert the canary is absent from the error message, not merely from the happy-path
   envelope.
2. Scan **stderr as well as stdout**, plus the history store on disk, for both new cases.
3. For the profile mechanism, assert the canary is absent from the config file echo, from any error
   naming the profile, and from `auth check` output.
4. The profile path is the one that widened `replayableEnv` (finding 18), and Task 3 deleted that
   function — assert a replay under this mechanism resolves its credential from the **current**
   profile, never from the stored entry.

**Green — minimal implementation:**
The tests are the deliverable. If either new case finds a live leak, fix it in the owning package
and note the fix in the commit message.

**Verify:** `go test ./internal/canary/...` then `go test ./...`

**Why:** §5a makes this suite the release gate: *"A CI suite injects canary secrets through every
auth mechanism and greps every output surface."* Two of the mechanisms and one of the error paths
were never covered, which is a direct reason the suite passed with eight critical findings live.

---

### Task 14: `--output tsv` emits structurally valid rows — finding 8

**Depends on:** none

**Test files:**
- `internal/output/render_test.go` (modify) — cells containing tab, CR, LF

**Implementation files:**
- `internal/output/render.go` (modify) — TSV join at line 104; pretty joins at 123 and 126

**Red — write failing tests:**
1. Two operations, one with summary `"line one\nline two\twith tab"`, produce exactly **two** TSV
   rows with equal column counts. Today they produce three rows with differing counts, so `cut -f3`
   silently returns garbage with no way for the caller to detect it.
2. Every TSV row a payload emits has the same number of columns as every other.
3. A cell containing a literal `\t` round-trips to something a consumer can distinguish from a
   column break — pick escaping or stripping, and assert the choice.
4. The pretty renderer stays column-aligned with the same hostile cell — `text/tabwriter` treats
   `\t` as a cell terminator and re-partitions the whole column block.
5. `TestTSVRendersTabSeparatedRows` (`render_test.go:72`) keeps passing for clean ASCII cells.

**Adversarial — what does hostile or malformed input do here?**
Every cell is spec-derived or read back from the history file. Nine call sites feed them:
`cmd/talaria/list.go:105` (`op.Summary`), `search.go:66` (`r.Summary`, `r.Name`, `r.Where`),
`history.go:290-299` and `history.go:330-345` (recorded URLs, headers and **raw bodies** —
`bodyLine` returns `body.Data` verbatim), `uses.go:56`, `describe.go:192` (spec descriptions
routinely contain real newlines), `call.go:469-486`, `auth.go:102`.
1. A `\r` alone — in a terminal it overwrites the rendered line, hiding what was printed.
2. A cell containing ANSI escape sequences.
3. A recorded response body of 64 KiB of binary landing in a `history show` cell.
4. A cell containing the field separator repeatedly (1,000 tabs).
5. `fitSummaries` (`list.go:120`) truncates by rune count for pretty only and does not run for TSV —
   assert the interaction: a truncated cell must still be escaped.
6. NUL bytes and invalid UTF-8.

**Green — minimal implementation:**
1. Add a cell sanitiser in `internal/output` applied by both the TSV and pretty renderers.
2. Escape (`\t`, `\r`, `\n`) rather than strip, so information is not silently lost, and document
   the choice in README beside the *"`tsv` prints bare tab-separated rows … for `cut` and `awk`"*
   claim it makes honest.

**Verify:** `go test ./internal/output/... ./cmd/...`

**Why:** The current output breaks the contract README states. Worse, it breaks it *silently* — a
caller running `cut -f3` gets a wrong answer with no error, which is the failure mode this tool
exists to eliminate.

---

### Task 15: Ctrl-C works — finding 14

**Depends on:** none

**Test files:**
- `cmd/talaria/root_test.go` (create or modify) — signal disposition restored after the first signal
- `internal/request/body_test.go` (modify) — stdin read honours the context

**Implementation files:**
- `cmd/talaria/root.go` (modify) — `run` at line 201, `signal.NotifyContext` at **206** (the
  findings report says 207)
- `internal/request/body.go` (modify) — `stdinBody` at line 95, `io.ReadAll` at 102

**Red — write failing tests:**
1. After the first SIGINT, the handler is uninstalled so a **second** signal terminates by default.
   Today `signal.NotifyContext` leaves the registration live for the process lifetime: its
   goroutine exits, every later SIGINT/SIGTERM is delivered to a buffered channel and discarded,
   and default termination is disabled for the whole run.
2. `--body -` with a stdin that never closes returns when the context is cancelled. Today
   `io.ReadAll` blocks: **Ctrl-C does nothing and `kill -TERM` does nothing — only Ctrl-D or
   `kill -9` gets out.** Under systemd or CI a SIGTERM shutdown hangs until the SIGKILL timeout.
3. A normal `--body -` from a closed pipe still reads to EOF — no regression.

**Adversarial — what does hostile or malformed input do here?**
Stdin is outside our control: a terminal, a stalled pipe, another process.
1. A pipe that delivers one byte per second forever — cancellable.
2. A stdin that is a terminal with no input.
3. A stdin delivering 10 GB — note `stdinBody` has **no size bound** either; decide whether to add
   one here and say which, or record it in `.ralph/refactor-backlog.md` as deliberately deferred.
4. `os.ReadFile` in `fileBody` (line 113) is unbounded too, and a FIFO path blocks it forever —
   same decision, same recording.
5. Two signals arriving within microseconds of each other.
6. SIGTERM while curl is mid-exec — assert the child is still reaped and temp files still removed.

**Green — minimal implementation:**
1. In `run` (root.go:201), after `signal.NotifyContext` (root.go:206), add
   `go func() { <-ctx.Done(); stop() }()` so the first signal restores default disposition and a
   second one terminates. `runContext` (root.go:217) already exists so tests can cancel without
   signalling the test process — use it.
2. Thread `context.Context` into `binder.body` → `bodyData` → `stdinBody`, and read stdin under it
   (a goroutine feeding a channel, selected against `ctx.Done()`). `Inputs` already carries
   `Stdin`; add the context beside it.

**Verify:** `go test ./cmd/... ./internal/request/...`

**Why:** The worst of the hang set. A tool whose stated principle is *"Never prompt. Never page. No
interactivity, ever."* currently cannot be interrupted at all on a reachable path.

---

### Task 16: curl subprocesses cannot hang the process — findings 10 and 15

**Depends on:** none

**Test files:**
- `internal/curl/config_test.go` (modify) — a basic credential with no colon is refused
- `internal/curl/exec_test.go` (modify) — the version preflight is bounded

**Implementation files:**
- `internal/curl/config.go` (modify) — `document.auth` at `config.go:214`, the `EncodeBasic` branch
  at `config.go:225` and the `d.directive("user", …)` it guards at `config.go:226`
- `internal/curl/version.go` (modify) — `preflight(path string) error` at `version.go:40` gains a
  context; the `sync.Once` that memoises it process-wide is at `version.go:30-31`
- `internal/curl/exec.go` (modify) — the sole caller, line 62, already holds a `ctx`

**Red — write failing tests:**
1. `TALARIA_AUTH_BASIC=alice` (no colon) fails with `clierr.CredentialMissing`
   (`"$TALARIA_AUTH_BASIC must be user:password"`) **before** any directive is written, and
   **without echoing the value** — matching the existing "its value is not echoed" convention. Today
   curl reads the missing password from `/dev/tty`, not the config pipe, and the call blocks: the
   finding measured 32.1 s wall clock, then a misleading *"curl outlived its 30s timeout"* that
   sends an agent looking for a slow API.
2. `TALARIA_AUTH_BASIC=user:pass` still works, as does `user:` (an empty password is legal) and
   `user:pass:word` (curl splits at the first colon).
3. A `curl` on `PATH` that hangs on `--version` fails within a short bounded deadline instead of
   wedging the process. Today `exec.Command(path, "--version").Output()` takes no context, no
   `WaitDelay` and no process group, while every other curl in the tool is bounded by
   `execCtx` + `WaitDelay` + `Setpgid`.
4. Cancelling the caller's context cancels the preflight.

**Adversarial — what does hostile or malformed input do here?**
`TALARIA_AUTH_BASIC` comes from the environment; `curl` comes from `PATH`, which the agent's own
process may have influenced.
1. A basic value that is empty, is only `:`, or contains a CRLF — assert each is refused and none
   is echoed in the error.
2. A basic value 1 MB long.
3. A `curl` that writes gigabytes to stdout on `--version` — `Output()` buffers it all; bound it.
4. A `curl` that exits non-zero, or is a script that forks a grandchild holding the pipe open.
5. The `sync.Once` in `preflight` caches the **first** result process-wide, so a transient failure
   poisons every later call in the same process. Decide whether to keep that and say why.
6. A `PATH` entry that is a directory, or a `curl` that is not executable.

**Green — minimal implementation:**
1. In `document.auth`, before `d.directive("user", …)`, reject a resolved basic credential with no
   `:` via `clierr.CredentialMissing`, naming the variable and not the value.
2. Change `preflight(path)` to `preflight(ctx, path)` using `exec.CommandContext` with an
   independent short deadline (5 s); pass `ctx` from `ExecuteWith` at `exec.go:62`.
3. Apply the same colon guard on the render path (`render.go:88-91` emits `-u` with no shape check).

**Verify:** `go test ./internal/curl/...`

**Why:** Both violate §3.1's *"Never prompt. Never page. No interactivity, ever."* A wrapper script
on `PATH` blocking on an NFS stall wedges `talaria call` before it has done anything, and per
finding 14 SIGTERM will not end it.

**Built, with one deviation.** Green step 3 — "apply the same colon guard on the render path" — is
not implementable and was not implemented. `headerArgs` (`render.go:88`) emits
`-u "$TALARIA_AUTH_BASIC"` built from `h.Value.Ref().Symbolic()`; `EncodeBasic` is only ever set by
`request.Secret`, so the value is always a ref and `Render` never calls `Resolve`. There is no
resolved string on that path whose shape a guard could read, and adding one would be the §5a
firewall breach the whole design exists to prevent. The consequence is stated rather than fixed:
`--dry-run` prints a command for a malformed `TALARIA_AUTH_BASIC` and exits 0 where the real call
exits 5, the same way it cannot tell an expired token from a live one. The guard is `basicPair` in
`internal/curl/firewall.go`, one seam called from `document.auth`, so if a resolving render path is
ever added it has a function to call.

Decision on adversarial item 5, the process-wide `sync.Once`: dropped. It cached the first result
for the life of the process, and now that `preflight` takes the caller's context a cancelled
Ctrl-C would have poisoned every later call. Replaced by `preflightCache`, keyed by path and
holding only verdicts about a binary that actually answered.

---

### Task 17: The history lock has a deadline — finding 17

**Depends on:** Task 9

**Test files:**
- `internal/corpus/lock_test.go` (create) — a held lock fails within a bounded time

**Implementation files:**
- `internal/corpus/lock_unix.go` (modify) — `lock(path string) (func(), error)` at
  `lock_unix.go:23`; the `LOCK_EX` with no `LOCK_NB` at `lock_unix.go:36`; release at `:44`
- `internal/corpus/store.go` (modify) — `Append` at 99 passes a context

**Red — write failing tests:**
1. With the lock held by another open file description, `Append` fails within a few seconds with
   the existing *"cannot lock the history file"* error instead of blocking forever. There is **no
   test file for `lock_unix.go`** today and no lock-contention test anywhere.
2. A cancelled context aborts the wait promptly.
3. `TestConcurrentAppendsKeepEveryEntryTheyAcknowledged` (`store_test.go:398`) still passes —
   ordinary contention must still serialise, not fail.

**Adversarial — what does hostile or malformed input do here?**
The lock file lives in a user-writable state directory.
1. A lock held for longer than the deadline by a process that never exits.
2. A lock file replaced by a symlink or a directory between `open` and `flock`.
3. A stale NFS mount holding the lock file — currently blocks forever.
4. A deadline of zero, and a context already cancelled on entry.
5. Assert the bounded wait **does not** drop history silently: `recordCall` (`call.go:251`) already
   downgrades an `Append` failure to a stderr warning at `call.go:264`, so a bounded wait costs a
   warning line, not a silent loss — assert the warning appears.

**Green — minimal implementation:**
1. `lock(ctx, path)` loops on `LOCK_EX|LOCK_NB` with a short sleep against a bounded deadline and
   the caller's context.
2. Thread the context from `Append` through; update the comment at `lock_unix.go:34-35`, which
   justifies the unbounded wait on grounds the bounded version does not violate.

**Verify:** `go test ./internal/corpus/...`

**Why:** `LOCK_EX` with no `LOCK_NB` and no deadline, and — because Go installs handlers with
`SA_RESTART` — not interruptible by a signal either. A `talaria call` in another terminal blocks in
`flock` with no output and cannot be Ctrl-C'd.

---

### Task 18: Refactor pass — tasks 13–17

**Depends on:** Task 17

**Deliverable is negative.** No new behaviour, no new files, no new tests beyond ones that replace
several others. The suite is green before and after, and the diff is net-negative in lines.

1. Read and drain `.ralph/refactor-backlog.md`. Task 15 was explicitly told to record the deferred
   `stdinBody`/`fileBody` size bounds there, and Task 16 the `sync.Once` decision — resolve or
   restate each with a reason. An absent file means nothing was recorded, not an error.
2. Read every file touched since Task 12.
3. Consolidate types and helpers duplicated across this block. Named suspects: the context-threading
   boilerplate added by tasks 15, 16 and 17 — three packages grew the same shape independently;
   bounded-wait helpers in `corpus` versus `curl`; the cell sanitiser from Task 14 if it was written
   per-renderer rather than once.
4. Move logic that accumulated in `cmd/talaria` down into the package that owns it. Measure and
   report.
5. Delete commented-out code, superseded helpers, and any comment asserting a property no test
   enforces. **Note:** Task 22 is the systematic sweep for these — do not pre-empt it, just remove
   what you touched.
6. Record every pattern you consolidated in `CLAUDE.md`.

**Verify:** `go test ./...` green, and report the net line delta in the commit message.

---

### Task 19: History never lies about what it recorded — findings 23, 24 and 31

**Depends on:** Task 17

**Test files:**
- `internal/corpus/store_test.go` (modify) — trim failure after a successful write; unreadable store
- `cmd/talaria/history_test.go` (modify) — a 0 ms entry keeps its field

**Implementation files:**
- `internal/corpus/store.go` (modify) — `Append` at `store.go:99` (lock `:109-113`, id `:115`,
  marshal `:117`, `write` `:122`, `return trim(path)` at `:126`); `storedIDs` at `:160`; `write` at
  `:213`; `trim` at `:247`; `replace` at `:298`
- `cmd/talaria/history.go` (modify) — `historyEntryView.TimingMS` at line 48, populated at 322

**Red — write failing tests:**
1. **Finding 23:** with the line already durably written and `trim` made to fail (a read-only
   directory, or a full filesystem simulated by a failing `replace`), `Append` returns **nil** —
   or an error a caller can distinguish as "recorded, but not trimmed". Today the trim error
   propagates and `recordCall` prints *"warning: the call was not recorded in history"* for a call
   that **was** recorded, so an operator re-runs a mutating call believing nothing happened.
2. The entry is present in the store afterwards — assert it, do not infer it.
3. **Finding 24:** with the history file mode changed to 0400 (readable-but-erroring on the write
   path) or an injected read error, `Append` **aborts** rather than assigning a duplicate id.
   Today `storedIDs` collapses every `os.ReadFile` error to `return nil` (`store.go:161-164`), so
   every candidate id looks free — contrast `Read` at `store.go:187-193`, which distinguishes
   `fs.ErrNotExist` correctly. `trim` (`store.go:248-251`) does not special-case it at all.
4. A missing store file still yields an empty id set with no error — the one case the current code
   is right about.
5. Two entries never share an id: assert `history replay <id>` and `history show <id>` resolve to
   the same entry. `selectEntry` (`cmd/talaria/history.go:415`, newest-first scan at :418-424)
   resolves an id to the *newest* match, so a duplicate makes replay silently re-issue a different
   request than show displayed.
6. **Finding 31:** an entry whose response took 0 ms renders `"timing_ms": 0` in
   `history --output json`. Today `int64` with `omitempty` (`history.go:48`) drops the field, so an
   agent cannot tell "0 ms" from "no response observed". An entry with **no** response omits the
   field entirely. Assert both cases in the same test, so they are distinguishable in one parse.

**Adversarial — what does hostile or malformed input do here?**
The store is a file on a filesystem that can be full, read-only, or failing, and every value is
read back from an untrusted history file.
1. `ENOSPC` between `write` and `trim`.
2. `EACCES` on the history file mid-run (mode changed by another process).
3. `EIO` on read.
4. The store directory replaced by a symlink between `Path()` and `open`.
5. A file whose every line is valid but whose ids all collide already — `uniqueID` must terminate,
   not loop.
6. A stored `timing_ms` that is negative, `int64` max, a JSON string, or null — the entry is
   skipped as malformed rather than rendered as garbage. An entry with a `response` object present
   but empty.
7. Assert the **new** error taxonomy is honest in both directions: nil must mean "on disk", and
   non-nil must mean "not on disk", with the trim case classified explicitly one way and tested.

**Green — minimal implementation:**
1. `Append` returns nil once the line is durably written; report a `trim` failure separately (a
   distinguishable error type, or a stderr warning from the caller) rather than as `Append`'s error.
2. `storedIDs` returns `(map, error)`, distinguishing `fs.ErrNotExist` from a genuine read failure;
   `uniqueID`/`Append` abort on the latter.
3. Fix the doc comment at `store.go:94-96` so what it claims about a nil return is what the code
   does.
4. Make `historyEntryView.TimingMS` a `*int64`, set only when `entry.Response != nil`, matching what
   the deleted `runResult` did for exactly this reason. `corpus.EntryResponse.TimingMS`
   (`entry.go:90`) already correctly has no `omitempty` — the loss is only in the list view.

**Verify:** `go test ./internal/corpus/... ./cmd/...`

**Why:** All three make the tool lie about history. 23 reports "not recorded" for a call that was,
so an operator re-runs a mutation; 24 lets `history replay <id>` re-issue a different request than
`history show <id>` displayed — precisely the failure the id field exists to prevent; 31 makes a
fast call against a local service indistinguishable from one that was never observed.

---

### Task 20: The spec cache expires and can be refreshed — finding 25

**Depends on:** Task 10

**Test files:**
- `internal/spec/source_test.go` (modify) — TTL, conditional GET, 304, `--refresh`
- `cmd/talaria/root_test.go` (create or modify) — `--refresh` is registered

**Implementation files:**
- `internal/spec/source.go` (modify) — `loadURL` at `source.go:82` (unconditional cache hit at
  `:85-90`, where *any* read error silently falls through to a fetch), `fetch` at `:111`,
  `cachePath` at `:138`, `writeCache` at `:156`
- `cmd/talaria/root.go` (modify) — register `--refresh` as a persistent flag beside `--spec` (:65)
- `cmd/talaria/list.go` (modify) — `loadSpec` at 170 reads the flag; replace the bare
  `spec.Load(ref)` at 186 with an explicit `Loader`
- `README.md` (modify) — lines 49-50 say a URL spec is *"fetched once per URL, not once per call"*,
  which is the forever-cache this task removes
- `AGENT.md` (modify) — says **nothing** about spec caching, so an agent whose spec changed under a
  URL has no documented way to know it is being served a stale contract, or to escape it

**Policy is already settled — do not invent it.** DESIGN.md §4 lines 235-239 (verified verbatim):
cached with its `ETag`/`Last-Modified`; inside 24 hours served from cache with no network call;
past that revalidated with a conditional GET; a 304 refreshes the timestamp without re-downloading;
`--refresh` forces a fetch.

**Red — write failing tests:**
1. A second `Load` inside the TTL makes no request. **`TestLoaderServesSecondLoadFromCache`
   (`source_test.go:132`) asserts today's forever-cache behaviour and must be rewritten**, not
   merely extended — it currently locks in the defect.
2. A `Load` past the TTL sends `If-None-Match`/`If-Modified-Since` and, on a 304, serves the cached
   bytes and refreshes the timestamp without re-downloading. Note `fetch` currently treats any
   non-200 as an error (`source.go:123-125`) — 304 must become a success path.
3. A 200 on revalidation replaces the cached bytes and metadata.
4. `--refresh` fetches unconditionally even inside the TTL. Note the flag is specified only in
   §4's policy prose — it is **absent from §4's CLI-surface flag block** (DESIGN.md:184-219), as is
   `--allow-host` from Task 2. Do not conclude from that block that either flag is out of scope.
5. Cache metadata is stored somewhere the current layout has no room for — `cachePath`
   (`source.go:138`) is a bare hex digest with no extension and no sidecar. Assert whatever layout
   you choose is private (0600/0700), matching `TestLoaderWritesCacheFilesPrivately`.

**Adversarial — what does hostile or malformed input do here?**
The cache is a file on disk; the ETag and Last-Modified are attacker-controlled response headers.
1. An `ETag` containing CRLF, 1 MB of text, or NUL — it is echoed back in a request header on the
   next call. **This is a header-injection vector of exactly the kind finding 5 covers.** Bound and
   validate it before storing, and again before sending.
2. A `Last-Modified` that is unparseable or set far in the future.
3. A corrupt or truncated metadata file — fall back to a full fetch, never crash.
4. A metadata file claiming a future timestamp, so the TTL never expires.
5. A 304 returned on a *first* fetch, when there is no cached body to serve.
6. A cache directory that is read-only, or a cache file replaced by a symlink between stat and read.
7. Concurrent `talaria` processes revalidating the same URL — `writeCache` is already temp+rename;
   keep that property and assert it. Note its `os.Rename` return value is **unchecked**
   (`source.go:183`) and every error in the function is swallowed — do not preserve that while you
   are adding a metadata file beside it.

**Green — minimal implementation:**
1. Store metadata alongside the body — a sidecar `<digest>.meta` or a small header prefix; keep
   `writeCache`'s temp-file + `Chmod` + `Rename` shape.
2. `loadURL` stats the metadata: inside TTL → serve; past TTL → conditional GET; `--refresh` →
   unconditional fetch.
3. Accept 304 in `fetch` and return a "not modified" signal.
4. Thread `--refresh` from `root.go` through `loadSpec` into an explicit `spec.Loader` — the
   package-level `spec.Load` (`source.go:62`) discards the `Loader`, so this is the one structural
   change needed. `loadIndex` (`list.go:161`) wraps `loadSpec` for the four discovery commands, so
   the flag reaches all six spec-reading commands through one edit.
5. **Document it in the same commit.** Rewrite README:49-50 — *"fetched once per URL, not once per
   call"* describes exactly the defect being fixed — with the settled §4 policy: 24h TTL,
   conditional revalidation, `--refresh` to force. Add `--refresh` to AGENT.md's flag list beside
   `--spec` (line 61) and one line under `## The spec` (line 23) saying a URL spec may be up to 24
   hours stale and `--refresh` forces a re-fetch. An agent that cannot tell a stale contract from a
   real schema violation will report the latter — which is the damage finding 25 names.

**Verify:** `go test ./internal/spec/... ./cmd/...`

**Why:** A cached spec is currently served forever with no TTL, no conditional request, no bypass
flag and no command to clear it — the code's own comment at `source.go:98` admits *"the cache has
no expiry an agent could reach."* Every downstream stage then works against a possibly-stale
contract, most damagingly `validate`, which reports violations the server never committed.

---

### Task 21: One pass over the history file per append — finding 16

**Depends on:** Task 19

> **Cut this task first if the phase runs long.** The design doc says so explicitly: `run` was its
> pathological case and `run` is gone. If you are dropping it, say so in the commit message and
> record it in `.ralph/refactor-backlog.md` rather than deleting the task.

**Test files:**
- `internal/corpus/store_test.go` (modify) — file is read once per append

**Implementation files:**
- `internal/corpus/store.go` (modify) — `Append` at 99, `uniqueID` at 138, `storedIDs` at 160,
  `trim` at 247

**Red — write failing tests:**
1. One `Append` against a store under the cap performs exactly one full read, not two. Today
   `Append` calls `uniqueID` → `storedIDs` → `os.ReadFile` + a per-line parse, then `write`, then
   `trim` re-reads and re-parses the whole file — both scans under `flock(LOCK_EX)`.
2. `trim` is skipped entirely when the file cannot be over cap (`os.Stat` size or a maintained line
   count). Note `trim` already returns early at `store.go:271-273` when nothing is over cap — the
   cost is the *read*, not the rewrite.
3. All existing cap and eviction behaviour is unchanged: `TestAppendCapsEachSourceSeparately`
   and `TestAppendTrimsOldestFirst` still pass untouched.

**Adversarial — what does hostile or malformed input do here?**
1. A store at exactly the cap boundary, and one entry over.
2. A cached line count that has gone stale because another process appended — the cheap check must
   be conservative (may trim unnecessarily, must never skip a needed trim).
3. A file whose size suggests it is under cap but whose per-source counts are over.
4. Interaction with Task 9's size bounds and Task 19's error taxonomy — assert both still hold.

**Green — minimal implementation:**
1. Fold `storedIDs` and `trim`'s scan into a single pass that returns both the id set and the
   per-source counts.
2. Run the rewrite only when that pass says a source is over cap.

**Verify:** `go test ./internal/corpus/...`

**Why:** Two full reads and unmarshals per append, both holding the cross-process lock, compounding
finding 17's unbounded wait. Lowest priority in the phase now that `run` is gone.

---

### Task 22: No comment asserts an invariant no test enforces

**Depends on:** Task 21

This task has no finding number. Four findings (19, 29, 30, 31) share one shape: **a comment
asserts a property the code does not have.** A finding-by-finding loop cannot do this — there is no
line number to anchor on — so it gets its own task. A sweep of the tree has already been done and
its results are below; **verify each before acting, and sweep beyond them.**

**Test files:** wherever the claim lives — this task's output is mostly tests.

**Implementation files:** comments, and any code needed to make a claim true.

**Confirmed unenforced claims, ranked. Either write the test or delete the claim:**

1. `internal/curl/sweep.go:55-56` — *"Info is lstat here, so a symlink somebody planted under one
   of these names is judged and removed as the link it is, never followed."*
   `TestSweepStaleRemovesOnlyTalariaTempFilesPastTheGrace` (`exec_test.go:593-633`) plants only
   regular files and directories — **no symlink case at all**. `os.RemoveAll` through a followed
   link is arbitrary deletion. **Highest blast radius in the sweep; write this test.**
2. `internal/secret/redact.go:121-123` — *"Everything but `*` is quoted, so a user-supplied pattern
   cannot be a regexp injection and compilation cannot fail."* Patterns come from `config.yaml`
   `redact.headers`. No test passes a metacharacter-bearing pattern; a `[` or `(` would panic the
   CLI via `regexp.MustCompile` (`redact.go:130`) if the quoting ever broke. **Cheapest valuable
   test in the repo — one table case.**
3. `internal/curl/exec.go:166-169` — *"never world-readable and never outlive the exec."* The
   lifetime half is enforced (`assertNoTalariaTemp`, `exec_test.go:590`); the **0700 mode half is
   not**. Mirror the existing store test (`store_test.go:317`).
4. `internal/curl/config.go:85-88` — the buffer-zeroing claim. **Task 11 fixes this**; confirm it
   was done and that the comment now matches the code.
5. `internal/e2e/boundary_test.go:21-36` — the prose reads as a boundary rule about `corpus`, but
   `shared` is only `{internal/operation, internal/validate}`. **Task 8 fixes this**; confirm the
   guard exists and the prose matches it.
6. `internal/canary/canary_test.go:385-387` — the stale `--fail-on-error` note. **Task 13 fixes
   this**; confirm the comment is gone.
7. `cmd/talaria/history.go:230-231` — *"replay reads a recorded request and needs no spec."*
   **Task 3 fixes this**; confirm the comment is gone.
8. `internal/corpus/store.go:129-130` — *"The caller holds the append lock, so what this reads off
   the store cannot change under it."* The textbook "callers must hold the lock" comment CLAUDE.md
   names. Structurally true (`uniqueID` is unexported, one call site) but unenforced — nothing
   fails if a second call site is added without the lock.
9. `internal/spec/source.go:162-164` — *"so a concurrent talaria never reads a half-written spec."*
   The 0600/0700 half is enforced; the concurrency half has no test.
10. `internal/corpus/store.go:296-297` — *"a reader never sees a half-written store and a crash
    mid-trim leaves the old file intact."* No concurrent-reader or crash-injection test.
11. `internal/curl/render.go:164-168` — *"is safe because render is Symbolic or String."* Relies on
    a caller invariant with no test that a resolving renderer is never passed to `Render`. The
    parallel claim at `request.go:305-311` guards itself at runtime **and** has a test
    (`TestQueryStringEscapesAResolvedSensitiveLiteral`).
12. `internal/curl/render.go:4-7` — the package doc claims an emitted command "never calls
    `SecretRef.Resolve`" and is *"runnable in a shell where the env var is set, useless to
    exfiltrate"*. True for headers, cookies and query; **false for the body**, which is raw
    `[]byte` and never becomes a `request.Value`. **Task 7 fixes this**; confirm the doc now
    matches. Same for `cmd/talaria/call.go:449-452` ("built from the request's *redacted*
    representation throughout").
13. `internal/corpus/lock_unix.go:21-22` — *"exactly as separate talaria processes do."* The
    goroutine half is covered; the multi-process half is asserted by argument only.
14. `internal/operation/index.go:63-64` — *"whatever order the two operations appear in."*
    `index_test.go:84-91` covers one direction only.
15. `internal/curl/version.go:65-67` — *"these cannot fail on anything but an implausibly long
    number."* Silent `return nil` on `Atoi` failure, no test.
16. `internal/clierr/clierr.go:128-130` — *"marshalling cannot fail."* Structurally true, benign
    fallback. Lowest priority; narrowing the wording is an acceptable resolution.

**Red — write failing tests:**
1. Items 1, 2 and 3 get real tests — they are security claims with no coverage. Each must fail
   before the fix if the claim is false, or pass immediately and thereby *become* the enforcement
   the comment was pretending existed. Say which of the two happened for each.
2. Items 4-7 and 12 are verification-only: assert the earlier tasks actually landed.
3. For items 8-11 and 13-16, either write the test or rewrite the comment to claim only what is
   true. **Record the decision for each in the commit message** — a silent drop is
   indistinguishable from an oversight.

**Adversarial — what does hostile or malformed input do here?**
The sweep exists *because* the adversarial cases were never written.
1. Item 1's test must plant a symlink pointing **outside** `TMPDIR` and assert the target survives.
2. Item 2's test must pass `[`, `(`, `.` and `\` as user patterns and assert no panic and no
   widened match.
3. Item 3's test must stat the capture directory, not assume the mode.

**Green — minimal implementation:**
1. Work the list top-down; stop when the remaining items are all wording.
2. Sweep beyond the list with the phrasings that found it: *"must hold", "is zeroed", "guaranteed",
   "always", "never", "validated upstream", "already checked", "cannot happen", "is safe because",
   "by construction", "is bounded", "impossible"*. Prioritise security and concurrency claims.
3. `CLAUDE.md` already states the rule strongly enough — instead record *where* the sweep looked,
   so the next one does not start over.

**Verify:** `go test ./...`

**Why:** Four of the review's findings were comments documenting an intention as if it were an
invariant. A comment that claims a security property nobody tests is worse than no comment: it
stops the next reader from checking.

---

### Task 23: The credential firewall holds end to end

**Depends on:** Task 22

The phase spans `spec` → `request` → `curl` → `output` → `corpus` → `cmd`. Every task above tested
its own component. This one asserts the data actually flows between them, because component N's
output reaching component N+1 is exactly what a per-package suite does not check — and is why the
existing suite is 2.4× the production code and caught none of the review's findings.

**Test files:**
- `internal/e2e/e2e_test.go` (modify) — the full loop with host binding and replay
- `internal/canary/canary_test.go` (modify) — if a surface is still unscanned

**Implementation files:** none expected. If this task needs production code, an earlier task was
incomplete — fix it there and say which.

**Red — write failing tests:**
1. **The whole rule, end to end, at the wire.** A spec with a server-variable-bearing `servers[]`
   (Task 1), a credential in the environment, and `--base-url` pointed at a capture listener:
   assert the credential is absent from what the listener received, `credentials_withheld` is in
   the envelope, one stderr line names the scheme and host, exit 0 — then `--allow-host` flips it,
   then `history replay` of that entry re-derives through the spec and lands on the spec's host,
   not the stored one. This is one test that would have caught findings 1, 2, 3, 11 and 22
   together.
2. Exit codes 0-5 still each reproduce where DESIGN.md §4 documents them, including the new exit 5
   from Task 4 and the exit 2 refusals from Task 3.
3. `--dry-run`'s emitted curl is still byte-identical to the executed request (`e2e_test.go:567`)
   under the new body-origin rules from Task 7.
4. A spec fetched over HTTP is bounded (Task 10), cached, revalidated and refreshable (Task 20),
   and leaves no credential in the cache.

**Adversarial — what does hostile or malformed input do here?**
This is the integration-level hostile pass: the earlier tasks each assumed one boundary was
hostile; this one assumes several at once.
1. A hostile spec **and** a hostile history file in the same run — the spec declares an off-spec
   server, the history entry names a different host, and a credential is set. Assert nothing
   reaches either attacker-controlled destination.
2. A spec whose media type injects (Task 5) reaching `history replay`, which does not go through
   `binder.contentType` — assert the `internal/curl` gate catches it.
3. `--base-url` at a listener that returns a schema-violating body with `--fail-on-error`: exit 4,
   credential withheld, and the canary absent from the validation error (Task 13's stage, at
   integration level).
4. Ctrl-C mid-call (Task 15) with a temp body file and a capture directory live — assert both are
   cleaned up.

**Green — minimal implementation:** none. This task is assertions.

**Verify:** `go test ./...`

**Why:** The phase's whole thesis is that the previous cycle shipped a green suite with eight
critical defects because every test asserted what a feature should do and none asserted what an
attacker would do across a boundary. This task is the one that fails if any earlier task's fix
stopped at its own package edge.

---

### Task 24: Refactor pass — tasks 19–23, end of phase

**Depends on:** Task 23

**Deliverable is negative.** No new behaviour, no new files, no new tests beyond ones that replace
several others. The suite is green before and after, and the diff is net-negative in lines. This is
the last task in the phase, so it also leaves the tree in the state phase 2b starts from.

1. Read and drain `.ralph/refactor-backlog.md` **completely**. Anything left must carry an explicit
   one-line reason and be visible to phase 2b. If Task 21 was cut, its entry is here — confirm it
   says so.
2. Read every file touched since Task 18.
3. Consolidate across the whole phase, not just the last block. By now the same shapes exist in
   several packages: bounded reads (`corpus`, `spec`), bounded waits (`corpus`, `curl`), context
   threading (`request`, `curl`, `corpus`), and host/URL normalisation.
4. **Measure `cmd/talaria`.** It held 1,589 of 5,511 non-comment production lines (28%) at the start
   of this phase, and 20 view/DTO structs (18 with JSON tags) — CLAUDE.md's "12 view structs" is
   already stale, so correct it while you are there. CLAUDE.md forbids growing this layer, and
   tasks 3, 4, 7, 15 and 20 all touched it. Report the before/after count in the commit message; if
   it grew, move something down.
5. Delete commented-out code and superseded helpers. Task 22 already swept unenforced comments —
   confirm nothing reintroduced one.
6. Update `CLAUDE.md` with every pattern this phase established, and update the scope note if the
   phase changed what is true. In particular: the host-binding rule, the replay re-derivation rule,
   and the fact that `replayableEnv` is gone (which resolves finding 18 without a design
   amendment).
7. Confirm `.ralph/stack.json`'s three command fields still match `.github/workflows/ci.yml`
   byte-for-byte — `internal/ci/workflow_test.go` fails if they drift, and tasks 2 and 20 added
   flags but should not have changed a command.
8. **Re-read README.md and AGENT.md against the shipped binary.** Finding 21 exists because a
   shipped document described behaviour the code no longer had, and this phase changed user-facing
   contracts in four places: the unsupported-scheme report and exit 5 (Task 4), host binding,
   `--allow-host`, `credentials_withheld` and replay's whole contract (Task 3), the spec cache TTL
   and `--refresh` (Task 20). Each was told to ship its own docs; this step confirms they did, and
   that the three sets do not contradict each other where they overlap — replay appears in both
   README's history section and AGENT.md's, and `--base-url` appears in the flag list of each.
   Nothing enforces this in CI, so it is a read, not a test.

**Verify:** `go test ./...` green, `go build ./...`, `test -z "$(gofmt -l .)" && go vet ./...`, and
the net line delta plus the `cmd/talaria` measurement in the commit message.
