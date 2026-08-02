// The §8 open question — libopenapi-validator strictness on 3.0 specs —
// is answered by TestResponseHonoursOpenAPI30SchemaSemantics below.
//
// Finding (libopenapi-validator v0.14.0, 2026-08-02): no false failure.
// The library reads the document's OpenAPI version and, for 3.0, rewrites the
// draft-04-era constructs before compiling: `nullable: true` becomes a union
// with "null", boolean `exclusiveMinimum`/`exclusiveMaximum` become the numeric
// 2020-12 spelling, and singular `example` is ignored rather than rejected.
// No per-document configuration is needed, so validate.Response passes no
// strictness options. The fixture (testdata/strict-3.0.yaml) is adversarial on
// purpose — it carries all four constructs and a body that only validates if
// every one of them is honoured — so the test would catch a regression that
// re-imposed 3.1 semantics on a 3.0 document.
package validate

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/spec"
)

// load reads a fixture spec, failing the test rather than returning an error:
// every test here needs a document before it can assert anything.
func load(t *testing.T, name string) *spec.Document {
	t.Helper()

	doc, err := spec.LoadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}

	return doc
}

// jsonInput is the common case: a JSON response to a GET.
func jsonInput(url string, status int, body string) Input {
	return Input{
		Method:  http.MethodGet,
		URL:     url,
		Status:  status,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(body),
	}
}

// errorText flattens a result's errors so a test can assert on the whole
// reported surface without caring which field carried the detail.
func errorText(r *Result) string {
	var b strings.Builder
	for _, e := range r.Errors {
		b.WriteString(e.Message)
		b.WriteString(" ")
		b.WriteString(e.Reason)
		b.WriteString(" ")
		b.WriteString(e.Field)
		b.WriteString("\n")
	}

	return b.String()
}

func TestResponseReportsDocumentedStatus(t *testing.T) {
	doc := load(t, "petstore-3.0.yaml")

	res, err := Response(doc, jsonInput("https://api.real.example.com/v1/pets", 404,
		`{"message":"none"}`))
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if !res.StatusDocumented {
		t.Errorf("StatusDocumented = false, want true; errors: %s", errorText(res))
	}
	if !res.BodyValid {
		t.Errorf("BodyValid = false, want true; errors: %s", errorText(res))
	}
	if len(res.Errors) != 0 {
		t.Errorf("Errors = %v, want none", res.Errors)
	}
}

// An undocumented status is a finding, not a failure: validation still returns
// a result so call can render the block and exit 0 without --fail-on-error.
func TestResponseReportsUndocumentedStatusWithoutErroring(t *testing.T) {
	doc := load(t, "petstore-3.0.yaml")

	res, err := Response(doc, jsonInput("https://api.real.example.com/v1/pets", 418,
		`{"message":"teapot"}`))
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if res.StatusDocumented {
		t.Error("StatusDocumented = true, want false")
	}
	if len(res.Errors) == 0 {
		t.Fatal("Errors is empty, want the undocumented status reported")
	}
	if !strings.Contains(errorText(res), "418") {
		t.Errorf("errors do not name the status: %s", errorText(res))
	}
}

func TestResponseValidatesBodyAgainstSchema(t *testing.T) {
	doc := load(t, "petstore-3.0.yaml")

	res, err := Response(doc, jsonInput("https://api.real.example.com/v1/pets", 200,
		`[{"id":1,"name":"fido"}]`))
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if !res.BodyValid {
		t.Errorf("BodyValid = false, want true; errors: %s", errorText(res))
	}
}

// The error has to name the field and the type the spec asked for, because the
// consumer is an agent deciding what to do next, not a human reading a stack
// trace.
func TestResponseReportsWrongFieldType(t *testing.T) {
	doc := load(t, "petstore-3.0.yaml")

	res, err := Response(doc, jsonInput("https://api.real.example.com/v1/pets", 200,
		`[{"id":"not-a-number","name":"fido"}]`))
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if res.BodyValid {
		t.Fatal("BodyValid = true, want false")
	}

	text := errorText(res)
	if !strings.Contains(text, "id") {
		t.Errorf("errors do not name the field %q: %s", "id", text)
	}
	if !strings.Contains(text, "integer") {
		t.Errorf("errors do not name the expected type %q: %s", "integer", text)
	}
}

func TestResponseFlagsUndeclaredContentType(t *testing.T) {
	doc := load(t, "petstore-3.0.yaml")

	in := jsonInput("https://api.real.example.com/v1/pets", 200, "plain text, not json")
	in.Headers.Set("Content-Type", "text/plain")

	res, err := Response(doc, in)
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if res.ContentTypeDocumented {
		t.Error("ContentTypeDocumented = true, want false")
	}
	if !strings.Contains(errorText(res), "text/plain") {
		t.Errorf("errors do not name the content type: %s", errorText(res))
	}
}

// `default` is the spec's own catch-all. A status it covers is documented, so
// reporting it as undocumented would make every well-specified error response
// look like a contract violation.
func TestResponseAcceptsStatusCoveredByDefault(t *testing.T) {
	doc := load(t, "petstore-3.0.yaml")

	res, err := Response(doc, jsonInput("https://api.real.example.com/v1/pets/7", 503,
		`{"message":"down"}`))
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if !res.StatusDocumented {
		t.Errorf("StatusDocumented = false, want true; errors: %s", errorText(res))
	}
	if !res.BodyValid {
		t.Errorf("BodyValid = false, want true; errors: %s", errorText(res))
	}
}

// A 204 declares no content at all. "No schema" must read as nothing to check,
// not as a missing body.
func TestResponseAcceptsEmptyBodyFor204(t *testing.T) {
	doc := load(t, "petstore-3.0.yaml")

	res, err := Response(doc, Input{
		Method:  http.MethodDelete,
		URL:     "https://api.real.example.com/v1/pets/7",
		Status:  204,
		Headers: http.Header{},
	})
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if !res.StatusDocumented {
		t.Errorf("StatusDocumented = false, want true; errors: %s", errorText(res))
	}
	if !res.BodyValid {
		t.Errorf("BodyValid = false, want true; errors: %s", errorText(res))
	}
	if len(res.Errors) != 0 {
		t.Errorf("Errors = %v, want none", res.Errors)
	}
}

// --base-url points a call at a twin or a staging host, which shares nothing
// with the spec's declared server but the paths. Validation has to route on the
// path, tolerating both a different host and the presence or absence of the
// server's /v1 prefix — otherwise every twin-served call in Phase 6 reports a
// spurious violation.
func TestResponseRoutesRegardlessOfHostAndServerPrefix(t *testing.T) {
	doc := load(t, "petstore-3.0.yaml")

	urls := []string{
		"https://api.real.example.com/v1/pets",
		"http://127.0.0.1:8931/pets",
		"http://127.0.0.1:8931/v1/pets",
	}

	for _, url := range urls {
		res, err := Response(doc, jsonInput(url, 200, `[{"id":1,"name":"fido"}]`))
		if err != nil {
			t.Fatalf("Response(%s): %v", url, err)
		}

		if !res.StatusDocumented || !res.BodyValid || !res.ContentTypeDocumented {
			t.Errorf("%s: got %+v, want everything documented and valid; errors: %s",
				url, res, errorText(res))
		}
	}
}

// A path the spec has never heard of documents nothing, so all three answers
// are false. This is the shape an agent sees when the spec has drifted from the
// server, and it must not read as "the status was documented".
func TestResponseReportsUndocumentedPath(t *testing.T) {
	doc := load(t, "petstore-3.0.yaml")

	res, err := Response(doc, jsonInput("https://api.real.example.com/v1/nope", 200, `{}`))
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if res.StatusDocumented || res.ContentTypeDocumented || res.BodyValid {
		t.Errorf("got %+v, want everything false for an undocumented path", res)
	}
	if !strings.Contains(errorText(res), "/nope") {
		t.Errorf("errors do not name the path: %s", errorText(res))
	}
}

// See the finding recorded at the top of this file.
func TestResponseHonoursOpenAPI30SchemaSemantics(t *testing.T) {
	doc := load(t, "strict-3.0.yaml")

	// Every field exercises one 3.0-only construct: the nulls need `nullable`
	// to be honoured, and id sits strictly inside the boolean-exclusive bounds.
	res, err := Response(doc, jsonInput("https://api.strict.example.com/widgets", 200,
		`{"id":5,"label":null,"score":null,"tags":null}`))
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if !res.BodyValid {
		t.Errorf("BodyValid = false on a valid 3.0 body — 3.1 strictness is being "+
			"applied to a 3.0 document; errors: %s", errorText(res))
	}
}

// The mirror of the test above: honouring 3.0 semantics must not mean skipping
// the constraints. `exclusiveMinimum: true` with `minimum: 0` still rejects 0.
func TestResponseEnforcesOpenAPI30ExclusiveBounds(t *testing.T) {
	doc := load(t, "strict-3.0.yaml")

	res, err := Response(doc, jsonInput("https://api.strict.example.com/widgets", 200,
		`{"id":0,"label":"w","score":1,"tags":[]}`))
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if res.BodyValid {
		t.Error("BodyValid = true, want false: id=0 violates exclusiveMinimum 0")
	}
}
