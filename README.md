# talaria

*Postman for agents. Point it at any API doc; your agent works the API and never sees
your credentials.*

> **Status: pre-implementation.** No code yet. The
> [design document](docs/design/DESIGN.md) is the source of truth.

Every API client — Postman, Insomnia, Bruno, curl itself — assumes the operator is a human who
is entitled to see their own secrets. Hand an agent a Postman collection with an environment, a
`.bru` file, or a shell with `$STRIPE_KEY` exported, and the agent has your key: in its context
window, in the transcript, in the logs.

**talaria is the trust boundary.** The agent operates on credential *names*; the binary
resolves *values* at the last possible moment and never emits them — not in output, not in
errors, not in the curl commands it prints, not in history.

A single Go binary that turns any OpenAPI/Swagger spec into three things:

1. **An explorable API client** — list, describe, search, and call operations, curl underneath.
2. **A spec-driven tester** — validate real responses against the contract, smoke-test whole APIs.
3. **A digital twin** — a local server that mimics the real API for safe, fast, offline testing.

Agents discover an API progressively (`list` → `describe` → `call`), paying token cost only for
what they need, instead of loading a 2MB `swagger.json` into context. Every request comes back
as a runnable curl command.

## Design commitments

- **Secrets never reach the agent.** There is no `--show-secrets` flag, by design.
- **No collection to maintain.** The spec *is* the collection — nothing to curate, nothing to
  drift, and it works against an API the agent has never seen before.
- **Safe by default.** Read-only unless `--allow-mutations`. The sanctioned way to test a
  `DELETE` is against the twin.
- **Not an MCP server.** Ever. The absence is the point.

## The name

*Talaria* — the winged sandals of Hermes. The tool isn't the messenger; the agent is. This is
what it wears to move fast.

See [DESIGN.md](docs/design/DESIGN.md) for the architecture, threat model, and roadmap.

## License

MIT — see [LICENSE](LICENSE).
