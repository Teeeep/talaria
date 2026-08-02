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

	// curl is invoked as `curl -K -`, so its own stdin is the config document
	// the executor pipes in — a reader talaria creates, never the process's.
	if strings.Join(argv, " ") != "curl -K -" {
		t.Errorf("argv = %v, want curl to read its config from its own stdin", argv)
	}
}

func TestBodyFlagGivenTwiceIsUsageError(t *testing.T) {
	in := bodyInputs(t)
	in.Body = []string{`{"a":1}`, "@body.json"}

	if msg := bodyUsageErr(t, in); !strings.Contains(msg, "--body") {
		t.Errorf("error = %q, want it to name --body", msg)
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
