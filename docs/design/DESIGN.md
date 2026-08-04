# talaria — Design Document

*Status: draft v0.6 · amended 2026-08-04 · Phases 1–3 built and merged; phase 2a remediation in flight*

> **Name resolved: `talaria`** — the winged sandals of Hermes. The tool is not the messenger;
> the agent is. This is what it wears to move fast. Fixes `cmd/talaria`, the binary on `$PATH`,
> `TALARIA_AUTH_*` env vars, and the `"schema": "talaria/v1"` output field.

**Changes in v0.6:** two rules the doc left unstated and the phase-2a reviews then found the code
guessing at. A selected profile's own `base-url` host is in the allowed set (§5a, source 4), which
makes the README's headline profile workflow send the credential the profile names — cycle-1
finding 7. And the referenced-body rule binds every stdout surface, not just the emitted curl
(§3.4, §4): `request.body` carries `"@/path"` / `"@-"` for a body the caller did not type, because
an envelope that withholds a body from one field and prints it in the next has withheld nothing —
cycle-2 finding 5. Reviews archived under [docs/review/](../review/).

**Changes in v0.5:** `run` is cut (§4, §5, §7) — spec-driven smoke testing is well served
elsewhere and was the largest, least differentiated part of the tool. Three rules the phase-2
design settled and the code is about to implement: an emitted curl references a file- or
stdin-supplied body rather than inlining it (§3.4); the remote spec cache gets a 24-hour TTL
with conditional revalidation and `--refresh` (§4); and `history replay` resolves nothing from a
stored entry, which retires the env-var-namespace rule in §5a's replay table. Rationale in
[docs/plans/2026-08-02-phase-2-boundary-design.md](../plans/2026-08-02-phase-2-boundary-design.md).

**Changes in v0.4:** three invariants the code needed and the doc never stated — credentials
bind to the spec's `servers[]` (§5a), a history entry is untrusted input when read (§5a), and a
security scheme v1 cannot resolve is reported rather than silently ignored (§5). Each was found
by review finding the same defect in a new place; rationale in
[docs/design/proposals/2026-08-03-amendments.md](proposals/2026-08-03-amendments.md).

**Changes in v0.3:** repositioned around a single idea — *an API client you can hand to an
agent*. The credential firewall moves from a design principle to the product's reason to exist
(§1, §2, §5a). Postman / Insomnia / Bruno enter the landscape as the actual incumbents (§2).
`history` added, unifying "what did I call" with the twin's corpus (§4, §5, §7). Name settled.

---

## 1. Vision

**Postman and Insomnia for agents.**

Point it at any API's OpenAPI/Swagger doc and an agent can explore, call, and test that API —
**without ever being given your credentials.**

That second clause is the product. Every existing API client — Postman, Insomnia, Bruno, curl
itself — assumes the operator is a human who is entitled to see their own secrets. Hand an
agent a Postman collection with an environment, or a `.bru` file, or a shell with
`$STRIPE_KEY` exported, and the agent has your key. It is in the context window, in the
transcript, in the logs, and on whatever server processed the request.

This tool is the trust boundary. The agent operates on credential *names*; the binary resolves
*values* at the last possible moment and never emits them. See §5a — it is the core of the
design, not a hardening pass.

Three capabilities, from one spec, in one binary:

1. **An explorable API client** — list, describe, search, and call operations, curl underneath.
2. **A spec-driven tester** — validate real responses against the contract; smoke-test whole APIs.
3. **A digital twin** — a local server that mimics the real API for safe, fast, offline testing.

**One-line pitch:** *Postman for agents. Point it at any API doc; your agent works the API
and never sees your credentials.*

### Why an agent needs a different client

Postman's value to a human is persistence and organisation: saved requests, curated
collections, shared workspaces. An agent needs none of that — it re-derives what it needs from
the spec on every run. What an agent needs instead, and what no existing client provides:

| Agent need | Why existing clients fail it |
|---|---|
| **Never see credentials** | Every client assumes a human operator who owns the secrets |
| **Progressive discovery** | A collection or spec must be loaded whole; a 2MB `swagger.json` is a context bomb |
| **No setup step** | Collections must be built and maintained before they are useful |
| **Deterministic, parseable results** | GUI-first tools bolt on CLI runners as an afterthought |
| **Safety rails in the tool, not the prompt** | Nothing stops a collection runner from issuing `DELETE /users` |

## 2. Why build this (landscape)

### The incumbents under this framing

| Tool | What it is | Why it does not serve an agent |
|---|---|---|
| **Postman** | The category default. GUI-first, cloud workspaces, `newman` CLI runner | Collection-centric; agent sees environment secrets; newman is a separate npm package on its own release cadence |
| **Insomnia** | GUI client with `inso` CLI, git-native storage | Same credential exposure; CLI is secondary to the GUI |
| **Bruno** | **The one that matters.** Git-native, offline-only, collections as plain `.bru` files, no account, has a CLI runner. **Crossed 44,000 GitHub stars in May 2026**, growth accelerated by Postman's March 2026 free-plan cut | Closest in spirit — local, plain-text, scriptable — but still collection-centric, and its `.bru` environments hand secrets straight to whoever runs them |
| **Hurl** | Plain-text `.hurl` files, assertions, git-native, CI-focused | A test runner, not an explorer; no spec awareness; secrets are plain in files or env |
| **Apidog** | Commercial all-in-one; **its CLI already emits structured JSON with `agentHints.nextSteps`** | Someone is already aiming at agent-readable output — but the credential model is unchanged |

**Two things separate this tool from all of them:**

1. **Spec-centric, not collection-centric.** They make you build and maintain a collection,
   which then drifts from the API. Here the OpenAPI doc *is* the collection. Nothing to curate,
   nothing to drift, and it works against an API the agent has never seen before — no setup step.
2. **The credential firewall.** They are built for a human who is entitled to the secrets.
   This is built for an operator who is not.

### The adjacent camps

| Camp | Examples | What they lack |
|---|---|---|
| Spec-aware CLI clients | **Restish** (Go, 1,348★, active) | No response validation — `--rsh-validate` is opt-in and covers *outbound JSON request bodies only*; it explicitly "trusts explicit input and lets the server validate API semantics." No twin. No credential firewall. Its v2 is adding **MCP-ready extension points** — moving toward MCP as this moves away |
| Spec exploration for LLMs | **phyllotaxis** (Rust) | Read-only: no execution, validation, testing, or mocking. Overlaps Phase 1 only |
| Spec-based testers | **Schemathesis**, Dredd | CI tools, not interactive exploration; Python/Node runtimes |
| OpenAPI→MCP bridges | FastMCP, AWS openapi-mcp-server, Speakeasy, Stainless | Context bloat (below); a running server per API |
| **Code execution / Code Mode** | Cloudflare Code Mode, Agentgateway, Anthropic's code-execution pattern | The most serious architectural rival. Requires a code sandbox; no response-vs-spec validation; no twin; leaves no artifact a human can paste into a terminal |
| Mock servers | Prism, **Specmatic**, Microcks, **WireMock**, Mockoon | Standalone; no integrated exploration/testing CLI; no agent ergonomics |
| curl generators | curlgenerator, Swama | No execution, validation, or testing |

### The token problem is real and now mainstream

- **Cloudflare's API has 2,500+ endpoints.** As an MCP server that is **1.17M+ tokens**; their
  Code Mode (Feb 2026) exposes the same API in ~1,000 tokens via two tools.
- **Anthropic's code-execution pattern** measured 150,000 → 2,000 tokens on one workflow — 98.7%.
- **Perplexity's CTO** announced an internal shift away from MCP over context waste and auth friction.

**Counterweight, stated honestly:** MCP has crossed 8M server downloads and 97M monthly SDK
downloads, with Google, OpenAI and Microsoft shipping it. "Not an MCP server, ever" (§9) is a
product *stance*, not a prediction that MCP loses.

### Honest competitive assessment

- **Bruno is the incumbent to beat on developer sentiment**, and it is winning that market on
  git-nativeness and price, not on agent ergonomics. It is not trying to solve the credential
  problem. That is the opening.
- **Restish is the closest technical competitor** and it is actively developed. Do not claim it
  has no validation; claim it has no *response* validation, no twin, and no credential firewall.
- **phyllotaxis proves the exploration idea by independent invention** — read-only, small, and
  worth reading before designing `describe` (§8).
- **The twin's mechanism is not novel.** Specmatic already records real traffic into
  spec-validated mocks; WireMock has record & playback plus stateful create-then-fetch.
  Replay and statefulness are **table stakes** — see §6 for what actually differentiates.
- **Code Mode is the most serious architectural rival**, backed by Cloudflare and Anthropic.
  The honest edge: it needs a code sandbox and produces nothing a human can paste into a
  terminal; this needs only a shell and emits a curl command every time.

Primary motivation remains: a tool for my own workflow. Open source is upside.

## 3. Design principles

0. **Secrets never reach the agent. Priority one, non-negotiable.**
   This is §1 — the reason the tool exists — not a hardening measure. The binary is the trust
   boundary: the agent operates entirely on credential *names* (env vars, profile names);
   credential *values* are injected at the last possible moment and never appear on stdout,
   stderr, in dry-runs, in emitted curl commands, in error messages, in history, or in
   recordings. See §5a for the threat model. Any feature that would print a secret is a bug,
   even if the user asked for it — **there is no `--show-secrets` flag.**

1. **Agent-friendly = good CLI design, enforced strictly.**
   - Machine-readable output everywhere: `--output json` on every command.
     Default: pretty when stdout is a TTY, JSON when piped.
   - Deterministic exit codes (§4). Agents branch on these.
   - Never prompt. Never page. No interactivity, ever. Dangerous behaviour is gated by flags
     set deliberately by a human, not by confirmations an agent can answer itself.
   - Errors to stderr as structured JSON: what failed, why, valid alternatives.
   - Versioned output schema (`"schema": "talaria/v1"`) so agent prompts don't break.

2. **Progressive disclosure is the product.** `list` is one compact line per operation;
   `describe` renders schemas in a terse readable form (`name*: (string) description`), never
   raw JSON Schema dumps. Neither human nor agent ever loads the raw spec into context.

3. **No collection to maintain.** The spec is the collection. There is no import step, no
   curation, no drift. A spec URL and a credential name is the entire setup.

4. **curl is the execution engine and the lingua franca.** Build argv via `os/exec` (never
   shell strings — no quoting hell, no injection). `--dry-run` prints the exact curl command.
   Every executed call returns its curl equivalent: a portable reproduction for bug reports,
   docs, and scripts. **Emitted curl always references secrets symbolically** — e.g.
   `-H "Authorization: Bearer $TALARIA_AUTH_BEARER"` — runnable in a shell where the env var is
   set, useless to exfiltrate (§5a). **A request body is referenced, never inlined, unless the
   caller typed it into argv:** `--body @file` emits `--data-binary @file` and `--body -` emits
   `--data-binary @-`, because a body read from a file or stdin may carry a credential the agent
   never saw. `--data-binary` rather than `--data`: `--data` strips the newlines out of a file, so
   a pretty-printed body file would make the emitted command send different bytes than the call
   did, and a reproduction that is not the request is worse than none. Only an argv-supplied body
   is inlined, since it is already in the agent's hands.
   This keeps the emitted command both runnable and safe to print. **The rule is about the body,
   not about the curl field:** it binds every stdout surface that would show the bytes, including
   the envelope's own `request.body`, which carries `"@/path"` / `"@-"` for a referenced body
   rather than its contents (§4). One envelope may not answer the same question two ways — a
   `--body @secrets.json` that is withheld from `request.curl` and printed in `request.body` is
   not withheld. **Check curl's version, not just presence:**
   `--write-out '%{json}'` requires curl ≥ 7.70 and is a hard floor.

5. **Safe by default.** Read-only (GET/HEAD/OPTIONS) unless `--allow-mutations`. The sanctioned
   answer to "I need to test a DELETE" is: do it on the twin. Recordings redact by default.

6. **Composability over features.** Plain JSON to stdout for jq; no built-in query language in
   v1. Spec via arg, `--spec`, or env var. **The process owns its own stdin** (§5a) — `--body -`
   works and never competes with how secrets reach curl.

7. **The agent README is a first-class deliverable.** An operating manual written for LLMs: the
   list→describe→dry-run→call workflow, exit codes, output shapes, and — most importantly — how
   to name a credential you cannot see. Ships as `AGENT.md`; doubles as a pi skill and a Claude
   Code skill wrapping the same binary. Treat it with the same care as code.

## 4. CLI surface

```
# Discovery (cheap, no network beyond fetching the spec)
talaria list [spec] [--tag t] [--output json|pretty|tsv]
talaria describe [spec] <operationId> [--output json|pretty]
talaria search [spec] <query> [--kind operation|schema|param]   # fuzzy find across the spec
talaria uses [spec] <schemaName>                                # reverse lookup: which operations use it

# Calling
talaria call [spec] <operationId>
    --param id=42                 # path params
    --query verbose=true          # query params (repeatable)
    --header X-Foo=bar            # extra headers (repeatable)
    --body '{"..."}' | --body @file.json | --body -   # stdin
    --base-url https://staging.example.com
    --profile staging             # named config: base-url + auth + headers
    --dry-run                     # print curl command, send nothing, exit 0
    --allow-mutations             # required for POST/PUT/PATCH/DELETE
    --timeout 30                  # seconds; give up on the request rather than hang
    --output json|pretty

# History — what did I call, what came back
talaria history [--operation id] [--since 1h] [--status 4xx] [--output json]
talaria history show <n>           # full request/response, redacted
talaria history replay <n>         # re-issue a past call

# Digital twin (later phases)
talaria twin serve [spec] --port 9000 [--corpus ./twin-data] [--stateful]
talaria twin record [spec] --upstream https://api.real.com --port 9000 --corpus ./twin-data
talaria twin fault <operationId> --status 429 --rate 0.1 [--header Retry-After=30]

# Meta
talaria auth check [spec] [--profile p]   # are credentials PRESENT for the spec's security
                                         # schemes? never prints values:
                                         # {"scheme":"bearerAuth","source":"env:TALARIA_AUTH_BEARER","present":true}
                                         # plus "withheld":true when a call under the same flags
                                         # would not send it (§5a) — present is not sendable
talaria version
```

**`search` and `uses`** are adopted from phyllotaxis. They matter because an agent usually knows
a *domain concept* ("something about invoices") or a *data model* ("what touches `Invoice`?")
rather than an operationId — and `list` on a large spec is the context dump this tool exists to
avoid.

**`history` is new in v0.3**, and it is the one genuine feature the Postman framing adds. It
answers "what have I already tried and what came back", which is the question an agent asks
constantly and currently cannot. It costs little because **history and the twin's corpus are the
same data** — every call already produces a redacted request/response pair. Local history is the
seed; the recording proxy (Phase 5) is the industrial version. `history replay` also gives the
useful half of request chaining without the scenario DSL (§8).

Spec argument accepts a file path or URL (with local cache); falls back to an env var.

**Cache policy.** A URL-sourced spec is cached with its `ETag`/`Last-Modified`. Inside 24 hours
it is served from cache with no network call. Past that it is revalidated with a conditional
GET — a 304 refreshes the timestamp without re-downloading. `--refresh` forces a fetch. An
unbounded cache is not acceptable: every downstream stage works against the contract, and a
stale one makes `validate` report violations the server never committed.
Supports Swagger 2.0 and OpenAPI 3.0/3.1/3.2, JSON and YAML (§5).

### Output shape for `call` (sketch)

```json
{
  "schema": "talaria/v1",
  "request": {
    "curl": "curl -q -s -H \"Authorization: Bearer $TALARIA_AUTH_BEARER\" 'https://…'",
    "method": "GET", "url": "…",
    "headers": { "Authorization": "<redacted:env:TALARIA_AUTH_BEARER>" },
    "body": "@/path/to/body.json"
  },
  "response": { "status": 200, "headers": {}, "body": {}, "timing_ms": 143 },
  "validation": { "status_documented": true, "body_valid": true, "errors": [] }
}
```

`request.body` is present only when the call had one, and it is the same answer `request.curl`
gives one field over (§3.4): the bytes for a body typed into argv, redacted; `"@/path"` or `"@-"`
for a body read from a file or from stdin. A value beginning with `@` is therefore a reference,
never a body — and a body is never a reference, because an argv body is inlined whatever it looks
like.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Success (HTTP 4xx/5xx is exit 0 for `call` — a 404 is a successful *observation*; use `--fail-on-error` to change) |
| 1 | Request could not be completed (network, curl failure) |
| 2 | Usage error (unknown operation, missing required param — stderr JSON lists valid options) |
| 3 | Spec parse/load error |
| 4 | Validation failure (response violates spec) — only with `--fail-on-error` |
| 5 | Credential missing for a required security scheme (distinct from usage error so agents can act on it: *ask the human to set `$NAME`*) |

## 5. Architecture

```
cmd/talaria/main.go         # cobra wiring
internal/spec/             # load (file/URL/cache), v2→v3 convert, normalize
internal/operation/        # THE core model: id, method, path, params, body schema,
                           #   auth requirements, response contracts
internal/secret/           # SecretRef, resolution, redaction  ← the trust boundary
internal/curl/             # argv + config-document builder, executor (os/exec)
internal/validate/         # request & response vs schema  ← shared with twin
internal/corpus/           # history + recordings: store, redact, index, generalize
internal/twin/             # http server: replay, fallback gen, state, faults
internal/output/           # json / pretty / tsv renderers, versioned schemas
internal/config/           # profiles, env vars, auth mapping
```

Key boundary rules:
- `operation`, `validate`, `gen` are consumed by **both** the curl executor and the twin server.
  Nothing in them may import `curl` or `twin`.
- `secret` is imported by nearly everything but **resolves** values for exactly one caller:
  the `curl` executor at exec time (§5a).
- `corpus` backs both `history` and the twin, so it is defined in Phase 2 even though the
  recording proxy lands in Phase 5.
- Twin's serve side uses `net/http` directly — curl is only for outbound calls.

### Library choices

- **CLI:** spf13/cobra (+ viper for profile/env layering). Cobra's generated help is
  well-structured text that agents read happily.
- **OpenAPI 3.x:** `pb33f/libopenapi` — a 3.2/3.1/3.0 + Overlays + Arazzo toolkit, actively
  developed, and what Restish migrated to.
- **Swagger 2.0:** `getkin/kin-openapi`'s **`openapi2conv`**, used *only* to convert 2.0 → 3.x
  at load time. libopenapi cannot do this: its v2 model is unmaintained, slated for removal,
  and its authors say "**DO NOT** take a dependency on it" and "we don't recommend using it…
  it's a commercial product." Two OpenAPI dependencies is the cost of the "any API doc"
  promise — much of the real world is still 2.0. Conversion runs once at load, so everything
  downstream sees one model. **Phase 1 must verify conversion fidelity against real 2.0 specs.**
- **Schema & message validation:** `pb33f/libopenapi-validator`. Validates `http.Request` *and*
  `http.Response` against a 3.x spec and ships a standalone OpenAPI router with auth callbacks
  and replayable bodies — most of what the twin needs, from the spec model the CLI already
  holds. Watch one behaviour: it defaults to OpenAPI 3.1+ strict JSON Schema semantics, which
  can be stricter than a 3.0 spec intends; verify against 3.0 specs early.
- **Execution:** system curl via `os/exec`. Not a Go HTTP client — the curl command *is* a
  feature (transparency, reproducibility).

### Auth

The spec declares security schemes; credentials come from outside and are named, never shown:
- Env vars by convention: `TALARIA_AUTH_BEARER`, `TALARIA_AUTH_BASIC`, `TALARIA_AUTH_APIKEY_<SCHEME>`.
- Profiles (`~/.config/talaria/config.yaml`, mode 0600): named sets of base-url + auth +
  headers, selected with `--profile`. Secrets in profiles may reference env vars.
- `auth check` reports which schemes are satisfied and from which *source* — never the value —
  so an agent can diagnose a broken auth setup blind and tell the human exactly what to set.
- v1 resolves `http bearer`, `http basic`, and `apiKey` (header, query, cookie) schemes.
- A scheme outside that set — `oauth2`, `openIdConnect`, `mutualTLS` — is **unsupported, not
  invisible**. When an operation requires one:
  - if `TALARIA_AUTH_BEARER` is set, the scheme is satisfied by that token. This *is* "bring
    your own token": the user obtained it however the flow demands, and talaria carries it;
  - if it is not set, the operation is unsatisfiable. `auth check` reports
    `{"scheme":"oauth2","supported":false,"present":false}` and exits 5; `call` and `run` exit 5
    with a structured error naming the scheme and the variable to set.
- **`auth check` never reports a scheme satisfied when the call would refuse it.** The two agree
  by construction, or `auth check` is worthless to an agent. Implementing OAuth *flows* remains
  out of scope; Restish shows the cost of doing them properly.
- **A spec `call` refuses for its own strings is exit 2 from `auth check` too**, with no report:
  a `paths:` key that is not an absolute path, and an `apiKey` scheme whose `name:` cannot be
  sent where the scheme puts it. Both are spec-controlled text that would reach the wire, both
  are refused where they are read, and neither is a state of the environment a report could
  describe — so the pre-flight answers what the call answers rather than exiting 0 on a document
  no call can be made from.

## 5a. The credential firewall

**Invariant: no consumer of stdout or stderr ever observes a credential value.**

The setup is asymmetric by design. A human (or CI) places secrets in env vars or a profile file
once. The agent only ever names them. This is what makes it safe to point an agent at a
production API doc.

### How secrets reach curl: the config document on curl's stdin

**Primary mechanism: `curl -K -`.** curl reads a config document from stdin when the filename
is `-`. Secret-bearing headers, credentials, and the request body go in that document;
**nothing sensitive is ever an argv element.** This is the documented way to keep credentials
out of the process list — `/proc/*/cmdline` is readable by any process on the host, including
ones the agent spawns.

**Resolving stdin contention.** `--body -` (body from stdin) and `-K -` (config from stdin)
appear to collide. They do not:

> **The Go process owns the real stdin. curl's stdin is always a fresh pipe that Go writes.**

`--body -` is read to EOF by the Go process from *its* stdin. curl is then spawned with a new
pipe on *its* stdin, carrying the config document — including the body as a `data` directive.
One stdin consumer at each level. For large or binary bodies, fall back to a 0600 temp file
referenced as `data = "@/path"`, deleted immediately after exec. The temp file is the exception.

### Credentials bind to hosts

**Invariant: a resolved credential is transmitted only to a host the spec declares, or one a
human has explicitly allowed.**

Redaction answers *does the secret appear in output*. It does not answer *which host may
receive it* — and without that second rule, `--base-url https://attacker.example` sends a
production key to an attacker while violating nothing.

The allowed host set for a call is:

1. every host in the spec's `servers[]`, after server-variable substitution; plus
2. every host passed as `--allow-host HOST` (repeatable); plus
3. every host in `allow_hosts:` in the active profile; plus
4. the host of the active profile's own `base-url`, when a profile was selected.

(4) is the same category as (2), not a weakening of it. A profile is a human-authored file at
mode 0600 that names a base URL and a credential together, and it applies only when the caller
selects it by name — that is a human explicitly allowing a host, spelled once instead of twice.
Without it the headline profile workflow resolves a credential and then withholds it from the
destination the same file names, which reads as a bug and is fixed by copying the host into
`allow_hosts:` — a step that teaches operators to keep a redundant list in sync and buys no
safety, since anyone who can write `base-url` can write `allow_hosts` in the same file. Note
what (4) does **not** cover: `--base-url` on the command line is still outside the set unless
some other source names it, because a flag is a per-invocation redirection and the twin case
above is exactly why withholding there is right.

When `--base-url` points outside that set the request still runs, but **every credential is
withheld**, and the omission is reported both ways — a one-line stderr warning naming the
withheld schemes and the offending host, and a machine-readable field in the envelope so an
agent can act on it instead of inferring it from a downstream 401:

```json
"credentials_withheld": [
  {"scheme": "bearerAuth", "reason": "host not in spec servers[]", "host": "localhost:9000"}
]
```

Withholding rather than refusing is deliberate: pointing at a local twin is the most common
`--base-url` use, and the twin accepts placeholder credentials by design (§6). An agent working
against the twin must not need a flag, and must not be handed a real secret.

`auth check` reports against the *resolved* host set, so "present" never means "will actually
be sent". The entry carries `"withheld": true` when it would not be — one verdict per
invocation, true when any of the spec's operations resolves off the set, since the path decides
the host as much as the base URL does. Like `supported` (§5), it appears only when it is true.

This is default-deny with a deliberate override, the same shape as `--allow-mutations` in §3.5.

### History is untrusted input when read

Redaction at write time protects what *leaves* the tool. It says nothing about what comes back
*in*. A history file is a persistent artifact: it may have been written by another project,
copied from another machine, or edited by hand. **Every field in a history entry is untrusted
input.**

`history replay` therefore re-derives rather than replays:

| Field | Treatment on replay |
|---|---|
| operationId, params, body | Re-bound through the normal request-construction path, re-validated against the current spec |
| Credentials | **Never taken from the entry.** Re-resolved from the current environment and profile, subject to the host-binding rule above |
| Target host | From the current `--base-url`, profile, or spec — never from the stored URL. If the stored host is outside the currently allowed set, replay refuses with exit 2 rather than silently retargeting |
| Env var names | Not applicable — nothing in a stored entry is resolved. Credentials come from the current environment and profile via the normal resolution path, so a stored entry cannot name a variable at all |
| Sizes and types | Bounded and type-checked before use. A corrupt or hostile entry fails that entry, never the process |

The rule in one line: **a history entry is data, never instruction.**

The bound a reader applies to a stored line is also a bound on the line talaria *writes*, or the
tool records entries it will then refuse to read. A body is capped at 64 KiB with an explicit
`"truncated": true`, and each side's headers at their own budget with `"headers_truncated": true`
— both appear only when true, like `supported` and `withheld`. Neither cap bounds the encoded
line on its own, since JSON expands a control byte six-fold, so a line still over the limit has
its bodies cut further until it fits, and one that cannot be made to fit is refused with a
warning on stderr rather than written. **An entry talaria reports as recorded is one every
reader returns**; silent loss is not an available outcome, in either direction.

### Leak channels and countermeasures

| Leak channel | Countermeasure |
|---|---|
| Emitted/dry-run curl commands | Always symbolic: `-H "Authorization: Bearer $TALARIA_AUTH_BEARER"`. Runnable where the env var exists; useless to exfiltrate |
| `request.headers` in JSON output | Sensitive headers rendered `<redacted:env:NAME>`. Built-in list (`Authorization`, `Cookie`, `Proxy-Authorization`, `*api*key*`, `*token*`, `*secret*`) + user-extensible. Applies to pretty output too |
| curl process argv (`ps`, `/proc/*/cmdline`) | Never argv. Config document on curl's stdin (above) |
| Response headers/bodies (`Set-Cookie`, login endpoints returning `access_token`) | Redact `Set-Cookie` and configurable response-body JSON paths by default; document that auth-issuing endpoints are for humans, not agents |
| **History** | Same redaction as output, applied at write time. History is a permanent artifact — it is the highest-risk surface in the tool |
| Error paths (curl stderr, validation errors quoting the request) | Errors are built from the redacted representation, never the raw one. Test explicitly — error paths are where redaction bugs live |
| Recordings / corpus | Redaction at **write** time, not read time. Un-redacted recording is not an option |
| Spec cache, verbose/debug logs | Debug output goes through the same redacted representation. No `--verbose` that bypasses it |
| Query-string API keys (`?api_key=`) | Symbolic in emitted curl, `<redacted>` in output URL fields. These still leak into *server* logs — warn once on stderr |

**Architecture consequence.** Exactly one component (`internal/curl`, at exec time) ever holds
resolved secret values, and only long enough to write the config document. Everything else —
output, validation, history, corpus, errors — operates on a `Request` whose secret fields are
*structurally* references (`SecretRef{EnvVar: "…"}`), not strings. Leaking then requires
deliberately resolving a ref, which is greppable and reviewable. Forgetting to scrub a string
is neither.

**Twin synergy.** The twin needs **zero real credentials**. It enforces that *a* credential is
present (auth realism) but accepts placeholders — so the agent's entire develop-and-test loop
runs secret-free, and real credentials exist only in a final human- or CI-gated run.

**Testing requirement.** A CI suite injects canary secrets through every auth mechanism and
greps every output surface — json, pretty, tsv, dry-run, errors, history store, corpus files,
debug logs — for them. Redaction regressions fail the build. **This suite is
Phase 2 work and gates every release thereafter.**

### Threats explicitly not covered

Honesty about the boundary's limits:
- **A response body containing a secret** is returned to the agent, because the tool cannot know
  a field is sensitive unless configured. Mitigated by default `Set-Cookie` redaction and
  configurable body paths; not solved.
- **An agent that can read env vars or the profile file directly** bypasses everything here.
  This tool secures the *API-calling* path, not the machine. Running the agent without those
  vars in its environment — and letting the binary read them from a file it alone opens — is
  the deployment that fully realises the boundary.
- **Server-side logs** may record query-string keys regardless of what this tool does.

## 6. The digital twin

**Goal: mimic the real API, not just return schema-valid noise.**

**What is table stakes and what is not.** Replay-from-recordings and stateful CRUD already exist
in mature products — Specmatic records real traffic into spec-validated mocks; WireMock does
record & playback plus stateful create-then-fetch. Building those buys parity, not advantage.
Three things are genuinely differentiated:

1. **Synthesis by generalization from the corpus** — synthesize user 99 by mutating recorded
   users 42 and 57: real field values, real enum usage, real nullability, instead of
   `"string"`/`0` schema noise.
2. **Deterministic fault injection from the same CLI that explores and tests** — no second
   tool, no second config format, no glue.
3. **Zero-credential operation** — the develop-and-test loop runs without a real secret ever
   existing on the machine (§5a). This is the twin's tightest link to §1.

| Dimension | Mechanism |
|---|---|
| **Data realism** | Recordings over generation; generalize from them (1 above) |
| **Behavioral realism (state)** | Infer CRUD lifecycles from REST conventions + spec (POST returns created object with id; path param matches id field). In-memory/bbolt store so create→get→delete coheres. Recorded sequences confirm inferred semantics |
| **Error realism** | Injectable faults via CLI or a `POST /_twin/faults` control endpoint. Real error bodies harvested from recordings. Deterministic faults are where the twin beats the real API |
| **Temporal realism** | Per-endpoint latency profiles from recorded timings (already captured via curl `--write-out`). Optional rate-limit simulation. Nice-to-have |
| **Auth realism** | Enforce the spec's security schemes; reject missing/bad credentials with the status codes seen in recordings. Accepts placeholder tokens |

**Recording is the spine.** The spec provides structure; recorded traffic provides truth. Every
twin feature consumes the same corpus — the same store that backs `history`. Day 1 is a
spec-only static mock; explore the real API through the recording proxy and the twin gets
progressively more real, automatically. *"Your twin gets better the more you use the real API."*

**Request validation on the twin.** It validates incoming requests against the spec and returns
precise schema errors, doubling as a client-side contract checker. `libopenapi-validator`'s
router and request validator provide this directly.

**The loop this unlocks** (identical commands, swap `--base-url`):

```
talaria twin record spec.yaml --upstream https://api.real.com --corpus ./twin
talaria call spec.yaml listUsers --base-url http://localhost:9000        # via proxy, recorded
talaria twin serve spec.yaml --corpus ./twin --stateful
talaria call spec.yaml deleteUser --param id=42 --base-url http://localhost:9000 \
    --allow-mutations                                                     # safe destructive testing
talaria call spec.yaml listUsers --profile staging                        # final verification
```

The agent explores the real API read-only with recording on, builds and tests destructive flows
against the twin while injecting faults, then re-issues the same calls against staging. It never
needs mutation rights on a shared environment for 95% of its work.

**Security requirement (non-negotiable, ships with the recording proxy):** recordings contain
real data. Redact `Authorization`, `Cookie`, `Set-Cookie`, and `*key*`/`*token*` headers by
default; support a redaction config before any cassette is written. Document "don't commit
unredacted corpora."

**Known-hard territory (be honest in docs):** state inference is heuristic and breaks on
non-CRUD APIs, cross-resource side effects, and server-computed fields. Ship it explicitly
best-effort with per-resource overrides, after the recording tiers prove out.

## 7. Roadmap

Each phase ships something independently useful. The differentiated, low-competition work
ships before the twin, which is the hardest and competes with mature funded products for the
least marginal advantage.

| Phase | Deliverable | Notes |
|---|---|---|
| 1 | `list`, `describe`, `search`, `uses`, `call --dry-run` | Spec loading (3.x via libopenapi, 2.0 via `openapi2conv`), operation model, curl builder. Fully offline-testable. Already useful as a spec→curl tool. **Verify 2.0 conversion against real specs here** |
| 2 | Real execution + **the credential firewall** + `history` | JSON output, exit codes, `--base-url`, profiles, env-var auth, mutation gating, `AGENT.md` v1. **All of §5a ships here:** `SecretRef`, symbolic curl, redacted output, `-K -` config-on-stdin, stdin-ownership rule, `auth check`, exit code 5, canary CI suite. Plus the corpus store behind `history`. **Not retrofittable — it shapes the core `Request` type.** This phase is the product |
| 3 | Response validation | Status documented? Body matches schema? Content-type? `validation` block; exit code 4. Built on libopenapi-validator |
| 4 | ~~`run` smoke mode~~ — **cut 2026-08-03** | Built, then removed: spec-driven smoke testing is well served by Schemathesis, Hurl and newman, and it was the largest and least differentiated part of the tool. `call`, `history` and the spec commands are the differentiated core. See `docs/plans/2026-08-02-phase-2-boundary-design.md` §2.1 |
| 5 | Recording proxy + redaction | The corpus spine, industrial version. Useful alone |
| 6 | `twin serve`: replay + spec fallback + request validation | Prism parity, corpus-fed. Router from libopenapi-validator |
| 7 | Stateful twin + data synthesis | CRUD inference, generalization — the actual differentiator (§6) |
| 8 | Fault & latency injection | Control endpoint + CLI; latency profiles from corpus |

**Decision gate after Phase 4.** Phases 5–8 are roughly as much work as 1–4 and land in a
crowded, well-funded market. Do not start them on faith; start them because using Phases 1–4
made the absence of a twin painful.

Distribution: single static binaries per platform (GitHub releases), `go install`, a curl-able
install script so agents can bootstrap the tool mid-session, Homebrew tap later. Packaged as a
pi package and a Claude Code skill wrapping the same binary.

## 8. Open questions

- ~~**Name.**~~ **Resolved: `talaria`** — Hermes's winged sandals. It names the *equipment*,
  not the messenger, which is the right relationship: the agent is the messenger, this is what
  it wears. Homebrew formula free; the only GitHub collision is `talariadb/talaria` (230★,
  quiet, a time-series store). Rejected after collision checks: `hermes` (NousResearch's
  hermes-agent, 224k★, same space — fatal), `iris` (kataras/iris, 25.5k★ Go web framework —
  fatal for a Go binary), `missive`, `legate`, `tambo`, `envoi`. Runners-up worth recording in
  case of a future pivot: `nuncio` (2★, cleanest namespace), `mochila` (the Pony Express
  mailbag, swapped horse-to-horse in under two minutes), `angareion` (the Persian relay
  Herodotus described in the passage that became "neither snow nor rain…").
- **Request chaining.** Out of scope for v1, but the Postman framing makes its absence
  conspicuous — chaining is the main reason people build collections. `history replay` (§4)
  covers the common case without a scenario DSL. Revisit only if real usage demands it;
  Schemathesis does this via OpenAPI `links` if a reference is needed.
- **History retention and location.** Where does it live (`~/.local/state/talaria/`? per-project
  `.talaria/`?), how much is kept, and is it opt-out? It is the highest-risk artifact in the
  tool (§5a) and needs a deliberate answer, not a default.
- **libopenapi-validator strictness on 3.0 specs.** ~~It defaults to 3.1+ strict JSON Schema
  behaviour. Confirm in Phase 3 whether that yields false failures on real 3.0 specs and
  whether it can be configured down per-document.~~ **Resolved, Phase 3 (v0.14.0):** no false
  failures, and no per-document configuration needed. The library reads the document's own
  OpenAPI version and, for 3.0, rewrites the draft-04-era constructs before compiling —
  `nullable: true` becomes a `"null"` union, boolean `exclusiveMinimum`/`exclusiveMaximum`
  become the numeric 2020-12 spelling, and singular `example` is ignored rather than rejected.
  `internal/validate` therefore passes no strictness options. Pinned by an adversarial 3.0
  fixture (`internal/validate/testdata/strict-3.0.yaml`) carrying all four constructs, so a
  library upgrade that re-imposed 3.1 semantics would fail the suite.
- **Default output.** Locked: pretty-on-TTY, JSON-when-piped — revisit if agents get confused
  by TTY detection in odd sandboxes (`--output` always wins).
- **`describe` compact schema format.** The agent-facing UX centrepiece; design it early and
  prototype against big real specs (GitHub, Stripe). phyllotaxis is prior art worth reading.
- **Body-in-config encoding.** The config document needs correct escaping for arbitrary JSON
  bodies (`data = "…"` with backslash escapes). Decide the cutover to the temp-file fallback
  (size? binary content-type?) in Phase 2.
- **Twin state overrides.** Config format for correcting bad CRUD inference (per-resource id
  field, collection path, relations).

## 9. Non-goals

- **Not a secrets manager.** It reads credentials from env vars and a profile file; it does not
  store, rotate, or broker them. Point it at whatever you already use.
- **Not a collection manager.** No saved requests, no workspaces, no sharing. The spec is the
  collection (§3.3). This is the deliberate break from Postman.
- Not a general HTTP client (that's curl/HTTPie/Restish).
- Not property-based fuzzing (that's Schemathesis; a future `fuzz` command is possible, but
  don't dilute v1).
- **Not an MCP server. Ever.** The absence is the point — see §2 for the honest framing.
- Not a Code Mode / SDK generator. Different bet, different tradeoffs (§2).
- No GraphQL, no gRPC. OpenAPI/Swagger REST only.
- No interactive TUI mode in v1.
