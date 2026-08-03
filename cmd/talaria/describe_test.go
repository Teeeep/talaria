package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/spec"
)

// describeSchemaJSON mirrors output.SchemaNode as an agent would parse it: a
// tree, not a rendered string.
type describeSchemaJSON struct {
	Name        string               `json:"name"`
	Type        string               `json:"type"`
	Required    bool                 `json:"required"`
	Description string               `json:"description"`
	Enum        []string             `json:"enum"`
	Properties  []describeSchemaJSON `json:"properties"`
	Circular    bool                 `json:"circular"`
	Truncated   bool                 `json:"truncated"`
}

type describeContentJSON struct {
	ContentType string              `json:"content_type"`
	Schema      *describeSchemaJSON `json:"schema"`
}

// describeJSON is the JSON shape describe emits.
type describeJSON struct {
	Schema string `json:"schema"`
	ID     string `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Params []struct {
		Name        string              `json:"name"`
		In          string              `json:"in"`
		Required    bool                `json:"required"`
		Description string              `json:"description"`
		Schema      *describeSchemaJSON `json:"schema"`
	} `json:"params"`
	RequestBody *struct {
		Required bool                  `json:"required"`
		Content  []describeContentJSON `json:"content"`
	} `json:"request_body"`
	Responses []struct {
		Status      string                `json:"status"`
		Description string                `json:"description"`
		Content     []describeContentJSON `json:"content"`
	} `json:"responses"`
}

// runDescribe runs `describe` with args, with the env var fallback out of the way.
func runDescribe(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv(spec.EnvSpec, "")

	var out, errOut strings.Builder
	code = run(append([]string{"describe"}, args...), &out, &errOut)

	return code, out.String(), errOut.String()
}

// property finds a property by name in a schema tree node.
func property(t *testing.T, node *describeSchemaJSON, name string) describeSchemaJSON {
	t.Helper()
	if node == nil {
		t.Fatalf("looking for property %q in a nil schema", name)
	}
	for _, p := range node.Properties {
		if p.Name == name {
			return p
		}
	}

	t.Fatalf("schema has no property %q, got %+v", name, node.Properties)

	return describeSchemaJSON{}
}

func TestDescribePrettyShowsParamsBodyAndResponses(t *testing.T) {
	code, stdout, stderr := runDescribe(t, "testdata/describe.yaml", "getPet", "--output", "pretty")
	if code != 0 {
		t.Fatalf("describe = %d, want 0; stderr: %s", code, stderr)
	}

	for _, want := range []string{
		"GET",
		"/pets/{petId}",
		"getPet",
		"Fetch a single pet by its identifier.",
		// The path-item parameter is merged in, and its location is shown.
		"petId*: (string) [path] The pet's identifier",
		"verbose: (boolean) [query] Include the pet's history",
		// Response schema, rendered compactly rather than dumped.
		"200",
		"application/json",
		"id*: (string) Unique identifier",
		"status: (string) one of: available, pending, sold",
		"tags: ([]string)",
		"owner: (object)",
		"  name: (string) Who owns the pet",
		// A response with no body still gets a line.
		"404",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("describe pretty output is missing %q:\n%s", want, stdout)
		}
	}

	// Progressive disclosure: no raw JSON Schema keywords leak through.
	for _, unwanted := range []string{`"$ref"`, "#/components/schemas/Pet", `"properties"`} {
		if strings.Contains(stdout, unwanted) {
			t.Errorf("describe pretty output leaked raw schema %q:\n%s", unwanted, stdout)
		}
	}
}

func TestDescribePrettyShowsTheRequestBody(t *testing.T) {
	code, stdout, stderr := runDescribe(t, "testdata/describe.yaml", "replacePet", "--output", "pretty")
	if code != 0 {
		t.Fatalf("describe replacePet = %d, want 0; stderr: %s", code, stderr)
	}

	for _, want := range []string{"application/json", "required", "name*: (string)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("describe pretty output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestDescribeJSONReturnsStructuredParamsAndSchemas(t *testing.T) {
	code, stdout, stderr := runDescribe(t, "testdata/describe.yaml", "getPet", "--output", "json")
	if code != 0 {
		t.Fatalf("describe --output json = %d, want 0; stderr: %s", code, stderr)
	}

	var payload describeJSON
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot: %s", err, stdout)
	}

	if payload.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", payload.Schema)
	}
	if payload.ID != "getPet" || payload.Method != "GET" || payload.Path != "/pets/{petId}" {
		t.Errorf("got id %q method %q path %q, want getPet GET /pets/{petId}", payload.ID, payload.Method, payload.Path)
	}

	if len(payload.Params) != 2 {
		t.Fatalf("got %d params, want 2 (the path-item one merged in): %s", len(payload.Params), stdout)
	}
	petID := payload.Params[0]
	if petID.Name != "petId" || petID.In != "path" || !petID.Required {
		t.Errorf("first param = %+v, want a required path param named petId", petID)
	}
	if petID.Schema == nil || petID.Schema.Type != "string" {
		t.Errorf("petId schema = %+v, want type string", petID.Schema)
	}

	if len(payload.Responses) != 2 {
		t.Fatalf("got %d responses, want 2: %s", len(payload.Responses), stdout)
	}
	ok := payload.Responses[0]
	if ok.Status != "200" || len(ok.Content) != 1 {
		t.Fatalf("first response = %+v, want 200 with one content type", ok)
	}
	if ok.Content[0].ContentType != "application/json" {
		t.Errorf("content type = %q, want application/json", ok.Content[0].ContentType)
	}

	// The tree, not a pretty string: an agent walks these fields.
	id := property(t, ok.Content[0].Schema, "id")
	if id.Type != "string" || !id.Required || id.Description != "Unique identifier" {
		t.Errorf("id property = %+v, want a required string with a description", id)
	}
	if status := property(t, ok.Content[0].Schema, "status"); strings.Join(status.Enum, ",") != "available,pending,sold" {
		t.Errorf("status enum = %v, want the three declared values", status.Enum)
	}
	if tags := property(t, ok.Content[0].Schema, "tags"); tags.Type != "[]string" {
		t.Errorf("tags type = %q, want []string", tags.Type)
	}
	owner := property(t, ok.Content[0].Schema, "owner")
	if owner.Type != "object" || len(owner.Properties) != 1 {
		t.Errorf("owner = %+v, want a nested object with one property", owner)
	}

	// A response with no declared body renders an empty content list, not null.
	if payload.Responses[1].Content == nil {
		t.Errorf("the 404 content is null, want an empty array: %s", stdout)
	}
}

func TestDescribeJSONReturnsTheRequestBody(t *testing.T) {
	code, stdout, stderr := runDescribe(t, "testdata/describe.yaml", "replacePet", "--output", "json")
	if code != 0 {
		t.Fatalf("describe replacePet --output json = %d, want 0; stderr: %s", code, stderr)
	}

	var payload describeJSON
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot: %s", err, stdout)
	}

	if payload.RequestBody == nil {
		t.Fatalf("request_body is absent: %s", stdout)
	}
	if !payload.RequestBody.Required {
		t.Errorf("request_body.required = false, want true")
	}
	if len(payload.RequestBody.Content) != 1 {
		t.Fatalf("request body has %d content types, want 1", len(payload.RequestBody.Content))
	}
	if name := property(t, payload.RequestBody.Content[0].Schema, "name"); !name.Required {
		t.Errorf("name property = %+v, want required", name)
	}
}

func TestDescribeUnknownOperationExitsTwoWithSuggestions(t *testing.T) {
	code, stdout, stderr := runDescribe(t, "testdata/describe.yaml", "getPets", "--output", "json")
	if code != 2 {
		t.Fatalf("describe getPets = %d, want 2; stderr: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("a failed describe wrote %q to stdout, want nothing", stdout)
	}
	if !strings.Contains(stderr, `"valid_alternatives"`) || !strings.Contains(stderr, "getPet") {
		t.Errorf("stderr does not suggest the close id: %s", stderr)
	}
}

func TestDescribeReadsTheSpecFromTheFlag(t *testing.T) {
	// One positional argument is the operationId, not the spec.
	code, stdout, stderr := runDescribe(t, "--spec", "testdata/describe.yaml", "getPet", "--output", "json")
	if code != 0 {
		t.Fatalf("describe --spec = %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"id":"getPet"`) {
		t.Errorf("describe --spec did not describe getPet:\n%s", stdout)
	}
}

func TestDescribeExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no operationId", []string{"--output", "json"}, 2},
		{"no spec anywhere", []string{"getPet", "--output", "json"}, 2},
		{"unparseable spec", []string{"testdata/malformed.yaml", "getPet", "--output", "json"}, 3},
		{"too many arguments", []string{"a.yaml", "getPet", "extra"}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runDescribe(t, tt.args...)
			if code != tt.want {
				t.Fatalf("describe %v = %d, want %d; stderr: %s", tt.args, code, tt.want, stderr)
			}
			if stdout != "" {
				t.Errorf("a failed describe wrote %q to stdout, want nothing", stdout)
			}
			if !strings.HasPrefix(stderr, `{"schema":"talaria/v1"`) {
				t.Errorf("stderr is not the structured envelope: %s", stderr)
			}
		})
	}
}
