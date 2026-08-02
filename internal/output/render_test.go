package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONRenderCarriesSchemaVersion(t *testing.T) {
	var out bytes.Buffer

	r := New(FormatJSON, &out)
	if err := r.Render(Payload{Data: map[string]any{"status": 200}}); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("JSON output is not valid JSON: %v\n%s", err, out.String())
	}

	if got["schema"] != "talaria/v1" {
		t.Errorf("top-level schema field = %v, want %q", got["schema"], "talaria/v1")
	}
	if got["status"] != float64(200) {
		t.Errorf("payload field status = %v, want 200 (payload fields must stay top-level)", got["status"])
	}
}

func TestJSONRenderWrapsNonObjectPayloads(t *testing.T) {
	var out bytes.Buffer

	r := New(FormatJSON, &out)
	if err := r.Render(Payload{Data: []string{"a", "b"}}); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("JSON output is not valid JSON: %v\n%s", err, out.String())
	}

	if got["schema"] != "talaria/v1" {
		t.Errorf("top-level schema field = %v, want %q", got["schema"], "talaria/v1")
	}
	if _, ok := got["data"].([]any); !ok {
		t.Errorf("non-object payload should land under \"data\", got %#v", got)
	}
}

func TestJSONRenderIsDeterministic(t *testing.T) {
	payload := Payload{Data: map[string]any{
		"zeta":  1,
		"alpha": 2,
		"mid":   map[string]any{"y": "b", "x": "a"},
	}}

	var first, second bytes.Buffer
	if err := New(FormatJSON, &first).Render(payload); err != nil {
		t.Fatalf("first Render returned error: %v", err)
	}
	if err := New(FormatJSON, &second).Render(payload); err != nil {
		t.Fatalf("second Render returned error: %v", err)
	}

	if first.String() != second.String() {
		t.Errorf("JSON output is not deterministic:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
}

func TestTSVRendersTabSeparatedRows(t *testing.T) {
	var out bytes.Buffer

	r := New(FormatTSV, &out)
	err := r.Render(Payload{Table: Table{
		Headers: []string{"OPERATION", "METHOD", "PATH"},
		Rows: [][]string{
			{"getUser", "GET", "/users/{id}"},
			{"createUser", "POST", "/users"},
		},
	}})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	want := "getUser\tGET\t/users/{id}\ncreateUser\tPOST\t/users\n"
	if got := out.String(); got != want {
		t.Errorf("TSV output = %q, want %q", got, want)
	}
}

func TestPrettyRendersHeadersAndRows(t *testing.T) {
	var out bytes.Buffer

	r := New(FormatPretty, &out)
	err := r.Render(Payload{Table: Table{
		Headers: []string{"OPERATION", "METHOD"},
		Rows:    [][]string{{"getUser", "GET"}},
	}})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	got := out.String()
	for _, want := range []string{"OPERATION", "METHOD", "getUser", "GET"} {
		if !strings.Contains(got, want) {
			t.Errorf("pretty output %q is missing %q", got, want)
		}
	}
	if lines := strings.Count(got, "\n"); lines != 2 {
		t.Errorf("pretty output has %d lines, want 2 (header + one row):\n%s", lines, got)
	}
}

func TestPrettyFallsBackToJSONWithoutATable(t *testing.T) {
	var out bytes.Buffer

	r := New(FormatPretty, &out)
	if err := r.Render(Payload{Data: map[string]any{"status": 200}}); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("tableless pretty output should be JSON, got: %s", out.String())
	}
	if got["schema"] != "talaria/v1" {
		t.Errorf("fallback output is missing the schema field: %#v", got)
	}
}

func TestParseFormat(t *testing.T) {
	for _, name := range []string{"json", "pretty", "tsv"} {
		got, err := ParseFormat(name)
		if err != nil {
			t.Errorf("ParseFormat(%q) returned error: %v", name, err)
		}
		if string(got) != name {
			t.Errorf("ParseFormat(%q) = %q, want %q", name, got, name)
		}
	}
}

func TestParseFormatRejectsUnknownValueAndNamesValidOnes(t *testing.T) {
	_, err := ParseFormat("xml")
	if err == nil {
		t.Fatal("ParseFormat(\"xml\") returned no error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "xml") {
		t.Errorf("error %q does not name the offending value", msg)
	}
	for _, valid := range []string{"json", "pretty", "tsv"} {
		if !strings.Contains(msg, valid) {
			t.Errorf("error %q does not list valid value %q", msg, valid)
		}
	}
}

func TestFormatsListsEveryValidValue(t *testing.T) {
	// Callers rendering a usage error need the valid values as data, not prose
	// dug out of ParseFormat's message.
	if got := strings.Join(Formats(), ","); got != "json,pretty,tsv" {
		t.Errorf("Formats() = %v, want [json pretty tsv]", Formats())
	}

	// The slice must be a copy; a caller sorting or truncating it must not
	// corrupt the package's own list.
	Formats()[0] = "mutated"
	if got := Formats()[0]; got != "json" {
		t.Errorf("Formats() returned an aliased slice: first value is now %q", got)
	}
}

func TestResolveDefaultsOnTTY(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		isTTY    bool
		want     Format
	}{
		{"piped defaults to json", "", false, FormatJSON},
		{"terminal defaults to pretty", "", true, FormatPretty},
		{"explicit wins over terminal", "json", true, FormatJSON},
		{"explicit wins over pipe", "pretty", false, FormatPretty},
		{"explicit tsv on a terminal", "tsv", true, FormatTSV},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.explicit, tt.isTTY)
			if err != nil {
				t.Fatalf("Resolve(%q, %v) returned error: %v", tt.explicit, tt.isTTY, err)
			}
			if got != tt.want {
				t.Errorf("Resolve(%q, %v) = %q, want %q", tt.explicit, tt.isTTY, got, tt.want)
			}
		})
	}
}

func TestResolveRejectsUnknownFormat(t *testing.T) {
	if _, err := Resolve("xml", true); err == nil {
		t.Error("Resolve(\"xml\", true) returned no error")
	}
}
