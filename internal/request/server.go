package request

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	v3high "github.com/pb33f/libopenapi/datamodel/high/v3"

	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/spec"
)

// Destination is where a call built from these inputs would go: the base URL —
// --base-url, then the profile's, then the spec's first server (DESIGN.md §4) —
// joined to in.Op's path, exactly as Request.URL joins them. The candidate is
// returned as it stands, without the check that it is usable as a base URL: an
// unusable one still names the host a caller meant.
//
// The path is part of the answer, not decoration. The join is textual, so a
// `paths:` key beginning `@` makes the base URL the userinfo of a request to
// another host; a destination computed without it names a host the call never
// reaches.
//
// It exists so a command that needs only the *host* — `auth check`, deciding
// whether a call under these same flags would have its credentials withheld —
// asks the same question binder.baseURL does rather than re-deriving the
// precedence. Two answers to "where would this go" is how `auth check` and
// `call` came to disagree.
//
// The empty string means nothing named a destination: no flag, no profile, and
// a spec with no server or one whose variables do not substitute.
func Destination(in Inputs) string {
	base := ""
	for _, c := range baseURLCandidates(in) {
		if c.raw != "" {
			base = c.raw
			break
		}
	}
	if base == "" {
		base, _ = firstServer(in.Doc)
	}
	if base == "" {
		return ""
	}

	return strings.TrimSuffix(base, "/") + in.Op.Path
}

// Withholds reports whether a call to any of ops, under these inputs, would
// have its credentials withheld because the host it reaches is outside
// in.Hosts (§5a). It is the whole of what `auth check`'s pre-flight asks.
//
// It is per operation because the destination is per operation: the base URL is
// one string for the whole spec, but the path joined to it is not, and a single
// `paths:` key able to move the authority is enough to divert a credential. One
// operation off the set is reported for the spec, because `auth check` answers
// about the spec.
func Withholds(in Inputs, ops []operation.Operation) bool {
	if len(ops) == 0 {
		return offSet(in)
	}

	for _, op := range ops {
		in.Op = op
		if offSet(in) {
			return true
		}
	}

	return false
}

// offSet reports whether these inputs name a destination outside in.Hosts. A
// destination it cannot name is not a withholding: there is no host to be
// outside the set.
func offSet(in Inputs) bool {
	dest := Destination(in)

	return dest != "" && !in.Hosts.Allows(dest)
}

// baseURLCandidate is one possible base URL and where it came from, so a
// rejection can name the source the caller has to fix.
type baseURLCandidate struct{ source, raw string }

// baseURLCandidates is the flag and profile halves of DESIGN.md §4's
// precedence, most specific first. The spec's server is deliberately not in the
// list: it is substituted, which can fail, and only binder.baseURL reports that.
func baseURLCandidates(in Inputs) []baseURLCandidate {
	out := []baseURLCandidate{{"--base-url", in.BaseURL}}
	if in.Profile != nil {
		out = append(out, baseURLCandidate{"profile " + in.Profile.Name, in.Profile.BaseURL})
	}

	return out
}

// baseURL resolves where the request goes, and reports what it could not use.
func (b *binder) baseURL() string {
	for _, c := range baseURLCandidates(b.in) {
		if c.raw == "" {
			continue
		}

		return b.absoluteBase(c.source, c.raw)
	}

	// Last, and substituted only here: a spec whose server variables do not
	// resolve is not a problem for a run that supplied its own base URL.
	raw, err := firstServer(b.in.Doc)
	switch {
	case err != nil:
		// Reported once. A spec that declares a server talaria cannot use must
		// not then be reported a second time as declaring no server at all.
		b.fail("%s", err)
		return ""
	case raw != "":
		return b.absoluteBase("the spec's servers[0].url", raw)
	}

	b.fail("no base URL: the spec declares no server, so pass --base-url or set one in a profile")

	return ""
}

// absoluteBase holds one base URL candidate to what a base URL may be, and
// returns it ready to have a path joined to it.
func (b *binder) absoluteBase(source, raw string) string {
	// Before the parse, so a URL that fails to parse cannot have its userinfo
	// quoted back by the message below.
	if host, ok := Userinfo(raw); ok {
		b.fail("base URL from %s carries a credential in its userinfo (user:password@%s); "+
			"remove it and set %s=user:password instead, which keeps the value out of the "+
			"request, the emitted curl and the history", source, host, config.EnvBasic)
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || !IsHTTPScheme(parsed.Scheme) {
		b.fail("base URL %q from %s is not an absolute http(s) URL", raw, source)
		return ""
	}

	return strings.TrimSuffix(raw, "/")
}

// ServerURLs returns every servers[].url with its variables substituted, in
// spec order.
//
// A server whose variables do not substitute is left out rather than returned
// as written: an unusable URL names no host it is safe to talk to, and the
// allowed host set §5a builds from these may only ever be narrower than the
// spec, never wider.
func ServerURLs(doc *spec.Document) []string {
	if doc == nil || doc.Model == nil {
		return nil
	}

	out := make([]string, 0, len(doc.Model.Servers))
	for _, s := range doc.Model.Servers {
		if s == nil {
			continue
		}

		u, err := serverURL(s)
		if err != nil || u == "" {
			continue
		}

		out = append(out, u)
	}

	return out
}

// firstServer returns the document's first server URL with its variables
// substituted. The error is a whole sentence naming the URL, because the only
// caller that reports it has nothing else to add.
func firstServer(doc *spec.Document) (string, error) {
	if doc == nil || doc.Model == nil || len(doc.Model.Servers) == 0 || doc.Model.Servers[0] == nil {
		return "", nil
	}

	u, err := serverURL(doc.Model.Servers[0])
	if err != nil {
		return "", fmt.Errorf("the spec's servers[0].url %q %s", doc.Model.Servers[0].URL, err)
	}

	return u, nil
}

// maxServerURL bounds a server URL before and after substitution. A base URL is
// a scheme, a host and a short prefix; nothing legitimate approaches this. The
// ceiling matters because both halves of the growth are spec-controlled: a
// template may repeat a placeholder and the variable it names may default to a
// long value, so the result is their product.
const maxServerURL = 8192

// serverURL is one servers[] entry's URL with its `{name}` spans replaced by the
// defaults of the variables it declares (OpenAPI 3.x servers[].variables). §5a
// defines the allowed host set as the servers *after* substitution, so this is
// what "the spec's server" means everywhere downstream.
//
// The pass is a single left-to-right sweep and a substituted value is never
// re-examined, so a variable whose default names a placeholder terminates
// instead of expanding.
//
// The returned error completes a sentence whose subject is the URL, so a caller
// can prefix it with wherever the URL came from.
func serverURL(s *v3high.Server) (string, error) {
	raw := s.URL
	if len(raw) > maxServerURL {
		return "", fmt.Errorf("is %d bytes long, past the %d-byte limit for a server URL", len(raw), maxServerURL)
	}
	if !strings.ContainsAny(raw, "{}") {
		return raw, nil
	}

	var out strings.Builder
	for rest := raw; ; {
		before, after, open := strings.Cut(rest, "{")
		if !open {
			out.WriteString(rest)
			break
		}

		name, tail, closed := strings.Cut(after, "}")
		if !closed {
			// Written back as it stands so the unmatched-brace check below is
			// the one place an unusable template is reported.
			out.WriteString(before + "{" + after)
			break
		}

		value, err := serverVariable(s, name)
		if err != nil {
			return "", err
		}

		out.WriteString(before)
		out.WriteString(value)
		if out.Len() > maxServerURL {
			return "", fmt.Errorf("grows past %d bytes once its variables are substituted", maxServerURL)
		}

		rest = tail
	}

	// serverVariable rejects a value containing a brace, so a brace surviving
	// the sweep is one the template never closed or never opened.
	got := out.String()
	if strings.ContainsAny(got, "{}") {
		return "", errors.New("has a { or } that opens or closes no variable")
	}

	return got, nil
}

// authorityChars are the characters a server variable's value may not contain.
// A variable fills a segment of a URL the spec already wrote; a value carrying
// one of these rewrites the URL's shape instead — a `region` defaulting to
// `evil.com#` turns `https://{region}.api.example.com` into a request to
// evil.com, and credentials follow the host. This is the same class of bug that
// url.PathEscape closes in binder.path, and the same answer: a value stays
// inside the field it was given.
const authorityChars = "/?#@:[]\\{}"

// serverVariable is the value to substitute for one `{name}` span.
func serverVariable(s *v3high.Server, name string) (string, error) {
	var v *v3high.ServerVariable
	if s.Variables != nil {
		v = s.Variables.GetOrZero(name)
	}
	if v == nil {
		return "", fmt.Errorf("has placeholder {%s}, which names no variable it declares", name)
	}

	// OpenAPI requires `default`; an absent one must fail rather than
	// substitute the empty string into a host.
	if v.Default == "" {
		return "", fmt.Errorf("declares variable %q with no default, which OpenAPI requires", name)
	}

	if len(v.Enum) > 0 && !slices.Contains(v.Enum, v.Default) {
		return "", fmt.Errorf("declares variable %q with default %q, which is not one of its enum values (%s)",
			name, v.Default, strings.Join(v.Enum, ", "))
	}

	if strings.ContainsAny(v.Default, authorityChars) || hasControl(v.Default) {
		return "", fmt.Errorf("declares variable %q with default %q, which holds a character that could move the URL's host",
			name, v.Default)
	}

	return v.Default, nil
}
