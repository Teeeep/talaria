# apitest — Design Document

*Status: draft v0.2 · revised 2026-08-02 after [research pass](../research/2026-08-02-design-review.md) · pre-implementation*

> Working name `apitest` throughout; **rename pending** — see §8. The v0.1 name candidates
> were unresearched; §8 now carries collision evidence.

**Changes from v0.1:** Swagger 2.0 handling rebuilt around `openapi2conv` (libopenapi cannot
do it); stdin ownership resolved (§5a); validator open question closed; Code Mode added to the
landscape; Restish claims corrected; `search`/`uses` added to Phase 1; twin claims made honest;
roadmap re-cut so the differentiated work ships first.

## 1. Vision

A single Go binary that turns any OpenAPI/Swagger spec into three things:

1. **An explorable API client** — list, describe, and call operations, with curl under the hood.
2. **A spec-driven tester** — validate real responses against the contract, smoke-test whole APIs.
3. **A digital twin** — a local server that mimics the real API for safe, fast, offline testing.

Built **CLI-first for both humans and coding agents**. No MCP server, no context bloat:
agents discover the API progressively (`list` → `describe` → `call`), paying token cost
only for what they need. Every request is reproducible as a plain curl command.

**One-line pitch:** *curl for OpenAPI — explore, test, and twin any API from its spec.
One binary, agent-first, no MCP required.*

## 2. Why build this (landscape)

The space splits into five camps; none occupies the intersection this tool targets.

| Camp | Examples | What they do | What they lack |
|---|---|---|---|
| curl generators | curlgenerator, Swama | Spec → printed curl commands | No execution, validation, or testing |
| Spec-aware CLI clients | **Restish** (closest competitor, Go) | Operations as CLI commands, auth/profiles, OAuth | **No response validation** (see below); own HTTP stack, not curl; no twin |
| Spec exploration for LLMs | **phyllotaxis** (Rust) | Progressive-disclosure spec browsing: list, inspect, search, reverse lookup | Read-only — no execution, validation, testing, or mocking |
| Spec-based testers | **Schemathesis**, Dredd | Property-based/contract testing in CI, JUnit output, exit codes | Heavyweight CI tools, not interactive exploration; Python/Node runtimes |
| OpenAPI→MCP bridges | FastMCP, AWS openapi-mcp-server, Speakeasy, Stainless | Expose spec operations as MCP tools | Context bloat (see below); running server per API |
| **Code execution / Code Mode** | Cloudflare Code Mode, Agentgateway, Anthropic's code-execution pattern | Generate a typed SDK on a filesystem; agent reads files on demand and writes code against it | Requires a code sandbox; no response-vs-spec validation; no twin; no artifact a human can paste into a terminal |
| Mock servers | Prism, **Specmatic**, Microcks, **WireMock**, Mockoon | Mock server from spec; Specmatic and WireMock also record real traffic; WireMock is stateful | Standalone tools, no integrated exploration/testing CLI; no agent ergonomics |

### The token problem is real and now mainstream

This design's founding bet — that dumping an API into an agent's context is the wrong
architecture — is no longer contrarian:

- **Cloudflare's API has 2,500+ endpoints.** As an MCP server that is **1.17M+ tokens**;
  their Code Mode (Feb 2026) exposes the same API in ~1,000 tokens via two tools.
- **Anthropic's code-execution pattern** measured 150,000 → 2,000 tokens on one workflow,
  a 98.7% reduction.
- **Perplexity's CTO** announced an internal shift away from MCP over context waste and
  auth friction.

**Counterweight, stated honestly:** MCP has crossed 8M server downloads and 97M monthly SDK
downloads, with Google, OpenAI, and Microsoft shipping it in flagship products. "Not an MCP
server, ever" (§9) is a product *stance*, not a prediction that MCP will lose.

### The wedge

The CLI-first agent ecosystem (pi, Claude Code skills) favours *CLI tools with README files* —
progressive disclosure, composability, token efficiency. An agent facing an API today either
reads a 2MB `swagger.json` into context, hand-writes curl and gets params wrong, or spins up an
MCP server per API.

The differentiation is **the integrated loop** — explore, test, and twin from one spec with one
binary — which today requires Restish + Schemathesis + Prism + glue.

### Honest competitive assessment

- **Restish is the closest competitor and it is moving.** 1,348 stars, actively developed, with
  a v2 in flight. Its `--rsh-validate` flag validates **outbound JSON request bodies only, and
  only when opted into**; it explicitly "trusts explicit input and lets the server validate API
  semantics." So response-vs-spec validation is genuinely absent — but do not claim Restish has
  no validation at all. Notably, Restish v2 is adding **MCP-ready extension points**: it is
  moving *toward* MCP while this design moves away. That directional contrast is the cleanest
  differentiation, more durable than any feature gap.
- **phyllotaxis proves the exploration idea by independent invention** (Rust, small, read-only).
  It overlaps Phase 1 and nothing beyond it. Two of its commands are worth adopting (§4).
- **The twin's mechanism is not novel.** Specmatic already records real traffic through a proxy
  and generates spec-validated mocks. WireMock has record & playback *and* stateful
  create-then-fetch behaviour. Microcks has the most momentum among spec-driven open-source
  mockers. Replay and statefulness are **table stakes**, not a moat — see §6 for what actually
  differentiates.
- **Code Mode is the most serious architectural rival**, backed by Cloudflare and Anthropic
  rather than hobbyists. It solves the same token problem with a different answer. The honest
  edge: Code Mode needs a code sandbox and leaves behind nothing a human can paste into a
  terminal; this tool needs only a shell and emits a curl command every time. And Code Mode has
  no equivalent of the twin.

Primary motivation remains: a tool for my own workflow. Open source is upside.

## 3. Design principles

0. **Secrets never reach the agent. Priority one, non-negotiable.**
   The binary is the trust boundary: the agent operates entirely on credential *names*
   (env vars, profile names); credential *values* are injected by the tool at the last
   possible moment and never appear on stdout, stderr, in dry-runs, in emitted curl
   commands, in error messages, or in recordings. See §5a for the full threat model.
   Any feature that would print a secret is a bug, even if the user asked for it —
   there is no `--show-secrets` flag.

1. **Agent-friendly = good CLI design, enforced strictly.**
   - Machine-readable output everywhere: `--output json` on every command.
     Default: pretty when stdout is a TTY, JSON when piped.
   - Deterministic exit codes (see §4). Agents branch on these.
   - Never prompt. Never page. No interactivity, ever. Dangerous behavior is gated
     by flags set deliberately by a human, not confirmations.
   - Errors to stderr as structured JSON: what failed, why, valid alternatives.
   - Versioned output schema (`"schema": "apitest/v1"` field) so agent prompts don't break.

2. **Progressive disclosure is the product.** `list` is one compact line per operation;
   `describe` renders schemas in a terse readable form (Restish-style
   `name*: (string) description`), never raw JSON Schema dumps. The whole point is that
   neither human nor agent ever loads the raw spec into their head/context.

3. **curl is the execution engine and the lingua franca.** Build argv slices via
   `os/exec` (never shell strings — no quoting hell, no injection). `--dry-run` prints
   the exact curl command. Every executed call returns its curl equivalent in the JSON
   output: a portable, copy-pasteable reproduction for bug reports, docs, and scripts.
   **Emitted curl commands always reference secrets symbolically** — e.g.
   `-H "Authorization: Bearer $APITEST_AUTH_BEARER"` — so they remain runnable in a
   shell where the env var is set, but never contain credential values (§5a).
   Inherit curl's maturity for TLS, proxies, HTTP versions.
   **Check curl's version, not just its presence, at startup:** `--write-out '%{json}'`
   requires curl ≥ 7.70 and is a hard floor.

4. **Safe by default.** Read-only (GET/HEAD/OPTIONS) unless `--allow-mutations`.
   The sanctioned answer to "I need to test a DELETE" is: do it on the twin.
   Recordings redact secrets by default.

5. **Composability over features.** Plain JSON to stdout for jq; no built-in query
   language in v1. Spec location via arg, `--spec`, or `APITEST_SPEC` env var so agents
   don't repeat it every call. **The process owns its own stdin** (§5a) — `--body -`
   works, and never competes with how secrets reach curl.

6. **The agent README is a first-class deliverable.** An operating manual written for
   LLMs: the list→describe→dry-run→call workflow, exit code semantics, output shapes,
   auth env vars. Ships as `AGENT.md` in the repo; doubles as a pi skill and a Claude
   Code SKILL.md wrapping the same binary. Treat it with the same care as code.

## 4. CLI surface

```
# Discovery (cheap, no network beyond fetching the spec)
apitest list [spec] [--tag t] [--output json|pretty|tsv]
apitest describe [spec] <operationId> [--output json|pretty]
apitest search [spec] <query> [--kind operation|schema|param]   # fuzzy find across the spec
apitest uses [spec] <schemaName>                                # reverse lookup: which operations use it

# Calling
apitest call [spec] <operationId>
    --param id=42                 # path params
    --query verbose=true          # query params (repeatable)
    --header X-Foo=bar            # extra headers (repeatable)
    --body '{"..."}' | --body @file.json | --body -   # stdin
    --base-url https://staging.example.com
    --profile staging             # named config: base-url + auth + headers
    --dry-run                     # print curl command, send nothing, exit 0
    --allow-mutations             # required for POST/PUT/PATCH/DELETE
    --output json|pretty

# Smoke testing
apitest run [spec] [--tag t] [--operation id ...]
    --base-url ... --profile ...
    --allow-mutations
    --report json|junit|pretty
    --fail-on-error               # nonzero exit if any HTTP >= 400

# Digital twin (later phases)
apitest twin serve [spec] --port 9000 [--corpus ./twin-data] [--stateful]
apitest twin record [spec] --upstream https://api.real.com --port 9000 --corpus ./twin-data
apitest twin fault <operationId> --status 429 --rate 0.1 [--header Retry-After=30]

# Meta
apitest auth check [spec] [--profile p]   # verify credentials are PRESENT for the spec's
                                          # security schemes without printing values:
                                          # {"scheme":"bearerAuth","source":"env:APITEST_AUTH_BEARER","present":true}
apitest version
```

`search` and `uses` are adopted from phyllotaxis's command set. They matter because an agent
often knows a *domain concept* ("something about invoices") or a *data model* ("what touches
the `Invoice` schema?") rather than an operationId, and `list` on a large spec is exactly the
context dump this tool exists to avoid.

Spec argument accepts a file path or URL (with local cache for URLs); falls back to
`APITEST_SPEC`. Supports Swagger 2.0 and OpenAPI 3.0/3.1/3.2, JSON and YAML (§5).

### Output shape for `call` (sketch)

```json
{
  "schema": "apitest/v1",
  "request": {
    "curl": "curl -s -H \"Authorization: Bearer $APITEST_AUTH_BEARER\" 'https://…'",
    "method": "GET", "url": "…",
    "headers": { "Authorization": "<redacted:env:APITEST_AUTH_BEARER>" }
  },
  "response": { "status": 200, "headers": {}, "body": {}, "timing_ms": 143 },
  "validation": { "status_documented": true, "body_valid": true, "errors": [] }
}
```

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Success (note: HTTP 4xx/5xx is exit 0 for `call` — a 404 is a successful *observation* in exploration; use `--fail-on-error` to change) |
| 1 | Request could not be completed (network, curl failure) |
| 2 | Usage error (unknown operation, missing required param — stderr JSON lists valid options) |
| 3 | Spec parse/load error |
| 4 | Validation failure (response violates spec) — only with `--fail-on-error` / `run` |

## 5. Architecture

```
cmd/apitest/main.go        # cobra wiring
internal/spec/             # load (file/URL/cache), v2→v3 convert, normalize
internal/operation/        # THE core model: id, method, path, params, body schema,
                           #   auth requirements, response contracts
internal/curl/             # argv + config builder, executor (os/exec, --write-out json)
internal/validate/         # request & response vs schema  ← shared with twin
internal/gen/              # example/schema-based data generation ← shared with twin
internal/corpus/           # recordings: store, redact, index, generalize ← twin spine
internal/twin/             # http server: replay, fallback gen, state, faults
internal/output/           # json / pretty / tsv renderers, versioned schemas
internal/config/           # profiles, env vars, auth mapping
```

Key boundary rules:
- `operation`, `validate`, `gen` are consumed by **both** the curl executor path and the
  twin server path. Nothing in them may import `curl` or `twin`.
- `corpus` is defined early (even though implemented later) so recording hooks in
  the executor don't require re-architecture.
- Twin's serve side uses `net/http` directly — curl is only for outbound calls.

### Library choices

- **CLI:** spf13/cobra (+ viper for profile/env layering). Cobra's generated help is
  well-structured text that agents read happily.
- **OpenAPI 3.x:** `pb33f/libopenapi` — a 3.2/3.1/3.0 + Overlays + Arazzo toolkit, actively
  developed, and what Restish migrated to.
- **Swagger 2.0:** `getkin/kin-openapi`'s **`openapi2conv`**, used *only* to convert 2.0 → 3.x
  at load time. **This is a correction to v0.1**, which assumed libopenapi could do the
  conversion. It cannot: libopenapi's v2 model is explicitly unmaintained, slated for removal,
  and its authors say "**DO NOT** take a dependency on it" and "we don't recommend using
  it… it's a commercial product."
  Accepting two OpenAPI dependencies is the cost of the "any API" promise — a large share of
  real-world specs are still 2.0. The conversion runs once at load, so everything downstream
  still sees exactly one model and the boundary rules above are unaffected.
  **Phase 1 must verify conversion fidelity against real 2.0 specs**, not just synthetic ones.
- **Schema & message validation:** `pb33f/libopenapi-validator`. **This closes v0.1's open
  question.** It validates `http.Request` *and* `http.Response` against a 3.x spec and ships a
  standalone OpenAPI router with auth callbacks and replayable bodies — which is most of what
  the twin needs in Phase 5, from the same spec model the CLI already holds. Watch one
  behaviour: it defaults to OpenAPI 3.1+ strict JSON Schema semantics, which can be stricter
  than a 3.0 spec intends; verify against 3.0 specs early and configure down if needed.
- **Execution:** system curl via `os/exec`. Not a Go HTTP client — the curl command *is*
  a feature (transparency, reproducibility).

### Auth

Spec declares security schemes; credentials come from outside:
- Env vars by convention: `APITEST_AUTH_BEARER`, `APITEST_AUTH_BASIC`,
  `APITEST_AUTH_APIKEY_<SCHEME_NAME>`.
- Profiles (`~/.config/apitest/config.yaml`, mode 0600): named sets of base-url + auth +
  headers, selected with `--profile`. Secrets in profiles may reference env vars.
- `apitest auth check` reports which schemes are satisfied and from which *source*
  (never the value), so agents can diagnose auth setup blind.
- v1 scope: bearer, basic, API key (header/query/cookie). OAuth flows: out of scope
  (user supplies a token obtained elsewhere); revisit later.

## 5a. Secret handling — threat model

**Invariant: the agent (any consumer of stdout/stderr) never observes a credential
value.** The setup is asymmetric by design: a human (or CI) places secrets in env vars
or the profile file once; the agent only ever names them.

### How secrets reach curl: the config document on curl's stdin

**Primary mechanism: `curl -K -`.** curl reads a config document from its stdin when the
filename is `-`. Secret-bearing headers, credentials, and the request body are written into
that document; **nothing sensitive is ever an argv element.** This is the documented approach
for keeping credentials out of the process list.

This is a change from v0.1, which made a 0600 temp file primary. Stdin is strictly better:
nothing touches disk, so there is no unpredictable-name requirement, no delete-after-exec
cleanup, and no window in which another process can read the file.

**Resolving the stdin contention.** v0.1 promised both `--body -` (read the request body from
stdin) and `-K -` (read curl's config from stdin) without noticing they collide. They do not
have to:

> **The Go process owns the real stdin. curl's stdin is always a pipe that Go writes.**

`--body -` is read to EOF by the Go process from *its* stdin. curl is then spawned with a
fresh pipe on *its* stdin, into which Go writes the config document — including the body as a
`data` directive. One stdin consumer at each level; no contention.

For large or binary bodies, where inlining into the config document is impractical, fall back
to a 0600 temp file referenced as `data = "@/path"`, deleted immediately after exec. The temp
file is the exception, not the rule.

### Leak channels and countermeasures

| Leak channel | Countermeasure |
|---|---|
| Emitted/dry-run curl commands | Always symbolic: `-H "Authorization: Bearer $APITEST_AUTH_BEARER"`. Runnable where the env var exists; useless to exfiltrate. |
| `request.headers` in JSON output | Sensitive headers rendered as `<redacted:env:NAME>`. Built-in list (`Authorization`, `Cookie`, `Proxy-Authorization`, `*api*key*`, `*token*`, `*secret*`) + user-extensible via config. Applied to pretty output too. |
| curl process argv (`ps`, `/proc/*/cmdline` — visible to any process on the host, including agent-spawned ones) | Never pass secret-bearing headers as argv. They go in the config document on curl's stdin (above). |
| Response headers/bodies (e.g. `Set-Cookie`, login endpoints returning `access_token`) | Redact `Set-Cookie` and configurable sensitive response-body JSON paths by default; document that auth-issuing endpoints should be called by humans, not agents. |
| Error paths (curl stderr, validation errors quoting the request) | Errors are constructed from the redacted request representation, never the raw one. Test this explicitly — error paths are where redaction bugs live. |
| Recordings / corpus | Redaction applied **at write time** (not read time): sensitive headers + configured body paths never touch disk. Un-redacted recording is not an option. |
| Spec cache, verbose/debug logs | Debug output goes through the same redacted representation. No `--verbose` mode that bypasses it. |
| Query-string API keys (some specs use `?api_key=`) | Same treatment: symbolic in emitted curl (`?api_key=$APITEST_AUTH_APIKEY_X`), `<redacted>` in output URL fields. Note: these still leak into server logs — warn once on stderr. |

**Architecture consequence:** there is exactly one component (`internal/curl` executor,
at exec time) that ever holds resolved secret values, and it holds them only to write
the config document. Everything else in the codebase — output, validation, corpus,
errors — operates on a `Request` type whose secret fields are *structurally* references
(`SecretRef{EnvVar: "..."}`), not strings. Leaking then requires deliberately resolving
a ref, which is greppable and reviewable, rather than forgetting to scrub a string,
which is not.

**Twin synergy:** the twin needs **zero real credentials**. It enforces that *a*
credential is present (auth realism) but accepts placeholder values, so the agent's
entire develop-and-test loop runs secret-free; real credentials only exist in the
final human/CI-gated run against staging or prod.

**Testing requirement:** a CI test suite greps every output mode (json, pretty, tsv,
dry-run, errors, junit reports, corpus files, debug logs) for canary secret values
injected via each auth mechanism. Redaction regressions fail the build.

### Test data for `run` mode

Priority order: spec `example`/`examples` values → user fixture files
(`--fixtures dir/`, matched by operationId) → schema-generated fake data (`gen`).
Examples-first keeps requests realistic; generation is the fallback.

## 6. The digital twin

**Goal: mimic the real API, not just return schema-valid noise.**

**What is table stakes and what is not.** Replay-from-recordings and stateful CRUD behaviour
already exist in mature products — Specmatic records real traffic into spec-validated mocks;
WireMock does record & playback plus stateful create-then-fetch. Building those buys parity,
not advantage. Three things are genuinely differentiated:

1. **Synthesis by generalization from the corpus** — synthesize user 99 by mutating recorded
   users 42 and 57, yielding real field values, real enum usage, real nullability, instead of
   `"string"`/`0` schema noise.
2. **Deterministic fault injection driven by the same CLI that explores and tests** — no
   second tool, no second config format, no glue.
3. **Zero-credential operation** — the whole develop-and-test loop runs without a real secret
   ever existing on the machine (§5a).

Everything else in this section is the substrate those three need.

| Dimension | Mechanism |
|---|---|
| **Data realism** | Recordings over generation. Replay recorded responses; *generalize* from them (see 1 above). |
| **Behavioral realism (state)** | Infer CRUD lifecycles from REST conventions + spec (POST returns created object with id; path param matches id field). In-memory/bbolt store so create→get→delete sequences behave coherently. Recorded real sequences confirm inferred semantics. |
| **Error realism** | Injectable faults: `twin fault getUser --status 429 --rate 0.1` or a `POST /_twin/faults` control endpoint the agent/test-harness can hit. Real error bodies harvested from recordings. Deterministic fault injection is where the twin is *better* than the real API. |
| **Temporal realism** | Per-endpoint latency profiles extracted from recorded timings (already captured via curl `--write-out`). Optional rate-limit simulation. Nice-to-have tier. |
| **Auth realism** | Enforce the spec's security schemes; reject missing/bad credentials with the same status codes observed in recordings. Accepts placeholder tokens — the twin requires **no real credentials**. |

**Recording is the spine.** The spec provides structure; recorded traffic provides truth.
Every twin feature consumes the same corpus: replay directly, synthesis generalizes from
it, state inference validates against it, error/latency profiles are extracted from it.

This yields a natural maturity model: day 1 = spec-only static mock → explore the real
API through the recording proxy → the twin gets progressively more real, automatically.
*"Your twin gets better the more you use the real API."*

**Request validation on the twin.** The twin validates incoming requests against the
spec and returns precise schema errors — so it doubles as a client-side contract checker
during development. `libopenapi-validator`'s router and request validator provide this
directly (§5).

**The loop this unlocks** (identical commands, swap `--base-url`):

```
apitest twin record spec.yaml --upstream https://api.real.com --corpus ./twin
apitest call spec.yaml listUsers --base-url http://localhost:9000        # via proxy, recorded
apitest twin serve spec.yaml --corpus ./twin --stateful
apitest run spec.yaml --base-url http://localhost:9000 --allow-mutations # safe destructive testing
apitest run spec.yaml --profile staging                                   # final verification, same commands
```

Agent workflow: explore real API read-only (recording on) → build/test destructive flows
against the twin, injecting faults to harden error handling → identical smoke run against
staging. The agent never needs mutation rights on shared environments for 95% of its work.

**Security requirement (non-negotiable, ships with the recording proxy):** recordings contain
real data. Redact `Authorization`, `Cookie`, `Set-Cookie`, and `*key*`/`*token*` headers by
default; support a redaction config (headers, body JSON paths) before any cassette is
written. Document "don't commit unredacted corpora."

**Known-hard territory (be honest in docs):** state inference is heuristic and breaks on
non-CRUD APIs, cross-resource side effects, server-computed fields. Ship it as
explicitly best-effort with per-resource overrides, after the recording tiers prove out.

## 7. Roadmap

Each phase ships something independently useful. **Re-cut from v0.1**: the differentiated,
low-competition work (explore → validate → smoke test) now ships in full before the twin,
which is the hardest work and competes with mature funded products for the least marginal
advantage.

| Phase | Deliverable | Notes |
|---|---|---|
| 1 | `list`, `describe`, `search`, `uses`, `call --dry-run` | Spec loading (3.x via libopenapi, 2.0 via `openapi2conv`), operation model, curl builder. Fully testable offline. Already useful as a spec→curl tool. **Verify 2.0 conversion against real specs here.** |
| 2 | Real execution | JSON output, exit codes, `--base-url`, profiles, env-var auth, mutation gating, `AGENT.md` v1. **Full §5a secrets machinery ships here: `SecretRef` type, symbolic curl, redacted output, `-K -` config-on-stdin exec, stdin-ownership rule, `auth check`, canary CI tests.** Not retrofittable — it shapes the core `Request` type. |
| 3 | Response validation | Status documented? Body matches schema? Content-type? `validation` block in output; exit code 4 semantics. Built on libopenapi-validator. |
| 4 | `run` smoke mode | Tag/operation filters, examples→fixtures→gen data, JUnit/JSON reports. CI-ready. **End of the differentiated core — ship, use it, and let real usage decide whether the twin earns its keep.** |
| 5 | Recording proxy + redaction | The corpus spine. `twin record`. Useful on its own even without a twin server. |
| 6 | `twin serve`: replay + spec fallback + request validation | Prism parity, but corpus-fed. Router comes from libopenapi-validator. |
| 7 | Stateful twin + data synthesis | CRUD inference, generalization from recordings — the actual differentiator (§6). |
| 8 | Fault & latency injection | Control endpoint + CLI; latency profiles from corpus. |

**Decision gate after Phase 4.** Phases 5–8 are roughly as much work as 1–4 and land in a
crowded, well-funded market. Do not start them on faith; start them because using Phases 1–4
made the absence of a twin painful.

Distribution: single static binaries per platform (GitHub releases), `go install`,
curl-able install script (agents can bootstrap the tool mid-session), Homebrew tap later.
Packaging as a pi package and a Claude Code skill wrapping the same binary.

## 8. Open questions

- **Name — the live decision.** `apitest` is a placeholder and undersells the twin.
  `any-api` (the current repo name) is crowded: GitHub already carries an R package
  (`jonthegeek/anyapi`), a Swift client (`jpmcglone/AnyAPI`), two Python wrapper libraries,
  a zero-config REST server (`RoryCombe/anyapi`), and an `anyapi-io` organisation. The repo
  name is claimable, but search discoverability is poor. Because this ships as a Go binary via
  GitHub releases / `go install` / Homebrew, the namespaces that actually matter are **the
  Homebrew formula, the GitHub repo, and the binary name on `$PATH`** — npm and crates.io are
  noise. Candidates and collision evidence are tracked in the research doc; decide before
  Phase 1 so `cmd/<name>` and the `"schema": "<name>/v1"` field are right the first time.
- **Default output.** Locked: pretty-on-TTY, JSON-when-piped — revisit if agents get
  confused by TTY detection in odd sandboxes (`--output` always wins).
- ~~**kin-openapi vs libopenapi.**~~ **Resolved (§5):** libopenapi for 3.x + libopenapi-validator,
  kin-openapi's `openapi2conv` for the 2.0→3.x hop only.
- **libopenapi-validator strictness on 3.0 specs.** It defaults to 3.1+ strict JSON Schema
  behaviour. Confirm in Phase 3 whether that produces false validation failures against
  real 3.0 specs, and whether it can be configured down per-document.
- **Request chaining / scenarios.** Explicitly out of scope for v1; the twin reduces the
  need. Revisit if real usage demands it. (Note: Schemathesis does this via OpenAPI `links`.)
- **OAuth flows.** Out of scope v1 (bring your own token). Restish shows the cost of
  doing this properly.
- **`describe` compact schema format.** Design the terse rendering early — it's the
  agent-facing UX centerpiece. Prototype against big real specs (GitHub, Stripe).
  phyllotaxis is prior art worth reading before designing this.
- **Body-in-config encoding.** The config document needs correct escaping for arbitrary JSON
  bodies (`data = "..."` with backslash escapes). Decide the exact cutover point to the
  0600 temp-file fallback (size? binary content-type?) in Phase 2.
- **Twin state overrides.** Config format for correcting bad CRUD inference
  (per-resource: id field, collection path, relations).

## 9. Non-goals

- Not a general HTTP client (that's curl/HTTPie/Restish).
- Not property-based fuzzing (that's Schemathesis; potential future `fuzz` command,
  but don't dilute v1).
- Not an MCP server. Ever. The absence is the point — see §2 for the honest framing
  of that stance.
- Not a Code Mode / SDK generator. Different bet, different tradeoffs (§2).
- No GraphQL, no gRPC. OpenAPI/Swagger REST only.
- No interactive TUI mode in v1.
