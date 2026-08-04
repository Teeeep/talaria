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

func TestTSVEmitsOneRowPerRowWithHostileCells(t *testing.T) {
	var out bytes.Buffer

	// A spec summary is attacker-controlled text that reaches a TSV cell. Left
	// raw, this row becomes two lines with two and three columns, and `cut -f3`
	// returns garbage with no way for the caller to notice.
	err := New(FormatTSV, &out).Render(Payload{Table: Table{
		Rows: [][]string{
			{"getUser", "GET", "line one\nline two\twith tab"},
			{"createUser", "POST", "plain"},
		},
	}})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	rows := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(rows) != 2 {
		t.Fatalf("TSV emitted %d rows for a 2-row table:\n%q", len(rows), out.String())
	}

	want := len(strings.Split(rows[0], "\t"))
	for i, row := range rows {
		if got := len(strings.Split(row, "\t")); got != want {
			t.Errorf("row %d has %d columns, row 0 has %d: %q", i, got, want, row)
		}
	}
	if got, want := rows[0], "getUser\tGET\t"+`line one\nline two\twith tab`; got != want {
		t.Errorf("hostile row = %q, want %q", got, want)
	}
}

func TestPrettyStaysColumnAlignedWithAHostileCell(t *testing.T) {
	var out bytes.Buffer

	// text/tabwriter reads an embedded tab as a cell terminator and
	// re-partitions the whole column block, so one hostile summary misaligns
	// every row around it.
	err := New(FormatPretty, &out).Render(Payload{Table: Table{
		Headers: []string{"OPERATION", "SUMMARY"},
		Rows: [][]string{
			{"getUser", "line one\nline two\twith tab"},
			{"createUser", "plain"},
		},
	}})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	got := lines(out.String())
	if len(got) != 3 {
		t.Fatalf("pretty emitted %d lines for a header and 2 rows:\n%s", len(got), out.String())
	}

	second := []string{"SUMMARY", `line one\nline two\twith tab`, "plain"}
	want := strings.Index(got[0], second[0])
	for i, line := range got {
		at := strings.Index(line, second[i])
		if at < 0 {
			t.Fatalf("line %d %q does not contain its second cell %q", i, line, second[i])
		}
		if at != want {
			t.Errorf("line %d starts its second column at %d, line 0 starts at %d:\n%s", i, at, want, out.String())
		}
	}
}

// lines splits rendered output into its non-empty lines.
func lines(s string) []string {
	trimmed := strings.TrimRight(s, "\n")
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "\n")
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

// TestBothRenderersPrintALineVerbatim is the whole point of Payload.Lines: a
// curl command is text the caller pastes into a shell, not a cell of untrusted
// text, and escapeCell doubles the backslashes and folds the tabs of a cell.
// Both text renderers have to agree, because either can be the default.
func TestBothRenderersPrintALineVerbatim(t *testing.T) {
	line := `curl -q -s -X POST --data-raw '{"p":"a\b","q":"x` + "\t" + `y"}' https://api.example.com/pets`

	for _, f := range []Format{FormatPretty, FormatTSV} {
		var out bytes.Buffer
		if err := New(f, &out).Render(Payload{Lines: []string{line}}); err != nil {
			t.Fatalf("%s Render returned error: %v", f, err)
		}

		if got := out.String(); got != line+"\n" {
			t.Errorf("%s printed %q, want the line unchanged: %q", f, got, line+"\n")
		}
	}
}

// TestALineIsPrintedBeforeTheTable pins the order the call envelope depends on:
// the request and the curl come first, the status summary after them.
func TestALineIsPrintedBeforeTheTable(t *testing.T) {
	for _, f := range []Format{FormatPretty, FormatTSV} {
		var out bytes.Buffer
		err := New(f, &out).Render(Payload{
			Lines: []string{"first", "second"},
			Table: Table{Rows: [][]string{{"third"}}},
		})
		if err != nil {
			t.Fatalf("%s Render returned error: %v", f, err)
		}

		if got := out.String(); got != "first\nsecond\nthird\n" {
			t.Errorf("%s printed %q, want lines before rows", f, got)
		}
	}
}

// TestPrettyPrintsLinesRatherThanFallingBackToJSON: a payload whose only human
// shape is a line still has a human shape.
func TestPrettyPrintsLinesRatherThanFallingBackToJSON(t *testing.T) {
	var out bytes.Buffer

	p := Payload{Data: map[string]any{"status": 200}, Lines: []string{"200 OK"}}
	if err := New(FormatPretty, &out).Render(p); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	if got := out.String(); got != "200 OK\n" {
		t.Errorf("pretty output = %q, want the line", got)
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
