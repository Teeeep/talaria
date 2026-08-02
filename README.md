# talaria

*Postman for agents. Point it at any API doc; your agent works the API and never sees
your credentials.*

> **Status: early implementation.** `talaria version`, `talaria list` and `talaria describe`
> work so far; the rest of the command tree is scaffolded. The [design document](docs/design/DESIGN.md) is the source
> of truth.

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

## Pointing talaria at a spec

Spec-reading commands take an OpenAPI 3.x or Swagger 2.0 spec as a file path or an `http(s)` URL,
from whichever of these is set first:

1. the positional argument
2. `--spec`
3. `$TALARIA_SPEC`

Swagger 2.0 is converted to OpenAPI 3.x at load time, so every command sees one shape. Remote
specs are cached under `$XDG_CACHE_HOME/talaria/specs` (falling back to `~/.cache/talaria/specs`)
and fetched once per URL, not once per call. Cache files are written `0600` in a `0700` directory.

## Listing operations

```sh
talaria list https://petstore3.swagger.io/api/v3/openapi.json
talaria list ./openapi.yaml --tag pets
```

One line per operation — method, path, operationId and a truncated summary — because the whole
point is that nobody pipes a 2 MB `swagger.json` into a context window. `--tag` filters to a
single tag; a tag nothing carries prints an empty list and exits 0, since "nothing has that tag"
is an answer rather than a failure.

Operations the spec never named get a synthesised operationId derived from method and path
(`GET /pets/{petId}` → `getPetsByPetId`). It is stable across loads and as callable as an
authored one.

## Describing one operation

```sh
talaria describe ./openapi.yaml getPet
talaria describe getPet --spec ./openapi.yaml --depth 2
```

The next step after `list`: the parameters, request body and declared responses of a single
operation. With one positional argument, that argument is the operationId and the spec comes
from `--spec` or `$TALARIA_SPEC`; with two, the spec comes first.

Schemas render one field per line in the form `name*: (type) description`, where `*` marks a
required field:

```
GET /pets/{petId}  getPet
Get one pet

Params:
  petId*: (string) [path] The pet's identifier
  verbose: (boolean) [query] Include the pet's history

Responses:
  200 The pet
    application/json:
      id*: (string) Unique identifier
      status: (string) one of: available, pending, sold
      tags: ([]string)
      owner: (object)
        name: (string) Who owns the pet
```

Arrays show their element type as `([]string)`. Enums are listed inline. `allOf` branches are
merged into one field list, since that is what a caller has to supply; `oneOf` and `anyOf`
render as a summary of their branch types (`(oneOf: object|string)`) rather than expanding
each branch.

Two things stop the output from growing without bound. A schema that refers back to one of its
own ancestors — which real specs do constantly — ends at `[circular]` instead of recursing. A
schema nested deeper than `--depth` (6 by default) ends at `[max depth]`, so a truncated branch
is visibly truncated rather than silently absent.

`--output json` returns the same information as a structured tree, not the rendered string.

## Output

Every command takes `--output json|pretty|tsv`. With no flag, talaria prints `pretty` when
stdout is a terminal and `json` when it is piped — an explicit `--output` always wins.

Every JSON payload carries a top-level `"schema": "talaria/v1"` field. It is versioned so agent
prompts keep working; it only changes on a breaking change to the output shape. JSON output is
deterministic — the same result renders byte-identically every time. `tsv` prints bare
tab-separated rows with no header line, for `cut` and `awk`.

## Errors and exit codes

Failures go to stderr as a single line of JSON in the same versioned envelope, saying what
failed, why, and — where there is a fixed set of right answers — what would have been valid:

```json
{"schema":"talaria/v1","error":{"code":2,"message":"unknown output format \"xml\": valid values are json, pretty, tsv","valid_alternatives":["json","pretty","tsv"]}}
```

Exit codes are deterministic, so agents can branch on them:

| Code | Meaning |
|---|---|
| 0 | Success. An HTTP 4xx/5xx is still a successful *observation* for `call`; use `--fail-on-error` to change that |
| 1 | The request could not be completed (network, curl failure) |
| 2 | Usage error — unknown operation, missing required parameter, bad flag value |
| 3 | Spec parse/load error |
| 4 | Validation failure: the response violates the spec (only with `--fail-on-error` or `run`) |
| 5 | A required security scheme has no credential — distinct from a usage error so an agent can ask a human to set `$NAME` |

## Building

Requires Go 1.26+ and curl 7.70+ (curl is the execution engine; `--write-out '%{json}'` is
required).

```sh
go build -o talaria ./cmd/talaria
./talaria version
```

The version string defaults to `dev` and is stamped at release time:

```sh
go build -ldflags "-X main.version=v0.1.0" -o talaria ./cmd/talaria
```

Run the tests with `go test ./...`.

## The name

*Talaria* — the winged sandals of Hermes. The tool isn't the messenger; the agent is. This is
what it wears to move fast.

See [DESIGN.md](docs/design/DESIGN.md) for the architecture, threat model, and roadmap.

## License

MIT — see [LICENSE](LICENSE).
