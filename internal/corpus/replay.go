package corpus

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
)

// redactionMark opens both forms history writes where a credential stood: the
// bare secret.Placeholder, `<redacted>`, and the `<redacted:env:NAME>` a
// SecretRef renders to. A value carrying either is not a value — it is the note
// saying one was removed — so replaying it would send the note itself.
const redactionMark = "<redacted"

// builtinCredentialNames is §5a's non-configurable list of names that carry a
// credential — Authorization, Cookie, *api*key*, *token*, *secret*. A recorded
// field under one of those names is dropped on replay whatever it holds:
// config.Resolve owns those positions, and the entry does not.
var builtinCredentialNames = secret.NewRedactor()

// Replayable is a stored entry read back as the flag-shaped inputs that rebuild
// it: everything request.Inputs takes from the *entry*, and nothing it takes
// from the spec, the profile or the flags. Those come from the current
// invocation, which is the whole of §5a's "a history entry is data, never
// instruction".
type Replayable struct {
	// Params, Query and Headers are `name=value` strings in the form
	// request.Inputs takes them. A stored value whose name the operation
	// declares as a parameter lands in Params, so a required one is bound
	// rather than reported missing.
	Params, Query, Headers []string
	// Body is the decoded request body. HasBody distinguishes an entry that
	// recorded an empty body from one that recorded none at all.
	Body    []byte
	HasBody bool
	// Dropped names the recorded fields this replay will not send, for the
	// caller to warn about. A credential position is the common case: history
	// holds a redaction marker there, never a value.
	Dropped []string
}

// Replay reads a stored entry back as the inputs that rebuild it against op.
//
// Every field of the entry is untrusted input (DESIGN.md §5a): the store is a
// JSONL file that may have been written by another machine or edited by hand.
// So op supplies the path template and the parameter names, the entry supplies
// only values, and a value that still carries a redaction marker is dropped
// rather than sent. The stored URL contributes its path and query and nothing
// else — the host is re-derived by the caller from the spec and the flags.
func (e Entry) Replay(op operation.Operation) (Replayable, error) {
	// Both checks come before the parse error below, which quotes the URL it
	// could not read: an edited entry must not be able to replay as a file read
	// or to smuggle a credential into a message every later surface prints.
	if host, ok := request.Userinfo(e.URL); ok {
		return Replayable{}, clierr.Usage(
			"the recorded URL for %s carries a credential in its userinfo, so it will not be replayed; "+
				"re-run the call with the credential in the environment instead", host)
	}

	parsed, err := url.Parse(e.URL)
	if err != nil {
		return Replayable{}, clierr.Usage("the recorded URL %q cannot be parsed: %v", e.URL, err)
	}
	if !request.IsHTTPScheme(parsed.Scheme) {
		return Replayable{}, clierr.Usage("the recorded URL %q has scheme %q; only http and https can be replayed",
			e.URL, parsed.Scheme)
	}

	out := Replayable{}
	if out.Params, err = pathParams(op, parsed.EscapedPath()); err != nil {
		return Replayable{}, err
	}

	declared := declaredLocations(op)
	if err := out.query(declared, parsed.RawQuery); err != nil {
		return Replayable{}, err
	}
	out.headers(declared, e.Request.Headers)
	out.cookies(declared, e.Request.Cookies)

	if err := out.body(e.Request.Body); err != nil {
		return Replayable{}, err
	}

	return out, nil
}

// query rebuilds the query parameters in the order they were recorded. The raw
// string is walked rather than url.ParseQuery'd because a map would lose that
// order, and a replay that reorders the query string is not the same request.
func (r *Replayable) query(declared map[string]string, raw string) error {
	for _, field := range strings.Split(raw, "&") {
		if field == "" {
			continue
		}

		rawName, rawValue, _ := strings.Cut(field, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			return clierr.Usage("the recorded query parameter %q cannot be parsed: %v", rawName, err)
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return clierr.Usage("the recorded value of query parameter %q cannot be parsed: %v", name, err)
		}

		r.take(declared, "query parameter", operation.InQuery, name, value, &r.Query)
	}

	return nil
}

// headers rebuilds the request headers. The stored form is a map, so the order
// is gone and is restored as sorted-by-name: byte-for-byte header order is not
// something an HTTP request depends on, and the config document has to be
// deterministic.
func (r *Replayable) headers(declared map[string]string, stored map[string]string) {
	for _, name := range sortedNames(stored) {
		r.take(declared, "header", operation.InHeader, name, stored[name], &r.Headers)
	}
}

// cookies rebuilds the cookies the operation declares as parameters. Anything
// else is dropped and reported: request.Inputs has no cookie flag, because a
// cookie talaria sends is either a declared parameter or a credential, and a
// credential is re-resolved rather than replayed.
func (r *Replayable) cookies(declared map[string]string, stored map[string]string) {
	for _, name := range sortedNames(stored) {
		r.take(declared, "cookie", operation.InCookie, name, stored[name], nil)
	}
}

// take routes one recorded name=value: into Params when the operation declares
// a parameter of that name in that location, otherwise onto flags, otherwise
// dropped. A value carrying a redaction marker, and a name the built-in
// credential list covers, are dropped either way — the first is a note where a
// credential stood, and the second is a position config.Resolve owns.
func (r *Replayable) take(declared map[string]string, kind, in, name, value string, flags *[]string) {
	switch {
	case strings.Contains(value, redactionMark), builtinCredentialNames.IsSensitive(name):
	case declared[name] == in:
		r.Params = append(r.Params, name+"="+value)
		return
	case flags != nil:
		*flags = append(*flags, name+"="+value)
		return
	}

	r.Dropped = append(r.Dropped, fmt.Sprintf("%s %q", kind, name))
}

// body decodes the recorded request body, refusing anything that would send
// something the original call did not.
func (r *Replayable) body(body *Body) error {
	if body == nil {
		return nil
	}

	if body.Truncated {
		return clierr.Usage(
			"the recorded request body was truncated at %d bytes, so replaying it would send something the original did not",
			MaxBody)
	}

	data, err := body.Bytes()
	if err != nil {
		return err
	}

	// A body is redacted by JSON path, so a credential position in one holds the
	// marker rather than a value. Sending the marker is worse than not sending
	// the request: the API sees a login attempt whose password is the literal
	// text `<redacted>`, and the caller sees a 401 with no explanation.
	if strings.Contains(string(data), redactionMark) {
		return clierr.Usage(
			"the recorded request body still carries a redaction marker where a credential stood, " +
				"so it cannot be replayed; re-run the original call instead")
	}

	r.Body, r.HasBody = data, true

	return nil
}

// pathParams recovers the operation's path parameters from a stored path.
//
// The entry holds the concrete path (/v1/pets/42) and the operation holds the
// template (/pets/{petId}), so the match runs from the right and whatever
// precedes it is the server's own path prefix. That prefix is discarded rather
// than checked: the base URL replay sends to is re-derived from the spec and
// the flags, so the stored one has no say in it.
func pathParams(op operation.Operation, storedPath string) ([]string, error) {
	tmpl, got := splitPath(op.Path), splitPath(storedPath)
	if len(got) < len(tmpl) {
		return nil, clierr.Usage(
			"the recorded path %q has %d segments and %s's path %q has %d, so it cannot be replayed as that operation",
			storedPath, len(got), operationName(op), op.Path, len(tmpl))
	}
	got = got[len(got)-len(tmpl):]

	var params []string
	for i, segment := range tmpl {
		before, rest, templated := strings.Cut(segment, "{")
		if !templated {
			if segment != got[i] {
				return nil, clierr.Usage(
					"the recorded path %q does not match %s's path %q at segment %q",
					storedPath, operationName(op), op.Path, segment)
			}
			continue
		}

		name, after, closed := strings.Cut(rest, "}")
		if !closed || strings.ContainsAny(after, "{}") {
			return nil, clierr.Usage(
				"%s's path segment %q holds more than one placeholder, which replay cannot take apart",
				operationName(op), segment)
		}

		value, err := segmentValue(got[i], before, after)
		if err != nil {
			return nil, clierr.Usage("the recorded path %q does not match %s's path %q: %s",
				storedPath, operationName(op), op.Path, err)
		}

		params = append(params, name+"="+value)
	}

	return params, nil
}

// segmentValue is what one templated segment's placeholder stood for, with the
// literal text around it removed and the percent-encoding undone — Build
// escapes it again, so a value holding a slash or a NUL stays inside its own
// segment.
func segmentValue(got, before, after string) (string, error) {
	if !strings.HasPrefix(got, before) || !strings.HasSuffix(got, after) ||
		len(got) < len(before)+len(after) {
		return "", clierr.Usage("segment %q is not of the form %s{...}%s", got, before, after)
	}

	value, err := url.PathUnescape(got[len(before) : len(got)-len(after)])
	if err != nil {
		return "", clierr.Usage("segment %q is not valid percent-encoding: %v", got, err)
	}

	return value, nil
}

// splitPath is a URL path as its non-empty segments, so a leading, trailing or
// doubled slash does not become a segment nothing can match.
func splitPath(path string) []string {
	var out []string
	for _, segment := range strings.Split(path, "/") {
		if segment != "" {
			out = append(out, segment)
		}
	}

	return out
}

// declaredLocations maps each parameter the operation declares to where it
// goes, so a recorded value can be routed back to the flag that binds it.
func declaredLocations(op operation.Operation) map[string]string {
	out := make(map[string]string, len(op.Params))
	for _, p := range op.Params {
		out[p.Name] = p.In
	}

	return out
}

// sortedNames is the deterministic iteration order for the maps a stored entry
// holds: what replay binds, and what capHeaders keeps when it cannot keep
// everything — two entries recording the same headers must keep the same ones.
func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// operationName is the operation's ID, falling back to method and path for a
// spec that sets no operationId.
func operationName(op operation.Operation) string {
	if op.ID != "" {
		return op.ID
	}

	return strings.TrimSpace(op.Method + " " + op.Path)
}
