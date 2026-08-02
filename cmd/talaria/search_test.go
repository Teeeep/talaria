package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/spec"
)

// searchJSON is the JSON shape search emits.
type searchJSON struct {
	Schema  string `json:"schema"`
	Query   string `json:"query"`
	Results []struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Where   string `json:"where"`
		Summary string `json:"summary"`
	} `json:"results"`
}

// runSearch runs `search` with args, with the env var fallback out of the way.
func runSearch(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv(spec.EnvSpec, "")

	var out, errOut strings.Builder
	code = run(append([]string{"search"}, args...), &out, &errOut)

	return code, out.String(), errOut.String()
}

// searchResults runs search and decodes its JSON payload, failing on any
// non-zero exit.
func searchResults(t *testing.T, args ...string) searchJSON {
	t.Helper()

	code, stdout, stderr := runSearch(t, append(args, "--output", "json")...)
	if code != 0 {
		t.Fatalf("search %v = %d, want 0; stderr: %s", args, code, stderr)
	}

	var payload searchJSON
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot: %s", err, stdout)
	}

	return payload
}

// names returns the result names in the order search ranked them.
func (p searchJSON) names() []string {
	out := make([]string, 0, len(p.Results))
	for _, r := range p.Results {
		out = append(out, r.Name)
	}

	return out
}

// kindOf returns the kind search labelled name with, or "" if it is absent.
func (p searchJSON) kindOf(name string) string {
	for _, r := range p.Results {
		if r.Name == name {
			return r.Kind
		}
	}

	return ""
}

func rankOf(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}

	return -1
}

func TestSearchMatchesIdPathSummaryAndDescriptionWithIdMatchesFirst(t *testing.T) {
	payload := searchResults(t, "testdata/search.yaml", "invoice", "--kind", "operation")

	got := payload.names()
	for _, want := range []string{
		"listInvoices",   // matches on its operationId
		"voidDocument",   // matches only on its path
		"createDocument", // matches only on its description
	} {
		if rankOf(got, want) < 0 {
			t.Errorf("search invoice did not match %s: %v", want, got)
		}
	}

	// The id match leads: an agent that typed a concept expects the endpoint
	// named after it before the ones that merely mention it.
	if len(got) == 0 || got[0] != "listInvoices" {
		t.Fatalf("search invoice ranked %v, want the operationId match listInvoices first", got)
	}
	if idMatch, pathMatch := rankOf(got, "listInvoices"), rankOf(got, "voidDocument"); idMatch > pathMatch {
		t.Errorf("path match voidDocument ranked above id match listInvoices: %v", got)
	}
	if pathMatch, proseMatch := rankOf(got, "voidDocument"), rankOf(got, "createDocument"); pathMatch > proseMatch {
		t.Errorf("description match createDocument ranked above path match voidDocument: %v", got)
	}

	// listPets matches nothing about invoices.
	if rankOf(got, "listPets") >= 0 {
		t.Errorf("search invoice matched the unrelated listPets: %v", got)
	}
}

func TestSearchIsCaseInsensitive(t *testing.T) {
	lower := searchResults(t, "testdata/search.yaml", "invoice", "--kind", "operation").names()
	upper := searchResults(t, "testdata/search.yaml", "INVOICE", "--kind", "operation").names()

	if strings.Join(lower, ",") != strings.Join(upper, ",") {
		t.Fatalf("search INVOICE = %v, want the same as search invoice %v", upper, lower)
	}
}

func TestSearchKindRestrictsResultsToOneKind(t *testing.T) {
	schemas := searchResults(t, "testdata/search.yaml", "invoice", "--kind", "schema")
	for _, want := range []string{"Invoice", "InvoiceLine", "InvoiceList"} {
		if rankOf(schemas.names(), want) < 0 {
			t.Errorf("--kind schema did not match the component schema %s: %v", want, schemas.names())
		}
	}
	for _, r := range schemas.Results {
		if r.Kind != "schema" {
			t.Errorf("--kind schema returned a %s result %q", r.Kind, r.Name)
		}
	}

	params := searchResults(t, "testdata/search.yaml", "invoice", "--kind", "param")
	for _, want := range []string{"invoiceStatus", "invoiceId"} {
		if rankOf(params.names(), want) < 0 {
			t.Errorf("--kind param did not match the parameter %s: %v", want, params.names())
		}
	}
	for _, r := range params.Results {
		if r.Kind != "param" {
			t.Errorf("--kind param returned a %s result %q", r.Kind, r.Name)
		}
	}
}

func TestSearchWithoutKindSearchesAllThreeAndLabelsEach(t *testing.T) {
	payload := searchResults(t, "testdata/search.yaml", "invoice")

	for name, want := range map[string]string{
		"listInvoices":  "operation",
		"Invoice":       "schema",
		"invoiceStatus": "param",
	} {
		if got := payload.kindOf(name); got != want {
			t.Errorf("%s is labelled %q, want kind %q; results: %v", name, got, want, payload.names())
		}
	}
}

func TestSearchLocatesEachResult(t *testing.T) {
	payload := searchResults(t, "testdata/search.yaml", "invoice")

	where := map[string]string{}
	for _, r := range payload.Results {
		where[r.Name] = r.Where
	}

	if got := where["listInvoices"]; got != "GET /invoices" {
		t.Errorf("listInvoices is located at %q, want its method and path", got)
	}
	if got := where["invoiceStatus"]; got != "query" {
		t.Errorf("invoiceStatus is located at %q, want its parameter location", got)
	}
	if got := where["Invoice"]; got != "#/components/schemas/Invoice" {
		t.Errorf("Invoice is located at %q, want its component pointer", got)
	}
}

func TestSearchWithNoMatchesSucceedsWithAnEmptyResultSet(t *testing.T) {
	// Probing for a concept the API does not have is not a usage error: the
	// answer "this API has no such thing" is the useful one.
	code, stdout, stderr := runSearch(t, "testdata/search.yaml", "zzzznothing", "--output", "json")
	if code != 0 {
		t.Fatalf("search for an absent term = %d, want 0; stderr: %s", code, stderr)
	}

	var payload searchJSON
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot: %s", err, stdout)
	}
	if len(payload.Results) != 0 {
		t.Fatalf("got %d results for an absent term: %v", len(payload.Results), payload.names())
	}
	if !strings.Contains(stdout, `"results":[]`) {
		t.Errorf("empty results rendered as null, want an empty array: %s", stdout)
	}
}

func TestSearchPrettyPrintsOneLinePerResult(t *testing.T) {
	code, stdout, stderr := runSearch(t, "testdata/search.yaml", "invoice", "--output", "pretty")
	if code != 0 {
		t.Fatalf("search --output pretty = %d, want 0; stderr: %s", code, stderr)
	}

	for _, want := range []string{"operation", "listInvoices", "GET /invoices", "schema", "Invoice", "param", "invoiceStatus"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("search pretty output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestSearchExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no query", []string{"--output", "json"}, 2},
		{"no spec anywhere", []string{"invoice", "--output", "json"}, 2},
		{"unknown kind", []string{"testdata/search.yaml", "invoice", "--kind", "endpoint", "--output", "json"}, 2},
		{"unparseable spec", []string{"testdata/malformed.yaml", "invoice", "--output", "json"}, 3},
		{"too many arguments", []string{"a.yaml", "invoice", "extra"}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runSearch(t, tt.args...)
			if code != tt.want {
				t.Fatalf("search %v = %d, want %d; stderr: %s", tt.args, code, tt.want, stderr)
			}
			if stdout != "" {
				t.Errorf("a failed search wrote %q to stdout, want nothing", stdout)
			}
			if !strings.HasPrefix(stderr, `{"schema":"talaria/v1"`) {
				t.Errorf("stderr is not the structured envelope: %s", stderr)
			}
		})
	}
}

func TestSearchUnknownKindSuggestsTheValidOnes(t *testing.T) {
	_, _, stderr := runSearch(t, "testdata/search.yaml", "invoice", "--kind", "endpoint", "--output", "json")

	for _, want := range []string{"operation", "schema", "param"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the unknown-kind error does not offer %q: %s", want, stderr)
		}
	}
}
