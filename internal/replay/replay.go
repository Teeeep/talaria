// Package replay re-derives a runnable request from a recorded history entry.
//
// DESIGN.md §5a's rule is that a history entry is data, never instruction. The
// entry says which operation ran, with which parameters and which body. It does
// not say where the request goes — that comes from --base-url, the profile or
// the spec — and it does not say which credential to attach, because nothing in
// it is resolved: credentials come back through config.Resolve exactly as they
// do for a call.
//
// It lives here rather than in cmd/talaria because it is a whole transformation
// rather than wiring: every refusal below is a decision with a test, and none of
// it needs a command tree to exercise.
package replay

import (
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
	"github.com/Teeeep/talaria/internal/spec"
)

// Inputs is one `history replay`: a stored entry, plus the spec, the flags and
// the environment in force now. The entry is the only untrusted member.
type Inputs struct {
	Entry      corpus.Entry
	Doc        *spec.Document
	Index      *operation.Index
	Profile    *config.Profile
	BaseURL    string
	AllowHosts []string
	// Redactor is the config file's display patterns, passed through so a
	// replayed request hides the same fields the original did.
	Redactor *secret.Redactor
	// Stderr carries the warnings for fields the entry could not give back.
	Stderr io.Writer
}

// Build rebuilds the runnable request, or fails *this entry* with exit 2.
//
// Every refusal below is per-entry rather than per-process, which is what makes
// a hostile store survivable: one edited line stops one replay, and `history`,
// `history show` and every other entry go on working.
func Build(in Inputs) (*request.Request, error) {
	op, err := in.operation()
	if err != nil {
		return nil, err
	}

	stored, err := in.storedURL()
	if err != nil {
		return nil, err
	}

	body, err := in.body()
	if err != nil {
		return nil, err
	}

	target, err := in.target(stored)
	if err != nil {
		return nil, err
	}

	params, err := pathParams(op, stored.EscapedPath())
	if err != nil {
		return nil, err
	}

	query, extra, err := in.query(op, stored.RawQuery)
	if err != nil {
		return nil, err
	}

	declaredHeaders, headerFlags := in.headers(op)

	creds, err := config.Resolve(op, in.Doc, in.Profile)
	if err != nil {
		return nil, err
	}

	req, err := request.Build(request.Inputs{
		Op:         op,
		Doc:        in.Doc,
		Profile:    in.Profile,
		Creds:      creds,
		BaseURL:    target,
		AllowHosts: in.AllowHosts,
		Params:     append(append(params, query...), declaredHeaders...),
		Query:      extra,
		Headers:    headerFlags,
		Redactor:   in.Redactor,
	})
	if err != nil {
		return nil, err
	}

	// Set after Build rather than through Inputs.Body: that field carries
	// --body's three forms, so a stored body beginning `@` or equal to `-` would
	// be read as a file path or as this process's stdin. The bytes recorded are
	// the bytes to send, and nothing about them selects a source.
	req.Body = body

	return req, nil
}

// operation looks the entry's operationId up in the *current* spec. An entry
// that names none, or one the spec no longer has, cannot be re-derived — and
// re-deriving is the whole of what replay now does.
func (in Inputs) operation() (operation.Operation, error) {
	if in.Entry.OperationID == "" {
		return operation.Operation{}, clierr.Usage(
			"the entry records no operationId, so there is nothing in the spec to replay it against")
	}

	op, err := in.Index.Lookup(in.Entry.OperationID)
	if err != nil {
		return operation.Operation{}, err
	}

	// The method is part of what the operation is. An entry whose method has
	// drifted from the spec's is either a spec that changed under it or a line
	// somebody edited; either way, sending the stored one would issue a request
	// this spec does not describe.
	if !strings.EqualFold(in.Entry.Method, op.Method) {
		return operation.Operation{}, clierr.Usage(
			"the entry records a %s but %s is a %s in this spec",
			in.Entry.Method, op.ID, op.Method)
	}

	return op, nil
}

// storedURL parses the entry's URL, which is read for its path and its query
// and never for its host.
func (in Inputs) storedURL() (*url.URL, error) {
	// The store is a file on disk, so what it holds is checked on the way out as
	// well as on the way in. Both checks come before the parse error below,
	// which quotes the URL it could not read.
	if host, ok := request.Userinfo(in.Entry.URL); ok {
		return nil, clierr.Usage(
			"the recorded URL for %s carries a credential in its userinfo, so it will not be replayed; "+
				"re-run the call with %s=user:password set instead", host, config.EnvBasic)
	}

	parsed, err := url.Parse(in.Entry.URL)
	if err != nil {
		return nil, clierr.Usage("the recorded URL %q cannot be parsed: %v", in.Entry.URL, err)
	}
	if !request.IsHTTPScheme(parsed.Scheme) {
		return nil, clierr.Usage("the recorded URL %q has scheme %q; only http and https can be replayed",
			in.Entry.URL, parsed.Scheme)
	}

	return parsed, nil
}

// target is where the replay actually goes: --base-url, the profile, or the
// spec — never the stored URL (DESIGN.md:407).
//
// The stored host still has to agree with it. Silently retargeting a recorded
// call at a different host would make `replay` mean something other than "do
// that again", so a stored host that is neither the target nor in the allowed
// set fails the entry rather than being quietly redirected.
func (in Inputs) target(stored *url.URL) (string, error) {
	target, err := request.ResolveBaseURL(in.BaseURL, in.Profile, in.Doc)
	if err != nil {
		return "", err
	}

	storedHost := request.Host(in.Entry.URL)
	if storedHost == request.Host(target) {
		return target, nil
	}

	allowed, err := request.AllowedHosts(in.Doc, in.Profile, in.AllowHosts)
	if err != nil {
		return "", err
	}
	if allowed.Allows(in.Entry.URL) {
		return target, nil
	}

	return "", clierr.Usage(
		"the entry was recorded against %s, which is neither where this invocation would send (%s) "+
			"nor a host the spec declares; replay will not silently retarget it, so pass --base-url "+
			"or --allow-host %s if that is what you mean",
		storedHost, request.Host(target), storedHost)
}

// body returns the recorded request body as the bytes to send.
//
// A body holding a redaction placeholder is refused. The store is written
// redacted, so such a body is one talaria itself hollowed out — sending it would
// put the literal text `<redacted>` where a client_secret stood, which is not
// the request that was recorded and is not one anybody asked for.
func (in Inputs) body() (*request.Body, error) {
	stored := in.Entry.Request.Body
	if stored == nil {
		return nil, nil
	}
	if stored.Truncated {
		return nil, clierr.Usage(
			"the recorded request body was truncated at %d bytes, so replaying it would send something the original did not",
			corpus.MaxBody)
	}

	data, err := stored.Bytes()
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(data), secret.Placeholder) {
		return nil, clierr.Usage(
			"the recorded request body holds a redacted value, which history does not store, " +
				"so it cannot be replayed; re-run the call instead")
	}

	return &request.Body{ContentType: stored.ContentType, Data: data}, nil
}

// query splits the recorded query string into the parameters the operation
// declares and the ones it does not, so a declared one is bound and checked
// like any other rather than appended raw.
//
// A value that is a redaction is dropped with a warning: it was a credential
// position, and config.Resolve is what puts a credential back.
func (in Inputs) query(op operation.Operation, raw string) (declared, extra []string, err error) {
	locations := declaredParams(op)
	seen := map[string]bool{}

	// Walked rather than url.ParseQuery'd because a map would lose the order,
	// and a replay that reorders the query string is not the same request.
	for _, field := range strings.Split(raw, "&") {
		if field == "" {
			continue
		}

		rawName, rawValue, _ := strings.Cut(field, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			return nil, nil, clierr.Usage("the recorded query parameter %q cannot be parsed: %v", rawName, err)
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return nil, nil, clierr.Usage("the recorded value of query parameter %q cannot be parsed: %v", name, err)
		}

		if isRedacted(value) {
			warnUnreplayable(in.Stderr, "query parameter", name)
			continue
		}

		// The first occurrence binds as the declared parameter; later ones go to
		// the raw query, which appends. binder.params is a map[string]string, so
		// routing every occurrence through it collapsed ?tag=a&tag=b — the
		// default array encoding, and what `call` emits from repeated --query —
		// to ?tag=b, at exit 0 with nothing on stderr. Order is preserved, so the
		// bytes on the wire are the bytes that were recorded.
		if locations[name] == "query" && !seen[name] {
			seen[name] = true
			declared = append(declared, name+"="+value)

			continue
		}
		extra = append(extra, name+"="+value)
	}

	return declared, extra, nil
}

// headers returns the recorded headers as --header flags, dropping the ones a
// stored entry must not decide.
//
// Anything that is a redaction goes: a `<redacted:env:NAME>` is the store
// naming a variable, and nothing in an entry is resolved — the credential comes
// back through config.Resolve or not at all. Passing it through as a literal
// would put that text on the wire, and resolving it would make talaria a "read
// $ANY_VAR and send it" primitive driven by a file.
func (in Inputs) headers(op operation.Operation) (declared, flags []string) {
	locations := declaredParams(op)

	flags = make([]string, 0, len(in.Entry.Request.Headers))
	for _, name := range slices.Sorted(maps.Keys(in.Entry.Request.Headers)) {
		value := in.Entry.Request.Headers[name]
		if isRedacted(value) {
			warnUnreplayable(in.Stderr, "header", name)
			continue
		}

		// By declaration, not by transport. A header the operation declares as a
		// parameter has to bind as one: routed to --header instead, the binder
		// still reports it missing, and an entry talaria had just written came
		// back "--param X-Tenant is required" with no flag able to satisfy it.
		if locations[name] == "header" {
			declared = append(declared, name+"="+value)
			continue
		}

		flags = append(flags, name+"="+value)
	}

	// A cookie has no flag of its own, so a recorded one is replayable only as a
	// declared parameter. This used to warn and then send nothing at all: the
	// replay exited 0 and the wire carried no Cookie header, a different request
	// from the one `history show` displays.
	for _, name := range slices.Sorted(maps.Keys(in.Entry.Request.Cookies)) {
		value := in.Entry.Request.Cookies[name]
		if locations[name] != "cookie" || isRedacted(value) {
			warnUnreplayable(in.Stderr, "cookie", name)
			continue
		}

		declared = append(declared, name+"="+value)
	}

	return declared, flags
}

// pathParams recovers the operation's path parameters by matching the stored
// path against the operation's path template.
//
// The template is matched against the *tail* of the stored path: a recorded URL
// carries whatever prefix its server had (/v1, /api/v2), and the replay's own
// base URL supplies its own. Everything before the template's segments is
// therefore discarded rather than compared.
func pathParams(op operation.Operation, storedPath string) ([]string, error) {
	want := pathSegments(op.Path)
	got := pathSegments(storedPath)
	if len(got) < len(want) {
		return nil, clierr.Usage(
			"the recorded path %q has %d segments, too few for %s's %q",
			storedPath, len(got), op.ID, op.Path)
	}
	got = got[len(got)-len(want):]

	var params []string
	for i, segment := range want {
		name, ok := strings.CutPrefix(segment, "{")
		if !ok || !strings.HasSuffix(name, "}") {
			if segment != got[i] {
				return nil, clierr.Usage(
					"the recorded path %q does not match %s's %q", storedPath, op.ID, op.Path)
			}
			continue
		}

		value, err := url.PathUnescape(got[i])
		if err != nil {
			return nil, clierr.Usage(
				"the recorded path segment for %s will not percent-decode: %v",
				strings.TrimSuffix(name, "}"), err)
		}

		params = append(params, strings.TrimSuffix(name, "}")+"="+value)
	}

	return params, nil
}

// pathSegments splits a path into its non-empty segments, so a leading or
// trailing slash does not change the count either side of the comparison.
func pathSegments(path string) []string {
	var out []string
	for _, segment := range strings.Split(path, "/") {
		if segment != "" {
			out = append(out, segment)
		}
	}

	return out
}

// declaredParams maps each parameter the operation declares to where it goes,
// so a recorded value can be routed to the flag that binds it.
func declaredParams(op operation.Operation) map[string]string {
	out := make(map[string]string, len(op.Params))
	for _, p := range op.Params {
		out[p.Name] = p.In
	}

	return out
}

// isRedacted reports whether a stored value is a redaction rather than
// something that was really sent: either the bare placeholder a literal became,
// or the `<redacted:env:NAME>` a credential reference became.
func isRedacted(value string) bool {
	if value == secret.Placeholder {
		return true
	}

	// The prefix a scheme puts in front of a credential is part of the stored
	// text — `Bearer <redacted:env:…>` — so the ref is looked for after it.
	for _, enc := range []request.Encoding{request.EncodeBearer, request.EncodeBasic, request.EncodeRaw} {
		prefix := request.Secret(secret.SecretRef{}, enc).Prefix()
		if _, ok := secret.ParseRef(strings.TrimPrefix(value, prefix)); ok {
			return true
		}
	}

	return false
}

// warnUnreplayable reports a field the replay had to drop. It is a warning
// rather than a failure: the request is still worth making, the credential a
// redacted field held is re-resolved from the environment anyway, and a 401
// with an explanation on stderr is more useful than a refusal.
func warnUnreplayable(stderr io.Writer, kind, name string) {
	clierr.Warnf(stderr,
		"the recorded %s %q held a redacted value, which history does not store; replaying without it",
		kind, name)
}
