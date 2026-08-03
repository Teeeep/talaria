// This file is an external test package because assertion 4 of Task 18 — that a
// body read from the process's stdin still reaches curl — has to watch the body
// cross into internal/curl, and internal/curl imports internal/request. Testing
// from outside the package is what keeps that a one-way dependency.
package request_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/spec"
)

// bodyInputs is the common case for a body test: the fixture's POST, which
// declares a JSON request body, with no parameters bound.
func bodyInputs(t *testing.T) request.Inputs {
	t.Helper()

	doc, err := spec.LoadFile(filepath.Join("testdata", "request.yaml"))
	if err != nil {
		t.Fatalf("LoadFile(request.yaml): %v", err)
	}

	for _, op := range operation.Extract(doc) {
		if op.ID == "createPet" {
			return request.Inputs{Op: op, Doc: doc}
		}
	}

	t.Fatal("fixture has no operation createPet")
	return request.Inputs{}
}

func buildBody(t *testing.T, in request.Inputs) *request.Body {
	t.Helper()

	req, err := request.Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if req.Body == nil {
		t.Fatal("Build returned a request with no body, want the --body bytes")
	}

	return req.Body
}

// bodyUsageErr asserts Build refused the inputs with exit code 2 and returns the
// message, so each test can go on to assert what it has to name.
func bodyUsageErr(t *testing.T, in request.Inputs) string {
	t.Helper()

	req, err := request.Build(in)
	if err == nil {
		t.Fatalf("Build succeeded, want a usage error; got %+v", req)
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("Build error = %v (code %d), want a usage error (%d)", err, code, clierr.CodeUsage)
	}

	return err.Error()
}

func TestBodyFlagLiteralIsSentVerbatim(t *testing.T) {
	in := bodyInputs(t)
	in.Body = []string{`{"a":1}`}

	if got := string(buildBody(t, in).Data); got != `{"a":1}` {
		t.Errorf("body = %q, want the literal --body value", got)
	}
}

func TestNoBodyFlagLeavesTheRequestBodyNil(t *testing.T) {
	req, err := request.Build(bodyInputs(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if req.Body != nil {
		t.Errorf("body = %+v, want nil when no --body was given", req.Body)
	}
}

func TestBodyFlagAtFileReadsTheFile(t *testing.T) {
	// Pretty-printed with a trailing newline: a file body has to arrive byte for
	// byte, because a signed or whitespace-sensitive payload does not survive
	// being tidied up.
	want := "{\n  \"a\": 1\n}\n"
	path := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	in := bodyInputs(t)
	in.Body = []string{"@" + path}

	if got := string(buildBody(t, in).Data); got != want {
		t.Errorf("body = %q, want the file's bytes %q", got, want)
	}
}

func TestBodyFlagAtMissingFileIsUsageError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")

	in := bodyInputs(t)
	in.Body = []string{"@" + path}

	if msg := bodyUsageErr(t, in); !strings.Contains(msg, path) {
		t.Errorf("error = %q, want it to name the missing file %q", msg, path)
	}
}

func TestBodyFlagDashReadsTheProcessStdin(t *testing.T) {
	in := bodyInputs(t)
	in.Body = []string{"-"}
	in.Stdin = strings.NewReader(`{"from":"stdin"}`)

	if got := string(buildBody(t, in).Data); got != `{"from":"stdin"}` {
		t.Errorf("body = %q, want the bytes read from stdin", got)
	}
}

func TestBodyFlagDashWithNoStdinIsUsageError(t *testing.T) {
	in := bodyInputs(t)
	in.Body = []string{"-"}

	if msg := bodyUsageErr(t, in); !strings.Contains(msg, "stdin") {
		t.Errorf("error = %q, want it to explain that there is no stdin to read", msg)
	}
}

// TestBodyFlagDashFromAPipeReadsToEOF is the no-regression half of the
// cancellable read: a real pipe, closed by its writer, still arrives whole.
// strings.Reader returns EOF on the first call and would not notice a read that
// stopped after one chunk.
func TestBodyFlagDashFromAPipeReadsToEOF(t *testing.T) {
	const want = "{\"from\":\"a pipe\"}\n"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close() //nolint:errcheck // Test cleanup.

	go func() {
		for _, chunk := range []string{want[:5], want[5:]} {
			w.WriteString(chunk) //nolint:errcheck // The read side asserts what arrived.
		}
		w.Close() //nolint:errcheck // Closing is what produces the EOF under test.
	}()

	in := bodyInputs(t)
	in.Body = []string{"-"}
	in.Stdin = r

	if got := string(buildBody(t, in).Data); got != want {
		t.Errorf("body = %q, want the pipe read to EOF (%q)", got, want)
	}
}

// hangingReader is a stdin that never delivers a byte and never reaches EOF: a
// terminal nobody is typing at, or a pipe whose writer has wandered off. Read
// blocks until the test lets go.
type hangingReader struct{ release chan struct{} }

func (r *hangingReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

// drippingReader never reaches EOF either, but it is always making progress, so
// a read that waits for EOF waits forever while looking healthy.
type drippingReader struct{}

func (drippingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	time.Sleep(time.Millisecond)
	p[0] = 'x'
	return 1, nil
}

// buildCancelled runs Build against a stdin that will not finish and asserts it
// returns once ctx is cancelled rather than blocking on the read. The deadline
// is the whole point: before the fix io.ReadAll owned the goroutine outright and
// neither Ctrl-C nor `kill -TERM` could get it back.
func buildCancelled(t *testing.T, stdin io.Reader) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	in := bodyInputs(t)
	in.Body = []string{"-"}
	in.Stdin = stdin
	in.Ctx = ctx

	type outcome struct {
		req *request.Request
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		req, err := request.Build(in)
		done <- outcome{req, err}
	}()

	cancel()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("Build succeeded on a cancelled context, want a refusal; got %+v", got.req)
		}
		// Exit 1, the code an interrupted request already has (AGENT.md), not
		// the usage error every other binding problem produces: the caller's
		// command line was fine, and exit 2 would tell an agent to change it.
		if code := clierr.From(got.err).Code; code != clierr.CodeRequestFailed {
			t.Errorf("Build error = %v (code %d), want a request failure (%d)",
				got.err, code, clierr.CodeRequestFailed)
		}
		return got.err.Error()
	case <-time.After(5 * time.Second):
		t.Fatal("Build did not return within 5s of the context being cancelled; the stdin read is not cancellable")
		return ""
	}
}

// TestStdinBodyStopsWhenTheContextIsCancelled is finding 14's reachable half:
// `--body -` on a stdin that never closes. The signal context cancels, and this
// is what has to notice.
func TestStdinBodyStopsWhenTheContextIsCancelled(t *testing.T) {
	stdin := &hangingReader{release: make(chan struct{})}
	defer close(stdin.release)

	msg := buildCancelled(t, stdin)
	for _, want := range []string{"stdin", "cancel"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to mention %q", msg, want)
		}
	}
}

// TestStdinBodyStopsWhileStillReceivingBytes is the same cancellation against
// the adversary that never blocks: a pipe delivering a byte at a time forever.
// A read that only checks the context between whole reads still hangs here.
func TestStdinBodyStopsWhileStillReceivingBytes(t *testing.T) {
	if msg := buildCancelled(t, drippingReader{}); !strings.Contains(msg, "stdin") {
		t.Errorf("error = %q, want it to name the stdin read it gave up on", msg)
	}
}

// TestStdinBodyReachesCurlThroughTheConfigDocument is the regression test for
// the `--body -` versus `curl -K -` stdin collision (docs/research §8). The Go
// process owns the real stdin and reads the body from it in full; curl's stdin
// carries the config document and nothing else.
func TestStdinBodyReachesCurlThroughTheConfigDocument(t *testing.T) {
	const body = `{"from":"stdin"}`
	stdin := strings.NewReader(body)

	in := bodyInputs(t)
	in.Body = []string{"-"}
	in.Stdin = stdin

	req, err := request.Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Nothing is left on the process's stdin for a second consumer to find: the
	// body was read to EOF before any curl existed.
	rest, err := io.ReadAll(stdin)
	if err != nil {
		t.Fatalf("ReadAll(stdin): %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("%d bytes left on stdin (%q), want it read to EOF", len(rest), rest)
	}

	config, argv, cleanup, err := curl.BuildConfig(req, curl.Capture{})
	defer cleanup()
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}

	// The body travels as a directive, so its quotes are escaped for curl's
	// config parser. Asserting on the escaped form is asserting on what curl
	// will actually read back out.
	want := `data-raw = "` + strings.ReplaceAll(body, `"`, `\"`) + `"`
	if !bytes.Contains(config, []byte(want)) {
		t.Errorf("config document = %q, want it to carry %s", config, want)
	}

	// curl is invoked as `curl -q -K -`, so its own stdin is the config document
	// the executor pipes in — a reader talaria creates, never the process's.
	if strings.Join(argv, " ") != "curl -q -K -" {
		t.Errorf("argv = %v, want curl to read its config from its own stdin", argv)
	}
}

func TestBodyFlagGivenTwiceIsUsageError(t *testing.T) {
	in := bodyInputs(t)
	in.Body = []string{`{"a":1}`, "@body.json"}

	msg := bodyUsageErr(t, in)
	// The count and the kind of each value locate the mistake; the bytes are
	// what a request body carries a client_secret in.
	for _, want := range []string{"--body", "2 times", "literal", "@file"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to mention %q", msg, want)
		}
	}
}

// TestARepeatedBodyFlagEchoesNoBytes is the credential firewall on the body's
// error path (DESIGN.md §5a): request bodies routinely carry client_secret and
// password, so a message that joined the rejected values would publish them.
func TestARepeatedBodyFlagEchoesNoBytes(t *testing.T) {
	const secret = "s3cr3t-canary-value"

	in := bodyInputs(t)
	in.Body = []string{`{"client_secret":"` + secret + `"}`, "@/tmp/" + secret + ".json", "-"}

	msg := bodyUsageErr(t, in)

	if strings.Contains(msg, secret) {
		t.Errorf("the rejected body reached the error message:\n%s", msg)
	}
	for _, want := range []string{"3 times", "literal", "@file", "stdin"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to mention %q", msg, want)
		}
	}
}

func TestContentTypeDefaultsToTheOperationsFirstMediaType(t *testing.T) {
	in := bodyInputs(t)
	in.Body = []string{`{"a":1}`}

	if got := buildBody(t, in).ContentType; got != "application/json" {
		t.Errorf("content type = %q, want the operation's first declared media type", got)
	}
}

func TestUserContentTypeHeaderBeatsTheOperationMediaType(t *testing.T) {
	in := bodyInputs(t)
	in.Body = []string{"a=1"}
	in.Headers = []string{"Content-Type=application/x-www-form-urlencoded"}

	if got := buildBody(t, in).ContentType; got != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q, want the --header value to win", got)
	}
}

// TestUserContentTypeHeaderIsNotSentTwice guards the seam between the two ways a
// content type reaches curl: the header pair and the body's own field.
func TestUserContentTypeHeaderIsNotSentTwice(t *testing.T) {
	in := bodyInputs(t)
	in.Body = []string{"a=1"}
	in.Headers = []string{"Content-Type=application/x-www-form-urlencoded"}

	req, err := request.Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	config, _, cleanup, err := curl.BuildConfig(req, curl.Capture{})
	defer cleanup()
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}

	if n := strings.Count(strings.ToLower(string(config)), "content-type:"); n != 1 {
		t.Errorf("config document has %d Content-Type directives, want 1:\n%s", n, config)
	}
}

// withMediaType returns body inputs whose operation declares exactly one
// request-body media type, so a case can name the hostile string directly
// instead of carrying a spec fixture per variation.
func withMediaType(t *testing.T, mediaType string) request.Inputs {
	t.Helper()

	in := bodyInputs(t)
	in.Op.RequestBody = &operation.RequestBody{
		Content: []operation.MediaType{{ContentType: mediaType}},
	}
	in.Body = []string{`{"a":1}`}

	return in
}

// TestAHostileMediaTypeIsRefusedAtBindTime covers the one header value that
// reaches the wire without ever passing the --header path's CRLF check: the key
// of the spec's `content:` map. The spec is untrusted input by talaria's own
// premise, and this value is read inside the component that holds resolved
// credentials — a double CRLF ends the header block and smuggles a second
// request onto the connection carrying the token.
func TestAHostileMediaTypeIsRefusedAtBindTime(t *testing.T) {
	cases := []struct{ name, mediaType string }{
		{"CRLF injection", "application/json\r\nX-Injected: pwned"},
		{"bare carriage return", "application/json\rX-Injected: pwned"},
		{"bare newline", "application/json\nX-Injected: pwned"},
		{"double CRLF smuggling a request", "application/json\r\n\r\nGET /admin HTTP/1.1\r\n"},
		{"NUL byte", "application/json\x00"},
		{"whitespace only", "   "},
		{"non-ASCII", "application/jsön"},
		{"names a header", "application/json: x"},
		{"no subtype", "application"},
		{"a parameter with no value", "text/plain; charset"},
		{"a parameter carrying a newline", "text/plain; charset=utf-8\r\nX-Injected: pwned"},
		{"a megabyte long", "application/" + strings.Repeat("j", 1<<20)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := bodyUsageErr(t, withMediaType(t, tc.mediaType))
			if !strings.Contains(msg, "media type") {
				t.Errorf("error = %q, want it to name the media type as the fault", msg)
			}
			// The message goes to stderr, where a raw CR or LF would let the
			// rejected value forge lines of talaria's own output.
			if strings.ContainsAny(msg, "\r\n") {
				t.Errorf("error message carries a raw CR or LF: %q", msg)
			}
		})
	}
}

// TestAnOrdinaryMediaTypeStillBinds is the other half of the gate: the check
// refuses a header split, not the parameter form real specs use.
func TestAnOrdinaryMediaTypeStillBinds(t *testing.T) {
	for _, mediaType := range []string{
		"application/json",
		"application/vnd.api+json",
		"application/x-www-form-urlencoded",
		"text/plain; charset=utf-8",
		"text/plain;charset=utf-8",
		`multipart/form-data; boundary="----talaria0123"`,
	} {
		t.Run(mediaType, func(t *testing.T) {
			if got := buildBody(t, withMediaType(t, mediaType)).ContentType; got != mediaType {
				t.Errorf("content type = %q, want %q unchanged", got, mediaType)
			}
		})
	}
}

func TestBodyWithNoDeclaredMediaTypeHasNoContentType(t *testing.T) {
	doc, err := spec.LoadFile(filepath.Join("testdata", "request.yaml"))
	if err != nil {
		t.Fatalf("LoadFile(request.yaml): %v", err)
	}

	var in request.Inputs
	for _, op := range operation.Extract(doc) {
		// listPets declares no request body, so talaria has nothing to guess from
		// and says so by omission rather than by inventing application/json.
		if op.ID == "listPets" {
			in = request.Inputs{Op: op, Doc: doc, Params: []string{"limit=1"}}
		}
	}

	in.Body = []string{"raw bytes"}

	if got := buildBody(t, in).ContentType; got != "" {
		t.Errorf("content type = %q, want empty when the operation declares no body", got)
	}
}
