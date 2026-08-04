// This file is an external test package because assertion 4 of Task 18 — that a
// body read from the process's stdin still reaches curl — has to watch the body
// cross into internal/curl, and internal/curl imports internal/request. Testing
// from outside the package is what keeps that a one-way dependency.
package request_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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

// hostileMediaTypeSpec is a spec whose content: map key is the media type under
// test. The key is where the value actually comes from — spec.LoadBytes carries
// a CRLF in it through untouched, so nothing upstream of the binder is going to
// catch this.
func hostileMediaTypeSpec(t *testing.T, mediaType string) request.Inputs {
	t.Helper()

	var y strings.Builder
	y.WriteString("openapi: 3.0.0\ninfo: {title: t, version: '1'}\n" +
		"servers: [{url: 'https://api.example.com'}]\npaths:\n  /pets:\n    post:\n" +
		"      operationId: createPet\n      requestBody:\n        content:\n          ")
	y.WriteString(strconv.Quote(mediaType))
	y.WriteString(":\n            schema: {type: object}\n      responses:\n        '201': {description: created}\n")

	doc, err := spec.LoadBytes([]byte(y.String()))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	ops := operation.Extract(doc)
	if len(ops) != 1 {
		t.Fatalf("fixture yielded %d operations, want 1", len(ops))
	}
	if got := ops[0].RequestBody.Content[0].ContentType; got != mediaType {
		t.Fatalf("the loader changed the media type to %q; the fixture no longer tests what it claims", got)
	}

	return request.Inputs{Op: ops[0], Doc: doc, Body: []string{"{}"}}
}

// TestSpecDeclaredMediaTypeThatCannotBeAHeaderIsRefused covers the untrusted
// source: an OpenAPI content: key becomes a Content-Type header verbatim, and it
// is the one header value the binder's pair check never sees.
func TestSpecDeclaredMediaTypeThatCannotBeAHeaderIsRefused(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
	}{
		{"CRLF appending a header", "application/json\r\nX-Injected: pwned"},
		{"double CRLF ending the header block", "application/json\r\n\r\nGET /admin HTTP/1.1"},
		{"bare LF", "application/json\nX-Injected: pwned"},
		{"bare CR", "application/json\rX-Injected: pwned"},
		{"NUL byte", "application/json\x00"},
		{"whitespace only", "   "},
		{"control byte in a parameter", "application/json; charset=utf-8\x01"},
		{"not a media type at all", "application json"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := bodyUsageErr(t, hostileMediaTypeSpec(t, tc.mediaType))

			if !strings.Contains(msg, "createPet") {
				t.Errorf("error = %q, want it to name the operation", msg)
			}
			// Never echoed: the message goes to stderr, and echoing it would put
			// the CRLF — and the header it smuggles — on that surface instead.
			if strings.ContainsAny(msg, "\r\n\x00") || strings.Contains(msg, "X-Injected") {
				t.Errorf("error = %q, want the offending media type not echoed", msg)
			}
		})
	}
}

// TestWellFormedMediaTypesAreUnaffected pins the refusal against over-rejection.
// Length is not the threat — leaving the header field is — so a 64 KB media type
// of legal token characters is passed through like any other.
func TestWellFormedMediaTypesAreUnaffected(t *testing.T) {
	for _, mediaType := range []string{
		"application/json",
		"application/vnd.api+json;charset=utf-8",
		"text/plain; charset=utf-8",
		"multipart/form-data; boundary=----WebKitFormBoundary7MA4YWxk",
		"*/*",
		"application/" + strings.Repeat("a", 64<<10),
	} {
		t.Run(mediaType[:min(len(mediaType), 40)], func(t *testing.T) {
			in := hostileMediaTypeSpec(t, mediaType)

			if got := buildBody(t, in).ContentType; got != mediaType {
				t.Fatalf("content type = %q, want the declared media type unchanged", got)
			}

			req, err := request.Build(in)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			config, _, cleanup, err := curl.BuildConfig(req, curl.Capture{})
			t.Cleanup(cleanup)
			if err != nil {
				t.Fatalf("BuildConfig: %v", err)
			}
			if !strings.Contains(string(config), `header = "Content-Type: `+mediaType+`"`) {
				t.Errorf("config document has no Content-Type directive for %q", mediaType)
			}
		})
	}
}
