# talaria — operating manual for agents

You drive an HTTP API through this binary. It reads an OpenAPI/Swagger spec and runs the calls
through curl. **You never see credentials.** Values are resolved inside the process at the moment
of the request and appear in no output — not in JSON, not in errors, not in the curl it prints,
not in history. You work with credential *names*.

There is no `--show-secrets` flag and no way to reveal a value. Asking a human to paste one into
your context is not a supported workflow — see [Credentials](#credentials) for what to do instead.

## The loop

```sh
talaria list ./openapi.yaml                       # what operations exist
talaria search ./openapi.yaml invoice             # find one by concept
talaria describe ./openapi.yaml getPet            # its params, body, responses
talaria call ./openapi.yaml getPet --param petId=42 --dry-run   # check the request
talaria call ./openapi.yaml getPet --param petId=42             # send it
```

Do not read the spec file yourself. It is often megabytes; these commands are the point.

## The spec

Every spec-reading command takes it from the first of: the positional argument, `--spec`, then
`$TALARIA_SPEC`. Export `TALARIA_SPEC` once and drop the argument. A file path or an `http(s)`
URL, OpenAPI 3.x or Swagger 2.0.

With two positional arguments the spec is first (`talaria describe ./openapi.yaml getPet`); with
one, the argument is the operationId.

## Commands

| Command | What it gives you |
|---|---|
| `talaria list [spec] [--tag t]` | one line per operation: method, path, operationId, summary |
| `talaria search [spec] <query> [--kind operation\|schema\|param]` | substring match across operations, schemas and params |
| `talaria describe [spec] <operationId> [--depth n]` | one operation's params, request body and responses |
| `talaria uses [spec] <schema>` | every operation that touches a component schema |
| `talaria call [spec] <operationId>` | build and send one request; `--dry-run` builds only |
| `talaria auth check [spec]` | which credentials the spec needs and whether they are set |
| `talaria history [--operation id] [--since 1h] [--status 4xx] [--source call\|run\|replay]` | what has already been called |
| `talaria history show <n>` | one recorded request/response in full |
| `talaria history replay <n>` | send a recorded request again |
| `talaria version` | the binary's version |

Operations the spec did not name get a synthesised operationId (`GET /pets/{petId}` →
`getPetsByPetId`). It is stable and callable.

## Calling

```sh
talaria call getPet --param petId=42 --query verbose=true --header 'X-Trace: abc'
```

- `--param name=value` binds a parameter the operation declares, in any location. `--query` and
  `--header` add ones it does not. All three repeat.
- `--body` takes a literal, `@file`, or `-` for stdin. Content type comes from your
  `--header Content-Type`, else from the operation's declared media type.
- Every binding problem is reported at once. Fix them in one edit, not one per run.
- `--base-url` overrides the spec's server. `--profile` selects a named profile from the user's
  config file.

The response block carries status, headers, timing and body; a JSON body is embedded as JSON, so
one parse gets you the fields. **An HTTP 4xx or 5xx exits 0** — it is a successful observation.
Read `response.status`; do not infer failure from the exit code.

### Mutations are gated

`GET`, `HEAD` and `OPTIONS` run. Every other method needs `--allow-mutations` and exits 2 without
it. The gate is checked before the request is built, and `--dry-run` does not exempt it.

Needing `--allow-mutations` is a decision, not a formality. Do not add it to a command that
failed on it without saying what you are about to change and on which host. The sanctioned way
to exercise a `DELETE` is against a disposable environment — point `--base-url` at one. (The
built-in digital twin that will make this cheap is designed but not yet shipped.)

## Credentials

You cannot read a credential and you do not need to. talaria maps each security scheme the spec
declares onto an environment variable, and carries the name:

| Scheme in the spec | Variable |
|---|---|
| `type: http`, `scheme: bearer` | `TALARIA_AUTH_BEARER` |
| `type: http`, `scheme: basic` | `TALARIA_AUTH_BASIC` (`user:password`) |
| `type: apiKey`, named `petKey` | `TALARIA_AUTH_APIKEY_PETKEY` (scheme name, upper-cased) |

```sh
talaria auth check ./openapi.yaml
{"scheme":"bearerAuth","source":"env:TALARIA_AUTH_BEARER","present":true}
```

`present` is a lookup; the value is never read. A profile can redirect a scheme to a different
variable, and `auth check` reports whichever one is actually in force.

**When a credential is missing** (exit 5, or `present: false`), the fix is one sentence to a
human — name the variable and stop:

> This API needs a bearer token. Export it before I retry:
> `export TALARIA_AUTH_BEARER=…`. Do not paste the value to me; talaria reads it from the
> environment and I never see it.

Never ask for the value, never suggest putting a literal token in a config file (talaria refuses
them), and never try to read one out of the environment yourself. A credential you supply inline
— `--header 'X-Api-Key: sk-live-…'` — is redacted by name in the output and in the emitted curl,
so it is not copy-pasteable either.

The emitted curl references variables symbolically:

```
curl -s -H "Authorization: Bearer $TALARIA_AUTH_BEARER" 'https://api.example.com/v1/pets/42'
```

That command is runnable in a shell where the variable is set and useless to anyone else. Quote
it freely in reports.

One thing redaction cannot fix: an API key that belongs in the *query string* travels in the URL
and lands in server access logs. talaria warns once on stderr. Report the warning; it is a
property of the API, not a bug you can work around.

Response bodies are redacted only at the OAuth2 token fields and whatever JSON paths the user
configured. A login or token-issuing endpoint is for a human to run, not for you.

## Output

`--output json|pretty|tsv` on every command. Piped output defaults to `json`, so you normally get
JSON without asking. Every payload carries `"schema": "talaria/v1"` — branch on it if you cache
parsing logic.

Errors go to **stderr** as one line of JSON in the same envelope:

```json
{"schema":"talaria/v1","error":{"code":2,"message":"unknown operation \"getpet\"","valid_alternatives":["getPet","getPets"]}}
```

When `valid_alternatives` is present it is the complete set of right answers. Pick from it rather
than guessing again.

## Exit codes

| Code | Meaning | What to do |
|---|---|---|
| 0 | Success. A 4xx/5xx response is still success | read `response.status` |
| 1 | The request could not be completed: network, TLS, curl itself | check the host and `--base-url`; retrying once is reasonable |
| 2 | Usage error: unknown operation, missing parameter, bad flag, or a mutation without `--allow-mutations` | fix the invocation using `valid_alternatives` and the message |
| 3 | The spec could not be read or parsed | check the path or URL; do not retry unchanged |
| 4 | A response violated the spec | report the violation (no command emits this yet — response validation is not shipped) |
| 5 | A required credential is not set | tell a human which variable to export; do not retry until they have |

## History

Every real call is recorded, redacted at write time, newest first. Use it instead of re-issuing a
request to remember what happened:

```sh
talaria history --operation getPet --status 4xx
talaria history show 3
```

The index `history` prints is the entry's position in the whole store, so it stays valid under
filters. `talaria history replay <n>` re-sends an entry and records the result as a new one;
replaying a mutation needs `--allow-mutations` too. Dry runs are never recorded. Recording is off
where a profile says so, or everywhere under `TALARIA_HISTORY=off` — if history is empty, that is
usually why.

Credentials are stored as names, so nothing in history can be read back into a value.

## Rules

1. Never ask a human for a credential value. Ask them to export a named variable.
2. Never add `--allow-mutations` silently.
3. Treat a 4xx as data, not as a crash. Exit 0 means talaria worked.
4. Prefer `describe` over reading the spec, and `history` over repeating a call.
5. Use `--dry-run` when you are unsure what a request will look like. It costs nothing.
