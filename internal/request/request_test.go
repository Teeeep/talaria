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

// A scheme other than http(s) is not merely unsupported: curl speaks gopher,
// dict, smb and file, so an unchecked scheme turns a request into a file read
// or a raw TCP write. Every source is untrusted here — the premise is that an
// agent points talaria at whatever spec it found.
func TestBuildRejectsABaseURLWhoseSchemeIsNotHTTP(t *testing.T) {
	for _, raw := range []string{
		"gopher://127.0.0.1:1234",
		"file:///etc/passwd",
		"dict://127.0.0.1:2628/d:talaria",
		"smb://127.0.0.1/share",
	} {
		t.Run(raw, func(t *testing.T) {
			in := inputs(t, "listPets")
			in.Params = []string{"limit=10"}
			in.BaseURL = raw

			if err := buildErr(t, in); !strings.Contains(err.Error(), "http(s)") {
				t.Errorf("error %v does not say the scheme must be http(s)", err)
			}
		})
	}
}

func TestBuildRejectsANonHTTPServerDeclaredByTheSpec(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Doc.Model.Servers[0].URL = "gopher://127.0.0.1:1234"

	err := buildErr(t, in)
	if !strings.Contains(err.Error(), "servers[0].url") {
		t.Errorf("error %v does not name the spec as the source", err)
	}
}

func TestBuildRejectsANonHTTPProfileBaseURL(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Profile = &config.Profile{Name: "staging", BaseURL: "file:///etc/passwd"}

	err := buildErr(t, in)
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("error %v does not name the profile as the source", err)
	}
}

// Schemes are case-insensitive per RFC 3986, so rejecting HTTPS:// would refuse
// a URL that is valid everywhere else.
func TestBuildAcceptsHTTPSchemesInAnyCase(t *testing.T) {
	for _, raw := range []string{"http://api.example.com", "HTTPS://api.example.com", "HtTp://api.example.com"} {
		t.Run(raw, func(t *testing.T) {
			in := inputs(t, "listPets")
			in.Params = []string{"limit=10"}
			in.BaseURL = raw

			if got := build(t, in).BaseURL; got != raw {
				t.Errorf("BaseURL = %q, want %q unchanged", got, raw)
			}
		})
	}
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

func TestValueHidesASensitiveLiteralFromEveryDisplayForm(t *testing.T) {
	v := Literal(canary).Sensitive()

	for name, rendering := range map[string]string{
		"String":   v.String(),
		"Symbolic": v.Symbolic(),
		"GoString": v.GoString(),
		"%#v":      fmt.Sprintf("%#v", v),
	} {
		if strings.Contains(rendering, canary) {
			t.Errorf("%s() = %q, want the value hidden", name, rendering)
		}
	}

	// Hidden from display, not from the request: Reveal is what the config
	// document is built from, and it is the only way back to the value.
	if got := v.Reveal(); got != canary {
		t.Errorf("Reveal() = %q, want the literal back", got)
	}
	if !v.IsSensitive() || v.IsSecret() {
		t.Errorf("IsSensitive() = %v, IsSecret() = %v; want a sensitive literal, not a ref",
			v.IsSensitive(), v.IsSecret())
	}
}

func TestBuildHidesALiteralUnderACredentialShapedHeaderName(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Headers = []string{
		"X-Api-Key=" + canary,
		"Authorization=Bearer " + canary,
		"X-Request-Id=trace-42",
	}

	req := build(t, in)

	for _, name := range []string{"X-Api-Key", "Authorization"} {
		value := find(t, req.Headers, name)
		if got := value.String(); strings.Contains(got, canary) {
			t.Errorf("header %s displays as %q; a literal under a credential-shaped name is redacted", name, got)
		}
		if !strings.Contains(value.Reveal(), canary) {
			t.Errorf("header %s no longer carries the value it will send: %q", name, value.Reveal())
		}
	}

	// Redaction is by name, so an ordinary header is left alone: a request whose
	// every header read <redacted> would tell an agent nothing.
	if got := find(t, req.Headers, "X-Request-Id").String(); got != "trace-42" {
		t.Errorf("header X-Request-Id = %q, want trace-42", got)
	}
}

func TestBuildHidesALiteralUnderACredentialShapedQueryName(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Query = []string{"api_key=" + canary, "page=2"}

	req := build(t, in)

	// §5a names "query-string API keys (?api_key=)" as a credential location,
	// so the built-in name matcher applies here exactly as it does to headers.
	value := find(t, req.Query, "api_key")
	if !value.IsSensitive() {
		t.Error("--query api_key is not sensitive; a query-string API key is a credential (§5a)")
	}
	if got := value.String(); got != secret.Placeholder {
		t.Errorf("api_key displays as %q, want %q", got, secret.Placeholder)
	}
	if got := value.Reveal(); got != canary {
		t.Errorf("api_key no longer carries the value it will send: %q", got)
	}

	// By name, as everywhere else: an ordinary query parameter is untouched.
	if got := find(t, req.Query, "page").String(); got != "2" {
		t.Errorf("query page = %q, want 2", got)
	}
}

// TestQueryStringEscapesAResolvedSensitiveLiteral pins the half of the fix that
// is easy to lose: QueryString is shared with the wire path, so skipping the
// percent-encoding is only ever safe for a placeholder, never for a value.
func TestQueryStringEscapesAResolvedSensitiveLiteral(t *testing.T) {
	req := Request{
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query:   []Pair{{Name: "api_key", Value: Literal("a b&c").Sensitive()}},
	}

	resolve := func(v Value) (string, error) { return v.Reveal(), nil }

	got, err := req.QueryString(resolve)
	if err != nil {
		t.Fatalf("QueryString: %v", err)
	}
	if got != "api_key=a+b%26c" {
		t.Errorf("QueryString(resolve) = %q, want the resolved value percent-encoded", got)
	}

	// The display form is the placeholder, and it is the one case that skips
	// the escape so it stays readable.
	got, err = req.QueryString(Redacted)
	if err != nil {
		t.Fatalf("QueryString(Redacted): %v", err)
	}
	if got != "api_key="+secret.Placeholder {
		t.Errorf("QueryString(Redacted) = %q, want %q", got, "api_key="+secret.Placeholder)
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
