# any-api

*curl for OpenAPI — explore, test, and twin any API from its spec.
One binary, agent-first, no MCP required.*

> **Status: pre-implementation.** No code yet. The
> [design document](docs/design/DESIGN.md) is the source of truth; it is under
> active review and will change before the first line is written.

A single Go binary that turns any OpenAPI/Swagger spec into three things:

1. **An explorable API client** — list, describe, and call operations, with curl under the hood.
2. **A spec-driven tester** — validate real responses against the contract, smoke-test whole APIs.
3. **A digital twin** — a local server that mimics the real API for safe, fast, offline testing.

Built CLI-first for both humans and coding agents. Agents discover an API progressively
(`list` → `describe` → `call`), paying token cost only for what they need, instead of
loading a 2MB `swagger.json` into context. Every request is reproducible as a plain curl
command.

## Design commitments

- **Secrets never reach the agent.** The binary is the trust boundary: agents operate on
  credential *names*, never values. No `--show-secrets` flag exists, by design.
- **Safe by default.** Read-only unless `--allow-mutations`. The sanctioned way to test a
  `DELETE` is against the twin.
- **Not an MCP server.** Ever. The absence is the point.

See [DESIGN.md](docs/design/DESIGN.md) for the full architecture, threat model, and roadmap.

## License

MIT — see [LICENSE](LICENSE).
