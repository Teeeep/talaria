package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/spec"
)

// usesJSON is the JSON shape uses emits.
type usesJSON struct {
	Schema     string       `json:"schema"`
	Name       string       `json:"name"`
	Operations []usesOpJSON `json:"operations"`
}

type usesOpJSON struct {
	ID     string   `json:"id"`
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Where  []string `json:"where"`
	Direct bool     `json:"direct"`
}

// runUses runs `uses` with args, with the env var fallback out of the way.
func runUses(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv(spec.EnvSpec, "")

	var out, errOut strings.Builder
	code = run(append([]string{"uses"}, args...), &out, &errOut)

	return code, out.String(), errOut.String()
}

// usesOf runs uses over the search fixture and decodes its JSON payload.
func usesOf(t *testing.T, schema string) usesJSON {
	t.Helper()

	code, stdout, stderr := runUses(t, "testdata/search.yaml", schema, "--output", "json")
	if code != 0 {
		t.Fatalf("uses %s = %d, want 0; stderr: %s", schema, code, stderr)
	}

	var payload usesJSON
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot: %s", err, stdout)
	}

	return payload
}

// find returns the reported use of the schema by operation id.
func (p usesJSON) find(t *testing.T, id string) usesOpJSON {
	t.Helper()

	for _, op := range p.Operations {
		if op.ID == id {
			return op
		}
	}

	t.Fatalf("uses %s does not report %s; got %+v", p.Name, id, p.Operations)

	return usesOpJSON{}
}

func (p usesJSON) ids() []string {
	out := make([]string, 0, len(p.Operations))
	for _, op := range p.Operations {
		out = append(out, op.ID)
	}

	return out
}

func TestUsesReportsParamsRequestBodiesAndResponses(t *testing.T) {
	// A response: getPet returns Pet itself.
	pet := usesOf(t, "Pet")
	getPet := pet.find(t, "getPet")
	if !getPet.Direct {
		t.Errorf("getPet returns Pet itself, want direct = true: %+v", getPet)
	}
	if strings.Join(getPet.Where, ",") != "response:200" {
		t.Errorf("getPet uses Pet at %v, want its 200 response", getPet.Where)
	}

	// A parameter: getPet's petId is a $ref to PetId.
	petID := usesOf(t, "PetId")
	if got := petID.find(t, "getPet"); strings.Join(got.Where, ",") != "param:petId" {
		t.Errorf("getPet uses PetId at %v, want its petId parameter", got.Where)
	}

	// A request body: createDocument posts an Invoice.
	invoice := usesOf(t, "Invoice")
	body := invoice.find(t, "createDocument")
	if !body.Direct || strings.Join(body.Where, ",") != "request_body" {
		t.Errorf("createDocument uses Invoice at %v (direct %v), want its request body", body.Where, body.Direct)
	}

	// An operation touching none of these schemas is not reported.
	if rankOf(invoice.ids(), "getPet") >= 0 {
		t.Errorf("uses Invoice reported the unrelated getPet: %v", invoice.ids())
	}
}

func TestUsesFollowsIndirectionThroughOtherSchemas(t *testing.T) {
	// listPets returns PetList, which contains an array of Pet. The operation
	// still has to be reported: an agent changing Pet needs to know.
	pet := usesOf(t, "Pet")

	listPets := pet.find(t, "listPets")
	if listPets.Direct {
		t.Errorf("listPets reaches Pet only through PetList, want direct = false: %+v", listPets)
	}
	if strings.Join(listPets.Where, ",") != "response:200" {
		t.Errorf("listPets uses Pet at %v, want its 200 response", listPets.Where)
	}

	// And through the reference cycle Owner -> Pet: an inline array of Owner
	// still reaches Pet.
	if got := pet.find(t, "listOwners"); got.Direct {
		t.Errorf("listOwners reaches Pet only through Owner, want direct = false: %+v", got)
	}

	// The same indirection through a request body's schema graph.
	line := usesOf(t, "InvoiceLine")
	if got := line.find(t, "createDocument"); got.Direct {
		t.Errorf("createDocument reaches InvoiceLine only through Invoice, want direct = false: %+v", got)
	}
	if got := line.find(t, "listInvoices"); strings.Join(got.Where, ",") != "response:200" {
		t.Errorf("listInvoices uses InvoiceLine at %v, want its 200 response", got.Where)
	}
}

func TestUsesOfADeclaredButUnreferencedSchemaSucceedsWithNoOperations(t *testing.T) {
	// The schema exists, so this is not a usage error — the answer "nothing
	// reaches it" is exactly what the caller asked for.
	code, stdout, stderr := runUses(t, "testdata/search.yaml", "AuditEntry", "--output", "json")
	if code != 0 {
		t.Fatalf("uses AuditEntry = %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"operations":[]`) {
		t.Errorf("uses of an unreferenced schema did not render an empty array: %s", stdout)
	}
}

func TestUsesPrettyListsTheOperations(t *testing.T) {
	code, stdout, stderr := runUses(t, "testdata/search.yaml", "Pet", "--output", "pretty")
	if code != 0 {
		t.Fatalf("uses --output pretty = %d, want 0; stderr: %s", code, stderr)
	}

	for _, want := range []string{"GET", "/pets/{petId}", "getPet", "response:200", "listPets", "indirect"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("uses pretty output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestUsesUnknownSchemaExitsTwoWithValidNames(t *testing.T) {
	code, stdout, stderr := runUses(t, "testdata/search.yaml", "Pets", "--output", "json")
	if code != 2 {
		t.Fatalf("uses Pets = %d, want 2; stderr: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("a failed uses wrote %q to stdout, want nothing", stdout)
	}
	if !strings.Contains(stderr, `"valid_alternatives"`) || !strings.Contains(stderr, `"Pet"`) {
		t.Errorf("stderr does not offer the spec's schema names: %s", stderr)
	}
}

func TestUsesExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no schema name", []string{"--output", "json"}, 2},
		{"no spec anywhere", []string{"Pet", "--output", "json"}, 2},
		{"unparseable spec", []string{"testdata/malformed.yaml", "Pet", "--output", "json"}, 3},
		{"too many arguments", []string{"a.yaml", "Pet", "extra"}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runUses(t, tt.args...)
			if code != tt.want {
				t.Fatalf("uses %v = %d, want %d; stderr: %s", tt.args, code, tt.want, stderr)
			}
			if stdout != "" {
				t.Errorf("a failed uses wrote %q to stdout, want nothing", stdout)
			}
			if !strings.HasPrefix(stderr, `{"schema":"talaria/v1"`) {
				t.Errorf("stderr is not the structured envelope: %s", stderr)
			}
		})
	}
}
