package request

import (
	"net/url"
	"strings"

	"github.com/Teeeep/talaria/internal/clierr"
)

// HostSet is the set of hosts a resolved credential may be sent to: DESIGN.md
// §5a's "every host in the spec's servers[], plus every --allow-host, plus
// every allow_hosts entry in the active profile".
//
// It is default-deny and has no wildcard. The zero HostSet allows nothing, and
// so does one built from a spec whose servers all failed to parse — the set may
// be narrower than the spec, never wider. Nothing here enforces anything; the
// caller asks Allows and decides what to withhold.
type HostSet struct {
	// exact holds host:port keys, matched whole. A spec server and an entry
	// that named a port land here.
	exact map[string]bool
	// anyPort holds bare lowercase hostnames, matched on any port. Only an
	// --allow-host or allow_hosts entry written without a port lands here: a
	// human saying "localhost" means the twin, whatever port it came up on.
	anyPort map[string]bool
}

// NewHostSet builds the allowed host set from its three sources.
//
// The two halves are treated differently on purpose. specURLs comes from the
// spec, which is untrusted: an entry that does not parse, is relative, names no
// host or speaks a scheme talaria will not use contributes nothing, silently —
// it has already been reported where it was read. allowFlags and profileHosts
// were written by a human, so an entry that is not a host is a bad invocation
// and says so rather than being dropped, which would leave the user believing a
// host was allowed when it was not.
func NewHostSet(specURLs, allowFlags, profileHosts []string) (HostSet, error) {
	set := HostSet{
		exact:   make(map[string]bool, len(specURLs)+len(allowFlags)+len(profileHosts)),
		anyPort: map[string]bool{},
	}

	for _, raw := range specURLs {
		if _, key := splitHost(raw); key != "" {
			set.exact[key] = true
		}
	}

	for _, source := range []struct {
		label   string
		entries []string
	}{
		{"--allow-host", allowFlags},
		{"allow_hosts in the profile", profileHosts},
	} {
		for _, entry := range source.entries {
			if err := set.allow(source.label, entry); err != nil {
				return HostSet{}, err
			}
		}
	}

	return set, nil
}

// allow adds one human-written `host` or `host:port` entry.
func (s HostSet) allow(source, entry string) error {
	host, port, err := parseHostEntry(source, entry)
	if err != nil {
		return err
	}

	if port == "" {
		s.anyPort[host] = true
		return nil
	}
	s.exact[authority(host, port)] = true

	return nil
}

// Allows reports whether a resolved credential may be sent to rawURL.
//
// A URL this cannot make sense of is not allowed: the answer to "is this host
// in the set" for something that names no host is no.
func (s HostSet) Allows(rawURL string) bool {
	host, key := splitHost(rawURL)
	if key == "" {
		return false
	}

	return s.exact[key] || s.anyPort[host]
}

// Key is rawURL's host:port, the form DESIGN.md §5a's credentials_withheld[].host
// shows ("localhost:9000"). It is the empty string for a URL with no usable
// host, and an IPv6 literal comes back bracketed so the result is a valid
// authority.
func (s HostSet) Key(rawURL string) string {
	_, key := splitHost(rawURL)

	return key
}

// splitHost returns rawURL's lowercase hostname and its host:port key, or two
// empty strings if rawURL names no host talaria would make a request to.
//
// The port is always explicit in the key, filled in from the scheme when the
// URL omitted it, so https://api.example.com and https://api.example.com:443
// are one entry rather than two. Nothing else about the URL survives: not the
// scheme, not the path, and not the userinfo — url.Parse keeps that out of
// Hostname, which is what stops a credential in a servers[].url becoming part
// of a set entry.
//
// No name normalisation happens beyond lowercasing. A trailing dot and a
// punycode homograph are different hosts, and folding either into its neighbour
// would admit a host nobody named.
func splitHost(rawURL string) (host, key string) {
	u, err := url.Parse(rawURL)
	if err != nil || !IsHTTPScheme(u.Scheme) {
		return "", ""
	}

	host = strings.ToLower(u.Hostname())
	if host == "" {
		return "", ""
	}

	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}

	return host, authority(host, port)
}

// defaultPort is the port a scheme implies when a URL omits it.
func defaultPort(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return "443"
	}

	return "80"
}

// authority joins a hostname and a port back into a `host:port` key, bracketing
// an IPv6 literal so the colons in the address cannot be read as the separator.
func authority(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}

	return host + ":" + port
}

// hostEntryChars are the characters a `host[:port]` entry may not contain. Each
// one either belongs to a part of a URL an entry does not name (`/?#` a path or
// query, `@` a userinfo) or would let one entry stand for hosts nobody wrote
// down (`*` a wildcard, `%` an escape whose decoded form differs from the text).
// `\` is here because Windows and some parsers read it as `/`.
const hostEntryChars = "/?#@*%\\"

// parseHostEntry reads one human-written `host` or `host:port` and returns the
// lowercase hostname and the port, with an empty port meaning "any".
//
// It is strict rather than forgiving: an entry it cannot read as exactly a host
// is refused with exit 2, because the alternative — dropping it — leaves the
// user believing a host is allowed when it is not, and the failure only shows
// up as a credential silently withheld much later.
func parseHostEntry(source, entry string) (host, port string, err error) {
	bad := func() (string, string, error) {
		return "", "", clierr.Usage(
			"%s %q is not a host: give a bare host or host:port, such as api.example.com, "+
				"localhost:9000 or [::1]:9000", source, entry)
	}

	if entry == "" || strings.ContainsAny(entry, hostEntryChars) || hasControl(entry) {
		return bad()
	}
	// url.Parse accepts a trailing colon as an empty port, which would mean
	// "any port" by a different spelling than the bare host.
	if strings.HasSuffix(entry, ":") {
		return bad()
	}

	// Parsed as an authority rather than a URL: `localhost:9000` on its own is
	// a scheme and an opaque body to url.Parse, and the leading `//` is what
	// makes it read the string as `host:port`. It also gets IPv6 bracketing
	// right, which hand-splitting on the last colon does not.
	u, err := url.Parse("//" + entry)
	if err != nil || u.Host != entry {
		return bad()
	}

	if host = strings.ToLower(u.Hostname()); host == "" {
		return bad()
	}

	return host, u.Port(), nil
}
