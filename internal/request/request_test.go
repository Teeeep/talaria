package request

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/secret"
	"github.com/Teeeep/talaria/internal/spec"
)

// canary is the credential value the firewall must never let out. Every test
// that touches auth sets it, so "the canary is absent" is the assertion that no
// value escaped the Request.
const canary = "s3cr3t-canary-value"

// fixture loads the request fixture and returns the named operation with the
// document it came from.
func fixture(t *testing.T, id string) (operation.Operation, *spec.Document) {
	t.Helper()

	doc, err := spec.LoadFile(filepath.Join("testdata", "request.yaml"))
	if err != nil {
		t.Fatalf("LoadFile(request.yaml): %v", err)
	}

	for _, op := range operation.Extract(doc) {
		if op.ID == id {
			return op, doc
		}
	}

	t.Fatalf("fixture has no operation %q", id)
	return operation.Operation{}, nil
}

// inputs is the common case: an operation from the fixture, no profile, no
// credentials. Tests layer their own flags on top.
func inputs(t *testing.T, id string) Inputs {
	t.Helper()

	op, doc := fixture(t, id)
	return Inputs{Op: op, Doc: doc}
}

func build(t *testing.T, in Inputs) *Request {
	t.Helper()

	req, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return req
}

// buildErr asserts Build failed with a usage error (exit 2) and returns it, so
// each test can go on to assert what the message has to name.
func buildErr(t *testing.T, in Inputs) error {
	t.Helper()

	req, err := Build(in)
	if err == nil {
		t.Fatalf("Build succeeded, want a usage error; got %+v", req)
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("Build error = %v (code %d), want a usage error (%d)", err, code, clierr.CodeUsage)
	}

	return err
}

// find returns the value of the first pair named name, case-insensitively.
func find(t *testing.T, pairs []Pair, name string) Value {
	t.Helper()

	for _, p := range pairs {
		if strings.EqualFold(p.Name, name) {
			return p.Value
		}
	}

	t.Fatalf("no pair named %q in %v", name, pairs)
	return Value{}
}

func TestBuildSubstitutesPathParams(t *testing.T) {
	in := inputs(t, "getPet")
	in.Params = []string{"petId=42"}

	req := build(t, in)

	if got, want := req.Path, "/pets/42"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	if got, want := req.Method, "GET"; got != want {
		t.Errorf("Method = %q, want %q", got, want)
	}

	gotURL, err := req.URL(Redacted)
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	if want := "https://api.example.com/v1/pets/42"; gotURL != want {
		t.Errorf("URL = %q, want %q", gotURL, want)
	}
}

func TestBuildReportsAMissingRequiredPathParam(t *testing.T) {
	err := buildErr(t, inputs(t, "getPet"))

	if !strings.Contains(err.Error(), "petId") {
		t.Errorf("error does not name the missing parameter: %v", err)
	}
}

func TestBuildReportsAMissingRequiredQueryParam(t *testing.T) {
	err := buildErr(t, inputs(t, "listPets"))

	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error does not name the missing parameter: %v", err)
	}
}

func TestBuildRoutesDeclaredParamsToTheirLocation(t *testing.T) {
	in := inputs(t, "getPet")
	in.Params = []string{"petId=1", "verbose=true", "X-Trace=abc", "flavour=mint"}

	req := build(t, in)

	if got := find(t, req.Query, "verbose").String(); got != "true" {
		t.Errorf("query verbose = %q, want %q", got, "true")
	}
	if got := find(t, req.Headers, "X-Trace").String(); got != "abc" {
		t.Errorf("header X-Trace = %q, want %q", got, "abc")
	}
	if got := find(t, req.Cookies, "flavour").String(); got != "mint" {
		t.Errorf("cookie flavour = %q, want %q", got, "mint")
	}
}

func TestBuildAccumulatesRepeatedQueryFlags(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Query = []string{"tag=cat", "tag=dog", "q=a b"}

	req := build(t, in)

	raw, err := req.URL(Redacted)
	if err != nil {
		t.Fatalf("URL: %v", err)
	}

	// The exact encoding of a space is curl's business, not this test's; what
	// matters is that the round trip gives back what was asked for.
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}

	values := parsed.Query()
	if got, want := values["tag"], []string{"cat", "dog"}; !equal(got, want) {
		t.Errorf("tag = %v, want %v — repeated --query flags accumulate", got, want)
	}
	if got, want := values.Get("q"), "a b"; got != want {
		t.Errorf("q = %q, want %q — the value must survive the encoding round trip", got, want)
	}
	if strings.Contains(raw, "q=a b") {
		t.Errorf("URL carries an unencoded space: %q", raw)
	}
}

func TestBuildAccumulatesRepeatedHeaderFlags(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Headers = []string{"X-One=1", "X-Two=2", "X-Eq=a=b"}

	req := build(t, in)

	if got := find(t, req.Headers, "X-One").String(); got != "1" {
		t.Errorf("X-One = %q, want %q", got, "1")
	}
	if got := find(t, req.Headers, "X-Two").String(); got != "2" {
		t.Errorf("X-Two = %q, want %q", got, "2")
	}
	// Only the first `=` separates; header values may contain more.
	if got := find(t, req.Headers, "X-Eq").String(); got != "a=b" {
		t.Errorf("X-Eq = %q, want %q", got, "a=b")
	}
}

func TestBuildRejectsAMalformedHeaderFlag(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Headers = []string{"foo"}

	err := buildErr(t, in)

	if !strings.Contains(err.Error(), "foo") {
		t.Errorf("error does not quote the malformed flag: %v", err)
	}
}

func TestBuildRejectsAMalformedParamFlag(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit"}

	if err := buildErr(t, in); !strings.Contains(err.Error(), "limit") {
		t.Errorf("error does not quote the malformed flag: %v", err)
	}
}

func TestBuildRejectsAnUndeclaredParam(t *testing.T) {
	in := inputs(t, "getPet")
	in.Params = []string{"petId=1", "nosuchparam=1"}

	err := buildErr(t, in)

	if !strings.Contains(err.Error(), "nosuchparam") {
		t.Errorf("error does not name the offending parameter: %v", err)
	}

	alts := clierr.From(err).Alternatives
	for _, want := range []string{"petId", "verbose", "X-Trace", "flavour"} {
		if !contains(alts, want) {
			t.Errorf("valid_alternatives = %v, want it to include %q", alts, want)
		}
	}
}

func TestBuildCollectsEveryBindingErrorAtOnce(t *testing.T) {
	// An agent should be able to fix a bad invocation in one round trip rather
	// than discovering the next problem on the next attempt.
	in := inputs(t, "getPet")
	in.Params = []string{"nosuchparam=1"}
	in.Headers = []string{"malformed"}

	err := buildErr(t, in)

	for _, want := range []string{"nosuchparam", "petId", "malformed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; all binding errors report together", err, want)
		}
	}
}

func TestBuildPercentEncodesPathParams(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "slash", value: "a/b", want: "/pets/a%2Fb"},
		{name: "question", value: "a?b", want: "/pets/a%3Fb"},
		{name: "traversal", value: "../admin", want: "/pets/..%2Fadmin"},
		{name: "space", value: "a b", want: "/pets/a%20b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := inputs(t, "getPet")
			in.Params = []string{"petId=" + tt.value}

			req := build(t, in)

			if req.Path != tt.want {
				t.Errorf("Path = %q, want %q — a path param must not escape its segment", req.Path, tt.want)
			}
		})
	}
}

func TestBaseURLPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		profile *config.Profile
		want    string
	}{
		{
			name:    "flag wins over everything",
			flag:    "https://flag.example.com",
			profile: &config.Profile{Name: "staging", BaseURL: "https://profile.example.com"},
			want:    "https://flag.example.com/pets",
		},
		{
			name:    "profile wins over the spec",
			profile: &config.Profile{Name: "staging", BaseURL: "https://profile.example.com"},
			want:    "https://profile.example.com/pets",
		},
		{
			name: "the spec's first server is the fallback",
			want: "https://api.example.com/v1/pets",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := inputs(t, "listPets")
			in.Params = []string{"limit=10"}
			in.BaseURL, in.Profile = tt.flag, tt.profile

			req := build(t, in)

			got, err := req.URL(Redacted)
			if err != nil {
				t.Fatalf("URL: %v", err)
			}
			if base, _, _ := strings.Cut(got, "?"); base != tt.want {
				t.Errorf("URL = %q, want it to start %q", got, tt.want)
			}
		})
	}
}

func TestBuildRequiresABaseURLFromSomewhere(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Doc.Model.Servers = nil

	if err := buildErr(t, in); !strings.Contains(err.Error(), "--base-url") {
		t.Errorf("error does not say how to supply a base URL: %v", err)
	}
}

func TestBuildRejectsABaseURLThatIsNotAbsolute(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.BaseURL = "/v1"

	buildErr(t, in)
}

func TestBuildAppliesProfileHeaders(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Profile = &config.Profile{Name: "staging", Headers: map[string]string{"X-Env": "staging"}}
	in.Headers = []string{"X-Env=explicit"}

	req := build(t, in)

	// The explicit flag is the more specific source and is what survives.
	var seen []string
	for _, h := range req.Headers {
		if strings.EqualFold(h.Name, "X-Env") {
			seen = append(seen, h.Value.String())
		}
	}
	if len(seen) != 1 || seen[0] != "explicit" {
		t.Errorf("X-Env = %v, want exactly [explicit]: --header overrides a profile header", seen)
	}
}

func TestBuildPlacesCredentialsByLocation(t *testing.T) {
	op, doc := fixture(t, "getSecured")

	creds, err := config.Resolve(op, doc, nil)
	if err != nil {
		t.Fatalf("config.Resolve: %v", err)
	}

	req := build(t, Inputs{Op: op, Doc: doc, Creds: creds})

	if got, want := find(t, req.Headers, "Authorization").Symbolic(), "Bearer $TALARIA_AUTH_BEARER"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if !find(t, req.Query, "api_key").IsSecret() {
		t.Error("the api_key query parameter is not a secret reference")
	}
	if got, want := find(t, req.Cookies, "session_id").Ref(), secret.Env("TALARIA_AUTH_APIKEY_COOKIEKEY"); got != want {
		t.Errorf("session_id cookie ref = %v, want %v", got, want)
	}
}

func TestBuildNeverCarriesACredentialValue(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)
	t.Setenv("TALARIA_AUTH_APIKEY_QUERYKEY", canary)
	t.Setenv("TALARIA_AUTH_APIKEY_COOKIEKEY", canary)

	op, doc := fixture(t, "getSecured")

	creds, err := config.Resolve(op, doc, nil)
	if err != nil {
		t.Fatalf("config.Resolve: %v", err)
	}

	req := build(t, Inputs{Op: op, Doc: doc, Creds: creds})

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal(req): %v", err)
	}

	redacted, err := req.URL(Redacted)
	if err != nil {
		t.Fatalf("URL(Redacted): %v", err)
	}
	symbolic, err := req.URL(Symbolic)
	if err != nil {
		t.Fatalf("URL(Symbolic): %v", err)
	}

	for name, rendering := range map[string]string{
		"json":            string(body),
		"%v":              fmt.Sprintf("%v", req),
		"%+v":             fmt.Sprintf("%+v", req),
		"%#v":             fmt.Sprintf("%#v", req),
		"URL(Redacted)":   redacted,
		"URL(Symbolic)":   symbolic,
		"header.String":   find(t, req.Headers, "Authorization").String(),
		"header.Symbolic": find(t, req.Headers, "Authorization").Symbolic(),
		"cookie.String":   find(t, req.Cookies, "session_id").String(),
		"cookie.Symbolic": find(t, req.Cookies, "session_id").Symbolic(),
	} {
		if strings.Contains(rendering, canary) {
			t.Errorf("%s rendering of the request leaked a credential: %s", name, rendering)
		}
	}

	// The env var name is what an agent needs and is safe to print.
	if !strings.Contains(string(body), "TALARIA_AUTH_BEARER") {
		t.Errorf("marshalled request does not name the env var to set: %s", body)
	}
}

func TestBuildLeavesUnauthenticatedRequestsAlone(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}

	req := build(t, in)

	for _, h := range req.Headers {
		if strings.EqualFold(h.Name, "Authorization") {
			t.Errorf("an operation with no security got an Authorization header: %v", h)
		}
	}
}

func TestValueRendersLiteralsUnchanged(t *testing.T) {
	v := Literal("plain")

	if got := v.String(); got != "plain" {
		t.Errorf("String() = %q, want %q", got, "plain")
	}
	if got := v.Symbolic(); got != "plain" {
		t.Errorf("Symbolic() = %q, want %q", got, "plain")
	}
	if v.IsSecret() {
		t.Error("IsSecret() is true for a literal")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
