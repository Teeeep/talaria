package request

import (
	"io"
	"os"
	"strings"
)

// stdinFlag is the --body value that means "read the body from this process's
// standard input". It is curl's own spelling, so a user who knows curl already
// knows it.
const stdinFlag = "-"

// fileFlagPrefix marks a --body value as a filename, again following curl.
const fileFlagPrefix = "@"

// contentTypeHeader is the header a body's media type travels in.
const contentTypeHeader = "Content-Type"

// body resolves the --body flag into the bytes that will be sent and the media
// type that describes them, or nil when no body was asked for.
//
// The bytes are resolved here, in the Go process, before any curl invocation
// exists. That is the whole point of this function: DESIGN.md §5a gives the Go
// process ownership of the real stdin, because curl's stdin is already taken by
// the `-K -` config document. If `--body -` were passed through to curl, the two
// would be the same pipe and the body and the credentials would be spliced into
// one stream (docs/research §8).
//
// req is read for the headers already bound, so a user's own Content-Type can
// beat the operation's declared media type.
func (b *binder) body(req *Request) *Body {
	data, ok := b.bodyData()
	if !ok {
		return nil
	}

	return &Body{ContentType: b.contentType(req), Data: data}
}

// bodyData reads whichever of the three sources --body named. The second return
// is false when there is no body to send, either because none was asked for or
// because reading it failed — in which case the problem is already recorded and
// Build will return it.
func (b *binder) bodyData() ([]byte, bool) {
	if len(b.in.Body) == 0 {
		return nil, false
	}
	// A repeated --body is a mistake worth naming rather than resolving by
	// last-one-wins: the two values are usually a literal and a file, and
	// silently dropping one of them sends a request the user did not write.
	//
	// What it is named by is the count and the kind of each value, never the
	// values themselves. A request body is where a client_secret or a password
	// travels, so joining them into an error message would put credentials on
	// stderr — the §5a firewall covers error paths too.
	if len(b.in.Body) > 1 {
		kinds := make([]string, 0, len(b.in.Body))
		for _, raw := range b.in.Body {
			kinds = append(kinds, bodyKind(raw))
		}

		b.fail("--body was given %d times (%s); a request has one body",
			len(b.in.Body), strings.Join(kinds, ", "))
		return nil, false
	}

	raw := b.in.Body[0]
	switch {
	case raw == stdinFlag:
		return b.stdinBody()
	case strings.HasPrefix(raw, fileFlagPrefix):
		return b.fileBody(strings.TrimPrefix(raw, fileFlagPrefix))
	default:
		return []byte(raw), true
	}
}

// bodyKind names which of the three sources a --body value is, so a message can
// tell the user which of their bodies is which without quoting any of them.
// A file's path is elided along with the bytes: it is chosen by the same
// command line and can name the secret it holds.
func bodyKind(raw string) string {
	switch {
	case raw == stdinFlag:
		return "stdin"
	case strings.HasPrefix(raw, fileFlagPrefix):
		return "@file"
	default:
		return "literal"
	}
}

// stdinBody reads the process's standard input to EOF.
func (b *binder) stdinBody() ([]byte, bool) {
	if b.in.Stdin == nil {
		b.fail("--body %s reads the request body from stdin, but this process has no stdin to read",
			stdinFlag)
		return nil, false
	}

	data, err := io.ReadAll(b.in.Stdin)
	if err != nil {
		b.fail("cannot read the request body from stdin: %v", err)
		return nil, false
	}

	return data, true
}

// fileBody reads a body from disk verbatim. Nothing is trimmed: a body is bytes,
// and a signed payload stops verifying if its trailing newline is tidied away.
func (b *binder) fileBody(path string) ([]byte, bool) {
	if path == "" {
		b.fail("--body %s names no file", fileFlagPrefix)
		return nil, false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		b.fail("cannot read the request body from %s: %v", path, err)
		return nil, false
	}

	return data, true
}

// contentType is what the body will be sent as: the Content-Type the user set
// explicitly, otherwise the first media type the operation declares.
//
// The user wins because the spec describes what the server accepts, not what
// this particular call is sending — an operation declaring application/json can
// still be handed a form body deliberately. When the operation declares no body
// at all the result is empty, and talaria sends no Content-Type rather than
// guessing one.
func (b *binder) contentType(req *Request) string {
	for _, h := range req.Headers {
		if strings.EqualFold(h.Name, contentTypeHeader) {
			return h.Value.String()
		}
	}

	rb := b.in.Op.RequestBody
	if rb == nil || len(rb.Content) == 0 {
		return ""
	}

	declared := rb.Content[0].ContentType
	if !isMediaType(declared) {
		// Named but never echoed, following the rule the rest of this file
		// keeps: the text is attacker-supplied, and echoing it would put the
		// CRLF — and the header it smuggles — onto stderr instead of the wire.
		b.fail("the operation declares a media type that cannot be sent as a %s header "+
			"(its text is not echoed)", contentTypeHeader)
		return ""
	}

	return declared
}

// isMediaType reports whether s can be sent as a Content-Type header value: a
// type/subtype pair of tokens (RFC 9110 §8.3.1), optionally followed by
// parameters holding no control character.
//
// The user's own --header Content-Type is not checked against this, and does not
// need to be: it has already been through pairs → SplitsRequest, and the spec
// describes what the server accepts rather than what this call is sending. What
// this guards is the other source — a key in the spec's content: map, which is
// untrusted input that becomes a header verbatim, and is the one header value
// the binder otherwise never inspects.
//
// The parameter section is checked for control characters rather than parsed.
// What has to be stopped is a media type that leaves the field it sits in; a
// parameter that is merely strange is the server's business.
func isMediaType(s string) bool {
	base, params, _ := strings.Cut(s, ";")

	typ, sub, ok := strings.Cut(base, "/")
	if !ok || !isFieldName(typ) || !isFieldName(sub) {
		return false
	}

	return strings.IndexFunc(params, isControl) < 0
}

// isControl reports whether r is a character that cannot appear in a header
// field value. HTAB is included: it is legal whitespace between parameters, but
// nothing declares a media type with a tab in it, and refusing is the safer
// direction for a value this file otherwise cannot vouch for.
func isControl(r rune) bool { return r < ' ' || r == 0x7f }
