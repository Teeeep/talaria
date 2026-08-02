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
	if len(b.in.Body) > 1 {
		b.fail("--body was given %d times (%s); a request has one body",
			len(b.in.Body), strings.Join(b.in.Body, ", "))
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

	return rb.Content[0].ContentType
}
