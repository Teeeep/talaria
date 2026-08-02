# Research pass on DESIGN.md v0.1

*2026-08-02 · pre-implementation · sources linked inline*

Purpose: stress-test the design doc's factual claims before any code is written.
Findings are grouped by what they do to the design: **confirmed**, **needs revision**,
**blocking**. Confidence is stated where it matters.

---

## 1. Blocking: libopenapi cannot deliver the Swagger 2.0 story

**The doc says** (§5, Library choices):

> `pb33f/libopenapi` — handles 3.1 well; it's what Restish migrated to.
> Convert Swagger 2.0 → OpenAPI 3 at load time so everything downstream sees one model.

**This is not implementable as written.** libopenapi has no Swagger→OpenAPI 3 conversion,
and its v2 model is being removed. From pb33f's own documentation:

> "If you're using the v2 model package in libopenapi, please note that it is no longer
> maintained and will be removed in a future version. **DO NOT** take a dependency on it."

> "Even though libopenapi supports Swagger, we don't recommend using it. We're not working
> on swagger tooling because it's a commercial product." — [pb33f.io/libopenapi/swagger](https://pb33f.io/libopenapi/swagger/)

libopenapi bills itself as a "3.2, 3.1, 3.0, Overlays and Arazzo" toolkit. 2.0 is not in scope
and is on a removal path.

Meanwhile `getkin/kin-openapi` ships `openapi2conv`, a maintained OpenAPI 2 → 3 converter,
alongside `openapi2` and `openapi3` models.

**Options:**

| Option | Cost |
|---|---|
| **kin-openapi `openapi2conv` for the v2→v3 hop, libopenapi for the 3.x model** (recommended) | Two OpenAPI deps. Conversion runs once at load; downstream still sees one model, so the architecture in §5 survives intact. |
| Drop Swagger 2.0 from v1 | Cheapest, but a large share of real-world specs in the wild are still 2.0 — this is exactly the "any API" promise. Undercuts the name. |
| Shell out to `swagger2openapi` (Node) | Breaks the single-static-binary distribution promise. Reject. |
| kin-openapi for everything | Loses 3.1/3.2 quality and the libopenapi-validator story below. Reject. |

**Recommendation:** take the two-dependency hit. The doc should name `openapi2conv` explicitly
in §5 and add a Phase 1 task to verify conversion fidelity against real 2.0 specs.

## 2. Confirmed and stronger than stated: the validator story

`pb33f/libopenapi-validator` validates `http.Request` **and** `http.Response` against an
OpenAPI 3+ spec, and ships a standalone OpenAPI router, auth callbacks with replayable bodies,
and custom body codecs. ([repo](https://github.com/pb33f/libopenapi-validator),
[pb33f.io](https://pb33f.io/libopenapi/validation/))

This de-risks **Phase 3** (response validation) and a meaningful chunk of **Phase 6** (the twin
needs exactly this router + request-validation combination). The doc treats the validator as an
open question ("libopenapi-validator or santhosh-tekuri/jsonschema (evaluate)"); the evaluation
mostly resolves in libopenapi-validator's favour, because it gives the router for free and keeps
one spec model across CLI and twin.

Caveat worth testing early: it defaults to OpenAPI 3.1+ strict JSON Schema behaviour, which can
be stricter than 3.0 specs expect.

## 3. Confirmed: Restish does not validate responses

The doc's central competitive claim survives, but needs sharpening. Restish **does** have a
validation flag — `--rsh-validate` — and the doc's flat "no response-vs-spec validation" reads
as wrong to anyone who knows the tool. What it actually does:

> "That validation runs after body assembly, only for JSON request bodies, and does not
> rewrite or coerce values." — [rest.sh OpenAPI CLI integration](https://rest.sh/docs/reference/openapi-cli-integration/)

So: **opt-in, outbound request bodies only, JSON only.** No response validation. Restish's
stated philosophy is that it "trusts explicit input and lets the server validate API semantics."

**Revision needed:** §2's table should say *"request-body validation only (`--rsh-validate`,
opt-in); no response-vs-spec validation"* rather than implying no validation exists.

**Also:** Restish is not a static target — 1,348 stars, last push 2026-08-02 (today), with a v2
in flight that has "MCP-ready extension points". It is moving *toward* MCP while this design
moves away. That is a cleaner differentiation than "we validate and they don't", and it should
be the headline contrast.

## 4. Confirmed, emphatically: the MCP context-bloat premise

This is the doc's foundational bet and it is now mainstream, not contrarian:

- **Cloudflare Code Mode** (launched 2026-02-20): their API has 2,500+ endpoints; the native
  MCP-server equivalent would consume **over 1.17M tokens**. Code Mode exposes the same API
  through two tools in ~1,000 tokens.
- **Anthropic's code-execution pattern**: 150,000 → 2,000 tokens for the same workflow, a
  98.7% reduction.
- **Perplexity's CTO** announced an internal shift away from MCP, citing context waste and
  auth friction.
- Typical framing: three MCP servers at 40 tools each burns 100K+ tokens before the agent
  does anything.

Sources: [Apideck](https://www.apideck.com/blog/mcp-server-eating-context-window-cli-alternative),
[MCP.Directory](https://mcp.directory/blog/mcp-context-bloat-fix-2026-tool-search-code-mode-progressive-disclosure),
[Firecrawl](https://www.firecrawl.dev/blog/mcp-vs-cli).

Counterweight for honesty: MCP crossed 8M server downloads and 97M monthly SDK downloads,
with Google, OpenAI and Microsoft all shipping it. "Not an MCP server. Ever." is a defensible
product stance, but the doc should own that it is a stance against a still-growing standard,
not a prediction of MCP's death.

## 5. New competitor the doc misses: Code Mode

§2's landscape table has four camps. There is a fifth, and it is the most serious one:

| Camp | Examples | What they do | What they lack |
|---|---|---|---|
| **Code execution / Code Mode** | Cloudflare Code Mode, Agentgateway Code Mode for OpenAPI→MCP, Anthropic code-execution pattern | Generate a typed SDK on a filesystem; the agent reads files on demand and writes code against them | No response-vs-spec validation, no twin, no curl reproducibility; requires a code sandbox |

This attacks the *same* problem (token cost of large APIs) with a *different* answer (SDK +
sandbox instead of CLI + subcommands), and it is backed by Cloudflare and Anthropic rather
than by hobbyists. Any positioning that says "the alternative to MCP is CLIs" is now incomplete.

**The honest differentiation against Code Mode:** it needs a code sandbox and produces no
artifact a human can paste into a terminal; any-api produces a curl command and needs only a
shell. And Code Mode has nothing resembling the twin.

## 6. Prior art on progressive disclosure: phyllotaxis

[OpenScribbler/phyllotaxis](https://github.com/OpenScribbler/phyllotaxis) — "An LLM-friendly CLI
for exploring OpenAPI documents one layer at a time." Rust, Apache-2.0, ~10 stars.

Commands: overview, resource listing, resource details, endpoint details, schema exploration,
**search across all API elements**, auth-scheme inspection, webhook/callback inspection, example
generation, and **reverse lookup (which endpoints use a given schema)**.

It is **read-only**: no HTTP execution, no response validation, no mocking, no testing. So it
overlaps Phase 1 (`list`/`describe`) and nothing else.

**Read this as validation, not threat** — independent invention of the same core insight at a
tiny scale. But two of its commands are genuinely good ideas absent from DESIGN.md and cheap
to add in Phase 1:

- `search` across operations/schemas/params — an agent that knows "something about invoices"
  but not the operationId currently has no entry point other than dumping `list`.
- **reverse lookup** (schema → endpoints using it) — high value when an agent is reasoning
  about a data model rather than an endpoint.

## 7. The twin is less novel than §6 implies

The doc's own caveat ("the moat is integration and agent ergonomics, not raw mocking features")
is correct, and research says it should be stated harder:

- **Specmatic** already records real HTTP traffic through a proxy and generates mocks that are
  validated against the OpenAPI spec — the doc's "recording is the spine" thesis, shipped.
- **WireMock** has record & playback *and* explicit stateful behaviour for create-then-fetch
  sequences — the doc's Phase 7, shipped, in a mature Java product.
- **Microcks** is described as having the most momentum among spec-driven open-source mockers
  in 2026, with a foundation behind it.

Sources: [Speedscale](https://speedscale.com/blog/wiremock-alternatives/),
[Specmatic proxy](https://specmatic.io/demonstration/replace-live-services-with-openapi-mocks-from-real-http-traffic-with-specmatic-proxy/).

What remains genuinely differentiated in §6: **generalization/synthesis from the corpus**
(synthesize user 99 from recorded users 42 and 57) and **deterministic fault injection driven
by the same CLI that does exploration and testing**. Those two, plus zero-credential operation,
are the twin's actual claim. Replay and statefulness alone are table stakes.

**Roadmap consequence:** Phases 6 and 7 buy the least differentiation per unit of effort, and
they are the hardest. Phases 1–4 are where the unique product is.

## 8. Secrets: the mechanism is right, with one collision

**Confirmed.** curl reads a config file from stdin with `-K -`, and this is the documented way
to keep credentials out of the process list:

> "You can specify the filename to -K/--config as '-' to make curl read the file from stdin."

Canonical pattern: `{ echo -n 'user = "'; cat password.txt; echo '"'; } | curl -K -`.
([curl manpage](https://curl.se/docs/manpage.html))

**Revision:** §5a lists the 0600 temp file *first* and `-K -` as the alternative. Invert that.
Stdin is strictly better — no file on disk, no unpredictable-name requirement, no
delete-after-exec race, nothing for another process to read in the window between write and
unlink. The temp file should be the fallback for the case below, not the primary.

**The collision the doc does not notice:** §3.5 promises `--body -` (request body from stdin)
and §5a wants `-K -` (curl config from stdin). **Both consume stdin. They cannot coexist.**

Resolution options, in order of preference:
1. Put the body *into* the curl config passed on stdin (config files accept `data = "..."`).
   Keeps one stdin consumer. Awkward for large or binary bodies.
2. `--body -` reads stdin into memory in the Go process, then the secret config goes via a
   0600 temp file for that invocation only.
3. Body to a 0600 temp file referenced as `--data @file`, secrets via `-K -`.

This needs deciding in **Phase 2**, since §7 correctly notes the secrets machinery is not
retrofittable.

## 9. Environment check (this machine)

| Requirement | Doc assumption | Actual |
|---|---|---|
| curl with `--write-out '%{json}'` | curl ≥ 7.70 | **curl 8.14.1** — verified working, emits certs/timing/status JSON |
| Go | not stated | **go1.26.0** |

No constraint problems. Worth keeping the doc's startup check for curl availability, since the
distribution story targets machines that are not this one — and worth checking the *version*,
not just presence, because `%{json}` is the hard floor.

## 10. Name: `any-api` is crowded

GitHub already carries, under this name or a near-collision: an R package
(`jonthegeek/anyapi`, "Quickly Access Hundreds of APIs"), a Swift Alamofire client
(`jpmcglone/AnyAPI`), two Python API-wrapper libraries (`FKLC/AnyAPI`, `c0ntribut0r/anyapi`),
a zero-config REST server (`RoryCombe/anyapi`), `AnyDevCode/Any-API-Package`, and an
`anyapi-io` organisation.

The repo name is claimed and fine. The problems are downstream: search discoverability is poor,
and the binary name is likely contested in Homebrew/npm/crates namespaces. DESIGN.md §8 already
flags naming as open — this is the evidence for resolving it deliberately rather than by default.

---

## Summary of required doc changes

| § | Change | Severity |
|---|---|---|
| 5 | libopenapi cannot convert Swagger 2.0; add `kin-openapi/openapi2conv` or drop 2.0 | **Blocking** |
| 5 | Resolve validator open question toward libopenapi-validator (gives router + response validation) | Decided |
| 5a | Make `-K -` the primary secret channel, temp file the fallback | Revision |
| 3.5 / 5a | Resolve the `--body -` vs `-K -` stdin collision | **Blocking for Phase 2** |
| 2 | Restish: "request-body validation only, opt-in" not "no validation"; note v2 + MCP direction | Accuracy |
| 2 | Add Code Mode / code-execution as a fifth camp — the most serious rival | Gap |
| 2 | Cite the Cloudflare 1.17M-token figure; acknowledge MCP's growth | Strengthen |
| 4 / 7 | Add `search` and reverse-lookup to Phase 1 (prior art: phyllotaxis) | Enhancement |
| 6 | State plainly that replay + statefulness are table stakes; the moat is synthesis + fault injection + zero-credential | Honesty |
| 7 | Consider deferring Phases 6–7; least differentiation per unit effort | Strategy |
| 8 | Name collision evidence for `any-api` | Decision input |
