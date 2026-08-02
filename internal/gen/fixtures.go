package gen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/pb33f/libopenapi/datamodel/high/base"
)

// Fixture is one operation's test data as written on disk, in
// `<fixtures-dir>/<operationId>.json`. Every field is optional: a fixture that
// only pins the path id and leaves the body to generation is the common case.
type Fixture struct {
	// Params supply declared parameters by name, in any location. Values are
	// scalars; they are rendered to text the way they will travel on the wire.
	Params map[string]any `json:"params"`
	// Headers are sent verbatim, and need not be declared by the spec — a
	// tenant or trace header the API expects but the document never mentions is
	// exactly what this is for.
	Headers map[string]string `json:"headers"`
	// Body is kept raw so a fixture reaches the server as the author wrote it,
	// rather than round-tripped through a Go value.
	Body json.RawMessage `json:"body"`
}

// Fixtures is a loaded --fixtures directory, keyed by operationId. The zero
// value and a nil *Fixtures are both usable and empty, which is what `run`
// holds when the flag is unset.
type Fixtures struct {
	dir  string
	byID map[string]Fixture
}

// LoadFixtures reads every `*.json` file in dir into a set keyed by the file's
// base name. An empty path is not an error — it is an unset --fixtures flag —
// and yields an empty set, so `run` builds the priority chain the same way with
// or without the flag.
//
// The whole directory is read once, up front. A fixture that only fails to
// parse on the operation that happens to need it would report a typo halfway
// through a suite, after real requests have already gone out.
func LoadFixtures(dir string) (*Fixtures, error) {
	fx := &Fixtures{dir: dir, byID: map[string]Fixture{}}
	if dir == "" {
		return fx, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, clierr.Usage("reading fixtures directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}

		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, clierr.Usage("reading fixture %s: %w", path, err)
		}

		var f Fixture
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, clierr.Usage("parsing fixture %s: %w", path, err)
		}

		fx.byID[strings.TrimSuffix(name, filepath.Ext(name))] = f
	}

	return fx, nil
}

// Get returns the fixture for an operationId, and whether there was one.
func (f *Fixtures) Get(id string) (Fixture, bool) {
	if f == nil {
		return Fixture{}, false
	}

	fixture, ok := f.byID[id]

	return fixture, ok
}

// Len is how many fixtures were loaded.
func (f *Fixtures) Len() int {
	if f == nil {
		return 0
	}

	return len(f.byID)
}

// Data is one operation's request data: what to bind to its declared
// parameters, what extra headers to send, and the body, JSON-encoded and nil
// when there is none.
type Data struct {
	// Params are declared parameters by name, already rendered as text, ready
	// for request.Inputs.Params.
	Params map[string]string
	// Headers are fixture-supplied headers, ready for request.Inputs.Headers.
	Headers map[string]string
	// Body is the encoded request body, or nil.
	Body []byte
}

// DataFor returns the request data for one operation, applying the DESIGN.md
// §5a priority chain: a spec example, then a fixture, then generation.
//
// This is the only entry point that decides the order, so `run` cannot get it
// wrong by assembling the pieces itself. The chain runs per field rather than
// per request — an example on one parameter beats the fixture for that
// parameter while the fixture still supplies the next — because a spec that
// documents one id and a fixture that supplies the rest is the normal case, not
// a conflict to resolve wholesale.
func (g *Generator) DataFor(op operation.Operation) Data {
	fixture, _ := g.Fixtures.Get(op.ID)

	data := Data{
		Params:  make(map[string]string, len(op.Params)),
		Headers: make(map[string]string, len(fixture.Headers)),
		Body:    g.bodyFor(op, fixture),
	}

	for name, value := range fixture.Headers {
		data.Headers[name] = value
	}

	for _, p := range op.Params {
		if v, ok := g.paramValue(p, fixture); ok {
			data.Params[p.Name] = scalarString(v)
		}
	}

	// Fixture parameters the operation does not declare are passed through
	// rather than dropped: the request builder reports an unknown parameter by
	// name, which is a better answer to a typo than a value that silently never
	// arrives.
	for name, value := range fixture.Params {
		if _, ok := data.Params[name]; !ok {
			data.Params[name] = scalarString(value)
		}
	}

	return data
}

// paramValue resolves one parameter through the chain, reporting whether it
// found anything at all.
//
// An optional parameter with no example and no fixture is deliberately left
// out. Generating one means a smoke test filtering on an invented cursor or
// searching for a made-up name, which tests the API's error handling rather
// than the operation. A required one is always supplied: a request the server
// rejects for a missing parameter never reaches the behaviour under test.
func (g *Generator) paramValue(p operation.Param, fixture Fixture) (any, bool) {
	if p.Schema != nil {
		if s := p.Schema.Schema(); s != nil {
			if v, ok := exampleValue(s); ok {
				return v, true
			}
		}
	}

	if v, ok := fixture.Params[p.Name]; ok {
		return v, true
	}

	if !p.Required {
		return nil, false
	}

	return g.value(p.Schema, 0, make(map[string]int))
}

// bodyFor resolves the request body through the same chain.
//
// A fixture body is used even for an operation that declares no request body:
// specs under-document bodies all the time, and a fixture is the escape hatch
// for exactly that. The reverse — inventing a body for an operation the spec
// says takes none — is not something anyone asked for, so generation only runs
// when there is a schema to generate from.
func (g *Generator) bodyFor(op operation.Operation, fixture Fixture) []byte {
	sp := bodySchema(op)

	if sp != nil {
		if s := sp.Schema(); s != nil {
			if v, ok := exampleValue(s); ok {
				return encode(v)
			}
		}
	}

	if len(fixture.Body) > 0 {
		return compact(fixture.Body)
	}

	if sp == nil {
		return nil
	}

	v, ok := g.value(sp, 0, make(map[string]int))
	if !ok {
		return nil
	}

	return encode(v)
}

// bodySchema picks the media type to generate for: JSON if the operation
// accepts it, otherwise the first declared. Generation produces a Go value
// shaped for encoding/json, so a JSON media type is the one it can actually
// satisfy.
func bodySchema(op operation.Operation) *base.SchemaProxy {
	if op.RequestBody == nil {
		return nil
	}

	for _, mt := range op.RequestBody.Content {
		if strings.Contains(mt.ContentType, "json") {
			return mt.Schema
		}
	}

	if len(op.RequestBody.Content) > 0 {
		return op.RequestBody.Content[0].Schema
	}

	return nil
}

// encode marshals a generated value. The value came from the generator or from
// a spec example that already round-tripped through YAML, so a marshalling
// failure is not a state the caller can act on: an empty body is the honest
// result.
func encode(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}

	return out
}

// compact strips the formatting from a fixture body so two runs of the same
// fixture produce byte-identical requests regardless of how it was indented.
func compact(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}

	return buf.Bytes()
}

// scalarString renders a parameter value as the text that will travel in a URL
// or a header. Numbers decoded from a fixture arrive as float64, so 42 has to
// come back as "42" and not "42.000000" — a path segment is not the place to
// discover Go's default float formatting.
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	default:
		if out, err := json.Marshal(t); err == nil {
			return string(out)
		}

		return fmt.Sprint(t)
	}
}
