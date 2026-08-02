package gen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/pb33f/libopenapi/datamodel/high/base"
)

// writeFixtures builds a fixtures directory from name/content pairs and returns
// its path. Content is written verbatim so a test can hand in malformed JSON.
func writeFixtures(t *testing.T, files ...string) string {
	t.Helper()

	dir := t.TempDir()
	for i := 0; i < len(files); i += 2 {
		path := filepath.Join(dir, files[i])
		if err := os.WriteFile(path, []byte(files[i+1]), 0o600); err != nil {
			t.Fatalf("writing fixture %s: %v", path, err)
		}
	}

	return dir
}

// loadFixtures loads a directory, failing the test on error: the loader's own
// failures have their own tests.
func loadFixtures(t *testing.T, dir string) *Fixtures {
	t.Helper()

	fx, err := LoadFixtures(dir)
	if err != nil {
		t.Fatalf("LoadFixtures(%s): %v", dir, err)
	}

	return fx
}

// bodyOp is an operation taking a JSON body of the given schema.
func bodyOp(id string, schema *base.SchemaProxy) operation.Operation {
	return operation.Operation{
		ID:     id,
		Method: "POST",
		Path:   "/pets",
		RequestBody: &operation.RequestBody{
			Required: true,
			Content:  []operation.MediaType{{ContentType: "application/json", Schema: schema}},
		},
	}
}

// wantCode asserts the exit code a failure carries, which is the part of an
// error an agent actually branches on.
func wantCode(t *testing.T, err error, code clierr.Code) *clierr.Error {
	t.Helper()

	if err == nil {
		t.Fatal("got no error, want one")
	}
	cerr := clierr.From(err)
	if cerr.Code != code {
		t.Fatalf("error %q has code %d, want %d", cerr.Message, cerr.Code, code)
	}

	return cerr
}

func TestFixtureFileSuppliesTheBody(t *testing.T) {
	dir := writeFixtures(t, "createPet.json", `{"body": {"name": "Fido", "age": 3}}`)
	g := New(testSeed)
	g.Fixtures = loadFixtures(t, dir)

	got := g.DataFor(bodyOp("createPet", schemaProxy(objectSchema(nil, "name", scalarSchema("string")))))

	var body map[string]any
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatalf("unmarshalling body %s: %v", got.Body, err)
	}
	if body["name"] != "Fido" || body["age"] != float64(3) {
		t.Errorf("DataFor() body = %s, want the fixture verbatim", got.Body)
	}
}

func TestFixtureIsMatchedByOperationID(t *testing.T) {
	dir := writeFixtures(t, "createPet.json", `{"body": {"name": "Fido"}}`)
	g := New(testSeed)
	g.Fixtures = loadFixtures(t, dir)

	// Same shape, different operationId: the fixture must not leak across.
	got := g.DataFor(bodyOp("updatePet", schemaProxy(objectSchema(nil, "name", scalarSchema("string")))))

	var body map[string]any
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatalf("unmarshalling body %s: %v", got.Body, err)
	}
	if body["name"] == "Fido" {
		t.Errorf("DataFor() body = %s, want generated data — the createPet fixture applied to updatePet", got.Body)
	}
}

// TestPriorityChain pins the §5a order — spec example, then fixture, then
// generation — by asserting each of the three with the sources above it
// removed. Two sources are available in the first two cases, so each assertion
// is about precedence and not merely about a lone source being used.
func TestPriorityChain(t *testing.T) {
	dir := writeFixtures(t, "createPet.json", `{"body": {"name": "from-fixture"}}`)

	withExample := objectSchema(nil, "name", scalarSchema("string"))
	withExample.Example = yamlNode(t, `{"name": "from-example"}`)
	plain := objectSchema(nil, "name", scalarSchema("string"))

	fixtures := loadFixtures(t, dir)
	empty := loadFixtures(t, t.TempDir())

	cases := []struct {
		name   string
		schema *base.Schema
		fx     *Fixtures
		want   string
	}{
		{"example beats fixture", withExample, fixtures, "from-example"},
		{"fixture beats generation", plain, fixtures, "from-fixture"},
		{"generation is the fallback", plain, empty, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := New(testSeed)
			g.Fixtures = tc.fx

			got := g.DataFor(bodyOp("createPet", schemaProxy(tc.schema)))

			var body map[string]any
			if err := json.Unmarshal(got.Body, &body); err != nil {
				t.Fatalf("unmarshalling body %s: %v", got.Body, err)
			}
			name, _ := body["name"].(string)
			switch {
			case tc.want == "" && (name == "from-example" || name == "from-fixture"):
				t.Errorf("DataFor() body = %s, want generated data", got.Body)
			case tc.want != "" && name != tc.want:
				t.Errorf("DataFor() body = %s, want name %q", got.Body, tc.want)
			}
		})
	}
}

func TestLoadFixturesMissingDirectoryIsAUsageError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")

	_, err := LoadFixtures(missing)

	cerr := wantCode(t, err, clierr.CodeUsage)
	if !strings.Contains(cerr.Message, missing) {
		t.Errorf("error %q does not name the missing directory %s", cerr.Message, missing)
	}
}

func TestLoadFixturesRejectsAFileInPlaceOfADirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixtures.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	_, err := LoadFixtures(path)

	wantCode(t, err, clierr.CodeUsage)
}

// An empty directory is a legitimate state — the flag is set, no operation has
// a fixture yet — and must fall through to generation rather than fail.
func TestEmptyFixturesDirectoryFallsThroughToGeneration(t *testing.T) {
	g := New(testSeed)
	g.Fixtures = loadFixtures(t, t.TempDir())

	got := g.DataFor(bodyOp("createPet", schemaProxy(objectSchema([]string{"name"}, "name", scalarSchema("string")))))

	var body map[string]any
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatalf("unmarshalling body %s: %v", got.Body, err)
	}
	if _, ok := body["name"].(string); !ok {
		t.Errorf("DataFor() body = %s, want generated data with a name", got.Body)
	}
}

// An unset --fixtures flag reaches the loader as an empty path and must not be
// an error: `run` builds the chain the same way with or without the flag.
func TestLoadFixturesAcceptsAnEmptyPath(t *testing.T) {
	fx, err := LoadFixtures("")
	if err != nil {
		t.Fatalf("LoadFixtures(%q): %v", "", err)
	}
	if fx.Len() != 0 {
		t.Errorf("LoadFixtures(%q) loaded %d fixtures, want 0", "", fx.Len())
	}
}

func TestLoadFixturesRejectsInvalidJSON(t *testing.T) {
	dir := writeFixtures(t, "createPet.json", `{"body": {`)

	_, err := LoadFixtures(dir)

	cerr := wantCode(t, err, clierr.CodeUsage)
	if !strings.Contains(cerr.Message, "createPet.json") {
		t.Errorf("error %q does not name the offending file", cerr.Message)
	}
}

func TestLoadFixturesIgnoresNonJSONFiles(t *testing.T) {
	dir := writeFixtures(t,
		"createPet.json", `{"body": {"name": "Fido"}}`,
		"README.md", "not a fixture at all {",
	)

	fx := loadFixtures(t, dir)

	if fx.Len() != 1 {
		t.Errorf("loaded %d fixtures, want only createPet.json", fx.Len())
	}
}

// A fixture is the escape hatch for a whole request, not just its body: real
// ids in the path and a tenant header are exactly the data generation cannot
// invent.
func TestFixtureSuppliesParamsAndHeaders(t *testing.T) {
	dir := writeFixtures(t, "getPet.json", `{
		"params": {"petId": 42, "verbose": true},
		"headers": {"X-Tenant": "acme"}
	}`)
	g := New(testSeed)
	g.Fixtures = loadFixtures(t, dir)

	op := operation.Operation{
		ID:     "getPet",
		Method: "GET",
		Path:   "/pets/{petId}",
		Params: []operation.Param{
			{Name: "petId", In: "path", Required: true, Schema: scalarSchema("integer")},
			{Name: "verbose", In: "query", Schema: scalarSchema("boolean")},
		},
	}

	got := g.DataFor(op)

	if got.Params["petId"] != "42" {
		t.Errorf("Params[petId] = %q, want %q", got.Params["petId"], "42")
	}
	if got.Params["verbose"] != "true" {
		t.Errorf("Params[verbose] = %q, want %q", got.Params["verbose"], "true")
	}
	if got.Headers["X-Tenant"] != "acme" {
		t.Errorf("Headers[X-Tenant] = %q, want %q", got.Headers["X-Tenant"], "acme")
	}
	if len(got.Body) != 0 {
		t.Errorf("Body = %s, want none for an operation with no request body", got.Body)
	}
}

// The chain applies per parameter, not once for the whole request: an example
// on one parameter beats the fixture for that parameter while the fixture still
// supplies the next one.
func TestParamPriorityChain(t *testing.T) {
	dir := writeFixtures(t, "getPet.json", `{"params": {"petId": 42, "status": "sold"}}`)
	g := New(testSeed)
	g.Fixtures = loadFixtures(t, dir)

	petID := scalarSchema("integer")
	petID.Schema().Example = yamlNode(t, `7`)

	op := operation.Operation{
		ID:     "getPet",
		Method: "GET",
		Path:   "/pets/{petId}",
		Params: []operation.Param{
			{Name: "petId", In: "path", Required: true, Schema: petID},
			{Name: "status", In: "query", Schema: scalarSchema("string")},
			{Name: "limit", In: "query", Required: true, Schema: scalarSchema("integer")},
		},
	}

	got := g.DataFor(op)

	if got.Params["petId"] != "7" {
		t.Errorf("Params[petId] = %q, want the schema example %q", got.Params["petId"], "7")
	}
	if got.Params["status"] != "sold" {
		t.Errorf("Params[status] = %q, want the fixture value %q", got.Params["status"], "sold")
	}
	if got.Params["limit"] == "" {
		t.Error("Params[limit] is empty, want a generated value for a required parameter")
	}
}

// A required parameter is always supplied — a request missing one never reaches
// the behaviour under test — while an optional one is left out unless the spec
// or a fixture said what it should be, so a smoke test does not invent filters
// nobody asked for.
func TestOptionalParamsAreOmittedWithoutASource(t *testing.T) {
	g := New(testSeed)

	op := operation.Operation{
		ID:     "listPets",
		Method: "GET",
		Path:   "/pets",
		Params: []operation.Param{
			{Name: "limit", In: "query", Required: true, Schema: scalarSchema("integer")},
			{Name: "cursor", In: "query", Schema: scalarSchema("string")},
		},
	}

	got := g.DataFor(op)

	if got.Params["limit"] == "" {
		t.Error("Params[limit] is empty, want a generated value for a required parameter")
	}
	if _, ok := got.Params["cursor"]; ok {
		t.Errorf("Params[cursor] = %q, want an optional parameter with no source omitted", got.Params["cursor"])
	}
}

// A nil Fixtures is what `run` holds when --fixtures is unset. It must behave
// as an empty set rather than panic.
func TestDataForWithoutFixtures(t *testing.T) {
	g := New(testSeed)

	got := g.DataFor(bodyOp("createPet", schemaProxy(objectSchema([]string{"name"}, "name", scalarSchema("string")))))

	if len(got.Body) == 0 {
		t.Error("DataFor() produced no body with no fixtures loaded, want generated data")
	}
}

// Determinism has to survive the chain: `run` results are only diffable if the
// same seed and the same fixtures produce the same request twice.
func TestDataForIsDeterministic(t *testing.T) {
	op := bodyOp("createPet", schemaProxy(objectSchema([]string{"name"}, "name", scalarSchema("string"), "tag", scalarSchema("string"))))

	first := New(testSeed).DataFor(op)
	second := New(testSeed).DataFor(op)

	if string(first.Body) != string(second.Body) {
		t.Errorf("two runs at seed %d produced %s and %s", testSeed, first.Body, second.Body)
	}
}
