package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// builtinBodyPaths are the response-body JSON paths redacted without any
// configuration: the RFC 6749 token-response fields. A body carrying one of
// these came from a credential-issuing endpoint, which is exactly the case
// DESIGN.md §5a names. Everything else is API-specific — talaria cannot tell a
// secret field from an ordinary one by looking at it — and has to be configured.
var builtinBodyPaths = []string{"access_token", "refresh_token", "id_token"}

// ResponseRedactor redacts what came back off the wire, which is the one leak
// channel §5a admits it does not close: a response body may contain a secret
// and the tool cannot know which field it is. What it can do is what this type
// does — always redact the response headers that carry credentials by name, and
// redact the body paths the user has told it about.
//
// It is deliberately separate from the request-side Redactor. A request's
// secrets are structural: they are SecretRefs, and a ref cannot print its
// value. A response's secrets are just bytes, so redacting them is pattern
// matching, with the weaker guarantee that implies.
//
// The zero value — including a nil *ResponseRedactor, which is what an unset
// struct field is — redacts the built-in lists, so redaction is never
// accidentally opt-in.
type ResponseRedactor struct {
	headers *Redactor
	paths   [][]string
}

// NewResponseRedactor returns a redactor matching the built-in header list plus
// extraHeaders, and the built-in body paths plus extraPaths.
//
// A body path is dotted — `data.token` names the `token` key of the top-level
// `data` object. Neither list can be shrunk: a firewall a misconfiguration can
// switch off is not one.
func NewResponseRedactor(extraHeaders, extraPaths []string) *ResponseRedactor {
	paths := make([][]string, 0, len(builtinBodyPaths)+len(extraPaths))
	for _, path := range builtinBodyPaths {
		paths = append(paths, strings.Split(path, "."))
	}
	for _, path := range extraPaths {
		if segments := splitPath(path); segments != nil {
			paths = append(paths, segments)
		}
	}

	return &ResponseRedactor{headers: NewRedactor(extraHeaders...), paths: paths}
}

// Headers returns a copy of h with the values of sensitive headers replaced.
//
// Repetitions are kept — Set-Cookie legitimately appears more than once, and a
// caller counting cookies should still see the right number — and the whole
// value goes, cookie name included, because `session=` in front of a token does
// not make the token safe to print.
//
// The original map is left untouched — a redactor reports, it does not edit
// what came off the wire — and only the copy travels onward. Everything
// downstream of this point, response validation included, reads the copy.
func (r *ResponseRedactor) Headers(h map[string][]string) map[string][]string {
	if h == nil {
		return nil
	}

	redacted := make(map[string][]string, len(h))
	for name, values := range h {
		sensitive := r.headerRedactor().IsSensitive(name)

		copied := make([]string, len(values))
		for i, value := range values {
			if sensitive {
				copied[i] = Placeholder
			} else {
				copied[i] = value
			}
		}
		redacted[name] = copied
	}

	return redacted
}

// Body returns body with the configured JSON paths replaced.
//
// A body that is not a single JSON object is returned verbatim: a dotted path
// has no meaning in a text or binary body, and rewriting bytes talaria does not
// understand would report a document the server never sent. So is a body in
// which nothing matched, which keeps the common case exact — the caller sees
// the server's own formatting, not encoding/json's.
func (r *ResponseRedactor) Body(body []byte) []byte {
	paths := r.bodyPaths()
	if len(paths) == 0 || len(body) == 0 {
		return body
	}

	doc, ok := decodeObject(body)
	if !ok {
		return body
	}

	redacted := false
	for _, path := range paths {
		if redactPath(doc, path) {
			redacted = true
		}
	}
	if !redacted {
		return body
	}

	out, err := json.Marshal(doc)
	if err != nil {
		// Unreachable for a document that just decoded, but the alternative to
		// returning the original here is returning nothing at all.
		return body
	}

	return out
}

// headerRedactor returns the header matcher, treating a nil receiver as the
// built-in list.
func (r *ResponseRedactor) headerRedactor() *Redactor {
	if r == nil {
		return nil // A nil *Redactor is the built-in list; see Redactor.match.
	}

	return r.headers
}

// bodyPaths returns the paths in effect, treating a nil receiver as the
// built-in list.
func (r *ResponseRedactor) bodyPaths() [][]string {
	if r == nil {
		return NewResponseRedactor(nil, nil).paths
	}

	return r.paths
}

// decodeObject decodes body as exactly one JSON object. Trailing content means
// this was never one JSON document — a body ending in junk is not something to
// re-encode — and a top-level array is skipped because a dotted path is rooted
// at a named key.
func decodeObject(body []byte) (map[string]any, bool) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	// Numbers stay as their original text: without this an id of
	// 1234567890123456789 comes back out as 1.2345678901234568e+18, and a
	// redacted body must still report what the server actually sent.
	dec.UseNumber()

	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}

	return doc, true
}

// redactPath replaces the value at path within doc, reporting whether anything
// was there to replace. A path that names nothing — a missing key, or a segment
// that turns out not to be an object — is a no-op rather than an error: a
// redaction list is written once and applied to every response an API returns.
func redactPath(doc map[string]any, path []string) bool {
	if len(path) == 1 {
		if _, ok := doc[path[0]]; !ok {
			return false
		}
		doc[path[0]] = Placeholder

		return true
	}

	child, ok := doc[path[0]].(map[string]any)
	if !ok {
		return false
	}

	return redactPath(child, path[1:])
}

// splitPath splits a dotted path into segments, returning nil for one that
// cannot name anything — empty, or with an empty segment. Silently ignoring it
// is right here: config validation belongs to whoever read the config, and a
// redactor that refused to be built would fail an unrelated call.
func splitPath(path string) []string {
	if path == "" {
		return nil
	}

	segments := strings.Split(path, ".")
	for _, segment := range segments {
		if segment == "" {
			return nil
		}
	}

	return segments
}

// QueryKeyWarner emits the one-time warning from §5a's leak-channel table: an
// API key in the query string is symbolic in emitted curl and redacted in
// output, and still ends up in the *server's* access log, where talaria has no
// say. The user is told once so they can decide whether that is acceptable.
//
// Once per process rather than once per call, because `run` makes hundreds of
// calls against the same spec and a warning repeated hundreds of times is one
// nobody reads.
type QueryKeyWarner struct{ once sync.Once }

// NewQueryKeyWarner returns a warner that has not yet fired.
func NewQueryKeyWarner() *QueryKeyWarner { return &QueryKeyWarner{} }

// Warn writes the warning for a credential travelling in query parameter param,
// the first time it is called and never again.
//
// The message names the parameter and the environment variable, both of which
// are names; a warning about a leak that quoted the value would be one.
func (w *QueryKeyWarner) Warn(out io.Writer, param string, ref SecretRef) {
	if w == nil || out == nil {
		return
	}

	w.once.Do(func() {
		fmt.Fprintf(out,
			"warning: this operation sends %s in the query parameter %q; "+
				"URLs are recorded in server access logs, so the key is exposed there "+
				"whatever talaria redacts\n",
			ref.Symbolic(), param)
	})
}
