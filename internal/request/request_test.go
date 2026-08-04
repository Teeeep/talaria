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

// specHosts is the allowed host set a real invocation with no --allow-host
// would have: the document's own servers and nothing else.
func specHosts(t *testing.T, doc *spec.Document) HostSet {
	t.Helper()

	set, err := NewHostSet(ServerURLs(doc), nil, nil)
	if err != nil {
		t.Fatalf("NewHostSet: %v", err)
	}

	return set
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

	// The position, not the argument: an argument with no separator in it is
	// indistinguishable from a bare credential, so nothing of it is echoed.
	if !strings.Contains(err.Error(), "--header 1") {
		t.Errorf("error does not locate the malformed flag: %v", err)
	}
}

func TestBuildRejectsAMalformedParamFlag(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit"}

	if err := buildErr(t, in); !strings.Contains(err.Error(), "--param 1") {
		t.Errorf("error does not locate the malformed flag: %v", err)
	}
}

// TestBuildNeverEchoesARejectedFlagsValue is the credential firewall on the
// error path (DESIGN.md §5a): `--header "Authorization: Bearer $TOKEN"` is the
// shape a user reaches for, the shell has already expanded it, and quoting the
// argument back in an exit-2 message publishes the token.
//
// What the message may carry is the flag's position and, when the argument
// holds a `=` or a `:`, the text before it — a name. Never a prefix of the
// value: a truncated credential is still a leak.
func TestBuildNeverEchoesARejectedFlagsValue(t *testing.T) {
	tests := []struct {
		name string
		// apply puts the offending argument on the inputs.
		apply func(in *Inputs, arg string)
		// arg builds that argument around the canary.
		arg func(value string) string
		// wants are the substrings the message must carry.
		wants []string
	}{
		{
			name:  "header in curl's colon form",
			apply: func(in *Inputs, arg string) { in.Headers = []string{arg} },
			arg:   func(v string) string { return "Authorization: Bearer " + v },
			wants: []string{"--header 1", "Authorization"},
		},
		{
			name:  "header with no separator at all",
			apply: func(in *Inputs, arg string) { in.Headers = []string{arg} },
			arg:   func(v string) string { return v },
			wants: []string{"--header 1"},
		},
		{
			name:  "header whose name half is not a field name",
			apply: func(in *Inputs, arg string) { in.Headers = []string{arg} },
			arg:   func(v string) string { return "Authorization: Bearer " + v + "=1" },
			wants: []string{"--header 1", "Authorization"},
		},
		{
			name:  "header with an empty name",
			apply: func(in *Inputs, arg string) { in.Headers = []string{arg} },
			arg:   func(v string) string { return "=" + v },
			wants: []string{"--header 1"},
		},
		{
			name:  "query in the colon form",
			apply: func(in *Inputs, arg string) { in.Query = []string{arg} },
			arg:   func(v string) string { return "api_key: " + v },
			wants: []string{"--query 1", "api_key"},
		},
		{
			name:  "param in the colon form",
			apply: func(in *Inputs, arg string) { in.Params = []string{arg} },
			arg:   func(v string) string { return "limit: " + v },
			wants: []string{"--param 1", "limit"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := inputs(t, "listPets")
			tc.apply(&in, tc.arg(canary))

			err := buildErr(t, in)

			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			assertNoCanary(t, err.Error())
			// Not even the first few characters of it.
			if strings.Contains(err.Error(), canary[:8]) {
				t.Errorf("error %q carries a prefix of the rejected value", err)
			}
		})
	}
}

// TestBuildRejectsAHeaderNameThatIsNotAFieldName closes the silent corruption
// the other half of the colon form causes: `--header 'X-Trace: abc=1'` parses,
// cuts at the `=`, and puts the malformed `X-Trace: abc: 1` on the wire. A clean
// exit 2 is the only honest answer, since DESIGN.md §4 specifies name=value.
func TestBuildRejectsAHeaderNameThatIsNotAFieldName(t *testing.T) {
	for _, arg := range []string{"X-Trace: abc=1", "X Trace=1", "X-Trace\t=1", "X(Trace)=1"} {
		t.Run(arg, func(t *testing.T) {
			in := inputs(t, "listPets")
			in.Params = []string{"limit=10"}
			in.Headers = []string{arg}

			if err := buildErr(t, in); !strings.Contains(err.Error(), "--header 1") {
				t.Errorf("error does not locate the malformed header: %v", err)
			}
		})
	}
}

// TestBuildAcceptsAQueryNameAFieldNameWouldReject keeps the header rule where
// it belongs. A query parameter is not an HTTP field: `filter[status]` and
// `page.size` are ordinary names an API declares, and the renderer escapes them.
func TestBuildAcceptsAQueryNameAFieldNameWouldReject(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Query = []string{"filter[status]=open", "page.size=10"}

	req := build(t, in)

	if got := find(t, req.Query, "filter[status]").String(); got != "open" {
		t.Errorf("filter[status] = %q, want %q", got, "open")
	}
}

// injection is the shape of a request-splitting value: a plausible value, a
// CRLF that ends the field line, and a header the caller never wrote. Tests
// assert its second half never survives into the request or into the message
// that rejected it.
const injection = "ok\r\nX-Injected: 1"

// TestBuildRejectsCRLFInAValue closes request splitting at the binder. A value
// carrying \r\n ends its own header line and starts whatever follows it — a
// second header, or with a bare CRLF pair a second request on the same
// connection. curl's config document is no defence: escapeDirective protects
// the *directive* syntax and curl un-escapes \r\n straight back to the two
// bytes it writes to the socket. The only place to stop it is before the value
// becomes a Pair.
func TestBuildRejectsCRLFInAValue(t *testing.T) {
	tests := []struct {
		name string
		// apply puts the offending value on the inputs, over petId=1.
		apply func(in *Inputs)
		// wants are the substrings the message must carry so the caller can
		// find what to fix.
		wants []string
	}{
		{
			name:  "spec-declared header parameter",
			apply: func(in *Inputs) { in.Params = append(in.Params, "X-Trace="+injection) },
			wants: []string{"X-Trace"},
		},
		{
			name:  "spec-declared cookie parameter",
			apply: func(in *Inputs) { in.Params = append(in.Params, "flavour="+injection) },
			wants: []string{"flavour"},
		},
		{
			name:  "spec-declared query parameter",
			apply: func(in *Inputs) { in.Params = append(in.Params, "verbose="+injection) },
			wants: []string{"verbose"},
		},
		{
			name:  "--header flag",
			apply: func(in *Inputs) { in.Headers = []string{"X-Trace=" + injection} },
			wants: []string{"--header 1"},
		},
		{
			name:  "--query flag",
			apply: func(in *Inputs) { in.Query = []string{"q=" + injection} },
			wants: []string{"--query 1"},
		},
		{
			name: "profile header",
			apply: func(in *Inputs) {
				in.Profile = &config.Profile{Name: "staging", Headers: map[string]string{"X-Env": injection}}
			},
			wants: []string{"staging", "X-Env"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := inputs(t, "getPet")
			in.Params = []string{"petId=1"}
			tc.apply(&in)

			err := buildErr(t, in)

			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			// Same rule as every other rejected value: it may be a credential,
			// so the message names the position and elides the content.
			if strings.Contains(err.Error(), "X-Injected") {
				t.Errorf("error %q echoes the rejected value back", err)
			}
		})
	}
}

// TestBuildRejectsASpecHeaderParamNameThatIsNotAFieldName holds a spec's own
// header names to the rule --header names already obey. A spec is untrusted
// input like any other: a parameter declared `in: header` with a colon or a
// space in its name renders a malformed wire header, and there is no reason the
// flag path should be the only one that catches it.
func TestBuildRejectsASpecHeaderParamNameThatIsNotAFieldName(t *testing.T) {
	in := Inputs{
		Op: operation.Operation{ID: "odd", Method: "GET", Path: "/odd", Params: []operation.Param{
			{Name: "X-Trace: abc", In: "header"},
		}},
		BaseURL: "https://api.example.com",
		Params:  []string{"X-Trace: abc=1"},
	}

	err := buildErr(t, in)

	if !strings.Contains(err.Error(), "X-Trace") {
		t.Errorf("error %q does not name the offending header parameter", err)
	}
}

// TestBuildRejectsAProfileHeaderNameThatIsNotAFieldName is the same rule for
// the third source of header names. A profile is a file on disk, and a name
// with a space in it is a typo that would otherwise reach the wire.
func TestBuildRejectsAProfileHeaderNameThatIsNotAFieldName(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Profile = &config.Profile{Name: "staging", Headers: map[string]string{"X Env": "1"}}

	err := buildErr(t, in)

	for _, want := range []string{"staging", "X Env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestBuildRejectsAMethodThatIsNotAToken guards the request line rather than a
// header line. The line is `METHOD SP target SP HTTP/1.1`, so a method holding
// a space rewrites the target and one holding a CRLF appends a request.
func TestBuildRejectsAMethodThatIsNotAToken(t *testing.T) {
	for _, method := range []string{"GET /admin HTTP/1.1", "GET\r\nX-Injected: 1", "GET\tPOST"} {
		t.Run(method, func(t *testing.T) {
			in := Inputs{
				Op:      operation.Operation{ID: "odd", Method: method, Path: "/odd"},
				BaseURL: "https://api.example.com",
			}

			if err := buildErr(t, in); !strings.Contains(err.Error(), "method") {
				t.Errorf("error %q does not say the method is the problem", err)
			}
		})
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

	for _, want := range []string{"nosuchparam", "petId", "--header 1"} {
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

// A relative servers[].url — legal OpenAPI, meaning "relative to wherever the
// spec was served" — is one talaria cannot turn into a request, because it does
// not know where the document came from. It is reported as the unusable server
// it is rather than read as no server at all: "the spec declares no server"
// sends a caller looking for a servers block that is right there in front of
// them.
func TestBuildReportsARelativeServerURLRatherThanIgnoringIt(t *testing.T) {
	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.Doc.Model.Servers[0].URL = "/v1"

	err := buildErr(t, in)
	if !strings.Contains(err.Error(), "servers[0].url") || !strings.Contains(err.Error(), "/v1") {
		t.Errorf("error %v does not name the relative server URL it could not use", err)
	}
}

// Destination is what `auth check` asks where a call would go, and Build is
// what decides it for the call itself. They are one precedence or they are a
// disagreement waiting to happen: an `auth check` that pre-flights a different
// host than the call reaches is worse than no pre-flight at all.
//
// The comparison is against the whole target, base *and* path, because the
// path is half of what decides the host: the join is textual, so a path
// template is able to move the authority the base named.
func TestDestinationAgreesWithTheBaseURLBuildChooses(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		profile *config.Profile
	}{
		{name: "spec only"},
		{name: "flag wins", baseURL: "https://flag.example.com"},
		{name: "profile beats spec", profile: &config.Profile{Name: "p", BaseURL: "https://profile.example.com"}},
		{name: "trailing slash", baseURL: "https://flag.example.com/"},
		{
			name:    "flag beats profile",
			baseURL: "https://flag.example.com",
			profile: &config.Profile{Name: "p", BaseURL: "https://profile.example.com"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := inputs(t, "listPets")
			in.Params = []string{"limit=10"}
			in.BaseURL, in.Profile = tc.baseURL, tc.profile

			req := build(t, in)
			if got, want := Destination(in), req.BaseURL+req.Path; got != want {
				t.Errorf("Destination = %q, but Build sent it to %q", got, want)
			}
		})
	}
}

// serverSpec is a one-operation spec whose servers block a test supplies, so a
// server-variable case reads as the YAML an agent would actually meet rather
// than as a hand-built model.
const serverSpec = `openapi: 3.0.3
info:
  title: Servers
  version: 1.0.0
servers:
%s
paths:
  /pets:
    get:
      operationId: listPets
      responses:
        '200':
          description: ok
`

func serverInputs(t *testing.T, servers string) Inputs {
	t.Helper()

	doc, err := spec.LoadBytes([]byte(fmt.Sprintf(serverSpec, servers)))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	ops := operation.Extract(doc)
	if len(ops) != 1 {
		t.Fatalf("fixture has %d operations, want 1", len(ops))
	}

	return Inputs{Op: ops[0], Doc: doc}
}

// A server URL is a template: OpenAPI 3.x lets `servers[].url` carry {name}
// spans filled from `servers[].variables`. Without substitution a large class of
// 3.x specs is uncallable without --base-url, and §5a's allowed host set would
// be computed from a URL naming the host `{region}.api.example.com`.
func TestBuildSubstitutesServerVariables(t *testing.T) {
	in := serverInputs(t, `  - url: https://{region}.{env}.example.com/{version}
    variables:
      region:
        default: eu
        enum:
          - eu
          - us
      env:
        default: prod
      version:
        default: v1`)

	if got, want := build(t, in).BaseURL, "https://eu.prod.example.com/v1"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
}

// Every string here comes from the spec, which is untrusted input. A variable
// fills one segment of a URL the spec already wrote; a value that can move the
// authority turns "substitute a region" into "send the credentials somewhere
// else", which is the same class the url.PathEscape rule in binder.path guards.
func TestBuildRejectsUnusableServerVariables(t *testing.T) {
	oneVar := func(url, body string) string {
		return "  - url: " + url + "\n    variables:\n      region:\n" + body
	}

	tests := []struct {
		name    string
		servers string
		want    []string
	}{
		{
			"default outside its own enum",
			oneVar("https://{region}.example.com", "        default: dev\n        enum:\n          - eu\n          - us"),
			[]string{"region", "enum"},
		},
		{
			"no default at all",
			oneVar("https://{region}.example.com", "        enum:\n          - eu"),
			[]string{"region", "default"},
		},
		{
			"placeholder the spec declares no variable for",
			"  - url: https://{region}.example.com",
			[]string{"region"},
		},
		{
			"placeholder with an empty name",
			"  - url: https://{}.example.com",
			[]string{"placeholder"},
		},
		{
			"unmatched brace",
			"  - url: https://api{.example.com",
			[]string{"{"},
		},
		{
			// Substitute once, never to a fixed point: a default naming its own
			// placeholder has to terminate rather than expand forever.
			"self-referential default",
			oneVar("https://{region}.example.com", "        default: \"{region}\""),
			[]string{"region"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := buildErr(t, serverInputs(t, tt.servers))
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %v does not mention %q", err, want)
				}
			}
		})
	}
}

// The authority separators, one subtest each: a default carrying one of these
// must never produce a URL whose host is the value's rather than the template's.
func TestBuildRejectsAServerVariableThatCouldMoveTheHost(t *testing.T) {
	for _, value := range []string{
		"evil.com/", "evil.com#", "evil.com?", "evil.com@", "evil.com:8443",
		"a\\b", "[::1]", "{region}", "a b", "a\rb", "a\nb",
	} {
		t.Run(value, func(t *testing.T) {
			in := serverInputs(t, "  - url: https://{region}.api.example.com\n"+
				"    variables:\n      region:\n        default: "+fmt.Sprintf("%q", value))

			req, err := Build(in)
			if err == nil {
				t.Fatalf("Build accepted a host-moving default; BaseURL = %q", req.BaseURL)
			}
			if !strings.Contains(err.Error(), "region") {
				t.Errorf("error %v does not name the variable", err)
			}
			if urls := ServerURLs(in.Doc); len(urls) != 0 {
				t.Errorf("ServerURLs = %v, want a host-moving default kept out of the allowed host set", urls)
			}
		})
	}
}

// A URL is a scheme, a host and a short prefix. Without a ceiling, a template
// with many placeholders and a long default grows by their product — the spec
// chooses both.
func TestBuildBoundsTheSubstitutedServerURL(t *testing.T) {
	tests := []struct {
		name        string
		url, value  string
		placeholder int
	}{
		{"long template", "", "eu", 10000},
		{"long result", "", strings.Repeat("a", 200), 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "https://" + strings.Repeat("{region}", tt.placeholder) + ".example.com"
			in := serverInputs(t, "  - url: "+url+"\n    variables:\n      region:\n        default: "+tt.value)

			req, err := Build(in)
			if err == nil {
				t.Fatalf("Build accepted an unbounded server URL; BaseURL is %d bytes", len(req.BaseURL))
			}
			if urls := ServerURLs(in.Doc); len(urls) != 0 {
				t.Errorf("ServerURLs returned %d entries, want the unbounded server left out", len(urls))
			}
		})
	}
}

// A spec whose server variables do not substitute is not a problem for a run
// that supplied its own base URL: the spec's server is the last candidate, so it
// is only read when nothing better exists.
func TestBuildIgnoresAnUnusableServerWhenTheBaseURLIsGiven(t *testing.T) {
	in := serverInputs(t, "  - url: https://{region}.example.com")
	in.BaseURL = "https://api.example.com"

	if got, want := build(t, in).BaseURL, "https://api.example.com"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
}

// ServerURLs is what Task 2's allowed host set is computed from, so it has to
// report the servers as they will be called, not as they were written.
func TestServerURLsSubstitutesEveryServerInSpecOrder(t *testing.T) {
	in := serverInputs(t, `  - url: https://{region}.api.example.com/v1
    variables:
      region:
        default: eu
  - url: https://backup.example.com/v1`)

	got := ServerURLs(in.Doc)
	want := []string{"https://eu.api.example.com/v1", "https://backup.example.com/v1"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("ServerURLs = %v, want %v", got, want)
	}
}

// A URL that does not substitute names no host. Leaving it out keeps the host
// set narrower than the spec, which is the only direction that is safe to be
// wrong in.
func TestServerURLsLeavesOutAServerItCannotSubstitute(t *testing.T) {
	in := serverInputs(t, `  - url: https://{region}.api.example.com/v1
  - url: https://backup.example.com/v1`)

	got := ServerURLs(in.Doc)
	if len(got) != 1 || got[0] != "https://backup.example.com/v1" {
		t.Errorf("ServerURLs = %v, want only the server that substitutes", got)
	}

	if got := ServerURLs(nil); got != nil {
		t.Errorf("ServerURLs(nil) = %v, want nil", got)
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

// Userinfo is the one credential position a URL has no symbolic form for:
// `http://user:pass@host` would land verbatim in request.url, in the emitted
// curl and in history.jsonl, which is permanent. §5a makes that a rejection
// rather than a redaction — talaria already has a basic-auth path that keeps the
// value symbolic, and silently stripping the userinfo would send an
// unauthenticated request the caller believes is authenticated. Every source is
// untrusted, the spec's servers[0].url most of all.
func TestBuildRejectsCredentialsInABaseURL(t *testing.T) {
	const user, password = "admin", "s3cr3t"
	raw := "http://" + user + ":" + password + "@127.0.0.1:8898"

	tests := []struct {
		name   string
		apply  func(*Inputs)
		source string
	}{
		{"flag", func(in *Inputs) { in.BaseURL = raw }, "--base-url"},
		{"profile", func(in *Inputs) { in.Profile = &config.Profile{Name: "staging", BaseURL: raw} }, "staging"},
		{"spec server", func(in *Inputs) { in.Doc.Model.Servers[0].URL = raw }, "servers[0].url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := inputs(t, "listPets")
			in.Params = []string{"limit=10"}
			tt.apply(&in)

			err := buildErr(t, in)
			if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), user) {
				t.Errorf("the refusal echoes the userinfo it refused: %v", err)
			}
			if !strings.Contains(err.Error(), tt.source) {
				t.Errorf("error %v does not name %s as the source", err, tt.source)
			}
			if !strings.Contains(err.Error(), "127.0.0.1:8898") {
				t.Errorf("error %v does not name the host it refused", err)
			}
			if !strings.Contains(err.Error(), config.EnvBasic) {
				t.Errorf("error %v does not point at %s as the supported path", err, config.EnvBasic)
			}
		})
	}
}

// A URL too malformed for url.Parse still must not have its userinfo quoted
// back: the parse error carries the whole string, so the userinfo check has to
// come first and work on the text.
func TestBuildRejectsCredentialsInAnUnparseableBaseURL(t *testing.T) {
	const password = "s3cr3t"

	in := inputs(t, "listPets")
	in.Params = []string{"limit=10"}
	in.BaseURL = "http://admin:" + password + "@127.0.0.1:88 98"

	if err := buildErr(t, in); strings.Contains(err.Error(), password) {
		t.Errorf("the refusal echoes the userinfo it refused: %v", err)
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

	req := build(t, Inputs{Op: op, Doc: doc, Creds: creds, Hosts: specHosts(t, doc)})

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

	req := build(t, Inputs{Op: op, Doc: doc, Creds: creds, Hosts: specHosts(t, doc)})

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

// The §5a invariant, at the level of the type: a credential bound for a host
// outside the allowed set is not on the request at all. Every later surface —
// the config document, the emitted curl, history — reads the Request, so a
// credential that never lands here cannot reach the wire by any route.
func TestBuildWithholdsCredentialsFromAHostOutsideTheSet(t *testing.T) {
	op, doc := fixture(t, "getSecured")

	creds, err := config.Resolve(op, doc, nil)
	if err != nil {
		t.Fatalf("config.Resolve: %v", err)
	}
	if len(creds) == 0 {
		t.Fatal("the fixture resolved no credentials, so this test proves nothing")
	}

	req := build(t, Inputs{
		Op: op, Doc: doc, Creds: creds,
		BaseURL: "http://localhost:9000",
		Hosts:   specHosts(t, doc),
	})

	for _, group := range []struct {
		where string
		pairs []Pair
	}{
		{"header", req.Headers}, {"query", req.Query}, {"cookie", req.Cookies},
	} {
		for _, p := range group.pairs {
			if p.Value.IsSecret() {
				t.Errorf("%s %q carries a credential reference to an off-set host", group.where, p.Name)
			}
		}
	}

	if len(req.Withheld) != len(creds) {
		t.Fatalf("credentials_withheld has %d entries, want one per resolved credential (%d): %+v",
			len(req.Withheld), len(creds), req.Withheld)
	}
	for _, w := range req.Withheld {
		if w.Scheme == "" {
			t.Errorf("a withheld entry names no scheme: %+v", w)
		}
		if w.Host != "localhost:9000" {
			t.Errorf("withheld host = %q, want localhost:9000", w.Host)
		}
		if w.Reason != WithheldOffSpec {
			t.Errorf("withheld reason = %q, want %q", w.Reason, WithheldOffSpec)
		}
	}
}

// The override, and the reason withholding is not refusing: a human who names
// the twin gets the credential delivered to it.
func TestBuildDeliversCredentialsToAnAllowedHost(t *testing.T) {
	op, doc := fixture(t, "getSecured")

	creds, err := config.Resolve(op, doc, nil)
	if err != nil {
		t.Fatalf("config.Resolve: %v", err)
	}

	hosts, err := NewHostSet(ServerURLs(doc), []string{"localhost"}, nil)
	if err != nil {
		t.Fatalf("NewHostSet: %v", err)
	}

	req := build(t, Inputs{
		Op: op, Doc: doc, Creds: creds,
		BaseURL: "http://localhost:9000",
		Hosts:   hosts,
	})

	if len(req.Withheld) != 0 {
		t.Errorf("credentials_withheld = %+v, want nothing withheld from an allowed host", req.Withheld)
	}
	if got, want := find(t, req.Headers, "Authorization").Symbolic(), "Bearer $TALARIA_AUTH_BEARER"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

// Default-deny has to mean deny. A caller that never built a host set gets the
// zero value, and the zero value must not be the shortcut that lets everything
// through.
func TestBuildWithholdsCredentialsUnderTheZeroHostSet(t *testing.T) {
	op, doc := fixture(t, "getSecured")

	creds, err := config.Resolve(op, doc, nil)
	if err != nil {
		t.Fatalf("config.Resolve: %v", err)
	}

	req := build(t, Inputs{Op: op, Doc: doc, Creds: creds})

	if len(req.Withheld) != len(creds) {
		t.Errorf("the zero HostSet delivered credentials: withheld %d of %d", len(req.Withheld), len(creds))
	}
}

// The host set has to be asked about the URL curl will receive, not about the
// base URL on its own. Request.URL joins BaseURL and Path as text, so a path
// beginning `@` turns the allowed host into userinfo and the request lands on
// whatever follows it — with the credential attached, because the check said
// yes about a host that is no longer the one being talked to.
//
// White-box, and deliberately: binder.path refuses a template this shape before
// Build ever reaches credentials, so the only way to hold *this* half of the
// answer honest is to ask credentials directly. Two independent gates, two
// tests.
func TestCredentialsAskTheHostSetTheURLTheRequestWillUse(t *testing.T) {
	op, doc := fixture(t, "getSecured")

	creds, err := config.Resolve(op, doc, nil)
	if err != nil {
		t.Fatalf("config.Resolve: %v", err)
	}
	if len(creds) == 0 {
		t.Fatal("the fixture resolved no credentials, so this test proves nothing")
	}

	b := &binder{in: Inputs{Op: op, Doc: doc, Creds: creds, Hosts: specHosts(t, doc)}}
	req := &Request{
		// The base is the spec's own host, so a check that stops here says yes.
		// It carries no path of its own, which is what lets the `@` reach the
		// authority: a base ending in /v1 would make the key a path segment.
		BaseURL: "https://api.example.com",
		Path:    "@evil.example.com/steal",
	}

	b.credentials(req)

	for _, group := range []struct {
		where string
		pairs []Pair
	}{
		{"header", req.Headers}, {"query", req.Query}, {"cookie", req.Cookies},
	} {
		for _, p := range group.pairs {
			if p.Value.IsSecret() {
				t.Errorf("%s %q carries a credential to %s, which the path moved the request to",
					group.where, p.Name, "evil.example.com")
			}
		}
	}

	if len(req.Withheld) != len(creds) {
		t.Fatalf("credentials_withheld has %d entries, want one per resolved credential (%d): %+v",
			len(req.Withheld), len(creds), req.Withheld)
	}
	for _, w := range req.Withheld {
		if w.Host != "evil.example.com:443" {
			t.Errorf("withheld host = %q, want the host the request actually reaches", w.Host)
		}
	}
}

// A `paths:` key is spec-controlled text that reaches the wire, and the spec is
// untrusted. Refusing it where it is read turns a hostile document into an exit
// 2 instead of a request nobody asked for.
func TestBuildRefusesAnOperationPathThatCouldMoveTheHost(t *testing.T) {
	refused := []struct{ name, path string }{
		{"userinfo", "@evil.example.com/steal"},
		{"bare host", "evil.example.com/steal"},
		{"subdomain suffix", ".evil.example.com/steal"},
		{"backslash", "\\evil.example.com/steal"},
		{"query first", "?next=/pets"},
		{"fragment first", "#/pets"},
		{"empty", ""},
		{"just an at", "@"},
		{"leading space", " /pets"},
		{"embedded newline", "/pets\r\nX-Injected: 1"},
		{"embedded space", "/pets HTTP/1.1"},
	}

	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			in := inputs(t, "listPets")
			in.Params = []string{"limit=10"}
			in.Op.Path = tc.path

			err := buildErr(t, in)
			if !strings.Contains(err.Error(), "path") {
				t.Errorf("error %v does not name the path it refused", err)
			}
		})
	}

	// The other half: an ordinary path, and the awkward-but-legal ones, still
	// build. A refusal that also refuses `/pets` proves nothing.
	for _, path := range []string{"/pets", "/a//b", "/../pets", "/pets@archive"} {
		t.Run("allowed "+path, func(t *testing.T) {
			in := inputs(t, "listPets")
			in.Params = []string{"limit=10"}
			in.Op.Path = path

			if got := build(t, in).Path; got != path {
				t.Errorf("Path = %q, want %q", got, path)
			}
		})
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

// TestBuildAppliesTheConfiguredPatternsInEveryLocation covers the
// user-extensible half of §5a's display redaction. The config file's
// `redact.headers` is a floor-raising list, and README's promise is that it
// reaches headers, query parameters and cookies alike — the setting is named
// after the location credentials most often sit in, not the only one it covers.
func TestBuildAppliesTheConfiguredPatternsInEveryLocation(t *testing.T) {
	in := inputs(t, "getPet")
	in.Redactor = secret.NewRedactor("x-trace", "verbose", "flavour")
	in.Params = []string{"petId=1", "X-Trace=" + canary, "verbose=" + canary, "flavour=" + canary}
	in.Query = []string{"page=2"}

	req := build(t, in)

	for _, tc := range []struct {
		location string
		pairs    []Pair
		name     string
	}{
		{"header", req.Headers, "X-Trace"},
		{"query", req.Query, "verbose"},
		{"cookie", req.Cookies, "flavour"},
	} {
		value := find(t, tc.pairs, tc.name)
		if got := value.String(); got != secret.Placeholder {
			t.Errorf("%s %s displays as %q, want %q", tc.location, tc.name, got, secret.Placeholder)
		}
		if got := value.Reveal(); got != canary {
			t.Errorf("%s %s no longer carries the value it will send: %q", tc.location, tc.name, got)
		}
	}

	// The configured list extends the built-in one; it does not replace the
	// judgement that a name nothing matches is ordinary and worth showing.
	if got := find(t, req.Query, "page").String(); got != "2" {
		t.Errorf("query page = %q, want 2; only the configured names are hidden", got)
	}
}

// TestBuildWithNoConfiguredPatternsHidesTheBuiltInsOnly pins the nil case: an
// Inputs that names no Redactor — every caller before the config file reached
// this far, and every test that does not care — still gets the built-in floor
// and nothing less.
func TestBuildWithNoConfiguredPatternsHidesTheBuiltInsOnly(t *testing.T) {
	in := inputs(t, "getPet")
	in.Params = []string{"petId=1", "X-Trace=" + canary}
	in.Headers = []string{"X-Api-Key=" + canary}

	req := build(t, in)

	if got := find(t, req.Headers, "X-Api-Key").String(); got != secret.Placeholder {
		t.Errorf("header X-Api-Key = %q, want %q from the built-in list", got, secret.Placeholder)
	}
	if got := find(t, req.Headers, "X-Trace").String(); got != canary {
		t.Errorf("header X-Trace = %q; with no configured patterns it matches nothing built in", got)
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

// assertNoCanary fails when a rendered surface carries the credential the tests
// inject.
func assertNoCanary(t *testing.T, rendered string) {
	t.Helper()

	if strings.Contains(rendered, canary) {
		t.Errorf("the canary reached a rendered surface:\n%s", rendered)
	}
}
