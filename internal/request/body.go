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

// maxMediaType bounds the media type an operation may declare. The value is a
// spec-supplied string that ends up in a header, and the house rule for those
// is to size-check before allocating: everything below splits it up to look at
// it, and the error message that refuses it quotes it back. A real media type
// is well under a hundred bytes.
const maxMediaType = 1024

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
	data, origin, path, ok := b.bodyData()
	if !ok {
		return nil
	}

	return &Body{ContentType: b.contentType(req), Data: data, Origin: origin, Path: path}
}

// bodyData reads whichever of the three sources --body named, and says which
// one it was: the bytes are the same on the wire either way, but only the argv
// case may be printed back to the caller. The last return is false when there
// is no body to send, either because none was asked for or because reading it
// failed — in which case the problem is already recorded and Build will return
// it.
func (b *binder) bodyData() (data []byte, origin BodyOrigin, path string, ok bool) {
	if len(b.in.Body) == 0 {
		return nil, BodyArgv, "", false
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
		return nil, BodyArgv, "", false
	}

	raw := b.in.Body[0]
	switch {
	case raw == stdinFlag:
		data, ok := b.stdinBody()
		return data, BodyStdin, "", ok
	case strings.HasPrefix(raw, fileFlagPrefix):
		file := strings.TrimPrefix(raw, fileFlagPrefix)
		data, ok := b.fileBody(file)
		return data, BodyFile, file, ok
	default:
		return []byte(raw), BodyArgv, "", true
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

// stdinBody reads the process's standard input to EOF, or gives up when Ctx is
// cancelled.
//
// The read runs in its own goroutine because there is no way to interrupt one
// already in progress: stdin is a terminal, a pipe or another process, and
// io.ReadAll on it returns when that party decides to, which may be never. Ctx
// is the signal context, so this select is the difference between Ctrl-C ending
// a `--body -` that is waiting on nothing and Ctrl-C doing nothing at all.
//
// The goroutine outlives this function when the context wins. That is
// deliberate and bounded: its channel is buffered so the send cannot block, and
// the only caller is a process on its way out.
func (b *binder) stdinBody() ([]byte, bool) {
	if b.in.Stdin == nil {
		b.fail("--body %s reads the request body from stdin, but this process has no stdin to read",
			stdinFlag)
		return nil, false
	}

	type read struct {
		data []byte
		err  error
	}
	done := make(chan read, 1)
	go func() {
		data, err := io.ReadAll(b.in.Stdin)
		done <- read{data, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			b.fail("cannot read the request body from stdin: %v", r.err)
			return nil, false
		}
		return r.data, true
	case <-b.ctx().Done():
		// Not a b.fail: a cancellation is not a bad invocation, and exit 2 tells
		// an agent to fix its command line. It is the same interruption curl's
		// executor reports as exit 1, reached one step earlier.
		b.stop("the request was cancelled while reading the body from stdin")
		return nil, false
	}
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
//
// The spec's media type is checked here and nowhere earlier because this is
// where it stops being a map key and becomes a header value. The --header
// branch above is not checked again: those values were already refused for a
// CRLF at bind time, and h.Value.String() is the redacted display form, which a
// media-type check would reject for a credential-valued header.
func (b *binder) contentType(req *Request) string {
	for _, h := range req.Headers {
		if strings.EqualFold(h.Name, contentTypeHeader) {
			return h.Value.String()
		}
	}

	rb := b.in.Op.RequestBody
	if rb == nil || len(rb.Content) == 0 || rb.Content[0].ContentType == "" {
		return ""
	}

	declared := rb.Content[0].ContentType
	if len(declared) > maxMediaType {
		b.fail("the operation declares a request body media type of %d bytes, over the %d-byte limit",
			len(declared), maxMediaType)
		return ""
	}
	if !isMediaType(declared) {
		b.fail("the operation declares the media type %q, which is not a type/subtype pair with "+
			"optional ; parameter=value and cannot be sent as a header", declared)
		return ""
	}

	return declared
}
