# talaria — Implementation Plan

Source of truth: [docs/design/DESIGN.md](docs/design/DESIGN.md) (v0.3). Stack: `.ralph/stack.json`.

## Scope

**This plan covers Phases 1–4 only** (DESIGN.md §7). Phase 4 ends the differentiated core, and
§7 sets a deliberate decision gate there: *"Do not start [5–8] on faith; start them because
using Phases 1–4 made the absence of a twin painful."* Phases 5–8 (recording proxy, twin serve,
stateful twin, fault injection) are therefore **not planned here** and must not be started as
part of this plan. When Task 33 passes, stop and hand the gate decision to a human.

Also deliberately **out of scope**, so no task should drift into them:

- `internal/twin` and every `talaria twin …` subcommand (Phases 6–8).
- Distribution and packaging — cross-platform release binaries, `go install` docs, the install
  script, the Homebrew tap, the pi package and the Claude Code skill (DESIGN.md §7 lists these
  under distribution, not under a phase). Task 33 adds a CI workflow that runs the test suite;
  it deliberately does **not** build or publish releases.
- Request chaining / scenario DSL, OAuth flows, a built-in query language, and everything in §9
  Non-goals. `history replay` (Task 23) is the sanctioned substitute for chaining.

## Commands (from `.ralph/stack.json` — do not invent others)

| Purpose | Command |
|---|---|
| All tests | `go test ./...` |
| One package | `go test ./<dir>/...` (expansion of `test_single_command`) |
| Build | `go build ./...` |
| Lint | `test -z "$(gofmt -l .)" && go vet ./...` |

Run lint before every commit. `golangci-lint` is **not** installed — do not add it to a task.

## Ground facts (verified 2026-08-02, not assumed)

Module path `github.com/Teeeep/talaria`; Go 1.26.0; curl 8.14.1 (design floor is ≥ 7.70).

Dependency versions, all resolvable from the module proxy:

```
github.com/pb33f/libopenapi           v0.38.7
github.com/pb33f/libopenapi-validator v0.14.0
github.com/getkin/kin-openapi         v0.145.0
github.com/spf13/cobra                v1.10.2
github.com/spf13/viper                v1.21.0
gopkg.in/yaml.v3                      v3.0.1
```

`yaml.v3` arrives transitively with libopenapi but Task 5 depends on it directly, so require it
explicitly rather than relying on a transitive edge.

**libopenapi API** — `libopenapi.NewDocument(b []byte) (Document, error)`, then
`doc.BuildV3Model()` returning `(*DocumentModel[v3high.Document], error)` — a single `error`,
**not** a slice. Collections are `orderedmap` types iterated with `.FromOldest()`:
`m.Model.Paths.PathItems`, `pathItem.GetOperations()`, `op.Responses.Codes`,
`m.Model.Components.SecuritySchemes`, `m.Model.Components.Schemas`. A schema proxy yields the
schema via `.Schema()`; `schema.Type` is a `[]string`.

**Swagger 2.0 bridge** — libopenapi cannot convert 2.0 (docs/research §1). The working path,
run end to end during planning: `json.Unmarshal(specBytes, &openapi2.T{})` →
`openapi2conv.ToV3(&doc2) (*openapi3.T, error)` → `json.Marshal(doc3)` →
`libopenapi.NewDocument(bytes)`. Verified to preserve operationId, tags, path params, response
schemas, `securityDefinitions` → `components.securitySchemes`, and to fold
`host`+`basePath`+`schemes` into a single `servers[0].url`. The two libraries meet at JSON
bytes; nothing downstream sees kin-openapi types.

**YAML 2.0 input** — `openapi2.T` only unmarshals JSON, so YAML 2.0 specs need a bridge step.
Verified end to end: `yaml.Unmarshal(specBytes, &any)` with **`gopkg.in/yaml.v3`** →
`json.Marshal(any)` → the v2→v3 chain above. yaml.v3 decodes mappings into
`map[string]interface{}` (yaml.v2 produced `map[interface{}]interface{}`, which `json.Marshal`
rejects) — so no key-normalisation pass is needed, but **the version matters**. A YAML 2.0
fixture round-tripped this way kept operationId, tags, path params, the folded
`https://api.example.com/v1` server URL and an apiKey `securityDefinition` intact.

**libopenapi-validator API** — `validator.NewValidator(doc libopenapi.Document, opts ...config.Option) (Validator, []error)`
and `ValidateHttpResponse(req *http.Request, resp *http.Response) (bool, []*errors.ValidationError)`.
Verified against a 3.0 spec: a conforming body returns `(true, nil)`; a body with a wrong field
type returns `(false, [1 error])` whose `.Message` and `.Reason` are both populated and
human-readable. **The router is host- and prefix-tolerant**: with the spec declaring
`servers: [https://api.real.example.com/v1]`, responses for request URLs
`https://api.real.example.com/v1/pets`, `http://127.0.0.1:PORT/pets` *and*
`http://127.0.0.1:PORT/v1/pets` all routed and validated identically. `--base-url` therefore
does not break validation, and tests may validate calls made against an `httptest.Server`.

**curl mechanisms** — `curl -K -` reads a config document from stdin; a secret-bearing
`header = "Authorization: Bearer …"` directive never appears in argv. `--write-out '%{json}'`
returns `http_code`, `time_total`, `num_headers` and more. Both verified locally.

**curl config directives cover output capture too** — verified that `output = "/path"`,
`dump-header = "/path"` and `write-out = "%{json}"` all work as config-document directives, not
just as argv flags. That yields three separate streams from one exec: response body → the
`output` file, response headers → the `dump-header` file, and the `%{json}` metadata → curl's
stdout, with nothing to disentangle. Use this instead of parsing a trailer out of mixed stdout.

**curl config body escaping** — escape the body in this order: `\` → `\\`, then `"` → `\"`,
`\t` → `\t`, `\n` → `\n`, `\r` → `\r`, and emit as `data = "<escaped>"`. A byte-identical
round-trip was verified with a JSON body containing embedded quotes, backslashes, real
newlines, tabs and multi-byte UTF-8. Order matters: escaping `\` after `"` corrupts the body.

## Planner decisions on DESIGN.md §8 open questions

These blocked implementation and are resolved here. Change them only with a design-doc update.

1. **History location & retention** (§8 demands "a deliberate answer, not a default"):
   `$XDG_STATE_HOME/talaria/history.jsonl`, falling back to `~/.local/state/talaria/history.jsonl`.
   Directory `0700`, file `0600`. Append-only JSONL, trimmed on write to the most recent
   **1000 entries per `source`** (`call`, `run`, `replay`) rather than 1000 overall — a single
   `run` over a large spec must not evict a session of interactive `call` history (Task 22).
   Per-entry request and response bodies truncated at **64 KiB** with an explicit
   `"truncated": true` marker. Opt out with `TALARIA_HISTORY=off` (read by the store itself,
   Task 22) or `history.enabled: false` in the profile config (layered by the command wiring,
   Task 23, because `internal/corpus` does not import `internal/config`); the env var wins when
   both are set. Rationale: a per-project `.talaria/` invites committing the tool's
   highest-risk artifact (§5a); a single mode-0600 file under XDG state does not.
2. **Body-in-config cutover** (§8): bodies that are ≤ **1 MiB** *and* valid UTF-8 go inline as a
   `data` directive. Anything larger or non-UTF-8 goes to a `0600` temp file referenced as
   `data = "@/path"`, deleted immediately after exec. The inline path is the default because it
   leaves nothing on disk.
3. **libopenapi-validator 3.0 strictness** (§8) stays open by design — Task 26 measures it
   against real 3.0 specs and records the finding. Planning found no false failure on a simple
   3.0 document (see Ground facts), so the risk is narrower than §8 feared, but it is untested
   against the constructs where 3.1 strictness actually bites: nullable, exclusiveMinimum/Maximum
   as booleans, and `example` vs `examples`. Task 26 must exercise exactly those.

## Conventions

- Tasks are executed in numeric order. **"Depends on" lists what must already be built for the
  task to compile and pass, not a claim that nothing else exists yet.** It is deliberately not a
  chain: e.g. Task 12 (`internal/secret`) depends only on Task 1, so if the ordering ever needs
  to change, the real constraints are written down rather than inferred from the numbering.
- Tests are colocated (`internal/spec/loader_test.go`), per Go convention and `.ralph/stack.json`.
- Spec fixtures live in `testdata/` beside the package that reads them.
- No package under `internal/operation`, `internal/validate`, or `internal/gen` may import
  `internal/curl`, `internal/corpus` or `internal/twin` (DESIGN.md §5 boundary rule; the design
  names `curl` and `twin`, and Task 26 adds `corpus` for the same reason — these three are
  consumed by the twin in Phase 6). No task in this plan relaxes it; if a task seems to require
  it, the dependency is pointing the wrong way. **Task 32 asserts this mechanically against the
  real import graph** — it is not an honour-system rule.
- Secret values are `SecretRef` structs everywhere except inside `internal/curl` at exec time.
  If a task tempts you to put a resolved secret in a struct field typed `string`, stop — that is
  the bug class §5a exists to prevent.

### Deviations from the DESIGN.md §5 package layout (deliberate)

§5 lists the packages the design cares about; it is not exhaustive. This plan adds three, and a
reviewer checking the implementation against §5 should read this rather than file a finding:

- **`internal/request`** — holds the `Request` type §5a describes ("a `Request` whose secret
  fields are *structurally* references"). §5 never says where it lives. It cannot live in
  `internal/curl`, because `internal/corpus`, `internal/output` and `internal/validate` all
  consume a `Request` and §5's boundary rule forbids them importing `curl`. A separate leaf
  package is the only placement that keeps that rule satisfiable.
- **`internal/clierr`** — the exit-code contract from §4. Every package returns these errors, so
  it must sit below all of them.
- **`internal/canary`** — test-only, holding the §5a leak suite and its surface enumeration.
- **`internal/ci`** — test-only, holding the Task 33 test that keeps the CI workflow in step with
  `.ralph/stack.json`. No production code.
- **`internal/e2e`** — test-only, holding the Task 32 cross-component workflow test.

Nothing else may be added without a design-doc update. In particular there is no `internal/http`
or `internal/client`: curl is the execution engine (§3.4).

---

### Task 1: Module scaffolding and `talaria version`

**Depends on:** none

**Test files:**
- `cmd/talaria/version_test.go` (create) — `version` command prints a version string and exits 0

**Implementation files:**
- `go.mod` (create) — module `github.com/Teeeep/talaria`, go 1.26
- `cmd/talaria/main.go` (create) — entrypoint calling into the root command
- `cmd/talaria/root.go` (create) — cobra root command `talaria`
- `cmd/talaria/version.go` (create) — `version` subcommand

**Red — write failing tests:**
1. Executing the root command with args `["version"]` writes a non-empty string containing
   `talaria` to the command's configured output writer.
2. The root command has `SilenceUsage` and `SilenceErrors` set, so an error does not dump the
   usage block (agents parse stderr; cobra's default usage dump is noise).

**Green — minimal implementation:**
1. `go mod init github.com/Teeeep/talaria`, then `go get github.com/spf13/cobra@v1.10.2`.
2. Root command with a `Version` string var (`dev` default, overridable via `-ldflags -X`).
3. Wire `main.go` to execute the root command and exit non-zero on error.
4. Make the root command's output writer injectable so tests capture output without touching
   the real stdout.

**Verify:** `go build ./...` then `go test ./cmd/...`

**Why:** Every later task needs a module and a command tree to hang things off. `go test ./...`
cannot run at all until `go mod init` happens — this task unblocks the entire loop.

---

### Task 2: Output envelope and json/pretty/tsv renderers

**Depends on:** Task 1

**Test files:**
- `internal/output/render_test.go` (create) — renderer selection, envelope shape, TTY defaulting

**Implementation files:**
- `internal/output/output.go` (create) — `Format` type, `Resolve` for default selection
- `internal/output/render.go` (create) — JSON, pretty and TSV renderers
- `cmd/talaria/root.go` (modify) — persistent `--output` flag on the root command

**Red — write failing tests:**
1. Every JSON-rendered payload carries `"schema": "talaria/v1"` as a top-level field (DESIGN.md §3.1).
2. `Resolve` returns JSON when stdout is not a TTY and pretty when it is; an explicit
   `--output` value always wins over both.
3. TSV rendering of a two-row table emits tab-separated fields and one `\n`-terminated line per
   row, with no trailing blank line.
4. JSON output is deterministic: rendering the same payload twice yields byte-identical output
   (map key ordering must not float).
5. `--output` is registered **once** as a persistent flag on the root command, so every
   subcommand accepts it (§3.1: "`--output json` on every command"). Assert by looking the flag
   up on a freshly built child command, not by re-registering it per command.
6. Parsing an invalid format (`xml`) returns an error naming all three valid values. Assert on
   the returned error, **not** on an exit code — `internal/clierr` does not exist yet, and Task 3
   (which owns exit codes) depends on this task. Task 3 wires this error to exit 2.

**Green — minimal implementation:**
1. `Format` enum (`json`, `pretty`, `tsv`) with parsing and an `Unknown format` error listing
   valid values.
2. A `Renderer` interface plus the three implementations; JSON marshals an envelope struct with
   `Schema string \`json:"schema"\`` set to `talaria/v1`.
3. TTY detection via `os.Stat` on the output file and `os.ModeCharDevice`; accept an injected
   `io.Writer` + an `isTTY bool` so tests never depend on the real terminal.

**Verify:** `go test ./internal/output/...`

**Why:** Machine-readable output on every command is design principle §3.1, and the versioned
schema field is what keeps agent prompts from breaking. Building it once here stops each
command from inventing its own shape.

---

### Task 3: Structured errors and the exit-code contract

**Depends on:** Tasks 1, 2

**Test files:**
- `internal/clierr/clierr_test.go` (create) — code mapping and stderr JSON shape
- `cmd/talaria/exitcode_test.go` (create) — the root command's error → exit code translation

**Implementation files:**
- `internal/clierr/clierr.go` (create) — error type carrying an exit code and alternatives
- `cmd/talaria/root.go` (modify) — translate errors into exit codes and stderr JSON

**Red — write failing tests:**
1. Each constructor maps to the DESIGN.md §4 code: usage → 2, spec load → 3, validation → 4,
   credential missing → 5, request failure → 1. Success is 0.
2. A rendered error is JSON on stderr with `schema`, `error.code`, `error.message` and, when
   supplied, `error.valid_alternatives` (§3.1: "what failed, why, valid alternatives").
3. An error carrying a list of valid operationIds renders them in `valid_alternatives`.
4. Wrapping an error with `errors.Is`/`errors.As` preserves the exit code through one level of
   `fmt.Errorf("%w")`.
5. The root command's execute function **returns** an exit code rather than calling `os.Exit`
   itself, so the mapping is assertable in-process: a `clierr.Usage` error yields 2, a
   `clierr.SpecLoad` error yields 3, an unrecognised error yields 1, and a nil error yields 0.
6. An invalid `--output xml` — the parse error Task 2 deliberately left as a bare error — reaches
   this translator and yields exit code 2 with the three valid values in the stderr JSON. This
   assertion is the other half of Task 2's assertion 6; neither task can hold both halves.

**Green — minimal implementation:**
1. Exported code constants and an `Error` struct implementing `error` plus `Unwrap`.
2. Constructors: `Usage`, `SpecLoad`, `Validation`, `CredentialMissing`, `RequestFailed`.
3. Root command execution returns `(code int)`: type-assert to `*clierr.Error`, render its JSON
   to stderr, return its code; anything else non-nil returns 1. **`main.go` (Task 1) stays the
   only place that calls `os.Exit`** — keeping it out of the run path is what makes assertions 5
   and 6 testable at all, and every later `cmd/` test asserting an exit code depends on it.

**Verify:** `go test ./internal/clierr/... ./cmd/...`

**Why:** "Deterministic exit codes. Agents branch on these" (§3.1). Code 5 in particular exists
so an agent can tell a human *"set $NAME"* instead of guessing — that only works if the mapping
is centralised from the start.

---

### Task 4: Load OpenAPI 3.x specs from bytes and files

**Depends on:** Task 3

**Test files:**
- `internal/spec/load_test.go` (create) — JSON and YAML 3.0/3.1 loading, error paths
- `internal/spec/testdata/petstore-3.0.yaml` (create) — small 3.0 spec, 2–3 operations
- `internal/spec/testdata/petstore-3.1.json` (create) — same API as 3.1 JSON
- `internal/spec/testdata/malformed.yaml` (create) — syntactically invalid YAML

**Implementation files:**
- `internal/spec/spec.go` (create) — `Document` wrapper around the libopenapi v3 model
- `internal/spec/load.go` (create) — `LoadBytes`, `LoadFile`

**Red — write failing tests:**
1. Loading the 3.0 YAML fixture returns a document whose `Version` starts with `3.0` and whose
   path-item count matches the fixture.
2. Loading the 3.1 JSON fixture succeeds and exposes the same operationIds as the 3.0 fixture —
   format and minor version are invisible downstream.
3. Loading `malformed.yaml` returns an error whose exit code is 3 (`clierr.SpecLoad`), not a panic.
4. Loading a path that does not exist returns exit code 3 with the path in the message.

**Green — minimal implementation:**
1. `go get github.com/pb33f/libopenapi@v0.38.7`.
2. `LoadBytes` calls `libopenapi.NewDocument` then `BuildV3Model()`; remember the second return
   is a single `error`, not a slice.
3. Wrap every failure in `clierr.SpecLoad`.
4. Expose the built `*v3high.Document` on the wrapper so later packages read the model without
   re-parsing.

**Verify:** `go test ./internal/spec/...`

**Why:** Every command starts by loading a spec. Establishing one wrapper type now means the
Swagger 2.0 path (Task 5) and the cache (Task 6) slot in behind a single interface.

---

### Task 5: Swagger 2.0 → OpenAPI 3.x conversion at load time

**Depends on:** Task 4

**Test files:**
- `internal/spec/convert_test.go` (create) — conversion fidelity against real 2.0 specs
- `internal/spec/testdata/petstore-2.0.json` (create) — the canonical Swagger 2.0 petstore
- `internal/spec/testdata/swagger2-apikey.json` (create) — 2.0 spec with `securityDefinitions`,
  `host`, `basePath`, `schemes`, and a `$ref` into `definitions`
- `internal/spec/testdata/swagger2.yaml` (create) — the same API as `swagger2-apikey.json` in
  YAML, to cover the YAML→JSON bridge

**Implementation files:**
- `internal/spec/convert.go` (create) — 2.0 detection and conversion
- `internal/spec/load.go` (modify) — route 2.0 documents through conversion before building

**Red — write failing tests:**
1. A document with a top-level `"swagger": "2.0"` key is detected as v2; one with `openapi: 3.x`
   is not.
2. Loading the 2.0 petstore yields a document reporting version `3.x` with every operationId
   from the original present — this is the §7 Phase 1 requirement to *"verify 2.0 conversion
   against real specs"*.
3. `host` + `basePath` + `schemes` collapse into exactly one server URL
   (`https://api.example.com/v1` for the fixture).
4. `securityDefinitions` become `components.securitySchemes` preserving type, `in` and header
   name — auth mapping in Task 14 depends on this surviving.
5. A `$ref` to `#/definitions/Pet` resolves after conversion, with `required` fields intact.
6. **The YAML 2.0 fixture converts to a document equal to the JSON fixture's** — same
   operationIds, same single server URL, same security schemes. Input format must be invisible
   downstream. This is the assertion that catches a yaml.v2 regression.

**Green — minimal implementation:**
1. `go get github.com/getkin/kin-openapi@v0.145.0`.
2. Detect version by unmarshalling into a struct with `Swagger` and `OpenAPI` string fields.
3. Convert with the verified bridge: `json.Unmarshal` → `openapi2.T`, `openapi2conv.ToV3`,
   `json.Marshal`, hand the bytes to the existing `LoadBytes`.
4. YAML 2.0 input must become JSON first, because `openapi2.T` only unmarshals JSON. Use
   `gopkg.in/yaml.v3`: `yaml.Unmarshal(b, &v)` into an `interface{}`, then `json.Marshal(v)`.
   **Do not use `gopkg.in/yaml.v2`** — it decodes mappings as `map[interface{}]interface{}`,
   which `json.Marshal` rejects outright. Detect YAML vs JSON by attempting `json.Unmarshal`
   first and falling back to YAML, rather than by file extension (URLs often have neither).
5. Record on the wrapper that conversion happened, so `describe` can note it later if useful.

**Verify:** `go test ./internal/spec/...`

**Why:** "Any API doc" is the promise in the README, and much of the real world is still 2.0.
docs/research §1 flags this as the one *blocking* finding in the design — libopenapi cannot do
it, and doing it wrong silently breaks every downstream command on half the specs in the wild.

---

### Task 6: Spec source resolution, URL fetch, and local cache

**Depends on:** Task 4

**Test files:**
- `internal/spec/source_test.go` (create) — precedence, URL fetch, cache hit/miss

**Implementation files:**
- `internal/spec/source.go` (create) — `Resolve(arg, flag string) (string, error)`, HTTP fetch, cache
- `cmd/talaria/root.go` (modify) — persistent `--spec` flag

**Red — write failing tests:**
1. Precedence is positional arg > `--spec` > `TALARIA_SPEC` env var; with none set, the error is
   exit code 2 naming all three ways to supply a spec.
2. A spec served by an `httptest.Server` loads successfully over HTTP.
3. The second load of the same URL reads from cache — assert by shutting the test server down
   after the first fetch and requiring the second load to still succeed.
4. Cache files are written mode `0600` under a cache dir created `0700`.
5. A URL returning 404 produces exit code 3 with the status in the message.

**Green — minimal implementation:**
1. `Resolve` applies precedence and classifies the result as file or URL by `http`/`https` prefix.
2. Fetch with `net/http` (the twin/serve rule about curl applies to *API* calls, not spec fetches).
3. Cache under `$XDG_CACHE_HOME/talaria/specs/` (fallback `~/.cache/talaria/specs/`), keyed by
   SHA-256 of the URL. Accept a cache dir override in the struct so tests use `t.TempDir()`.

**Verify:** `go test ./internal/spec/...`

**Why:** §4: "Spec argument accepts a file path or URL (with local cache); falls back to an env
var." An agent pointed at a remote spec should not re-download it on every one of a dozen calls.

---

### Task 7: The operation model

**Depends on:** Task 4

**Test files:**
- `internal/operation/operation_test.go` (create) — extraction from a built spec document

**Implementation files:**
- `internal/operation/operation.go` (create) — the core `Operation` type
- `internal/operation/extract.go` (create) — build `[]Operation` from a spec document

**Red — write failing tests:**
1. Extracting from the 3.0 petstore fixture returns one `Operation` per method/path pair, each
   with `ID`, `Method` (upper-case), `Path`, `Summary` and `Tags` populated.
2. Parameters carry name, location (`path`/`query`/`header`/`cookie`), `Required`, and a schema
   reference; a path param marked required in the fixture reports `Required: true`.
3. An operation with a request body exposes its media type and schema; one without reports nil.
4. Response contracts are keyed by status code string (`"200"`, `"default"`) with content types.
5. Security requirements resolve to scheme names, and an operation-level `security` block
   overrides the document-level one (including an empty `security: []` meaning *no auth*).
6. `IsMutation()` is false for GET/HEAD/OPTIONS and true for POST/PUT/PATCH/DELETE.

**Green — minimal implementation:**
1. Define `Operation`, `Param`, `RequestBody`, `Response`, `SecurityRequirement`.
2. Walk `m.Model.Paths.PathItems.FromOldest()` then `pathItem.GetOperations().FromOldest()`.
3. Merge path-item-level parameters into each operation's parameter list, operation-level
   entries winning on `(name, in)` collision.
4. Keep schemas as the libopenapi schema proxy — do not flatten to JSON here; `describe`
   (Task 10) and `gen` (Task 29) need the real thing.

**Verify:** `go test ./internal/operation/...`

**Why:** DESIGN.md §5 calls this "THE core model" — every command downstream consumes it, and
both the curl executor and (later) the twin share it. Getting the shape right here is what keeps
`internal/curl` out of `internal/validate`.

---

### Task 8: OperationId synthesis and lookup index

**Depends on:** Task 7

**Test files:**
- `internal/operation/index_test.go` (create) — synthesis, lookup, tag filtering
- `internal/operation/testdata/no-operation-ids.yaml` (create) — 3.0 spec with no `operationId`

**Implementation files:**
- `internal/operation/index.go` (create) — `Index` with by-id and by-tag lookup

**Red — write failing tests:**
1. An operation missing `operationId` gets a deterministic synthetic id derived from method and
   path (e.g. `GET /pets/{id}` → `getPetsById`); loading the same spec twice yields the same id.
2. Two operations that would synthesise the same id get distinct ids (suffix disambiguation).
3. `Lookup` by exact id returns the operation; an unknown id returns an exit-code-2 error whose
   `valid_alternatives` contains the closest matches, not the entire operation list.
4. `ByTag` returns only operations carrying that tag, preserving spec order.
5. Lookup is case-sensitive, but the not-found error surfaces a case-insensitive near match when
   one exists (agents get casing wrong constantly).

**Green — minimal implementation:**
1. Build the index once at construction: map id → index into the slice.
2. Synthesise ids only for operations lacking one; never overwrite an author's id.
3. Near-match suggestions via simple Levenshtein distance over ids, capped at 5 results.

**Verify:** `go test ./internal/operation/...`

**Why:** Real specs routinely omit `operationId`, and the entire CLI addresses operations by id.
Without synthesis those endpoints are unreachable. The suggestion list is what turns a failed
agent call into a self-correcting one.

---

### Task 9: `talaria list`

**Depends on:** Tasks 2, 6, 8

**Test files:**
- `cmd/talaria/list_test.go` (create) — output formats, tag filter, exit codes

**Implementation files:**
- `cmd/talaria/list.go` (create) — the `list` command

**Red — write failing tests:**
1. `list <spec>` in pretty mode emits exactly one line per operation, each containing method,
   path and id — §3.2 requires "one compact line per operation".
2. JSON mode emits the `talaria/v1` envelope with an `operations` array whose entries carry
   `id`, `method`, `path`, `tags`, `summary`.
3. TSV mode emits one tab-separated row per operation and no header-only blank output.
4. `--tag pets` restricts output to operations carrying that tag; an unmatched tag yields an
   empty list and exit 0, not an error.
5. A missing spec argument exits 2; an unparseable spec exits 3.

**Green — minimal implementation:**
1. Resolve the spec (Task 6), load, extract, index.
2. Apply the tag filter, then hand a view struct to the renderer from Task 2.
3. Keep the pretty line under ~100 chars: truncate long summaries rather than wrapping.

**Verify:** `go test ./cmd/...`

**Why:** `list` is the entry point of the list→describe→call workflow and the first thing that
makes the binary useful. Its compactness is the whole point — it exists so nobody ever pipes a
2 MB `swagger.json` into a context window.

---

### Task 10: `talaria describe` and the compact schema renderer

**Depends on:** Tasks 2, 8

**Test files:**
- `internal/output/schema_test.go` (create) — the compact schema format
- `cmd/talaria/describe_test.go` (create) — command behaviour
- `internal/output/testdata/recursive-schema.yaml` (create) — a self-referencing schema

**Implementation files:**
- `internal/output/schema.go` (create) — compact schema rendering
- `cmd/talaria/describe.go` (create) — the `describe` command

**Red — write failing tests:**
1. A required string property with a description renders exactly as `name*: (string) description`
   — the `*` marks required (§3.2).
2. An optional integer property renders without the `*`.
3. Nested objects indent one level per depth; arrays render their element type as `([]string)`.
4. Enums render inline as `status*: (string) one of: active, archived`.
5. A self-referencing schema terminates, emitting a cycle marker rather than recursing forever.
6. Rendering stops at a configurable max depth with an explicit truncation marker.
7. `describe <spec> <operationId>` shows params, request body schema and response schemas; an
   unknown id exits 2 with suggestions from Task 8.
8. `--output json` returns structured params/schemas, not the pretty string.

**Green — minimal implementation:**
1. Walk the libopenapi schema proxy recursively with a visited-set keyed by schema pointer.
2. Compose `name` + `*` if required + `(type)` + description; handle `oneOf`/`anyOf`/`allOf` by
   listing branch types rather than expanding each fully.
3. Command: load, look up by id, render params → body → responses in that order.

**Verify:** `go test ./internal/output/... ./cmd/...`

**Why:** §8 calls this "the agent-facing UX centrepiece". `describe` is where an agent decides
how to build a call, and a raw JSON Schema dump here would reintroduce the context bomb the
tool exists to prevent. The cycle guard is not optional — real specs self-reference constantly.

**As built — one deviation.** `oneOf` and `anyOf` list their branch types as planned
(`(oneOf: object|string)`), but `allOf` branches are *merged* into one field list instead.
`allOf` means every branch applies at once, so the fields a caller has to supply are the union;
listing branch types for it would render the extremely common inheritance idiom as the useless
`(allOf: object|object)`, defeating the purpose of the command. The merge reuses the same cycle
guard and depth limit, so it cannot run away. Also added beyond the plan: a `--depth` flag,
which is what makes the max depth "configurable" from outside the package.

---

### Task 11: `talaria search` and `talaria uses`

**Depends on:** Tasks 2, 8

**Test files:**
- `cmd/talaria/search_test.go` (create) — fuzzy search across kinds
- `cmd/talaria/uses_test.go` (create) — reverse schema lookup

**Implementation files:**
- `internal/operation/search.go` (create) — search and reverse-lookup over the index
- `cmd/talaria/search.go` (create) — the `search` command
- `cmd/talaria/uses.go` (create) — the `uses` command

**Red — write failing tests:**
1. `search <spec> invoice` matches operations whose id, path, summary or description contain the
   term, case-insensitively, ranked with id matches first.
2. `--kind schema` restricts results to component schemas; `--kind param` to parameter names;
   omitting `--kind` searches all three and labels each result with its kind.
3. A query matching nothing exits 0 with an empty result set — an agent probing for a concept
   has not made a usage error.
4. `uses <spec> Pet` lists every operation referencing the `Pet` schema in a parameter, request
   body or response.
5. `uses` follows one level of indirection: an operation whose response is `PetList`, which
   contains an array of `Pet`, is reported for `Pet`.
6. `uses` on an unknown schema name exits 2 with valid schema names as alternatives.

**Green — minimal implementation:**
1. Build a lowercase haystack per searchable element at index construction.
2. Substring match plus the Task 8 Levenshtein scorer for ranking; no external fuzzy library.
3. For `uses`, walk each operation's schemas collecting `$ref` names, memoising per schema to
   keep the indirection walk from blowing up on recursive specs.

**Verify:** `go test ./internal/operation/... ./cmd/...`

**Why:** §4 — an agent knows a *domain concept* ("something about invoices") or a *data model*
("what touches `Invoice`?"), rarely an operationId, and `list` on a large spec is exactly the
context dump this tool avoids. Both commands are adopted from phyllotaxis (docs/research §6).

**Divergence from the plan.** Three, all in the same direction.

`uses` follows *every* level of indirection, not one. Stopping at one is arbitrary — an
`InvoiceList` wrapping an `Invoice` wrapping an `InvoiceLine` is two levels and completely
ordinary — and the memoisation the plan already asks for is what makes the full walk cheap.
The closure over the component-schema reference graph is computed by iterating to a fixpoint
rather than by a memoised recursion, because a fixpoint terminates on a reference cycle
(`Pet` → `Owner` → `Pet`) without any cycle bookkeeping at all. Each reported operation carries
`direct`, which says whether the schema is named at the site itself or only reached through
another schema; without it "listPets uses Pet" is misleading about where the fields will be.

The haystacks are built on first search rather than at index construction. `NewIndexFor` is now
what every spec-reading command builds, and resolving every component schema in a large spec on
every `list` would be a real cost for something `list` never reads.

Also beyond the plan: `search` results carry a `where` field locating each hit (`GET /invoices`,
`#/components/schemas/Invoice`, `query`), so the result is directly actionable as the argument to
the next command rather than just a name to go looking for.

---

### Task 12: `internal/secret` — SecretRef and redaction

**Depends on:** Task 1

**Test files:**
- `internal/secret/secret_test.go` (create) — ref semantics, header matching, redaction

**Implementation files:**
- `internal/secret/secret.go` (create) — `SecretRef` and its display forms
- `internal/secret/redact.go` (create) — sensitive-name matching and redaction

**Red — write failing tests:**
1. `SecretRef` marshals to JSON as `<redacted:env:NAME>` and **never** as its value — assert the
   canary value is absent from the marshalled bytes.
2. `SecretRef.String()` (and therefore any `%v`/`%s` formatting, including accidental logging)
   returns the redacted form, not the value.
3. `Symbolic()` returns `$TALARIA_AUTH_BEARER` for shell interpolation in emitted curl.
4. The built-in sensitive-header list matches `Authorization`, `Cookie`, `Proxy-Authorization`
   and, case-insensitively, any header matching `*api*key*`, `*token*`, `*secret*` (§5a table).
5. User-supplied extra patterns extend the list; they never shrink it.
6. Redacting a header map replaces sensitive values and leaves non-sensitive ones untouched.
7. Redaction applies to a value supplied literally by the user via `--header`, not only to
   spec-derived auth — a user who types a bearer token into `--header` still must not see it
   echoed back.

**Green — minimal implementation:**
1. `SecretRef{Source, Name string}` with no exported value field; resolution is a separate
   method returning `(string, error)` that only `internal/curl` calls.
2. Implement `MarshalJSON`, `String` and `GoString` on the redacted form so every printf verb is
   safe by construction.
3. Compile the sensitive patterns once; match on lower-cased header names.

**Verify:** `go test ./internal/secret/...`

**Why:** §5a: "Exactly one component ever holds resolved secret values." Making the *type*
unable to print itself means leaking requires a deliberate, greppable call — "forgetting to
scrub a string is neither." This lands before the `Request` type on purpose: §7 says the
credential firewall is "not retrofittable — it shapes the core `Request` type."

**Divergence from the plan.**

`encoding/json` HTML-escapes `<` and `>`, and it applies that to whatever `MarshalJSON` returns —
a `MarshalJSON` implementation cannot opt out from the inside. So the ref reaches the wire as
`"<redacted:env:NAME>"` and decodes back to `<redacted:env:NAME>`. The guarantee is
unaffected (the *value* is absent either way) but **Task 24's canary suite must grep decoded
output, or accept both spellings**, or it will miss nothing today and mis-assert later.

`Set-Cookie` is in the built-in sensitive list. The plan's item 4 names only the request-header
row of the §5a table; the response-header row names `Set-Cookie`, and a `cookie` pattern matching
exactly does not catch it. Patterns are globs over the lower-cased name compiled once to anchored
regexps, with everything but `*` quoted, so a user-supplied pattern cannot be a regexp injection.

A nil `*Redactor` matches the built-in list rather than nothing, so a struct field nobody
initialised still redacts — an opt-in firewall is one a misconfiguration switches off.

`Resolve` classifies a missing variable as `clierr.CredentialMissing` (exit 5) itself rather than
returning a bare error for a caller to classify, since §4's whole reason for code 5 is that the
agent learns *which* variable to ask a human to set.

---

### Task 13: `internal/config` — profiles and env-var auth mapping

**Depends on:** Tasks 7, 12

**Test files:**
- `internal/config/config_test.go` (create) — env conventions, profile loading, permissions
- `internal/config/auth_test.go` (create) — spec security scheme → SecretRef mapping
- `cmd/talaria/flags_test.go` (create) — `--profile` and `--base-url` are registered once, on the root

**Implementation files:**
- `internal/config/config.go` (create) — profile file loading and layering
- `internal/config/auth.go` (create) — scheme-to-credential resolution
- `cmd/talaria/root.go` (modify) — persistent `--profile` and `--base-url` flags

**Red — write failing tests:**
1. A `bearer` HTTP security scheme maps to `TALARIA_AUTH_BEARER`; `basic` to `TALARIA_AUTH_BASIC`;
   an API-key scheme named `petKey` to `TALARIA_AUTH_APIKEY_PETKEY` (§5 Auth).
2. Mapping returns a `SecretRef`, never a value — assert the returned struct exposes no value
   and that a canary env var's contents appear nowhere in its rendering.
3. A profile file supplies `base-url`, headers and auth; `--profile staging` selects it and
   profile values override env-var-derived defaults.
4. A profile whose auth entry is `${OTHER_VAR}` produces a `SecretRef` pointing at that env var,
   not an inlined value.
5. A profile file with permissions looser than `0600` is refused with a clear error (§5 requires
   mode 0600).
6. A missing profile name exits 2 listing the profiles that do exist.
7. API keys in `query` and `cookie` locations map as well as `header` — v1 scope is bearer,
   basic and API key in all three locations.
8. `--profile` and `--base-url` are registered **once** as persistent flags on the root command,
   so `call`, `run` and `auth check` all accept them. Assert by looking each flag up on a freshly
   built child command, as Task 2 does for `--output`. No task registers either flag per-command;
   three commands in §4 take them, and a second registration is a silent shadowing bug.

**Green — minimal implementation:**
1. `go get github.com/spf13/viper@v1.21.0`; read `~/.config/talaria/config.yaml` with the path
   injectable for tests.
2. `Resolve(op, doc, profile)` returning, per required scheme, a `SecretRef` plus where it goes
   (header name, query param, cookie name).
3. Report *presence* by checking `os.LookupEnv` without reading the value into a returned struct.

**Verify:** `go test ./internal/config/...`

**Why:** This is the mapping that lets an agent name a credential it cannot see. `auth check`
(Task 21) and every authenticated call read it, and getting the env-var convention wrong here
means `AGENT.md` documents a lie.

**Divergence from the plan.**

No viper. The profile file is one YAML document read with `gopkg.in/yaml.v3`, already a
dependency. Viper's value is env/flag/file layering, and none of that layering can apply here:
the file is stat-ed before it is read (the 0600 check below), the auth values are *references*
that must not be expanded, and flag precedence lives in the request builder. Adding a
dependency to do less than `yaml.Unmarshal` was the wrong trade. Decoding uses
`KnownFields(true)`, so a mistyped `baseurl:` is an error rather than a request quietly sent
somewhere else.

A literal credential in a profile's `auth` map is **refused** (exit 2), not inlined. The plan's
item 4 only describes the `${VAR}` case; the unstated case matters more, because a value talaria
can read out of a config file is one it would have to carry, and §5a's claim is that it never
does. `${VAR}` and `$VAR` are both accepted. The refusal message names the profile and the
scheme and never the value.

Requirement selection: a spec's `security` block is a list of *alternatives*, so Resolve takes
the first one it can satisfy rather than the first one written. A spec offering `oauth2` or
`bearerAuth` therefore works, where failing on the first entry would make every such spec
uncallable. Only when no alternative is supported does it exit 2, listing why each was rejected.

`Present()` lives on `secret.SecretRef` rather than in this package, so `os.LookupEnv` stays
inside the firewall package next to `Resolve`. `Credential.Present()` delegates to it.

Permission check is `perm &^ 0600 != 0`, so 0400 passes: the requirement is that nobody *else*
can read the file, and stricter is not a misconfiguration.

---

### Task 14: Request construction and parameter binding

**Depends on:** Tasks 8, 13

**Test files:**
- `internal/request/request_test.go` (create) — binding, validation, base-url resolution

**Implementation files:**
- `internal/request/request.go` (create) — the `Request` type with symbolic secret fields
- `internal/request/build.go` (create) — bind CLI inputs to an operation

**Red — write failing tests:**
1. `--param id=42` substitutes into `/pets/{id}` producing `/pets/42`; a missing required path
   param exits 2 naming the parameter.
2. Repeated `--query` flags accumulate; values are URL-encoded (`q=a b` → `q=a%20b` or `a+b`,
   assert the decoded round trip rather than the exact encoding).
3. Repeated `--header` flags accumulate; a malformed `--header foo` (no `=`) exits 2.
4. An unknown `--param` name that the operation does not declare exits 2 with the valid names.
5. Base URL precedence: `--base-url` > profile `base-url` > the spec's first `servers[0].url`;
   with none available, exit 2.
6. A path param containing `/` or `?` is percent-encoded, not injected raw into the path.
7. Auth credentials land on the request as `SecretRef` fields — assert the constructed `Request`
   marshals to JSON with no canary value present.

**Green — minimal implementation:**
1. `Request{Method, URL, Path, Query, Headers, Body}` where header values are a small sum type of
   literal string or `SecretRef` — this is the §5a "structurally references, not strings" rule.
2. Bind params by matching declared `(name, in)`; collect all binding errors before returning so
   an agent fixes them in one round trip rather than one per attempt.
3. Build the URL with `net/url` so encoding is not hand-rolled.

**Verify:** `go test ./internal/request/...`

**Why:** This is the type §5a says the credential firewall shapes. Everything downstream —
output, history, corpus, errors, the twin — operates on it, so if it can hold a bare secret
string, every one of those surfaces becomes a leak channel.

---

### Task 15: Symbolic curl rendering, `call --dry-run`, and mutation gating

**Depends on:** Tasks 2, 14

**Test files:**
- `internal/curl/render_test.go` (create) — symbolic command rendering
- `cmd/talaria/call_dryrun_test.go` (create) — dry-run behaviour and mutation gating

**Implementation files:**
- `internal/curl/render.go` (create) — `Request` → displayable curl string
- `cmd/talaria/call.go` (create) — the `call` command, dry-run path only

**Red — write failing tests:**
1. A request with bearer auth renders `-H "Authorization: Bearer $TALARIA_AUTH_BEARER"` — the
   env var name, never the value (§3.4, §5a).
2. An API key in a query parameter renders symbolically in the URL and never as its value.
3. The rendered command is shell-safe: a header value containing a space or quote comes back
   correctly quoted (assert by round-tripping through `shlex`-style splitting or by asserting the
   quoting explicitly).
4. `call <spec> <op> --dry-run` prints the curl command, performs no network I/O, and exits 0.
5. `--dry-run` on a POST **without** `--allow-mutations` still exits 2 — gating is evaluated
   before dry-run, so an agent learns the rule without a network round trip.
6. A POST with `--allow-mutations` passes the gate; GET/HEAD/OPTIONS never need the flag.
7. The dry-run JSON output contains the `request` block from the §4 sketch, with headers shown
   as `<redacted:env:NAME>`.

**Green — minimal implementation:**
1. Render from the `Request`'s symbolic fields; the renderer has no access to resolved values.
2. Gate mutations on `operation.IsMutation()` before anything else in the command's run.
3. Dry-run returns the same envelope shape as a real call minus the `response` block, so agents
   parse one structure.

**Verify:** `go test ./internal/curl/... ./cmd/...`

**Why:** Ends Phase 1 — the binary is now a useful spec→curl tool with no execution path at all.
§3.4: the emitted curl is "a portable reproduction for bug reports, docs, and scripts", and it
is "runnable in a shell where the env var is set, useless to exfiltrate."

**Divergence from the plan.**

The `request.headers` block shows `Bearer <redacted:env:TALARIA_AUTH_BEARER>`, keeping the
scheme prefix, where §4's sketch shows the bare `<redacted:env:NAME>`. The header really does
read `Bearer <token>` on the wire, and a request block that drops the prefix misreports the
request it claims to describe. The redaction is unchanged; only the surrounding literal is.

`internal/curl` renders its own URL rather than calling `Request.URL`. `Request.QueryString`
percent-encodes every value, which is correct for the wire and wrong for both display forms:
`%24TALARIA_AUTH_BEARER` is no longer a reference a shell expands, and
`%3Credacted%3Aenv%3A…%3E` is no longer legible. So encoding is applied per value here —
literals escaped, credential renderings passed through — and `Request.URL` stays the
wire-correct counterpart Task 17's executor uses with a resolving renderer.

Shell quoting is decided per word rather than per character. A word of pure literal text is
single-quoted, which needs no escaping beyond the quote itself and leaves `$` and backticks
inert; a word carrying a credential must be double-quoted so the shell expands `$NAME`, so its
literal parts are escaped against the four characters double quotes still interpret. A ref
whose name is not a shell identifier, or whose source is not the environment, renders redacted
and inert — an emitted command that stops being copy-pasteable beats one that expands into
something unintended.

Basic auth renders as `-u "$TALARIA_AUTH_BASIC"` with no `Authorization` header, as
`internal/request` already documented: the header text is base64(user:password) and cannot be
built without the value, while `-u` takes the raw pair and stays symbolic.

`call` without `--dry-run` exits 2 naming `--dry-run`, because Phase 1 has no execution path
and silently printing a dry run would misreport what happened. Task 19 replaces that branch.
`loadIndex` grew a `loadSpec` sibling returning the document as well: security schemes live on
the document, not on an operation, so resolving credentials needs both. The config file is read
only when `--profile` is passed, so an invocation without one never touches — or refuses to
read — the user's configuration.

---

### Task 16: The curl config document (`-K -`) builder

**Depends on:** Tasks 12, 14

**Test files:**
- `internal/curl/config_test.go` (create) — directive emission, escaping, temp-file cutover

**Implementation files:**
- `internal/curl/config.go` (create) — build the config document written to curl's stdin

**Red — write failing tests:**
1. The config document contains `url`, `request`, `header`, `silent` and `show-error`
   directives, and a `write-out = "%{json}"` directive.
2. A `SecretRef` header is resolved *into the config document only* — assert the canary value is
   present in the config bytes and absent from the argv slice the builder returns alongside it.
3. The argv slice is exactly `["curl", "-K", "-"]` and nothing else — no URL, no headers, no
   credentials, no output paths as arguments (§5a: `/proc/*/cmdline` is world-readable).
   Everything curl needs is a directive, because `output`, `dump-header` and `write-out` are all
   valid config-document directives (verified — see Ground facts). Task 17 appends the `output`
   and `dump-header` directives when it allocates those temp files; the builder accepts them as
   parameters rather than Task 17 reaching for argv and breaking this assertion.
4. Body escaping is applied in the verified order (`\` first, then `"`, `\t`, `\n`, `\r`) and a
   JSON body containing embedded quotes, backslashes, newlines, tabs and multi-byte UTF-8
   round-trips byte-identically.
5. A body over 1 MiB is written to a `0600` temp file and referenced as `data = "@/path"`; the
   returned cleanup function deletes it.
6. A non-UTF-8 body takes the temp-file path regardless of size.
7. Resolving a `SecretRef` whose env var is unset returns exit code 5, not an empty header.

**Green — minimal implementation:**
1. `BuildConfig(req) (configBytes []byte, argv []string, cleanup func(), err error)`.
2. Escape with a `strings.Replacer` built in the correct order — a single `Replacer` scans once
   and avoids the double-escaping bug that ordered sequential replaces invite.
3. Zero the config buffer in `cleanup` after exec so resolved values do not linger.

**Verify:** `go test ./internal/curl/...`

**Deviation, recorded during implementation:** this section specified `data` as the body
directive. `data` is wrong in both positions, verified against curl 8.14.1 during the build:

- `data = "@/path"` **strips newlines and carriage returns** out of the file, so a
  pretty-printed JSON body or any signed payload arrives corrupted. The temp-file path emits
  `data-binary = "@/path"` instead, which sends the file's bytes verbatim.
- Inline, both `data` and `data-binary` read a value beginning with `@` as a *filename*, and a
  request body legitimately can begin with `@`. The inline path emits `data-raw`, which never
  interprets `@` and, unlike the file form, strips nothing.

The escape order in point 4 was confirmed correct as written. `internal/curl/config_test.go`
pins both choices by running the built document through the real `curl` binary against an
`httptest.Server` and asserting a byte-identical round trip of a body containing embedded
quotes, backslashes, tabs, newlines, a carriage return, multi-byte UTF-8 and a trailing `@`.

Also added beyond the section: a non-UTF-8 body is detected by `utf8.Valid` *and* an explicit
NUL scan, since a NUL byte truncates a quoted directive value rather than failing loudly.

**Why:** §5a's primary mechanism, and docs/research §8 confirms stdin is strictly better than a
temp file: no file on disk, no delete-after-exec race, no window for another process to read it.
This is the single place in the program where a resolved secret exists.

---

### Task 17: The curl executor

**Depends on:** Task 16

**Test files:**
- `internal/curl/exec_test.go` (create) — execution against `httptest`, version preflight

**Implementation files:**
- `internal/curl/exec.go` (create) — `os/exec` runner and `%{json}` parsing
- `internal/curl/version.go` (create) — curl version preflight

**Red — write failing tests:**
1. Executing a GET against an `httptest.Server` returns status 200, the response body and the
   response headers, and a non-zero `timing_ms` parsed from `write-out = "%{json}"`.
2. Body, headers and metadata arrive on three separate channels and cannot corrupt each other:
   the body goes to the `output` file, the headers to the `dump-header` file, and `%{json}` is
   the whole of curl's stdout. Assert with a response body that is itself a `%{json}`-shaped
   JSON object containing `}` and a fake `http_code` — parsing must still report the real status.
3. A connection to a closed port returns exit code 1 (`clierr.RequestFailed`) with curl's exit
   status in the message — an HTTP 500 does *not*, since that is a successful observation.
4. HTTP 404 and 500 both return exit code 0 with the status recorded (§4 exit-code table).
5. Version preflight rejects a reported curl below 7.70 with a clear message naming the floor,
   and accepts 7.70 and above (parse from a fixture string, not the live binary).
6. curl's stderr is captured and, if surfaced in an error, passes through the redactor first
   (§5a: "Errors are built from the redacted representation").

**Green — minimal implementation:**
1. `exec.Command("curl", "-K", "-")` with the config document written to `cmd.Stdin` via a pipe —
   never the Go process's real stdin.
2. Allocate two `0600` temp files in a `0700` dir, pass their paths to the Task 16 builder as
   `output` and `dump-header` directives, and delete both in a deferred cleanup. Parse curl's
   stdout as a single `%{json}` object; read status from `http_code`, timing from `time_total`.
   Do **not** parse a trailer out of a mixed stdout stream — the three-channel split exists to
   make that class of bug impossible.
3. Parse the `dump-header` file into an `http.Header`, taking the **last** header block so that
   redirects and `100 Continue` do not leave stale headers in front of the real response.
4. Preflight parses `curl --version` output once per process and caches it.

**Verify:** `go test ./internal/curl/...`

**Why:** §3.4 makes curl the execution engine, and `--write-out '%{json}'` requires curl ≥ 7.70 —
"a hard floor". Checking the version rather than mere presence is what stops a confusing failure
on an older machine, which matters because the distribution story targets machines that are not
this one.

**Divergence from the plan.**

Red test 6 says stderr "passes through the redactor first", but `secret.Redactor` decides by
*header name* and has nothing to say about a free-text error message. So `exec.go` scrubs with a
replacer built from the request's own resolved credentials, mapping each back to its
`<redacted:env:NAME>` form (and its percent-encoded spelling, since curl echoes URLs). That is
stronger than a pattern match: talaria knows exactly which bytes it put on the wire, so the scrub
is neither a guess nor defeatable by an unusual message format.

`readResponse` also treats a completed curl reporting `http_code: 0` as `RequestFailed`. curl
normally exits non-zero in that case and the process check catches it first; this is the second
gate, so a Response can never claim status 0.

`Response.Body` and `Response.Headers` are *unredacted*. Redaction of what came back off the wire
is Task 20, and applying it here would leave nothing for Task 26's validation to check against
the spec.

---

### Task 18: Request bodies from flag, file, and stdin

**Depends on:** Tasks 14, 16

**Test files:**
- `internal/request/body_test.go` (create) — the three body sources and stdin ownership

**Implementation files:**
- `internal/request/body.go` (create) — body source resolution

**Red — write failing tests:**
1. `--body '{"a":1}'` uses the literal string.
2. `--body @file.json` reads the file; a missing file exits 2.
3. `--body -` reads the Go process's stdin to EOF (inject an `io.Reader`, do not touch real stdin).
4. A body supplied via `--body -` still reaches curl through the config document — assert the
   config bytes contain the body and that curl's stdin is a *different* reader from the process's.
5. Supplying both `--body` and `--body @file` forms simultaneously exits 2.
6. Content-Type defaults from the operation's request body media type when the user sets none,
   and a user-supplied `--header Content-Type=…` wins.

**Green — minimal implementation:**
1. Resolve the body to `[]byte` in the Go process before any curl invocation exists.
2. Hand those bytes to the Task 16 config builder — the body becomes a `data` directive or a
   temp file, never a second stdin consumer.

**Verify:** `go test ./internal/request/...`

**Why:** docs/research §8 flags the `--body -` vs `-K -` stdin collision as **blocking for Phase
2**. §5a resolves it: "The Go process owns the real stdin. curl's stdin is always a fresh pipe
that Go writes." This task is where that rule becomes code and gets a test that would catch its
regression.

---

### Task 19: `talaria call` — real execution

**Depends on:** Tasks 15, 17, 18

**Test files:**
- `cmd/talaria/call_test.go` (create) — full call path against `httptest`

**Implementation files:**
- `cmd/talaria/call.go` (modify) — wire the executor behind the existing dry-run command

**Red — write failing tests:**
1. A GET against an `httptest.Server` returns the §4 envelope: `schema`, `request` (with `curl`,
   `method`, `url`, `headers`) and `response` (with `status`, `headers`, `body`, `timing_ms`).
2. `request.headers` renders sensitive values as `<redacted:env:NAME>` while the call still
   succeeds — proving the real value reached the server but not the output.
3. The `request.curl` field is the symbolic command from Task 15, identical to what `--dry-run`
   would have printed for the same invocation.
4. `--base-url` overrides the spec server, letting the same command hit the test server.
5. A JSON response body is embedded as JSON (not a quoted string); a non-JSON body is a string.
6. Pretty output for a call includes status and timing and does not print raw response headers
   that are sensitive.

**Green — minimal implementation:**
1. Compose: resolve spec → index → bind request → gate mutations → build config → exec → render.
2. Build the output envelope from the *redacted* request representation, never the resolved one.

**Verify:** `go test ./cmd/...`

**Why:** This is the command the whole tool exists to provide, and the first point where a real
credential and real network traffic meet. Assertion 2 is the product in a single test.

---

### Task 20: Response redaction and query-key warning

**Depends on:** Tasks 12, 19

**Test files:**
- `internal/secret/response_test.go` (create) — response-side redaction
- `cmd/talaria/call_redact_test.go` (create) — end-to-end response redaction

**Implementation files:**
- `internal/secret/response.go` (create) — response header and body-path redaction
- `internal/config/config.go` (modify) — configurable redaction paths

**Red — write failing tests:**
1. A `Set-Cookie` response header is redacted by default (§5a leak-channel table).
2. Configured response body JSON paths (e.g. `access_token`, `data.token`) are redacted; the
   surrounding body is untouched and remains valid JSON.
3. Redaction of a body path that does not exist is a no-op, not an error.
4. An API key carried in a query string produces a one-time stderr warning that it will appear in
   *server* logs regardless of this tool (§5a) — and the warning itself contains no value.
5. The warning fires once per process, not once per call.

**Green — minimal implementation:**
1. Header redaction reuses the Task 12 matcher.
2. Body-path redaction walks decoded JSON by dotted path; skip non-JSON bodies entirely.
3. Warning via a `sync.Once` guard.

**Verify:** `go test ./internal/secret/... ./cmd/...`

**Why:** §5a is explicit that a response body containing a secret is a *known uncovered threat*,
mitigated but not solved by default `Set-Cookie` redaction and configurable paths. Shipping the
mitigation and being honest about the gap is the design's stated position.

---

### Task 21: `talaria auth check` and exit code 5

**Depends on:** Tasks 3, 13, 19 (assertion 5 exercises the real `call` path)

**Test files:**
- `cmd/talaria/auth_test.go` (create) — presence reporting and exit codes

**Implementation files:**
- `cmd/talaria/auth.go` (create) — the `auth check` command

**Red — write failing tests:**
1. With `TALARIA_AUTH_BEARER` set, output is exactly the §4 shape:
   `{"scheme":"bearerAuth","source":"env:TALARIA_AUTH_BEARER","present":true}` — and the canary
   value appears nowhere in the output.
2. With it unset, `present` is `false` and the command exits 5, not 2.
3. A spec with multiple schemes reports each one; exit 5 if *any required* scheme is unsatisfied.
4. `--profile staging` reports the profile as the source when the profile supplies the credential.
5. A `call` against an operation whose required scheme has no credential also exits 5, before any
   network request is attempted.

**Green — minimal implementation:**
1. Reuse Task 13 resolution; report presence via `LookupEnv` without returning values.
2. Exit 5 from `clierr.CredentialMissing`.

**Verify:** `go test ./cmd/...`

**Why:** §4: code 5 is "distinct from usage error so agents can act on it: *ask the human to set
`$NAME`*". This is how an agent diagnoses a broken auth setup blind — the one thing it must be
able to do without ever seeing a secret.

---

### Task 22: `internal/corpus` — the history store

**Depends on:** Tasks 12, 14

**Test files:**
- `internal/corpus/store_test.go` (create) — write-time redaction, retention, permissions

**Implementation files:**
- `internal/corpus/entry.go` (create) — the record type shared by history and (later) the twin
- `internal/corpus/store.go` (create) — append, trim, read, permissions

**Red — write failing tests:**
1. An appended entry stores method, URL, operationId, request/response headers and bodies,
   status, timing, a `source` (`call` | `run` | `replay`) and an RFC3339 timestamp.
2. Redaction happens at **write** time: after appending an entry built from a request carrying a
   canary credential, the canary appears nowhere in the file's bytes (§5a: "Un-redacted recording
   is not an option").
3. The store file is created `0600` inside a directory created `0700`.
4. The 1000-entry cap is applied **per `source`**, trimming oldest-first: appending 1001 `run`
   entries leaves exactly 1000 of them and evicts no `call` entry. A global cap would let one
   `run` over a large spec (Task 30) wipe a session of interactive history.
5. Bodies over 64 KiB are truncated with `"truncated": true` and the file stays valid JSONL.
6. `TALARIA_HISTORY=off` makes append a no-op that creates no file at all.
7. Constructing the store with recording disabled (the setting Task 23 layers from
   `history.enabled: false`) is likewise a no-op that creates no file, and `TALARIA_HISTORY=off`
   still wins when the setting says enabled — the env var is the operator's override.
8. A corrupt line in the middle of the file is skipped on read rather than failing the whole read.

**Green — minimal implementation:**
1. `Entry` struct plus `Store` with an injectable base directory (tests use `t.TempDir()`) and an
   `Enabled bool` setting. The store reads `TALARIA_HISTORY` itself but must **not** import
   `internal/config` — the profile key arrives as that bool from the caller.
2. Append writes one JSON line; trimming rewrites the file atomically via temp file + rename,
   preserving `0600`.
3. Build every entry from the redacted request/response representation — the store must not be
   given a path to raw values at all.

**Verify:** `go test ./internal/corpus/...`

**Why:** §5a calls history "a permanent artifact — the highest-risk surface in the tool." §5 puts
`corpus` in Phase 2 deliberately even though the recording proxy is Phase 5, because history and
the twin's corpus are the same data. See the retention decision at the top of this plan.

---

### Task 23: `talaria history` — list, show, replay

**Depends on:** Tasks 13, 19, 22 (Task 13 supplies the profile `history.enabled` setting)

**Test files:**
- `cmd/talaria/history_test.go` (create) — filters, show, replay

**Implementation files:**
- `cmd/talaria/history.go` (create) — the `history` command group
- `cmd/talaria/call.go` (modify) — record each call to the corpus

**Red — write failing tests:**
1. A successful `call` writes exactly one history entry.
2. `history` lists entries newest-first with index, timestamp, method, path and status.
3. `--operation getPet` filters by operation; `--since 1h` filters by age; `--status 4xx` matches
   400–499 (and `--status 404` matches exactly); `--source call|run|replay` filters by origin.
   `--source` is an addition beyond the §4 flag list, needed because `run` (Task 30) writes to
   the same store and would otherwise bury interactive history.
4. `history show <n>` prints the full redacted request and response; the canary value is absent.
5. `history replay <n>` re-issues the call against the recorded URL and produces a *new* history
   entry, leaving the original intact.
6. `history replay` on a mutating operation still requires `--allow-mutations`.
7. `history show` with an out-of-range index exits 2 stating the valid range.
8. `history.enabled: false` in the selected profile suppresses recording: a successful `call`
   writes no entry and creates no history file. This is the second half of the opt-out promised in
   the retention decision at the top of this plan — Task 22 gave the store the `Enabled` setting,
   and this task is the only place that reads the profile and can supply it.

**Green — minimal implementation:**
1. Read the store, apply filters, render through Task 2.
2. Construct the store with `Enabled` taken from the resolved profile config (Task 13), so
   `internal/corpus` never imports `internal/config`.
3. Replay rebuilds a `Request` from the stored entry and re-runs the Task 17 executor —
   credentials are re-resolved from the environment, never read back from history (they are not
   there to read).

**Verify:** `go test ./cmd/...`

**Why:** §4: history "answers 'what have I already tried and what came back', which is the
question an agent asks constantly and currently cannot." `replay` also delivers "the useful half
of request chaining without the scenario DSL" (§8).

---

### Task 24: The canary-secret leak suite

**Depends on:** Tasks 15, 19, 20, 23

**Test files:**
- `internal/canary/canary_test.go` (create) — every output surface, every auth mechanism

**Implementation files:**
- `internal/canary/surfaces.go` (create) — helpers enumerating output surfaces for the test

**Red — write failing tests:**
1. For each auth mechanism (bearer, basic, API key in header, query and cookie), inject a unique
   canary value and assert it appears in **none** of: JSON output, pretty output, TSV output,
   dry-run output, stderr error output, the history file, or the spec cache. §5a also names
   "verbose/debug logs" as a surface, but **Phases 1–4 ship no debug or `--verbose` flag** — do
   not invent one to test. Instead assert the absence: no registered command exposes a
   `--verbose`/`--debug` flag. §5a's rule is "no `--verbose` that bypasses [redaction]", and
   having none is the strongest form of compliance. Whoever adds one later inherits this test.
2. Error paths specifically: force a failure at each of spec load, param binding, credential
   resolution, curl exec and response validation, then grep every emitted byte for the canary
   (§5a: "Test explicitly — error paths are where redaction bugs live").
3. A canary in a *user-supplied* `--header` value is redacted in output just like a spec-derived
   credential.
4. A canary present in a *response body* is reported as **not** redacted, documenting the known
   uncovered threat (§5a) — this test asserts the tool's actual boundary rather than pretending.
5. The suite fails loudly if a new output format is added without being registered — enumerate
   formats from the Task 2 `Format` enum, not a hand-written list.

**Green — minimal implementation:**
1. Table-driven test running real commands in-process with captured writers and `t.TempDir()`
   for state.
2. Canary values are high-entropy and unique per subtest so a match cannot be coincidence.

**Verify:** `go test ./internal/canary/...`

**Why:** §5a: "A CI suite injects canary secrets through every auth mechanism and greps every
output surface… Redaction regressions fail the build. **This suite is Phase 2 work and gates
every release thereafter.**" Assertion 5 is what keeps it honest as the tool grows.

---

### Task 25: `AGENT.md` v1

**Depends on:** Tasks 21, 23

**Test files:**
- `cmd/talaria/agentdoc_test.go` (create) — documented facts match the implementation

**Implementation files:**
- `AGENT.md` (create) — the operating manual for LLMs

**Red — write failing tests:**
1. Every exit code documented in `AGENT.md` exists in `internal/clierr`, and every code in
   `clierr` is documented — parse the doc's table, compare to the constants.
2. Every command named in `AGENT.md` is a registered cobra command, and every registered command
   appears in the doc.
3. Every `TALARIA_*` env var named in the doc is one the code actually reads.

**Green — minimal implementation:**
1. Write `AGENT.md` covering: the list→search→describe→dry-run→call loop; exit codes and what to
   do about each; output shapes; **how to name a credential you cannot see**; the mutation gate
   and that the sanctioned way to test a DELETE is against the twin; and the explicit statement
   that there is no way to reveal a secret and asking for one is not a supported workflow.
2. Keep it terse — it is read by a model paying per token, and it is the tool's user interface.

**Verify:** `go test ./cmd/...`

**Why:** §3.7: "The agent README is a first-class deliverable… Treat it with the same care as
code." The tests exist because a manual that drifts from the binary actively misleads the one
reader who cannot check the source.

---

### Task 26: `internal/validate` — response validation

**Depends on:** Tasks 3, 7

**Test files:**
- `internal/validate/validate_test.go` (create) — status, body and content-type validation
- `internal/validate/testdata/strict-3.0.yaml` (create) — a 3.0 spec exercising 3.1-strict edges

**Implementation files:**
- `internal/validate/validate.go` (create) — response-vs-spec validation

**Red — write failing tests:**
1. A response whose status is documented reports `status_documented: true`; an undocumented
   status reports `false` without erroring.
2. A body matching the response schema reports `body_valid: true`; a body with a wrong field type
   reports `false` with an error naming the field and the expected type.
3. A response whose content-type is absent from the spec's declared media types is flagged.
4. A `default` response entry satisfies an otherwise-undocumented status.
5. A 204 with an empty body validates rather than failing on "missing body".
6. Validation still routes when `--base-url` points somewhere other than the spec's server:
   validate the same response against request URLs `https://api.real.example.com/v1/pets`,
   `http://127.0.0.1:PORT/pets` and `http://127.0.0.1:PORT/v1/pets` and assert all three agree.
   Planning verified the router is host- and prefix-tolerant (Ground facts); this test pins that
   behaviour so a validator upgrade cannot silently break every test-server call.
7. **The §8 open question:** the 3.0 fixture must contain the constructs where 3.1 strictness
   actually bites — `nullable: true`, boolean `exclusiveMinimum`/`exclusiveMaximum`, and singular
   `example` — and the test asserts which, if any, produce a false failure under the validator's
   3.1-strict JSON Schema default. Record the finding in a comment at the top of the test file
   and, if a false failure occurs, configure the validator down per-document and assert it now
   passes. Planning saw no false failure on a *simple* 3.0 document, so absence of a finding here
   is only meaningful if the fixture is genuinely adversarial.

**Green — minimal implementation:**
1. `go get github.com/pb33f/libopenapi-validator@v0.14.0`.
2. Take **plain data** — method, URL, status, headers, body bytes — reconstruct
   `*http.Request`/`*http.Response` internally, call `ValidateHttpResponse`, and map its
   `[]*errors.ValidationError` (`.Message`, `.Reason`) into the `validation` block.
3. `internal/validate` must not import `internal/curl`, `internal/corpus` or `internal/twin`
   (§5 boundary rule). It is shared with the twin in later phases, so it takes a plain input
   struct that both a live `call` and a stored corpus entry can produce — the caller converts,
   not this package.

**Verify:** `go test ./internal/validate/...`

**Why:** This is the differentiator docs/research §3 confirms: Restish validates *outbound request
bodies only*, opt-in. Response-vs-spec validation is the claim, and §8 flags the 3.0 strictness
risk as something to confirm in this phase rather than discover in production.

---

### Task 27: `call` validation block and `--fail-on-error`

**Depends on:** Tasks 19, 26

**Test files:**
- `cmd/talaria/call_validate_test.go` (create) — the validation block and exit code 4

**Implementation files:**
- `cmd/talaria/call.go` (modify) — attach validation to the call envelope

**Red — write failing tests:**
1. A call against a conforming `httptest` response emits
   `"validation": {"status_documented": true, "body_valid": true, "errors": []}`.
2. A non-conforming response populates `errors` and still exits **0** without `--fail-on-error` —
   an observation is not a failure (§4).
3. With `--fail-on-error`, a validation failure exits 4.
4. With `--fail-on-error`, an HTTP 4xx/5xx exits 4 as well (§4 note on the flag's meaning).
5. Validation runs on the *redacted* representation and adds no new leak surface — assert no
   canary in the validation errors, which quote request and response content.

**Verify:** `go test ./cmd/...`

**Why:** Completes Phase 3. Assertion 5 matters because §5a lists "validation errors quoting the
request" as a named leak channel — validation is exactly the kind of code that helpfully echoes
inputs back.

---

### Task 28: `internal/gen` — schema-based data generation

**Depends on:** Task 7

**Test files:**
- `internal/gen/gen_test.go` (create) — generation from schemas and examples

**Implementation files:**
- `internal/gen/gen.go` (create) — example-first data generation

**Red — write failing tests:**
1. A schema carrying an `example` yields that example verbatim — examples always win (§5a "Test
   data for `run` mode").
2. A schema with no example generates a value matching its type for string, integer, number,
   boolean, array and object.
3. Required object properties are always present; optional ones may be omitted.
4. `format` hints are honoured for `date-time`, `uuid` and `email` (generated values parse).
5. An enum generates one of its members.
6. A recursive schema terminates at a depth limit rather than hanging.
7. Generation is deterministic given a fixed seed — two runs produce identical data, so `run`
   results are reproducible and diffable.

**Green — minimal implementation:**
1. Walk the schema proxy with a depth counter and a visited set.
2. Seeded `math/rand` source held on the generator, never the global one.
3. `internal/gen` must not import `internal/curl` or `internal/twin` (§5) — it is shared with the
   twin in later phases.

**Verify:** `go test ./internal/gen/...`

**Why:** `run` needs request data for operations without examples. Determinism is what makes a
smoke-test suite usable in CI — a random body that fails once and passes next run is worse than
no test.

---

### Task 29: Fixtures and the test-data priority chain

**Depends on:** Task 28

**Test files:**
- `internal/gen/fixtures_test.go` (create) — fixture loading and priority order

**Implementation files:**
- `internal/gen/fixtures.go` (create) — `--fixtures` directory loading

**Red — write failing tests:**
1. A fixture file named `<operationId>.json` in the fixtures directory is used for that operation.
2. Priority is exactly spec example → fixture → generated (§5a). Assert all three orderings with
   a case where two sources are available.
3. A fixtures directory that does not exist exits 2; an empty one is fine and falls through to
   generation.
4. A fixture containing invalid JSON exits 2 naming the file.
5. A fixture may supply params and headers, not just a body.

**Green — minimal implementation:**
1. Load the directory once into a map keyed by operationId.
2. A single `DataFor(op)` entry point implementing the priority chain, so `run` cannot get the
   order wrong.

**Verify:** `go test ./internal/gen/...`

**Why:** §5a: "Examples-first keeps requests realistic." Fixtures are the escape hatch for
operations whose generated data would be nonsense (real ids, valid foreign keys) — without them
`run` is only usable on toy APIs.

---

### Task 30: `talaria run` — smoke testing

**Depends on:** Tasks 19, 27, 29

**Test files:**
- `cmd/talaria/run_test.go` (create) — filters, execution, reporting, exit codes

**Implementation files:**
- `cmd/talaria/run.go` (create) — the `run` command, including its `--fixtures` flag

**Red — write failing tests:**
1. `run <spec>` executes every read-only operation against an `httptest` server and reports one
   result per operation with status, timing and validation outcome.
2. `--tag pets` and repeated `--operation <id>` filter the set; combining them is the union.
3. Mutating operations are skipped without `--allow-mutations` and reported as skipped, not
   failed — a skipped DELETE is correct behaviour, not an error.
4. `--report json` emits the envelope with a summary (total, passed, failed, skipped); `--report
   pretty` emits a human summary line. `--report` is `run`'s own flag and **wins over the
   persistent `--output`** when both are set; with only `--output` set, `run` follows it. Assert
   both orderings — §3.1 puts `--output` on every command (Task 2), so an agent will set it here
   and must not get a different format than it asked for.
5. Without `--fail-on-error`, a 500 from one operation still exits 0; with it, exit 4.
6. Operations requiring an unsatisfied credential are reported as such and exit 5.
7. Path params are filled from the priority chain (Task 29), and an operation whose required
   param cannot be supplied is reported as skipped with the reason.
8. Each executed operation writes one history entry tagged `"source": "run"`, and `history
   --source call` excludes them. A `run` over a large spec must not make `history` useless by
   evicting a session's worth of `call` entries under the 1000-entry cap — assert that after a
   `run` of N operations, prior `call` entries are still retrievable.
9. `--fixtures <dir>` is registered on `run` and reaches the Task 29 priority chain: with a
   fixture file present for an operation, that operation's request body is the fixture's; drop the
   flag and the same run falls through to generation. A non-existent directory exits 2. DESIGN.md
   §5a names `--fixtures dir/`, and Task 29 builds the loader, but **this is the only task that
   registers the flag** — without it the whole fixture path is unreachable from the CLI and Task
   29 ships dead code. It is `run`'s own flag, not a persistent one; no other command takes it.

**Green — minimal implementation:**
1. Resolve → filter → for each operation build data, build request, exec, validate, collect.
   Construct the Task 29 `DataFor` chain once from `--fixtures` (empty when the flag is unset) and
   reuse it across operations, so the priority order is decided in one place.
2. Sequential execution in spec order; no concurrency in v1 (determinism beats speed here, and
   parallel calls against a real API are a surprise nobody asked for).
3. Every call goes through the same corpus writer as `call`, with `Source: "run"`. The field, the
   per-source cap and the `--source` filter already exist (Tasks 22 and 23); this task only sets
   the value correctly.

**Verify:** `go test ./cmd/...`

**Why:** Phase 4 and the end of the differentiated core. `run` is what makes the tool CI-ready
and what an agent uses to answer "is this whole API behaving?" in one command.

---

### Task 31: JUnit report output

**Depends on:** Tasks 2, 30

**Test files:**
- `internal/output/junit_test.go` (create) — JUnit XML shape

**Implementation files:**
- `internal/output/junit.go` (create) — JUnit XML renderer
- `cmd/talaria/run.go` (modify) — wire `--report junit`

**Red — write failing tests:**
1. Output is well-formed XML with a `<testsuite>` carrying `tests`, `failures` and `skipped`
   counts matching the results.
2. One `<testcase>` per operation, named by operationId, with a `time` attribute.
3. A failed operation emits a `<failure>` child carrying the reason; a skipped one emits
   `<skipped>`.
4. Reason text is XML-escaped, so a message containing `<`, `&` or `"` does not corrupt the file.
5. No canary value appears in a JUnit report generated from an authenticated run (§5a lists
   "junit reports" as an output surface the canary suite must cover).

**Green — minimal implementation:**
1. Render with `encoding/xml` structs — do not hand-build XML strings.
2. Register the format with the Task 24 surface enumeration so the canary suite covers it.

**Verify:** `go test ./internal/output/... ./cmd/...`

**Why:** JUnit is what makes `run` land in CI without glue. Assertion 5 is why this task comes
after the canary suite rather than being bolted on later.

---

### Task 32: End-to-end integration across the whole loop

**Depends on:** Tasks 23, 30, 31

**Test files:**
- `internal/e2e/e2e_test.go` (create) — the full workflow against a local server
- `internal/e2e/boundary_test.go` (create) — the §5 package-import rule, asserted on the real
  import graph
- `internal/e2e/testdata/e2e-api.yaml` (create) — 3.0 spec matching the test server's behaviour
- `internal/e2e/testdata/e2e-api-2.0.json` (create) — the same API as Swagger 2.0, for assertion 5

**Implementation files:**
- none — this task adds no production code. If it cannot pass without changing production code,
  that change is a real bug the earlier tasks missed; fix it and say so in the commit message.

**Red — write failing tests:**
1. Against one `httptest.Server` and one spec, run the documented agent workflow in sequence:
   `list` → `search` → `describe` → `call --dry-run` → `call` → `history` → `history replay` →
   `run --report json`, asserting each step's exit code and that each step's output contains what
   the next step needs (the id from `list`/`search` is the id `describe` accepts; the curl from
   `--dry-run` matches the `request.curl` of the real `call`).
2. The whole sequence runs with a canary credential set, and the canary appears in **no** step's
   stdout, stderr, or the history file written along the way.
3. A mutating operation is refused without `--allow-mutations` and succeeds with it, at both the
   `call` and `run` levels.
4. Exit codes observed across the sequence cover 0, 2, 4 and 5 against the same spec and server.
5. Swagger 2.0: repeat the core of the sequence against a converted 2.0 spec, proving conversion
   output flows through operation model, curl builder, executor and validator unchanged.
6. **The §5 boundary rule holds in the built code, not only in prose.** Shell out to
   `go list -deps <pkg>` and assert that the transitive dependencies of `internal/operation`,
   `internal/validate` and `internal/gen` contain **none** of `internal/curl`, `internal/corpus`
   or `internal/twin`. Transitive, not direct — an indirect edge violates the rule just as
   completely and is far easier to add by accident. Every other test in this plan passes whether
   or not this invariant holds, so nothing else can catch its breach; the cost lands in Phase 6,
   when the twin has to consume these three packages and cannot.

**Green — minimal implementation:**
1. Table-driven sequence with `t.TempDir()` for history and cache so the suite is hermetic.
2. Drive the real cobra commands in-process with captured writers — not a mocked layer.

**Verify:** `go test ./...` and `test -z "$(gofmt -l .)" && go vet ./...`

**Why:** Every prior task tests one component against its own fixtures. This is the only task
that proves component N's output actually reaches component N+1 — particularly that the 2.0
conversion path (Task 5) survives all the way to validation, and that the credential firewall
holds across a whole session rather than one command at a time.

---

### Task 33: CI workflow running the test suite

**Depends on:** Task 32

**Test files:**
- `internal/ci/workflow_test.go` (create) — the workflow file stays in step with `.ralph/stack.json`

**Implementation files:**
- `.github/workflows/ci.yml` (create) — build, lint and test on push and pull request

**Red — write failing tests:**
1. `.github/workflows/ci.yml` parses as YAML and declares triggers for both `push` and
   `pull_request`.
2. Every shell command the workflow runs appears verbatim in `.ralph/stack.json`
   (`build_command`, `lint_command`, `test_command`) — parse both files and compare, so the
   workflow cannot drift into invented commands.
3. The Go version pinned in the workflow matches the `go` directive in `go.mod`.
4. The workflow runs `go test ./...`, which is what makes the Task 24 canary suite an actual
   build gate rather than a test somebody has to remember to run.

**Green — minimal implementation:**
1. A single `ubuntu-latest` job: checkout, `actions/setup-go` pinned to the `go.mod` version,
   then `go build ./...`, `test -z "$(gofmt -l .)" && go vet ./...`, `go test ./...`.
2. Nothing else. **No release, matrix, cross-compilation or publishing step** — distribution is
   out of scope (see Scope), and a release job here would quietly start Phase-9 work.
3. curl is preinstalled on `ubuntu-latest` and is well above the 7.70 floor, so no install step
   is needed; the Task 17 preflight covers the case where it is not.

**Verify:** `go test ./internal/ci/...` then `go build ./...` and
`test -z "$(gofmt -l .)" && go vet ./...` and `go test ./...`

**Why:** §5a requires that the canary suite "gates every release thereafter" and that "redaction
regressions fail the build". Until a build exists, nothing gates anything — the suite is only a
file somebody could skip. This is the smallest thing that makes the design's own security
requirement real, and the test keeps the workflow honest about which commands it runs.

---

**When Task 33 passes, Phases 1–4 are complete. Stop.** DESIGN.md §7 sets a decision gate here:
Phases 5–8 are roughly as much work again, land in a crowded market, and must not be started
until real usage has made the twin's absence painful. That call belongs to a human.
