package request

import (
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/secret"
)

// bearer is the one credential the host-binding tests bind, so an assertion
// about "the credential" is unambiguous.
func bearer() []config.Credential {
	return []config.Credential{{
		Scheme: "bearerAuth",
		Kind:   config.KindBearer,
		In:     config.InHeader,
		Name:   "Authorization",
		Ref:    secret.Env(config.EnvBearer),
	}}
}

// A credential goes to a host the spec declares and to nowhere else. This is
// the unit-level half of the exfiltration finding: --base-url is the flag an
// agent controls, and pointing it anywhere off-spec must not hand that host a
// production token.
func TestBuildWithholdsACredentialFromAnOffSpecHost(t *testing.T) {
	in := inputs(t, "getPet")
	in.Params = []string{"petId=42"}
	in.Creds = bearer()
	in.BaseURL = "https://elsewhere.example"

	req := build(t, in)

	for _, p := range req.Headers {
		if p.Value.IsSecret() {
			t.Fatalf("header %q carries a credential for an off-spec host", p.Name)
		}
	}
	if len(req.Withheld) != 1 {
		t.Fatalf("Withheld = %+v, want one entry", req.Withheld)
	}
	if got := req.Withheld[0]; got.Scheme != "bearerAuth" || got.Host != "elsewhere.example" {
		t.Errorf("Withheld[0] = %+v, want scheme bearerAuth and host elsewhere.example", got)
	}
	if req.Withheld[0].Reason == "" {
		t.Error("Withheld[0].Reason is empty; the envelope has to say why")
	}
}

// The no-regression case: a --base-url the spec itself declares is not an
// off-spec host, and the credential goes as it always did.
func TestBuildSendsACredentialToADeclaredServer(t *testing.T) {
	in := inputs(t, "getPet")
	in.Params = []string{"petId=42"}
	in.Creds = bearer()
	in.BaseURL = "https://backup.example.com/v1"

	req := build(t, in)

	if got := find(t, req.Headers, "Authorization"); !got.IsSecret() {
		t.Errorf("Authorization = %v, want the credential reference", got)
	}
	if len(req.Withheld) != 0 {
		t.Errorf("Withheld = %+v, want none for a declared server", req.Withheld)
	}
}

// --allow-host is the deliberate override: a human naming the host puts the
// credential back on the wire.
func TestAllowHostPutsTheCredentialBack(t *testing.T) {
	in := inputs(t, "getPet")
	in.Params = []string{"petId=42"}
	in.Creds = bearer()
	in.BaseURL = "http://localhost:9000"
	in.AllowHosts = []string{"localhost:9000"}

	req := build(t, in)

	if got := find(t, req.Headers, "Authorization"); !got.IsSecret() {
		t.Errorf("Authorization = %v, want the credential reference", got)
	}
	if len(req.Withheld) != 0 {
		t.Errorf("Withheld = %+v, want none once the host is allowed", req.Withheld)
	}
}

// A profile's allow_hosts is the same override, written down once instead of
// typed on every call.
func TestProfileAllowHostsPutsTheCredentialBack(t *testing.T) {
	in := inputs(t, "getPet")
	in.Params = []string{"petId=42"}
	in.Creds = bearer()
	in.BaseURL = "http://twin.internal:8080"
	in.Profile = &config.Profile{Name: "twin", AllowHosts: []string{"twin.internal:8080"}}

	req := build(t, in)

	if got := find(t, req.Headers, "Authorization"); !got.IsSecret() {
		t.Errorf("Authorization = %v, want the credential reference", got)
	}
}

// Host comparison is where this fix lives or dies. A suffix match would admit
// api.example.com.attacker.com, and a byte comparison would refuse the same
// host written with a default port or a trailing dot.
func TestAllowedHostsComparesHostsWithoutBeingFooled(t *testing.T) {
	cases := []struct {
		name    string
		allow   string
		baseURL string
		want    bool
	}{
		{"case folds", "API.Example.COM", "https://api.example.com", true},
		{"default https port", "api.example.com", "https://api.example.com:443", true},
		{"default http port", "api.example.com", "http://api.example.com:80", true},
		{"trailing dot", "api.example.com", "https://api.example.com./v1", true},
		{"ipv6 literal", "[::1]:9000", "http://[::1]:9000", true},
		{"ipv6 case", "[FE80::1]", "http://[fe80::1]", true},
		{"suffix is not a match", "api.example.com", "https://api.example.com.attacker.com", false},
		{"prefix is not a match", "api.example.com", "https://evil-api.example.com", false},
		{"a non-default port is a different host", "api.example.com", "https://api.example.com:8443", false},
		{"a different ipv6 port", "[::1]:9000", "http://[::1]:9001", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := AllowedHosts(nil, nil, []string{tc.allow})
			if err != nil {
				t.Fatalf("AllowedHosts(%q): %v", tc.allow, err)
			}

			if got := set.Allows(tc.baseURL); got != tc.want {
				t.Errorf("Allows(%q) with --allow-host %q = %v, want %v",
					tc.baseURL, tc.allow, got, tc.want)
			}
		})
	}
}

// --allow-host is attacker-reachable through the agent, so junk in it is a
// usage error rather than a malformed member of the set. A value carrying a
// CRLF matters most: the set is compared against a URL's authority, and a
// member that spans two lines is a member no URL can ever equal — a silent
// permanent withholding rather than a reported mistake.
func TestAllowHostRejectsJunk(t *testing.T) {
	cases := []struct {
		name  string
		allow string
	}{
		{"empty", ""},
		{"blank", "   "},
		{"a whole URL", "https://api.example.com/v1"},
		{"a path", "api.example.com/v1"},
		{"userinfo", "user:pass@api.example.com"},
		{"carriage return and newline", "api.example.com\r\nX-Injected: 1"},
		{"a bare port", ":8080"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := AllowedHosts(nil, nil, []string{tc.allow}); err == nil {
				t.Fatalf("AllowedHosts(%q) succeeded, want a usage error", tc.allow)
			}

			in := inputs(t, "getPet")
			in.Params = []string{"petId=42"}
			in.AllowHosts = []string{tc.allow}

			err := buildErr(t, in)
			if !strings.Contains(err.Error(), "--allow-host") {
				t.Errorf("error does not name the flag it came from: %v", err)
			}
		})
	}
}

// The spec's own servers are read through spec.Servers, so a server whose URL
// is a {variable} template never becomes a host anything is trusted with.
func TestAllowedHostsReadsTheSpecThroughServers(t *testing.T) {
	_, doc := fixture(t, "getPet")

	set, err := AllowedHosts(doc, nil, nil)
	if err != nil {
		t.Fatalf("AllowedHosts: %v", err)
	}

	for _, want := range []string{"https://api.example.com/v1", "https://backup.example.com/v1"} {
		if !set.Allows(want) {
			t.Errorf("Allows(%q) = false, want true: the spec declares it", want)
		}
	}
	if set.Allows("https://api.invalid") {
		t.Error("Allows(https://api.invalid) = true, want false")
	}
}
