# talaria

*Postman for agents. Point it at any API doc; your agent works the API and never sees
your credentials.*

> **Status: early implementation.** `talaria version`, `talaria list`, `talaria describe`,
> `talaria search`, `talaria uses`, `talaria call`, `talaria auth check`,
> `talaria history` and `talaria run` work so far; the rest of the command tree is scaffolded.
> The [design document](docs/design/DESIGN.md) is the source of truth.

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

## Searching a spec

```sh
talaria search ./openapi.yaml invoice
talaria search ./openapi.yaml invoice --kind schema
```

`list` assumes you know roughly what you are looking at. `search` is for when you know the
concept but not the endpoint. It matches a substring, case-insensitively, against three kinds of
thing at once, and labels each result with the kind it is:

| Kind | Matched against | `where` reports |
|------|-----------------|-----------------|
| `operation` | operationId, path, summary, description | `GET /invoices` |
| `schema` | component schema name and description | `#/components/schemas/Invoice` |
| `param` | parameter name and description | the location: `query`, `path`, … |

`--kind` restricts the search to one of them. Results are ranked with name matches first, then
path matches, then prose, so the endpoint actually named after the concept leads. A term that
matches nothing prints an empty result set and exits 0 — an agent probing for a concept the API
does not have has not made a usage error.

## Finding what uses a schema

```sh
talaria uses ./openapi.yaml Pet
```

The reverse lookup: every operation that touches a schema, and where — in a parameter, in the
request body, or in a response.

```
GET  /pets          listPets    response:200  indirect
GET  /pets/{petId}  getPet      response:200  direct
```

References are followed, so an operation returning a `PetList` that contains an array of `Pet` is
reported for `Pet` and marked `indirect` — the cue that the fields will not be at the top level of
the response. Reference cycles are handled; `Pet` referring to `Owner` referring back to `Pet` is
ordinary in real specs. A schema name the spec does not define exits 2 with the closest valid
names; a schema nothing references exits 0 with no operations.

## Credentials and profiles

Credentials are named, never shown. talaria maps each security scheme a spec declares onto an
environment variable by convention, and carries the *name* from there on:

| Scheme in the spec | Environment variable |
|---|---|
| `type: http`, `scheme: bearer` | `TALARIA_AUTH_BEARER` |
| `type: http`, `scheme: basic` | `TALARIA_AUTH_BASIC` (as `user:password`) |
| `type: apiKey`, named `petKey` | `TALARIA_AUTH_APIKEY_PETKEY` |

API keys are supported in all three locations — `header`, `query` and `cookie`. OAuth2 and
OpenID Connect flows are out of scope for v1: bring your own token and let a `bearer` scheme
carry it. When a spec offers several alternative security requirements, talaria uses the first
one it can satisfy, so a spec offering "OAuth2 or a bearer token" resolves to the bearer token.

For more than one environment, `~/.config/talaria/config.yaml` holds named profiles selected
with `--profile`. `--profile` and `--base-url` are accepted by every command that makes
requests:

```yaml
profiles:
  staging:
    base-url: https://staging.example.com
    headers:
      X-Env: staging
    auth:
      bearerAuth: ${STAGING_TOKEN}
```

A base URL must be `http` or `https`, whichever source it comes from — the flag, a profile, or
the spec's `servers[0].url`. Anything else is a usage error, because curl also speaks `file`,
`gopher` and `smb`, and a spec talaria was pointed at is untrusted input.

It must also carry no credentials in its userinfo: `http://user:password@host` is a usage error
from every source, including a stored history entry on the way to a replay. A base URL is copied
whole into the request, the emitted `curl` and `history.jsonl`, so a password written there would
be one talaria stores in cleartext forever. Use `TALARIA_AUTH_BASIC=user:password` instead, which
stays a reference everywhere but on the wire.

A profile may also switch its own recording off with `history: {enabled: false}` — see
[History](#history).

A profile's `auth` entries may only *reference* an environment variable — `${VAR}` or `$VAR`.
A literal token in the file is refused, because a credential talaria can read from a config file
is a credential it would have to carry. The file names credentials and internal hosts, so it
must be mode `0600` or stricter; anything looser exits 2 telling you to `chmod`. Unknown keys
are an error rather than silence, so a mistyped `baseurl:` does not quietly send the request
somewhere else.

The same file extends the redaction lists. Both keys only ever *add* — nothing in a config file
can stop talaria redacting an `Authorization` header:

```yaml
redact:
  headers:
    - x-session-*
  body-paths:
    - data.token
```

`headers` are globs over the header name, applied to request and response headers alike — and, as
the built-in list is, to query parameters and cookies, which are credential locations under
another name. They hide the value everywhere it is displayed: `request.headers`, the emitted
curl, `--dry-run`, pretty output and history. They go on top
of the built-in list (`Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`,
`*api*key*`, `*token*`, `*secret*`). `body-paths` are dotted JSON paths into a *response* body,
on top of the built-in `access_token`, `refresh_token` and `id_token`. `call` reads this file
whether or not you passed `--profile`, because a security setting that only takes effect when
you happen to be using a profile is one that silently does not.

## Checking credentials without seeing them

```sh
talaria auth check ./openapi.yaml
talaria auth check ./openapi.yaml --profile staging --output json
```

`auth check` answers the one question an agent must be able to ask about credentials: is one
there? It reports every security scheme the spec declares, the variable it comes from, and
whether that variable is set — never what it is set to.

```json
{"scheme":"bearerAuth","source":"env:TALARIA_AUTH_BEARER","present":true}
```

The source follows the same resolution `call` uses, so `--profile staging` reports the
profile's variable (`env:STAGING_TOKEN`) rather than the convention's. Presence is tested with
a lookup; the value is never read.

It exits **5** when an operation in the spec has no credential to authenticate it with, naming
each scheme and the variable to export. Exit 5 is distinct from a usage error on purpose: it is
the one failure whose fix is "ask a human to set `$NAME`" rather than "correct the invocation".
An operation that accepts several alternatives is satisfied by any one of them, and schemes
talaria cannot supply at all (OAuth2, OpenID Connect) are left out rather than reported missing.
The report is printed either way — a code 5 with nothing to read would say what failed but not
what to do.

## Making a call

```sh
talaria call ./openapi.yaml getPet --param petId=42 --query verbose=true
```

`call` binds parameters, headers and credentials to an operation, sends it through the system
curl, and returns the request and the response as one structured block. `--param` takes a
parameter the operation declares, in any location; `--query` and `--header` add ones it does
not, and all three repeat. Every binding problem is reported at once, so a wrong parameter name
and a missing required one arrive in the same message rather than one run apart.

`--body` supplies a request body three ways: a literal (`--body '{"name":"Rex"}'`), a file
(`--body @pet.json`, read verbatim), or `--body -` to read talaria's own stdin. The bytes are
resolved in the Go process before curl exists, so a body on stdin and the credentials curl
reads on *its* stdin never share a pipe. The content type is the `Content-Type` you set with
`--header`, otherwise the media type the operation declares.

The emitted command references credentials by environment-variable name and never by value:

```
curl -q -s -H "Authorization: Bearer $TALARIA_AUTH_BEARER" 'https://api.example.com/v1/pets/42?verbose=true'
```

It is runnable wherever the variable is set and useless to exfiltrate. The leading `-q` mirrors
the call talaria made: it stops curl reading `~/.curlrc`, so no directive in a file talaria did
not write can act on the request carrying the credential. An API key that belongs
in the query string is symbolic there too; a `basic` scheme renders as curl's `-u
"$TALARIA_AUTH_BASIC"`, because the header form would need the value base64-encoded into it.
`--output json` returns the same request as a structured block, where credentials read
`<redacted:env:NAME>` — that field is read, not run. The real value goes on the wire and
appears in no output surface at all.

A credential you supply yourself is treated the same way. `--header "X-Api-Key: sk-live-…"`,
a header from a profile, or a bound parameter whose name matches the redaction list all
display as `<redacted>` in the request block *and* in the emitted curl, while still being sent
verbatim. The name decides, not where the value came from — so the emitted command for a
literal credential is deliberately not copy-pasteable, and there is no flag that makes it so.

Alongside it, `response` carries the status, headers, timing and body; a JSON body is embedded
as JSON rather than as a quoted string, so an agent parses the envelope once instead of twice.
Pretty output prints the request line, the curl, and `200 OK in 143ms` — response headers stay
in `--output json`, since a `Set-Cookie` does not belong in someone's scrollback unasked.

What comes back is redacted too, within the limits of what a tool can know. Response headers go
through the same name matcher as request headers, so `Set-Cookie` reads `<redacted>` by default;
response *bodies* are redacted only at the JSON paths you configure, plus the OAuth2 token
fields (`access_token`, `refresh_token`, `id_token`). A body in which nothing matched is
returned byte for byte, so what you read is what the server sent. This is a mitigation, not a
solution: talaria cannot tell a secret field from an ordinary one by looking at it, so an
auth-issuing endpoint is for a human, not for an agent.

One thing redaction cannot reach: an API key that belongs in the *query string* travels in the
URL, and URLs are written to server access logs. talaria warns once on stderr when an operation
sends one — naming the parameter and the environment variable, never the value.

Two rules apply before anything is sent:

- **Read-only by default.** `GET`, `HEAD` and `OPTIONS` need no flag; every other method
  requires `--allow-mutations` and exits 2 without it. The gate is checked before the request
  is even built, and `--dry-run` does not exempt it, so the rule is learnable without a
  network round trip.
- **`--dry-run` sends nothing.** It prints the same request block a real call would, minus the
  response, and exits 0. The `curl` field is identical either way.

An HTTP 4xx or 5xx is a successful observation and exits 0. Only a request that could not be
completed at all — a refused connection, a TLS failure — is exit 1.

Every request is bounded in time: 10 seconds to connect and 30 seconds in total by default,
`--timeout <seconds>` to change the total. An API that stops answering becomes an exit 1 with
curl's status 28 in the message rather than a process that hangs.

## Validating what came back

Every executed call carries a `validation` block alongside the response:

```json
"validation": { "status_documented": true, "content_type_documented": true, "body_valid": true, "errors": [] }
```

The three booleans answer independently, so "the server returned an undocumented 500" is
distinguishable from "the documented 200 came back with the wrong shape" without parsing prose.
A schema failure produces one entry in `errors` per failing field, carrying the JSONPath of the
offending value, because the field is the actionable part. Pretty output adds a single line —
`validation: ok`, or `validation: 2 errors` — and leaves the messages to `--output json`.

A violation is an observation, like a 404: the call still exits 0. `--fail-on-error` asks for
the other behaviour, exiting **4** when the response is an HTTP error or violates the spec. It
changes the exit code and nothing else — the full envelope is still on stdout, which is where
an agent goes to find out what actually failed. A dry run has no validation block; nothing came
back to check.

Validation runs on the **redacted** response, not on the raw one. Validation errors quote the
content they rejected, so a validator fed the raw body would be the one place a secret
reappeared after redaction removed it. The cost is that a redacted field is validated as
`<redacted>`, so redacting a field the schema constrains reports a violation the server did not
commit — a visible, correctable error, which is the better failure of the two.

## History

Every call talaria makes is recorded, so an agent can answer "what have I already tried, and
what came back" without asking again:

```sh
talaria history
talaria history --operation getPet --since 1h --status 4xx
talaria history show 3
talaria history replay 3
```

`history` lists the store newest first: index, time, source, method, path, status, operation.
`--operation` filters by operationId, `--since` takes a duration (`30m`, `1h`, `168h`),
`--status` takes an exact code (`404`) or a class (`4xx`), and `--source` takes `call`, `run`
or `replay` — so a smoke run over a large spec does not bury the calls you made by hand. The
index is an entry's position in the whole store, not in the filtered list, so it stays the
number `show` and `replay` take.

`history show <n>` prints one entry in full; `history replay <n>` sends it again and records
the result as a new entry, leaving the original alone. Replay resolves credentials from the
environment exactly as the original call did — history holds their *names*, so there is nothing
in the file to read back. It is gated the same way `call` is: replaying a `POST` needs
`--allow-mutations`. A header whose value was a literal talaria redacted by name cannot be
reproduced, and replay says so on stderr rather than pretending it sent one.

Entries are written **redacted, at write time**. The store is the highest-risk artifact talaria
produces, so un-redacted recording is not an option and there is no flag for it. It lives at
`$XDG_STATE_HOME/talaria/history.jsonl` (falling back to `~/.local/state/talaria/`), directory
`0700` and file `0600`, one JSON entry per line, keeping the most recent 1000 entries *per
source* and truncating bodies at 64 KiB with an explicit `"truncated": true`. A body that is
not valid UTF-8 — a protobuf, an image, a gzip stream — is stored base64-encoded with
`"encoding": "base64"`, because a JSON string would otherwise replace each byte it cannot hold
and a replay would send something the original call did not; `history show` reports such a body
as `<binary body, N bytes, base64 in --output json>` rather than printing it. Concurrent
talaria processes can share one history file: each append takes an exclusive advisory lock on
a sibling `history.jsonl.lock` for the whole of the write and the retention trim, so an entry
talaria reported as recorded is one you will find in the file.

Recording is off for a profile that says so, and off everywhere when the environment says so:

```yaml
profiles:
  prod:
    history:
      enabled: false
```

```sh
TALARIA_HISTORY=off talaria call ./openapi.yaml getPet --param petId=42
```

`TALARIA_HISTORY` wins over the profile: the config file is what a user configured, the
variable is what someone auditing a machine sets. Either way nothing is written — no entry, no
file, no directory. A dry run is never recorded; a request that failed to complete is, with no
response block, because it is still something that was tried.

## Smoke testing

`run` is `call` over many operations at once — the command that answers "is this whole API
behaving?" and the one a CI job invokes:

```sh
talaria run ./openapi.yaml --base-url http://localhost:9000
talaria run ./openapi.yaml --tag pets --operation getPet --fail-on-error
talaria run ./openapi.yaml --fixtures ./fixtures --allow-mutations --report json
```

`--tag` and `--operation` both repeat and combine as a **union**; with neither, the whole spec
runs. A filter matching nothing exits 2 rather than passing with nothing tested. Operations run
one at a time, in spec order — determinism beats speed, and parallel calls against a real API
are a surprise nobody asked for.

Test data follows one priority chain: the spec's own `example`, then a fixture file, then
generation from the schema. `--fixtures dir/` supplies the middle one, matched by operationId —
`dir/createPet.json` holds `{"params": {…}, "headers": {…}, "body": {…}}`, every field optional.

A generated body is always JSON, so `run` sends it as `application/json` — or as the operation's
own JSON media type, `application/vnd.api+json` say — whatever else the spec lists first. An
operation that declares no JSON media type at all, such as a converted Swagger 2.0 `formData`
operation, still gets `application/json`, because that is what the bytes are; a server that only
speaks XML answering 415 is a truer result than JSON labelled `application/xml`. A fixture
`headers` entry setting `Content-Type` wins, as `--header` does in `call`.

Each operation is reported as `passed`, `failed` or `skipped`, with a `reason` for the last two
and a `summary` block counting all four numbers. A skip is not a failure: a `DELETE` left alone
without `--allow-mutations`, or an operation whose required parameter nothing could supply, is
correct behaviour rather than a broken API. `--report json|pretty|tsv|junit` is `run`'s own
format flag and overrides `--output`.

`--timeout` applies per operation, not per suite. An endpoint that never answers is one failed
line in the report with curl's status 28 in its reason, so the run still finishes and CI still
gets its output.

`--report junit` writes a JUnit XML suite — one `<testcase>` per operation, named by
operationId, with a `<failure>` or `<skipped>` child carrying the reason — which is what makes
`run` land in CI without glue:

```sh
talaria run ./openapi.yaml --base-url "$STAGING" --report junit > report.xml
```

`junit` is a `--report` value only: a suite of operations is the one thing there is to render
as a test report, so `--output junit` exits 2.

Failing operations exit 0 unless you pass `--fail-on-error`, which makes them exit 4. A missing
credential exits 5 either way — that operation was never tested, and the fix is exporting a
variable, not reading a report. Every request goes into history with `"source": "run"`, under
its own 1000-entry cap, so a run over a large spec cannot bury the calls you made by hand.

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

`internal/canary` is the leak suite: it builds the binary, drives every auth mechanism —
bearer, basic, and an API key in a header, a query parameter and a cookie — through every
command and every output format, and greps every byte talaria emits or writes for the injected
secret, raw, percent-encoded and base64. It is the check that gates a release, so a failure
there is a redaction regression, not a flaky test. Run it alone with
`go test ./internal/canary/...`.

CI runs the build, the lint and the whole suite on every push to `main` and every pull request
([.github/workflows/ci.yml](.github/workflows/ci.yml)), which is what makes the leak suite a
gate rather than something to remember. The workflow runs the commands recorded in
`.ralph/stack.json` and nothing else; `internal/ci` fails if the two drift apart or if the
pinned Go version stops matching `go.mod`.

## Driving talaria from an agent

[AGENT.md](AGENT.md) is the operating manual for the LLM on the other end: the
list→search→describe→dry-run→call loop, the exit codes and what to do about each, and — the part
no other API client has to answer — how to name a credential you cannot see. Hand it to the model
as a system prompt or a skill file.

It is tested like code. `cmd/talaria/agentdoc_test.go` checks the doc's exit-code table against
the `clierr` constants, every command it names against the registered command tree, and every
`TALARIA_*` variable against the ones talaria actually reads, so a manual that drifts from the
binary fails the build.

## The name

*Talaria* — the winged sandals of Hermes. The tool isn't the messenger; the agent is. This is
what it wears to move fast.

See [DESIGN.md](docs/design/DESIGN.md) for the architecture, threat model, and roadmap.

## License

MIT — see [LICENSE](LICENSE).
