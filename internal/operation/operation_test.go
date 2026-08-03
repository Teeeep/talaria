package operation

import (
	"path/filepath"
	"testing"

	"github.com/Teeeep/talaria/internal/spec"
)

// loadOperations extracts the fixture every test works from. The fixture is a
// petstore with the awkward parts a real spec has — path-item parameters, a
// document-level security block, an operation that overrides it and one that
// switches it off.
func loadOperations(t *testing.T) []Operation {
	t.Helper()

	doc, err := spec.LoadFile(filepath.Join("testdata", "petstore-3.0.yaml"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	return Extract(doc)
}

func find(t *testing.T, ops []Operation, method, path string) Operation {
	t.Helper()

	for _, op := range ops {
		if op.Method == method && op.Path == path {
			return op
		}
	}
	t.Fatalf("no %s %s among %d extracted operations", method, path, len(ops))

	return Operation{}
}

func param(t *testing.T, op Operation, name, in string) Param {
	t.Helper()

	for _, p := range op.Params {
		if p.Name == name && p.In == in {
			return p
		}
	}
	t.Fatalf("%s %s has no %s parameter %q, got %v", op.Method, op.Path, in, name, op.Params)

	return Param{}
}

func TestExtractReturnsOneOperationPerMethodPathPair(t *testing.T) {
	ops := loadOperations(t)

	want := []struct {
		method  string
		path    string
		id      string
		summary string
	}{
		{"GET", "/pets", "listPets", "List all pets"},
		{"POST", "/pets", "createPet", "Create a pet"},
		{"HEAD", "/pets", "headPets", "Pet collection headers"},
		{"OPTIONS", "/pets", "optionsPets", "Pet collection options"},
		{"GET", "/pets/{petId}", "getPet", "Get one pet"},
	}

	if len(ops) != len(want) {
		t.Fatalf("extracted %d operations, want %d: %v", len(ops), len(want), ops)
	}

	for _, w := range want {
		op := find(t, ops, w.method, w.path)
		if op.ID != w.id {
			t.Errorf("%s %s ID = %q, want %q", w.method, w.path, op.ID, w.id)
		}
		if op.Summary != w.summary {
			t.Errorf("%s %s Summary = %q, want %q", w.method, w.path, op.Summary, w.summary)
		}
		if len(op.Tags) != 1 || op.Tags[0] != "pets" {
			t.Errorf("%s %s Tags = %v, want [pets]", w.method, w.path, op.Tags)
		}
	}
}

// A path-item parameter applies to every operation under that path, and an
// operation-level entry with the same (name, in) replaces it rather than
// appearing twice.
func TestExtractMergesPathItemParametersWithOperationWinning(t *testing.T) {
	op := find(t, loadOperations(t), "GET", "/pets")

	if len(op.Params) != 2 {
		t.Fatalf("Params = %v, want 2 (the merged limit plus X-Request-Id)", op.Params)
	}

	limit := param(t, op, "limit", "query")
	if !limit.Required {
		t.Error("limit.Required = false, want true — the operation-level entry should win")
	}

	if reqID := param(t, op, "X-Request-Id", "header"); reqID.Required {
		t.Error("X-Request-Id.Required = true, want false")
	}
}

func TestExtractCarriesParameterLocationRequirednessAndSchema(t *testing.T) {
	op := find(t, loadOperations(t), "GET", "/pets/{petId}")

	petID := param(t, op, "petId", "path")
	if !petID.Required {
		t.Error("petId.Required = false, want true")
	}
	if petID.Schema == nil {
		t.Fatal("petId.Schema is nil, want the schema from the spec")
	}
	if types := petID.Schema.Schema().Type; len(types) != 1 || types[0] != "string" {
		t.Errorf("petId schema type = %v, want [string]", types)
	}

	if session := param(t, op, "session", "cookie"); session.Required {
		t.Error("session.Required = true, want false")
	}
}

func TestExtractExposesRequestBodyMediaTypeAndSchema(t *testing.T) {
	ops := loadOperations(t)

	post := find(t, ops, "POST", "/pets")
	if post.RequestBody == nil {
		t.Fatal("POST /pets RequestBody is nil, want the Pet body")
	}
	if !post.RequestBody.Required {
		t.Error("RequestBody.Required = false, want true")
	}
	if len(post.RequestBody.Content) != 1 {
		t.Fatalf("RequestBody.Content = %v, want one media type", post.RequestBody.Content)
	}

	media := post.RequestBody.Content[0]
	if media.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want application/json", media.ContentType)
	}
	if media.Schema == nil {
		t.Fatal("media.Schema is nil, want the resolved Pet schema")
	}
	// Reading a property proves the $ref resolved rather than being carried
	// as an unresolved reference.
	if _, ok := media.Schema.Schema().Properties.Get("name"); !ok {
		t.Error("Pet schema has no `name` property, want the $ref resolved")
	}

	if get := find(t, ops, "GET", "/pets"); get.RequestBody != nil {
		t.Errorf("GET /pets RequestBody = %v, want nil", get.RequestBody)
	}
}

func TestExtractKeysResponsesByStatusCode(t *testing.T) {
	op := find(t, loadOperations(t), "GET", "/pets")

	var codes []string
	for _, r := range op.Responses {
		codes = append(codes, r.Status)
	}
	if len(codes) != 2 || codes[0] != "200" || codes[1] != StatusDefault {
		t.Fatalf("response statuses = %v, want [200 default]", codes)
	}

	ok := op.ResponseFor("200")
	if ok == nil {
		t.Fatal(`ResponseFor("200") = nil, want the success response`)
	}
	if len(ok.Content) != 1 || ok.Content[0].ContentType != "application/json" {
		t.Errorf("200 content = %v, want one application/json entry", ok.Content)
	}
	if ok.Description != "A list of pets" {
		t.Errorf("200 Description = %q, want %q", ok.Description, "A list of pets")
	}

	fallback := op.ResponseFor(StatusDefault)
	if fallback == nil {
		t.Fatal(`ResponseFor("default") = nil, want the fallback response`)
	}
	if len(fallback.Content) != 1 || fallback.Content[0].ContentType != "application/json" {
		t.Errorf("default content = %v, want one application/json entry", fallback.Content)
	}

	if missing := op.ResponseFor("404"); missing != nil {
		t.Errorf(`ResponseFor("404") = %v, want nil`, missing)
	}
}

func TestExtractResolvesSecurityRequirements(t *testing.T) {
	ops := loadOperations(t)

	// No operation-level block: the document-level requirement applies.
	get := find(t, ops, "GET", "/pets")
	if len(get.Security) != 1 || len(get.Security[0].Schemes) != 1 {
		t.Fatalf("GET /pets Security = %v, want the document-level apiKey requirement", get.Security)
	}
	if name := get.Security[0].Schemes[0].Name; name != "apiKey" {
		t.Errorf("GET /pets scheme = %q, want apiKey", name)
	}

	// An operation-level block replaces the document-level one entirely.
	post := find(t, ops, "POST", "/pets")
	if len(post.Security) != 1 || len(post.Security[0].Schemes) != 1 {
		t.Fatalf("POST /pets Security = %v, want the oauth2 requirement only", post.Security)
	}
	scheme := post.Security[0].Schemes[0]
	if scheme.Name != "oauth2" {
		t.Errorf("POST /pets scheme = %q, want oauth2", scheme.Name)
	}
	if len(scheme.Scopes) != 1 || scheme.Scopes[0] != "pets:write" {
		t.Errorf("POST /pets scopes = %v, want [pets:write]", scheme.Scopes)
	}

	// `security: []` is how a spec says this endpoint needs no auth at all.
	if opts := find(t, ops, "OPTIONS", "/pets"); len(opts.Security) != 0 {
		t.Errorf("OPTIONS /pets Security = %v, want none", opts.Security)
	}
}

// DESIGN.md §4: read-only means GET/HEAD/OPTIONS; everything else needs
// --allow-mutations, so anything not on that list counts as a mutation.
func TestIsMutation(t *testing.T) {
	tests := map[string]bool{
		"GET":     false,
		"HEAD":    false,
		"OPTIONS": false,
		"POST":    true,
		"PUT":     true,
		"PATCH":   true,
		"DELETE":  true,
	}

	for method, want := range tests {
		if got := (Operation{Method: method}).IsMutation(); got != want {
			t.Errorf("%s IsMutation() = %t, want %t", method, got, want)
		}
	}
}
