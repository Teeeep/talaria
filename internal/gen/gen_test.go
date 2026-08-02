package gen

import (
	"encoding/json"
	"net/mail"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	"github.com/pb33f/libopenapi/orderedmap"
	"github.com/pb33f/libopenapi/utils"
	yaml "go.yaml.in/yaml/v4"
)

// The seed every test that does not care about a specific draw uses. Fixed
// rather than time-based on purpose: a generator test that is itself
// non-deterministic can only report flakes.
const testSeed = 42

func schemaProxy(s *base.Schema) *base.SchemaProxy { return base.CreateSchemaProxy(s) }

func scalarSchema(typ string) *base.SchemaProxy {
	return schemaProxy(&base.Schema{Type: []string{typ}})
}

func formatSchema(typ, format string) *base.SchemaProxy {
	return schemaProxy(&base.Schema{Type: []string{typ}, Format: format})
}

func arraySchema(items *base.SchemaProxy) *base.SchemaProxy {
	return schemaProxy(&base.Schema{
		Type:  []string{"array"},
		Items: &base.DynamicValue[*base.SchemaProxy, bool]{A: items},
	})
}

// objectSchema builds an object schema from name/schema pairs in the given
// order.
func objectSchema(required []string, props ...any) *base.Schema {
	m := orderedmap.New[string, *base.SchemaProxy]()
	for i := 0; i < len(props); i += 2 {
		m.Set(props[i].(string), props[i+1].(*base.SchemaProxy))
	}

	return &base.Schema{Type: []string{"object"}, Required: required, Properties: m}
}

// yamlNode turns a YAML fragment into the node shape libopenapi hands back for
// `example:` and `default:`, so tests can attach an example of any shape.
func yamlNode(t *testing.T, fragment string) *yaml.Node {
	t.Helper()

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fragment), &doc); err != nil {
		t.Fatalf("parsing example fragment: %v", err)
	}

	return doc.Content[0]
}

func TestExampleIsUsedVerbatim(t *testing.T) {
	s := objectSchema([]string{"id"}, "id", scalarSchema("string"))
	s.Example = yamlNode(t, `{"id": "pet-7", "name": "Fido", "tags": ["good", "dog"]}`)

	got := New(testSeed).Value(schemaProxy(s))

	want := map[string]any{
		"id":   "pet-7",
		"name": "Fido",
		"tags": []any{"good", "dog"},
	}
	if !jsonEqual(t, got, want) {
		t.Errorf("Value() = %s, want the example verbatim %s", toJSON(t, got), toJSON(t, want))
	}
}

func TestExampleOnAPropertyBeatsItsType(t *testing.T) {
	id := scalarSchema("string")
	id.Schema().Example = yamlNode(t, `"the-example"`)

	got := New(testSeed).Value(schemaProxy(objectSchema([]string{"id"}, "id", id)))

	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("Value() = %T, want map[string]any", got)
	}
	if obj["id"] != "the-example" {
		t.Errorf("id = %v, want the property's own example", obj["id"])
	}
}

func TestExamplesPluralIsUsedWhenSingularIsAbsent(t *testing.T) {
	s := &base.Schema{Type: []string{"string"}}
	s.Examples = []*yaml.Node{yamlNode(t, `"first"`), yamlNode(t, `"second"`)}

	if got := New(testSeed).Value(schemaProxy(s)); got != "first" {
		t.Errorf("Value() = %v, want the first entry of examples", got)
	}
}

func TestGeneratesAValueOfTheDeclaredType(t *testing.T) {
	g := New(testSeed)

	for _, tc := range []struct {
		typ  string
		want string // the JSON kind the value has to marshal to
	}{
		{"string", "string"},
		{"integer", "integer"},
		{"number", "number"},
		{"boolean", "boolean"},
	} {
		got := g.Value(scalarSchema(tc.typ))
		if kind := jsonKind(t, got); kind != tc.want {
			t.Errorf("Value(%s) = %v (%s), want a %s", tc.typ, got, kind, tc.want)
		}
	}
}

func TestGeneratesArrayElementsOfTheItemType(t *testing.T) {
	got := New(testSeed).Value(arraySchema(scalarSchema("integer")))

	items, ok := got.([]any)
	if !ok {
		t.Fatalf("Value(array) = %T, want []any", got)
	}
	if len(items) == 0 {
		t.Fatal("Value(array) generated an empty array; a smoke test needs at least one element")
	}
	for i, item := range items {
		if kind := jsonKind(t, item); kind != "integer" {
			t.Errorf("element %d = %v (%s), want an integer", i, item, kind)
		}
	}
}

func TestGeneratesObjectPropertiesOfTheirDeclaredTypes(t *testing.T) {
	got := New(testSeed).Value(schemaProxy(objectSchema(
		[]string{"name", "count", "active"},
		"name", scalarSchema("string"),
		"count", scalarSchema("integer"),
		"active", scalarSchema("boolean"),
	)))

	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("Value(object) = %T, want map[string]any", got)
	}
	for name, want := range map[string]string{"name": "string", "count": "integer", "active": "boolean"} {
		if kind := jsonKind(t, obj[name]); kind != want {
			t.Errorf("%s = %v (%s), want a %s", name, obj[name], kind, want)
		}
	}
}

func TestRequiredPropertiesAreAlwaysPresent(t *testing.T) {
	s := schemaProxy(objectSchema(
		[]string{"required1", "required2"},
		"required1", scalarSchema("string"),
		"required2", scalarSchema("integer"),
		"optional1", scalarSchema("string"),
		"optional2", scalarSchema("string"),
		"optional3", scalarSchema("string"),
	))

	omittedAnOptional := false
	for seed := int64(0); seed < 50; seed++ {
		obj, ok := New(seed).Value(s).(map[string]any)
		if !ok {
			t.Fatalf("seed %d: Value(object) did not produce an object", seed)
		}
		for _, name := range []string{"required1", "required2"} {
			if _, present := obj[name]; !present {
				t.Fatalf("seed %d: required property %s was omitted: %s", seed, name, toJSON(t, obj))
			}
		}
		for _, name := range []string{"optional1", "optional2", "optional3"} {
			if _, present := obj[name]; !present {
				omittedAnOptional = true
			}
		}
	}

	if !omittedAnOptional {
		t.Error("no seed omitted an optional property; optional properties are being treated as required")
	}
}

func TestFormatHintsProduceParseableValues(t *testing.T) {
	g := New(testSeed)

	dateTime, ok := g.Value(formatSchema("string", "date-time")).(string)
	if !ok {
		t.Fatal("date-time did not generate a string")
	}
	if _, err := time.Parse(time.RFC3339, dateTime); err != nil {
		t.Errorf("date-time generated %q, which does not parse as RFC3339: %v", dateTime, err)
	}

	id, ok := g.Value(formatSchema("string", "uuid")).(string)
	if !ok {
		t.Fatal("uuid did not generate a string")
	}
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidPattern.MatchString(id) {
		t.Errorf("uuid generated %q, which is not a v4 UUID", id)
	}

	email, ok := g.Value(formatSchema("string", "email")).(string)
	if !ok {
		t.Fatal("email did not generate a string")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		t.Errorf("email generated %q, which does not parse as an address: %v", email, err)
	}
}

func TestEnumGeneratesOneOfItsMembers(t *testing.T) {
	s := &base.Schema{Type: []string{"string"}}
	members := []string{"active", "archived", "deleted"}
	for _, m := range members {
		s.Enum = append(s.Enum, utils.CreateStringNode(m))
	}
	sp := schemaProxy(s)

	for seed := int64(0); seed < 20; seed++ {
		got := New(seed).Value(sp)
		if !slicesContains(members, got) {
			t.Fatalf("seed %d: Value(enum) = %v, want one of %v", seed, got, members)
		}
	}
}

func TestRecursiveSchemaTerminates(t *testing.T) {
	sp := fixtureResponseSchema(t, "testdata/recursive-schema.yaml", "getTree")

	// Termination is the assertion: without a guard these calls never return,
	// so arriving at the checks below is most of the result. Several seeds
	// because the recursive properties are optional — one seed that happens to
	// skip them would never walk the cycle at all.
	descended := false
	for seed := int64(0); seed < 20; seed++ {
		obj, ok := New(seed).Value(sp).(map[string]any)
		if !ok {
			t.Fatalf("seed %d: Value(recursive) did not produce an object", seed)
		}
		if _, present := obj["id"]; !present {
			t.Errorf("seed %d: the guard swallowed the non-recursive properties: %s", seed, toJSON(t, obj))
		}
		if depth := nestingDepth(obj); depth > DefaultMaxDepth {
			t.Errorf("seed %d: generated data nests %d levels deep, past the limit of %d",
				seed, depth, DefaultMaxDepth)
		}
		if _, present := obj["parent"]; present {
			descended = true
		}
	}

	if !descended {
		t.Error("no seed generated the self-referencing property; the cycle was never walked")
	}
}

func TestGenerationIsDeterministicForASeed(t *testing.T) {
	sp := schemaProxy(objectSchema(
		[]string{"id", "created"},
		"id", formatSchema("string", "uuid"),
		"created", formatSchema("string", "date-time"),
		"tags", arraySchema(scalarSchema("string")),
		"score", scalarSchema("number"),
		"nested", schemaProxy(objectSchema(nil, "flag", scalarSchema("boolean"))),
	))

	first := toJSON(t, New(testSeed).Value(sp))
	second := toJSON(t, New(testSeed).Value(sp))

	if first != second {
		t.Errorf("two runs at the same seed differ:\n%s\n%s", first, second)
	}
	if other := toJSON(t, New(testSeed+1).Value(sp)); other == first {
		t.Error("two different seeds produced identical data; the seed is not reaching the generator")
	}
}

// TestGenDoesNotDependOnCurlOrTwin enforces the §5 boundary: internal/gen is
// shared with the twin server, so it may not reach back into the CLI's
// execution path.
func TestGenDoesNotDependOnCurlOrTwin(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("listing dependencies: %v", err)
	}

	for _, forbidden := range []string{
		"github.com/Teeeep/talaria/internal/curl",
		"github.com/Teeeep/talaria/internal/twin",
	} {
		for _, dep := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(dep) == forbidden {
				t.Errorf("internal/gen depends on %s, which breaks the §5 package boundary", forbidden)
			}
		}
	}
}

// fixtureResponseSchema loads a spec fixture and returns the first response
// schema of one operation — the shortest path from a real spec, with real
// resolved $refs, to a schema proxy.
func fixtureResponseSchema(t *testing.T, path, operationID string) *base.SchemaProxy {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	doc, err := libopenapi.NewDocument(data)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	model, err := doc.BuildV3Model()
	if err != nil {
		t.Fatalf("building fixture model: %v", err)
	}

	for _, item := range model.Model.Paths.PathItems.FromOldest() {
		for _, op := range item.GetOperations().FromOldest() {
			if op.OperationId != operationID {
				continue
			}
			for _, resp := range op.Responses.Codes.FromOldest() {
				for _, media := range resp.Content.FromOldest() {
					return media.Schema
				}
			}
		}
	}

	t.Fatalf("fixture %s has no response schema for %s", path, operationID)

	return nil
}

// jsonKind names the JSON type a generated value marshals to. Going through
// encoding/json is the point: `run` puts these values on the wire as JSON, so
// what matters is what they serialise to, not their Go type.
func jsonKind(t *testing.T, v any) string {
	t.Helper()

	switch decoded := decodeJSON(t, v).(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if decoded == float64(int64(decoded)) && !strings.Contains(toJSON(t, v), ".") {
			return "integer"
		}

		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling generated value: %v", err)
	}

	return string(b)
}

func decodeJSON(t *testing.T, v any) any {
	t.Helper()

	var out any
	if err := json.Unmarshal([]byte(toJSON(t, v)), &out); err != nil {
		t.Fatalf("round-tripping generated value: %v", err)
	}

	return out
}

func jsonEqual(t *testing.T, got, want any) bool {
	t.Helper()

	return toJSON(t, got) == toJSON(t, want)
}

func slicesContains(members []string, got any) bool {
	s, ok := got.(string)
	if !ok {
		return false
	}
	for _, m := range members {
		if m == s {
			return true
		}
	}

	return false
}

// nestingDepth measures how many levels of container the generated data has, so
// the recursion test can assert the depth limit held rather than only that the
// call returned.
func nestingDepth(v any) int {
	switch value := v.(type) {
	case map[string]any:
		deepest := 0
		for _, child := range value {
			if d := nestingDepth(child); d > deepest {
				deepest = d
			}
		}

		return deepest + 1
	case []any:
		deepest := 0
		for _, child := range value {
			if d := nestingDepth(child); d > deepest {
				deepest = d
			}
		}

		return deepest + 1
	default:
		return 0
	}
}
