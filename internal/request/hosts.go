package request

import (
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/spec"
)

// WithheldReason is what an envelope says when a credential was not attached.
// It is one constant rather than a sentence built per call site because the
// field is machine-readable: an agent branches on it, and a reason that varied
// by caller would make that impossible.
const WithheldReason = "host not in spec servers[]"

// HostSet is the set of hosts a resolved credential may be sent to: the hosts
// the spec declares, plus the ones a human explicitly allowed.
//
// DESIGN.md §5a makes this default-deny. Redaction answers *does it print*; it
// does not answer *who receives it*, and a --base-url is chosen by whatever is
// driving talaria — so without this a spec's bearer token goes to any host the
// caller names. Membership is by canonical authority, never by substring: a
// suffix comparison would admit api.example.com.attacker.com.
type HostSet struct {
	hosts map[string]bool
}

// AllowedHosts builds the set for one call: the spec's own servers, the values
// of --allow-host, and the selected profile's allow_hosts.
//
// The spec's servers are read through spec.Servers, never from doc.Model
// directly, so a server URL still carrying a {variable} is not mistaken for a
// host — see internal/spec/servers.go.
//
// An unusable allow entry is an error rather than a member that can never
// match. Both sources are attacker-reachable — the flag through whatever drives
// the CLI, the profile through a file — and a silently dropped entry looks
// exactly like a credential that was withheld for a good reason.
func AllowedHosts(doc *spec.Document, prof *config.Profile, allow []string) (HostSet, error) {
	set := HostSet{hosts: map[string]bool{}}
	for _, raw := range spec.Servers(doc) {
		if host, ok := hostOf(raw); ok {
			set.hosts[host] = true
		}
	}

	var problems []string
	add := func(source string, values []string) {
		for _, raw := range values {
			host, problem := allowedHost(raw)
			if problem != "" {
				problems = append(problems, source+" "+problem)
				continue
			}

			set.hosts[host] = true
		}
	}

	add("--allow-host", allow)
	if prof != nil {
		add("profile "+prof.Name+" allow_hosts", prof.AllowHosts)
	}

	if len(problems) > 0 {
		return HostSet{}, clierr.Usage("%s", strings.Join(problems, "; "))
	}

	return set, nil
}

// Allows reports whether a credential may be sent to rawURL. A URL with no host
// — or one too malformed to have one — is not allowed: the caller has already
// refused it as a base URL, and answering "yes" here would make this function's
// zero case the permissive one.
func (s HostSet) Allows(rawURL string) bool {
	host, ok := hostOf(rawURL)
	if !ok {
		return false
	}

	return s.hosts[host]
}

// Hosts lists the set's members in sorted order, so an error can offer them as
// alternatives.
func (s HostSet) Hosts() []string {
	out := make([]string, 0, len(s.hosts))
	for host := range s.hosts {
		out = append(out, host)
	}
	sort.Strings(out)

	return out
}

// Target is where a call under these settings would go, or "" when nothing
// resolves one.
//
// It is ResolveBaseURL for the caller that treats "no base URL" as an answer
// rather than a failure: `auth check` reports on the environment, and a spec
// with no servers[] and an invocation with no --base-url leave it with no host
// to report against — which is not the same as leaving it unable to report.
func Target(flag string, prof *config.Profile, doc *spec.Document) string {
	url, err := ResolveBaseURL(flag, prof, doc)
	if err != nil {
		return ""
	}

	return url
}

// Host is rawURL's authority in the canonical form this package compares, for a
// message or an envelope field that names where a request was pointed. It is
// empty for a URL with no host.
func Host(rawURL string) string {
	host, _ := hostOf(rawURL)

	return host
}

// hostOf canonicalises the authority of a URL.
func hostOf(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "", false
	}

	return canonicalHost(parsed.Host, parsed.Scheme), true
}

// allowedHost canonicalises one --allow-host or allow_hosts value, returning
// the problem with it instead when there is one.
//
// The value is a host, optionally with a port — not a URL. Accepting a URL here
// would be friendlier and wrong: `https://api.example.com/v1` has a path, and a
// caller who wrote one is expecting the path to matter, which it does not.
//
// A value carrying a CR or an LF is refused rather than trimmed for the reason
// SplitsRequest gives: this set is compared against a URL's authority, so such a
// member can never match anything, and a caller who typed one would see a
// credential silently withheld instead of a mistake reported.
func allowedHost(raw string) (host, problem string) {
	quoted := func(what string) string {
		// %q, so a CR or an LF in the value renders as an escape rather than
		// ending the line of the error it is being reported in.
		return clierr.Usage("%q %s", raw, what).Message
	}

	switch {
	case strings.TrimSpace(raw) == "":
		return "", quoted("is empty; give a host such as localhost:8080")
	case strings.ContainsAny(raw, "\r\n\t "):
		return "", quoted("carries whitespace or a line break; give a host such as localhost:8080")
	case strings.Contains(raw, "://"), strings.ContainsAny(raw, "/?#@"):
		return "", quoted("is a URL; give a host, optionally with a port, such as localhost:8080")
	}

	host = canonicalHost(raw, "")
	if host == "" || strings.HasPrefix(host, ":") {
		return "", quoted("names no host; give a host such as localhost:8080")
	}

	return host, ""
}

// canonicalHost reduces an authority to the one spelling this package compares.
//
// Case, an explicit default port and a trailing root dot are all spellings of
// the same host, and a check that missed any of them would withhold a
// credential from a host the spec really does declare. Nothing else is
// normalised: a different port is a different service, and a parent domain is
// not the same host as a subdomain of it.
//
// scheme decides which port is the default. An empty scheme — an --allow-host
// value, which has none — treats both 80 and 443 as default, since the caller
// naming a host with its default port means the same host either way.
func canonicalHost(hostport, scheme string) string {
	hostport = strings.ToLower(strings.TrimSpace(hostport))

	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, ""
	}

	// An IPv6 literal is bracketed in a URL and unbracketed once split, so it is
	// unwrapped here and rewrapped below: the two spellings are one host.
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")

	// A trailing dot names the DNS root explicitly; it resolves to the same name
	// without one. An IPv6 literal has no such form, and its colons would make
	// the test below misread it as bracketed.
	if !strings.Contains(host, ":") {
		host = strings.TrimSuffix(host, ".")
	} else {
		host = "[" + host + "]"
	}

	if isDefaultPort(scheme, port) {
		port = ""
	}
	if port == "" {
		return host
	}

	return host + ":" + port
}

// isDefaultPort reports whether port is the one a URL of this scheme would use
// if it named none.
func isDefaultPort(scheme, port string) bool {
	switch strings.ToLower(scheme) {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	case "":
		return port == "80" || port == "443"
	}

	return false
}
