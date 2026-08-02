package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/spec"
)

// listJSON is the JSON shape list emits: the versioned envelope with the
// operations at the top level.
type listJSON struct {
	Schema     string `json:"schema"`
	Operations []struct {
		ID      string   `json:"id"`
		Method  string   `json:"method"`
		Path    string   `json:"path"`
		Tags    []string `json:"tags"`
		Summary string   `json:"summary"`
	} `json:"operations"`
}

// runList runs `list` with args, asserting only that the env var fallback is
// out of the way; every test here supplies its spec explicitly.
func runList(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv(spec.EnvSpec, "")

	var out, errOut strings.Builder
	code = run(append([]string{"list"}, args...), &out, &errOut)

	return code, out.String(), errOut.String()
}

// lines splits rendered output into its non-empty lines.
func lines(s string) []string {
	trimmed := strings.TrimRight(s, "\n")
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "\n")
}

func TestListPrettyEmitsOneLinePerOperation(t *testing.T) {
	code, stdout, stderr := runList(t, "testdata/petstore.yaml", "--output", "pretty")
	if code != 0 {
		t.Fatalf("list = %d, want 0; stderr: %s", code, stderr)
	}

	got := lines(stdout)
	// Four operations in the fixture, and no header line: §3.2 asks for one
	// compact line per operation, not a table with a caption.
	if len(got) != 4 {
		t.Fatalf("list pretty emitted %d lines, want 4:\n%s", len(got), stdout)
	}

	for _, want := range []string{"GET", "/pets", "listPets"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("first line %q does not contain %q", got[0], want)
		}
	}

	// The operation with no operationId is still addressable, by its
	// synthesised id.
	if !strings.Contains(stdout, "getHealth") {
		t.Errorf("list pretty output omits the synthesised id getHealth:\n%s", stdout)
	}
}

func TestListPrettyTruncatesLongSummaries(t *testing.T) {
	_, stdout, _ := runList(t, "testdata/petstore.yaml", "--output", "pretty")

	for _, line := range lines(stdout) {
		if len([]rune(line)) > maxPrettyWidth {
			t.Errorf("pretty line is %d runes, want <= %d:\n%s", len([]rune(line)), maxPrettyWidth, line)
		}
	}

	// Truncated, not dropped: the start of the long summary survives.
	if !strings.Contains(stdout, "Create a pet in the store") {
		t.Errorf("the long summary was dropped rather than truncated:\n%s", stdout)
	}
	if strings.Contains(stdout, "a tag and a home") {
		t.Errorf("the long summary was not truncated:\n%s", stdout)
	}
}

func TestListJSONEmitsTheEnvelopeAndOperations(t *testing.T) {
	code, stdout, stderr := runList(t, "testdata/petstore.yaml", "--output", "json")
	if code != 0 {
		t.Fatalf("list = %d, want 0; stderr: %s", code, stderr)
	}

	var payload listJSON
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot: %s", err, stdout)
	}

	if payload.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", payload.Schema)
	}
	if len(payload.Operations) != 4 {
		t.Fatalf("got %d operations, want 4: %s", len(payload.Operations), stdout)
	}

	first := payload.Operations[0]
	if first.ID != "listPets" || first.Method != "GET" || first.Path != "/pets" {
		t.Errorf("first operation = %+v, want id listPets, method GET, path /pets", first)
	}
	if first.Summary != "List all pets" {
		t.Errorf("first operation summary = %q, want %q", first.Summary, "List all pets")
	}
	if strings.Join(first.Tags, ",") != "pets" {
		t.Errorf("first operation tags = %v, want [pets]", first.Tags)
	}

	// Summaries are untruncated in JSON: only the pretty line has a width budget.
	for _, op := range payload.Operations {
		if op.ID == "createPet" && !strings.HasSuffix(op.Summary, "a tag and a home") {
			t.Errorf("createPet summary = %q, want the full text", op.Summary)
		}
	}
}

func TestListTSVEmitsOneRowPerOperation(t *testing.T) {
	code, stdout, stderr := runList(t, "testdata/petstore.yaml", "--output", "tsv")
	if code != 0 {
		t.Fatalf("list = %d, want 0; stderr: %s", code, stderr)
	}

	got := lines(stdout)
	if len(got) != 4 {
		t.Fatalf("list tsv emitted %d rows, want 4 and no header:\n%s", len(got), stdout)
	}

	fields := strings.Split(got[0], "\t")
	if len(fields) != 4 {
		t.Fatalf("row has %d tab-separated fields, want 4: %q", len(fields), got[0])
	}
	if fields[0] != "GET" || fields[1] != "/pets" || fields[2] != "listPets" {
		t.Errorf("row = %q, want method, path, id in the first three fields", got[0])
	}
}

func TestListFiltersByTag(t *testing.T) {
	code, stdout, stderr := runList(t, "testdata/petstore.yaml", "--tag", "pets", "--output", "tsv")
	if code != 0 {
		t.Fatalf("list --tag pets = %d, want 0; stderr: %s", code, stderr)
	}

	got := lines(stdout)
	if len(got) != 2 {
		t.Fatalf("list --tag pets emitted %d rows, want 2:\n%s", len(got), stdout)
	}
	if strings.Contains(stdout, "getStore") {
		t.Errorf("--tag pets included an operation tagged stores:\n%s", stdout)
	}
}

func TestListUnmatchedTagIsEmptyAndSucceeds(t *testing.T) {
	// An empty result is an answer, not a failure: the agent asked whether
	// anything carries that tag and the answer is "nothing".
	code, stdout, stderr := runList(t, "testdata/petstore.yaml", "--tag", "nosuchtag", "--output", "json")
	if code != 0 {
		t.Fatalf("list --tag nosuchtag = %d, want 0; stderr: %s", code, stderr)
	}

	var payload listJSON
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot: %s", err, stdout)
	}
	if payload.Operations == nil {
		t.Errorf("operations is null, want an empty array: %s", stdout)
	}
	if len(payload.Operations) != 0 {
		t.Errorf("got %d operations, want none: %s", len(payload.Operations), stdout)
	}
}

func TestListReadsTheSpecFromTheFlagAndTheEnv(t *testing.T) {
	code, stdout, stderr := runList(t, "--spec", "testdata/petstore.yaml", "--output", "tsv")
	if code != 0 {
		t.Fatalf("list --spec = %d, want 0; stderr: %s", code, stderr)
	}
	if len(lines(stdout)) != 4 {
		t.Errorf("list --spec emitted %d rows, want 4:\n%s", len(lines(stdout)), stdout)
	}

	t.Setenv(spec.EnvSpec, "testdata/petstore.yaml")

	var out, errOut strings.Builder
	if got := run([]string{"list", "--output", "tsv"}, &out, &errOut); got != 0 {
		t.Fatalf("list with $%s = %d, want 0; stderr: %s", spec.EnvSpec, got, errOut.String())
	}
	if len(lines(out.String())) != 4 {
		t.Errorf("list with $%s emitted %d rows, want 4:\n%s", spec.EnvSpec, len(lines(out.String())), out.String())
	}
}

func TestListExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no spec anywhere", []string{"--output", "json"}, 2},
		{"unparseable spec", []string{"testdata/malformed.yaml", "--output", "json"}, 3},
		{"missing file", []string{"testdata/nope.yaml", "--output", "json"}, 3},
		{"too many arguments", []string{"a.yaml", "b.yaml"}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runList(t, tt.args...)
			if code != tt.want {
				t.Fatalf("list %v = %d, want %d; stderr: %s", tt.args, code, tt.want, stderr)
			}
			if stdout != "" {
				t.Errorf("a failed list wrote %q to stdout, want nothing", stdout)
			}
			if !strings.HasPrefix(stderr, `{"schema":"talaria/v1"`) {
				t.Errorf("stderr is not the structured envelope: %s", stderr)
			}
		})
	}
}
