package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// readFixture returns the raw bytes of a testdata file.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}

	return data
}

// loadFixture loads a testdata spec, failing the test if it does not load.
func loadFixture(t *testing.T, name string) *Document {
	t.Helper()

	doc, err := LoadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("LoadFile %s: %v", name, err)
	}

	return doc
}

// rawOperationIDs pulls the operationIds straight out of a Swagger 2.0 JSON
// fixture, so the conversion test compares against the source of truth rather
// than a hand-copied list that can drift from the fixture.
func rawOperationIDs(t *testing.T, data []byte) []string {
	t.Helper()

	var raw struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshalling fixture: %v", err)
	}

	var ids []string
	for _, item := range raw.Paths {
		for _, op := range item {
			if op.OperationID != "" {
				ids = append(ids, op.OperationID)
			}
		}
	}
	sort.Strings(ids)

	return ids
}

func TestIsSwagger2DetectsTopLevelVersionKey(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    bool
	}{
		{name: "swagger 2.0 json", fixture: "petstore-2.0.json", want: true},
		{name: "swagger 2.0 yaml", fixture: "swagger2.yaml", want: true},
		{name: "openapi 3.0 yaml", fixture: "petstore-3.0.yaml", want: false},
		{name: "openapi 3.1 json", fixture: "petstore-3.1.json", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSwagger2(readFixture(t, tt.fixture)); got != tt.want {
				t.Errorf("isSwagger2(%s) = %v, want %v", tt.fixture, got, tt.want)
			}
		})
	}
}

// The Phase 1 requirement is that 2.0 conversion is verified against a real
// spec: every operation the author wrote must still be addressable afterwards.
func TestLoadConvertsSwagger2PetstoreKeepingEveryOperation(t *testing.T) {
	doc := loadFixture(t, "petstore-2.0.json")

	if !strings.HasPrefix(doc.Version, "3.") {
		t.Errorf("Version = %q, want a 3.x version", doc.Version)
	}
	if doc.ConvertedFrom != "2.0" {
		t.Errorf("ConvertedFrom = %q, want %q", doc.ConvertedFrom, "2.0")
	}

	want := rawOperationIDs(t, readFixture(t, "petstore-2.0.json"))
	got := operationIDs(t, doc)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("operationIds = %v, want %v", got, want)
	}
}

func TestLoadRecordsNoConversionForOpenAPI3(t *testing.T) {
	doc := loadFixture(t, "petstore-3.0.yaml")

	if doc.ConvertedFrom != "" {
		t.Errorf("ConvertedFrom = %q, want empty for a 3.x spec", doc.ConvertedFrom)
	}
}

// host + basePath + schemes are three separate 2.0 fields describing one base
// URL. Request construction reads exactly one server, so they must collapse.
func TestConversionCollapsesHostBasePathAndSchemesIntoOneServer(t *testing.T) {
	doc := loadFixture(t, "swagger2-apikey.json")

	if got := len(doc.Model.Servers); got != 1 {
		t.Fatalf("server count = %d, want 1", got)
	}
	if got, want := doc.Model.Servers[0].URL, "https://api.example.com/v1"; got != want {
		t.Errorf("server URL = %q, want %q", got, want)
	}
}

// Auth mapping later binds credentials by scheme name, location and header
// name, so all three have to survive the move to components.securitySchemes.
func TestConversionPreservesSecurityDefinitions(t *testing.T) {
	doc := loadFixture(t, "swagger2-apikey.json")

	if doc.Model.Components == nil || doc.Model.Components.SecuritySchemes == nil {
		t.Fatal("converted document has no components.securitySchemes")
	}

	scheme, ok := doc.Model.Components.SecuritySchemes.Get("apiKey")
	if !ok {
		t.Fatalf("securitySchemes has no %q entry", "apiKey")
	}
	if got, want := scheme.Type, "apiKey"; got != want {
		t.Errorf("scheme type = %q, want %q", got, want)
	}
	if got, want := scheme.In, "header"; got != want {
		t.Errorf("scheme in = %q, want %q", got, want)
	}
	if got, want := scheme.Name, "X-API-Key"; got != want {
		t.Errorf("scheme name = %q, want %q", got, want)
	}
}

// #/definitions/Pet has to become #/components/schemas/Pet and still resolve
// from the operation that referenced it, with required fields intact.
func TestConversionResolvesDefinitionRefs(t *testing.T) {
	doc := loadFixture(t, "swagger2-apikey.json")

	pathItem, ok := doc.Model.Paths.PathItems.Get("/pets/{petId}")
	if !ok {
		t.Fatal("converted document has no /pets/{petId} path item")
	}
	response, ok := pathItem.Get.Responses.Codes.Get("200")
	if !ok {
		t.Fatal("getPet has no 200 response")
	}
	media, ok := response.Content.Get("application/json")
	if !ok {
		t.Fatal("getPet 200 response has no application/json content")
	}

	schema := media.Schema.Schema()
	if schema == nil {
		t.Fatal("getPet 200 schema did not resolve")
	}

	got := append([]string(nil), schema.Required...)
	sort.Strings(got)
	if strings.Join(got, ",") != "id,name" {
		t.Errorf("required = %v, want [id name]", got)
	}
}

// The YAML fixture describes the same API as the JSON one. If they diverge, a
// YAML→JSON bridge that mangles mappings is the likely cause, and every
// downstream command would silently see a different API depending on the file
// extension it was handed.
func TestConversionIsIndifferentToInputFormat(t *testing.T) {
	fromJSON := loadFixture(t, "swagger2-apikey.json")
	fromYAML := loadFixture(t, "swagger2.yaml")

	if got, want := operationIDs(t, fromYAML), operationIDs(t, fromJSON); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("operationIds = %v, want %v", got, want)
	}

	if got := len(fromYAML.Model.Servers); got != 1 {
		t.Fatalf("server count = %d, want 1", got)
	}
	if got, want := fromYAML.Model.Servers[0].URL, fromJSON.Model.Servers[0].URL; got != want {
		t.Errorf("server URL = %q, want %q", got, want)
	}

	jsonScheme, ok := fromJSON.Model.Components.SecuritySchemes.Get("apiKey")
	if !ok {
		t.Fatal("JSON fixture lost its apiKey scheme")
	}
	yamlScheme, ok := fromYAML.Model.Components.SecuritySchemes.Get("apiKey")
	if !ok {
		t.Fatal("YAML fixture lost its apiKey scheme")
	}
	if yamlScheme.Type != jsonScheme.Type || yamlScheme.In != jsonScheme.In || yamlScheme.Name != jsonScheme.Name {
		t.Errorf("scheme = %+v, want %+v", yamlScheme, jsonScheme)
	}
}
