package request

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
)

// mustHostSet builds a HostSet from the three list sources and fails the test
// if construction errored. Most tests here are about matching, not about
// rejecting a bad entry. The profile's own base-url is the fourth source and
// has its own tests, which call NewHostSet directly.
func mustHostSet(t *testing.T, specURLs, allowFlags, profileHosts []string) HostSet {
	t.Helper()

	set, err := NewHostSet(specURLs, allowFlags, profileHosts, "")
	if err != nil {
		t.Fatalf("NewHostSet(%q, %q, %q): %v", specURLs, allowFlags, profileHosts, err)
	}

	return set
}

func TestSpecServerAdmitsItsOwnHostAndNothingElse(t *testing.T) {
	set := mustHostSet(t, []string{"https://api.example.com"}, nil, nil)

	if !set.Allows("https://api.example.com/v1/pets") {
		t.Error("the spec's own server is not in the allowed host set")
	}
	if set.Allows("http://127.0.0.1:8765/pets") {
		t.Error("a --base-url pointing elsewhere was admitted; credentials would follow it")
	}
}

func TestDefaultPortsNormalise(t *testing.T) {
	https := mustHostSet(t, []string{"https://api.example.com"}, nil, nil)
	if !https.Allows("https://api.example.com:443/x") {
		t.Error("https://host does not match the same host with an explicit :443")
	}

	http := mustHostSet(t, []string{"http://x.com"}, nil, nil)
	if !http.Allows("http://x.com:80/y") {
		t.Error("http://host does not match the same host with an explicit :80")
	}
	if http.Allows("http://x.com:8080/y") {
		t.Error("http://host matched a different port")
	}
}

func TestHostComparisonIsCaseInsensitiveAndIgnoresPathAndScheme(t *testing.T) {
	set := mustHostSet(t, []string{"https://API.Example.COM/v1"}, nil, nil)

	if !set.Allows("https://api.example.com/v1/pets") {
		t.Error("host matching is case-sensitive; RFC 3986 says the host is not")
	}
	// The key is host:port. The scheme picks the default port but is not itself
	// part of the key, and the path is not part of it at all.
	if !set.Allows("http://api.example.com:443/completely/other/path?q=1") {
		t.Error("path or scheme leaked into the host key")
	}
	if want := "api.example.com:443"; set.Key("https://API.Example.COM/v1/pets?x=1") != want {
		t.Errorf("Key = %q, want %q", set.Key("https://API.Example.COM/v1/pets?x=1"), want)
	}
}

func TestAllowHostWithAPortMatchesOnlyThatPort(t *testing.T) {
	set := mustHostSet(t, nil, []string{"localhost:9000"}, nil)

	if !set.Allows("http://localhost:9000/pets") {
		t.Error("--allow-host localhost:9000 does not admit localhost:9000")
	}
	if set.Allows("http://localhost:9001/pets") {
		t.Error("--allow-host localhost:9000 admitted port 9001")
	}
	if set.Allows("http://localhost/pets") {
		t.Error("--allow-host localhost:9000 admitted the default port")
	}
}

func TestAllowHostWithoutAPortMatchesAnyPort(t *testing.T) {
	set := mustHostSet(t, nil, []string{"localhost"}, nil)

	for _, raw := range []string{
		"http://localhost/pets",
		"http://localhost:9000/pets",
		"https://localhost:8443/pets",
	} {
		if !set.Allows(raw) {
			t.Errorf("--allow-host localhost does not admit %q", raw)
		}
	}
	if set.Allows("http://localhost.evil.com:9000/pets") {
		t.Error("--allow-host localhost admitted a different host that merely starts with it")
	}
}

func TestTheSetIsTheUnionOfSpecServersFlagsAndProfile(t *testing.T) {
	set := mustHostSet(t,
		[]string{"https://api.example.com"},
		[]string{"localhost:9000", "twin.internal"},
		[]string{"staging.example.com:8443"},
	)

	for _, raw := range []string{
		"https://api.example.com/pets",
		"http://localhost:9000/pets",
		"http://twin.internal:1234/pets",
		"https://staging.example.com:8443/pets",
	} {
		if !set.Allows(raw) {
			t.Errorf("union does not admit %q", raw)
		}
	}
	if set.Allows("https://attacker.example/pets") {
		t.Error("union admitted a host nobody named")
	}
}

func TestKeyRendersHostAndPort(t *testing.T) {
	var set HostSet

	cases := map[string]string{
		"http://localhost:9000/pets":  "localhost:9000",
		"https://api.example.com/v1":  "api.example.com:443",
		"http://api.example.com/v1":   "api.example.com:80",
		"http://[::1]:9000/pets":      "[::1]:9000",
		"https://[2001:db8::1]/pets":  "[2001:db8::1]:443",
		"ftp://api.example.com/x":     "",
		"file:///etc/passwd":          "",
		"/relative/path":              "",
		"":                            "",
		"https://user:pass@api.x.com": "api.x.com:443",
	}
	for raw, want := range cases {
		if got := set.Key(raw); got != want {
			t.Errorf("Key(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The dangerous failure mode: a server URL talaria cannot make sense of must
// contribute nothing, never a wildcard or an empty key that matches everything.
func TestAMalformedSpecServerOpensNothing(t *testing.T) {
	set := mustHostSet(t, []string{
		"://",
		"not a url at all",
		"/v1",
		"https://",
		"ftp://files.example.com",
		"file:///etc/passwd",
		"http://%zz",
	}, nil, nil)

	if len(set.exact) != 0 || len(set.anyPort) != 0 {
		t.Fatalf("malformed servers contributed entries: exact=%v anyPort=%v", set.exact, set.anyPort)
	}
	for _, raw := range []string{
		"https://api.example.com/pets",
		"http://127.0.0.1:8765/pets",
		"",
		"/v1",
	} {
		if set.Allows(raw) {
			t.Errorf("a set built only from malformed servers admitted %q", raw)
		}
	}
}

// An empty set allows nothing. Task 3 enforces against this set, so "empty
// means allow everything" would silently disable the whole rule.
func TestTheEmptySetAllowsNothing(t *testing.T) {
	for _, set := range []HostSet{{}, mustHostSet(t, nil, nil, nil)} {
		if set.Allows("https://api.example.com/pets") {
			t.Error("the empty host set admitted a host")
		}
	}
}

func TestUserinfoInASpecServerNeverEntersTheSet(t *testing.T) {
	set := mustHostSet(t, []string{"https://user:hunter2@api.example.com"}, nil, nil)

	if !set.Allows("https://api.example.com/pets") {
		t.Error("the host behind userinfo is not in the set")
	}
	for key := range set.exact {
		if strings.ContainsAny(key, "@") || strings.Contains(key, "hunter2") {
			t.Errorf("set entry %q carries the URL's userinfo", key)
		}
	}
	if got := set.Key("https://user:hunter2@api.example.com/pets"); got != "api.example.com:443" {
		t.Errorf("Key = %q, want the host only", got)
	}
}

// Source 4: the selected profile's own base-url. A profile that names a base
// URL and a credential together is a human allowing that host, so the host set
// admits it without a redundant allow_hosts entry beside it.
func TestTheProfilesOwnBaseURLIsInTheSet(t *testing.T) {
	set, err := NewHostSet(nil, nil, nil, "https://staging.example.com/v1")
	if err != nil {
		t.Fatalf("NewHostSet: %v", err)
	}

	if !set.Allows("https://staging.example.com/v1/pets") {
		t.Error("the profile's own base-url is not in the allowed host set")
	}
	// Only the host, and only the port it named: the base URL is a URL, so its
	// port is explicit or implied by the scheme, never "any".
	if set.Allows("https://staging.example.com:8443/pets") {
		t.Error("the profile's base-url admitted a port it did not name")
	}
	if set.Allows("https://attacker.example/pets") {
		t.Error("the profile's base-url admitted a host nobody named")
	}
}

// An empty base-url is the ordinary case — no profile selected, or one that
// names none — and must add nothing rather than fail.
func TestNoProfileBaseURLAddsNothing(t *testing.T) {
	set, err := NewHostSet(nil, nil, nil, "")
	if err != nil {
		t.Fatalf("NewHostSet: %v", err)
	}
	if len(set.exact) != 0 || len(set.anyPort) != 0 {
		t.Fatalf("an empty base-url contributed entries: exact=%v anyPort=%v", set.exact, set.anyPort)
	}
}

// The base-url is human-written, so an unreadable one is exit 2 rather than a
// silent drop — the same rule --allow-host and allow_hosts follow, for the same
// reason: a dropped entry reads as allowed until a credential goes missing.
func TestAnUnusableProfileBaseURLIsRefused(t *testing.T) {
	for _, raw := range []string{
		"not a url at all",
		"://staging.example.com",
		"/v1/pets",
		"https://",
		"ftp://staging.example.com",
		"file:///etc/passwd",
		"http://%zz",
		"https://staging.example.com\n",
	} {
		set, err := NewHostSet(nil, nil, nil, raw)
		if err == nil {
			t.Errorf("base-url %q was accepted", raw)
			continue
		}
		if got := clierr.From(err).Code; got != clierr.CodeUsage {
			t.Errorf("base-url %q exits %d, want %d", raw, got, clierr.CodeUsage)
		}
		// Quoted, so a value carrying a newline is named on one line rather than
		// splitting the message the caller prints.
		if !strings.Contains(err.Error(), strconv.Quote(raw)) {
			t.Errorf("the error does not name the value it refused: %v", err)
		}
		// Failing closed: a refusal returns the zero set, which allows nothing.
		if set.Allows("https://staging.example.com/pets") {
			t.Errorf("base-url %q was refused but its host is in the returned set", raw)
		}
	}
}

// The host, and nothing else. A base-url with userinfo is refused further along
// by binder.baseURL, with the message that names the fix; what must never
// happen is the credential in it becoming part of a set entry.
func TestUserinfoInTheProfileBaseURLNeverEntersTheSet(t *testing.T) {
	set, err := NewHostSet(nil, nil, nil, "https://user:hunter2@staging.example.com")
	if err != nil {
		t.Fatalf("NewHostSet: %v", err)
	}

	for key := range set.exact {
		if strings.ContainsAny(key, "@") || strings.Contains(key, "hunter2") {
			t.Errorf("set entry %q carries the base-url's userinfo", key)
		}
	}
}

func TestAnUnusableAllowHostEntryIsRefused(t *testing.T) {
	for _, entry := range []string{
		"",
		"*",
		"*.example.com",
		"http://x.com",
		"api.example.com/v1",
		"user:pass@api.example.com",
		"api.example.com:",
		"api.example.com:http",
		"api example.com",
		"api.example.com\n",
		"api%2Eexample.com",
	} {
		if _, err := NewHostSet(nil, []string{entry}, nil, ""); err == nil {
			t.Errorf("--allow-host %q was accepted", entry)
		} else if got := clierr.From(err).Code; got != clierr.CodeUsage {
			t.Errorf("--allow-host %q exits %d, want %d", entry, got, clierr.CodeUsage)
		}

		if _, err := NewHostSet(nil, nil, []string{entry}, ""); err == nil {
			t.Errorf("allow_hosts entry %q was accepted", entry)
		}
	}
}

func TestIPv6LiteralsMatch(t *testing.T) {
	withPort := mustHostSet(t, nil, []string{"[::1]:9000"}, nil)
	if !withPort.Allows("http://[::1]:9000/pets") {
		t.Error("--allow-host [::1]:9000 does not admit [::1]:9000")
	}
	if withPort.Allows("http://[::1]:9001/pets") {
		t.Error("--allow-host [::1]:9000 admitted port 9001")
	}

	anyPort := mustHostSet(t, nil, []string{"[::1]"}, nil)
	if !anyPort.Allows("http://[::1]:9001/pets") {
		t.Error("--allow-host [::1] does not admit an arbitrary port on ::1")
	}
	if anyPort.Allows("http://[::2]:9001/pets") {
		t.Error("--allow-host [::1] admitted a different address")
	}
}

// Matching is byte-wise after lowercasing. A trailing dot and a homograph are
// different hosts, and normalising either one would widen the set.
func TestNoNameNormalisationWidensTheSet(t *testing.T) {
	set := mustHostSet(t, []string{"https://api.example.com"}, nil, nil)

	for _, raw := range []string{
		"https://api.example.com./pets",
		"https://аpi.example.com/pets", // Cyrillic а
		"https://xn--pi-fmc.example.com/pets",
	} {
		if set.Allows(raw) {
			t.Errorf("%q matched api.example.com; matching must be byte-wise", raw)
		}
	}
}

func TestALargeAllowListStillLooksUpByKey(t *testing.T) {
	entries := make([]string, 0, 10000)
	for i := range 10000 {
		entries = append(entries, "host"+strconv.Itoa(i)+".example.com:8443")
	}
	set := mustHostSet(t, nil, nil, entries)

	// Lookup is a map index, so the set's size does not change what it costs.
	if len(set.exact) != len(entries) {
		t.Fatalf("exact has %d entries, want %d", len(set.exact), len(entries))
	}
	if !set.Allows("https://" + strings.TrimSuffix(entries[9999], ":8443") + ":8443/pets") {
		t.Error("the last of 10,000 entries is not in the set")
	}
	if set.Allows("https://not-in-the-list.example.com:8443/pets") {
		t.Error("a host outside a 10,000-entry list was admitted")
	}
}
