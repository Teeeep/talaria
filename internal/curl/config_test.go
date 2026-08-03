package curl

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
)

// buildConfig runs the builder and fails the test on error, for the cases whose
// subject is the document rather than the failure path.
func buildConfig(t *testing.T, req *request.Request, capture Capture) ([]byte, []string, func()) {
	t.Helper()

	config, argv, cleanup, err := BuildConfig(req, capture)
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	t.Cleanup(cleanup)

	return config, argv, cleanup
}

// directive reports whether the document contains the given line, ignoring the
// surrounding lines' order.
func hasDirective(config []byte, line string) bool {
	for _, got := range strings.Split(string(config), "\n") {
		if got == line {
			return true
		}
	}

	return false
}

func TestBuildConfigEmitsTheRequestAsDirectives(t *testing.T) {
	req := &request.Request{
		Method:  "POST",
		BaseURL: "https://api.example.com/v1",
		Path:    "/pets",
		Headers: []request.Pair{{Name: "Accept", Value: request.Literal("application/json")}},
	}

	config, _, _ := buildConfig(t, req, Capture{})

	want := []string{
		`url = "https://api.example.com/v1/pets"`,
		`request = "POST"`,
		`header = "Accept: application/json"`,
		"silent",
		"show-error",
		`write-out = "%{json}"`,
	}
	for _, line := range want {
		if !hasDirective(config, line) {
			t.Errorf("config is missing directive %s\ngot:\n%s", line, config)
		}
	}
}

// Defence in depth behind the scheme check in request.Build: a URL that slips
// past it, or a 302 pointing at file:///, still cannot make curl speak anything
// but http(s). The `=` prefix makes each list absolute rather than additive.
func TestBuildConfigConfinesCurlToHTTPSchemes(t *testing.T) {
	req := &request.Request{Method: "GET", BaseURL: "https://api.example.com", Path: "/pets"}

	config, _, _ := buildConfig(t, req, Capture{})

	for _, line := range []string{`proto = "=http,https"`, `proto-redir = "=http,https"`} {
		if !hasDirective(config, line) {
			t.Errorf("config is missing directive %s\ngot:\n%s", line, config)
		}
	}
}

func TestBuildConfigUsesTheHeadFlagForHEAD(t *testing.T) {
	dir := t.TempDir()
	capture := Capture{
		BodyPath:   filepath.Join(dir, "body"),
		HeaderPath: filepath.Join(dir, "headers"),
	}

	config, _, _ := buildConfig(t, &request.Request{
		Method:  http.MethodHead,
		BaseURL: "https://api.example.com",
		Path:    "/pets",
	}, capture)

	// `-X HEAD` leaves curl waiting for a Content-Length body a compliant server
	// never sends, and HEAD is a safe method, so one such operation would stall
	// a whole `run`.
	if !hasDirective(config, "head") {
		t.Errorf("config is missing the head flag\ngot:\n%s", config)
	}
	if hasDirective(config, `request = "HEAD"`) {
		t.Errorf("config still emits -X HEAD\ngot:\n%s", config)
	}

	// With head, curl writes the header block to the output file. Pointed at the
	// body capture, that reports the headers as the response body.
	if !hasDirective(config, `output = "`+os.DevNull+`"`) {
		t.Errorf("config does not divert head's output to %s\ngot:\n%s", os.DevNull, config)
	}
	if hasDirective(config, `output = "`+capture.BodyPath+`"`) {
		t.Errorf("config sends head's header block to the body capture\ngot:\n%s", config)
	}
	if !hasDirective(config, `dump-header = "`+capture.HeaderPath+`"`) {
		t.Errorf("config no longer captures the headers\ngot:\n%s", config)
	}
}

func TestBuildConfigResolvesCredentialsIntoTheDocumentButNotArgv(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	config, argv, _ := buildConfig(t, bearerReq(), Capture{})

	// The document is the one place a resolved credential is allowed to exist.
	if !hasDirective(config, `header = "Authorization: Bearer `+canary+`"`) {
		t.Errorf("config does not carry the resolved credential:\n%s", config)
	}

	for _, arg := range argv {
		if strings.Contains(arg, canary) {
			t.Fatalf("argv carries the credential value: %q", argv)
		}
	}
}

func TestBuildConfigArgvIsOnlyTheStdinConfigFlags(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	req := bearerReq()
	req.Query = []request.Pair{{Name: "limit", Value: request.Literal("10")}}
	req.Body = &request.Body{ContentType: "application/json", Data: []byte(`{"a":1}`)}

	_, argv, _ := buildConfig(t, req, Capture{
		BodyPath:   filepath.Join(t.TempDir(), "body"),
		HeaderPath: filepath.Join(t.TempDir(), "hdr"),
	})

	// §5a: /proc/*/cmdline is world-readable, so nothing about the request may be
	// an argument — not the URL, not a header, not an output path.
	assertArgv(t, argv)
}

// stdinArgv is the whole command line talaria ever gives curl. It is asserted
// by name in several places because both halves of it are load-bearing: `-K -`
// keeps the request out of /proc/*/cmdline, and `-q` keeps curl from reading a
// config file talaria did not write.
var stdinArgv = []string{"curl", "-q", "-K", "-"}

func assertArgv(t *testing.T, argv []string) {
	t.Helper()

	if len(argv) != len(stdinArgv) {
		t.Fatalf("argv = %q, want exactly %q", argv, stdinArgv)
	}
	for i, arg := range stdinArgv {
		if argv[i] != arg {
			t.Fatalf("argv = %q, want exactly %q", argv, stdinArgv)
		}
	}
}

// TestBuildConfigDisablesTheDefaultCurlrcOnEveryPath pins `-q`, and pins it
// first — curl ignores the option anywhere else.
//
// Without it curl parses $CURL_HOME/.curlrc (else $HOME/.curlrc) *before* the
// -K document, and every directive in that file applies to the request carrying
// the resolved credential: `trace-ascii` writes the plaintext Authorization
// header to a file of the writer's choosing, `proxy` ships it to a host of
// theirs. Writing one file under $HOME is a weaker capability than the env-var
// read §5a concedes, so this is inside the boundary talaria claims.
//
// The error paths are asserted too: BuildConfigWith returns argv on all three,
// and a caller that ran the failing one would be running an unprotected curl.
func TestBuildConfigDisablesTheDefaultCurlrcOnEveryPath(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		t.Setenv("TALARIA_AUTH_BEARER", canary)

		_, argv, cleanup, err := BuildConfig(bearerReq(), Capture{})
		t.Cleanup(cleanup)
		if err != nil {
			t.Fatalf("BuildConfig() error = %v", err)
		}

		assertArgv(t, argv)
	})

	t.Run("no request", func(t *testing.T) {
		_, argv, cleanup, err := BuildConfig(nil, Capture{})
		t.Cleanup(cleanup)
		if err == nil {
			t.Fatal("BuildConfig(nil) error = nil, want a failure")
		}

		assertArgv(t, argv)
	})

	t.Run("credential missing", func(t *testing.T) {
		t.Setenv("TALARIA_AUTH_BEARER", canary)
		os.Unsetenv("TALARIA_AUTH_BEARER") //nolint:errcheck // t.Setenv restores it.

		_, argv, cleanup, err := BuildConfig(bearerReq(), Capture{})
		t.Cleanup(cleanup)
		if err == nil {
			t.Fatal("BuildConfig() with the credential unset = nil, want a failure")
		}

		assertArgv(t, argv)
	})
}

func TestBuildConfigTakesCaptureFilesAsDirectivesNotArguments(t *testing.T) {
	dir := t.TempDir()
	capture := Capture{
		BodyPath:   filepath.Join(dir, "body"),
		HeaderPath: filepath.Join(dir, "headers"),
	}

	config, _, _ := buildConfig(t, &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
	}, capture)

	for _, line := range []string{
		`output = "` + capture.BodyPath + `"`,
		`dump-header = "` + capture.HeaderPath + `"`,
	} {
		if !hasDirective(config, line) {
			t.Errorf("config is missing directive %s\ngot:\n%s", line, config)
		}
	}
}

func TestBuildConfigEscapesBodyInTheVerifiedOrder(t *testing.T) {
	// A literal backslash-n next to a real newline is the double-escaping trap: a
	// sequential replace that escapes backslashes after newlines turns the real
	// newline into a literal one.
	req := &request.Request{
		Method:  "POST",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Body:    &request.Body{Data: []byte("a\\nb\nc\td\re\"f")},
	}

	config, _, _ := buildConfig(t, req, Capture{})

	const want = `data-raw = "a\\nb\nc\td\re\"f"`
	if !hasDirective(config, want) {
		t.Errorf("config body directive is wrong\ngot:\n%s\nwant it to contain:\n%s", config, want)
	}
}

func TestBuildConfigInlineBodyRoundTripsThroughRealCurl(t *testing.T) {
	// The escaping is only correct if curl's own parser agrees, so this asserts
	// against curl rather than against our idea of curl. A leading @ is in the
	// body deliberately: with `data` or `data-binary` curl would read it as a
	// filename, which is why the builder emits `data-raw`.
	body := []byte("{\"a\":\"x\ty\nz\\\"q\\\\b\r\",\"u\":\"héllo→€\"}@not-a-file")

	got := postThroughCurl(t, &request.Request{
		Method:  "POST",
		BaseURL: "", // filled in by postThroughCurl
		Path:    "/pets",
		Body:    &request.Body{ContentType: "application/json", Data: body},
	})

	if !bytes.Equal(got, body) {
		t.Errorf("curl sent %q, want byte-identical %q", got, body)
	}
}

func TestBuildConfigLargeBodyGoesToATempFile(t *testing.T) {
	body := bytes.Repeat([]byte("x"), maxInlineBody+1)
	req := &request.Request{
		Method:  "POST",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Body:    &request.Body{Data: body},
	}

	config, _, cleanup, err := BuildConfig(req, Capture{})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}

	path := bodyFilePath(t, config)
	if bytes.Contains(config, body) {
		t.Error("a body over the inline limit was still written into the config document")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("temp body file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("temp body file mode = %04o, want 0600", perm)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp body file: %v", err)
	}
	if !bytes.Equal(written, body) {
		t.Error("temp body file does not hold the body bytes")
	}

	cleanup()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cleanup left the temp body file behind: stat err = %v", err)
	}
}

func TestBuildConfigNonUTF8BodyGoesToATempFileRegardlessOfSize(t *testing.T) {
	// Short, but it cannot survive the config document: 0x00 would truncate the
	// quoted value and 0xff is not text curl's parser can carry back out.
	body := []byte{0x7b, 0xff, 0xfe, 0x00, 0x7d}
	req := &request.Request{
		Method:  "POST",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Body:    &request.Body{Data: body},
	}

	config, _, cleanup, err := BuildConfig(req, Capture{})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	defer cleanup()

	written, err := os.ReadFile(bodyFilePath(t, config))
	if err != nil {
		t.Fatalf("read temp body file: %v", err)
	}
	if !bytes.Equal(written, body) {
		t.Errorf("temp body file = %x, want %x", written, body)
	}
}

func TestBuildConfigMissingCredentialIsExitCodeFive(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)
	os.Unsetenv("TALARIA_AUTH_BEARER") //nolint:errcheck // t.Setenv restores it.

	config, _, cleanup, err := BuildConfig(bearerReq(), Capture{})
	if cleanup != nil {
		cleanup()
	}

	var cerr *clierr.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("BuildConfig() error = %v, want a *clierr.Error", err)
	}
	if cerr.Code != clierr.CodeCredentialMissing {
		t.Errorf("exit code = %d, want %d", cerr.Code, clierr.CodeCredentialMissing)
	}
	// An empty header is the failure mode this test exists to rule out: it would
	// send an unauthenticated request that looks authenticated.
	if config != nil {
		t.Errorf("a document was built despite the missing credential:\n%s", config)
	}
}

// owned is the whole array the document wrote into, not the slice it currently
// presents. Zeroing that a caller cannot see is the point: the defect this
// guards against scrubbed the copy handed back and left the original readable.
func owned(d *document) []byte { return d.b[:cap(d.b)] }

func TestBuildConfigCleanupZeroesTheDocument(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	doc, config, cleanup, err := buildDocument(bearerReq(), Capture{}, DefaultOptions())
	if err != nil {
		t.Fatalf("buildDocument() error = %v", err)
	}
	if !bytes.Contains(config, []byte(canary)) {
		t.Fatal("the document never carried the credential, so this test proves nothing")
	}
	// Zeroed at the copy-out rather than at cleanup: the shorter the window in
	// which two readable copies exist, the better.
	if bytes.Contains(owned(doc), []byte(canary)) {
		t.Error("the builder's own buffer still holds the credential after the copy out")
	}

	cleanup()

	if bytes.Contains(config, []byte(canary)) {
		t.Error("cleanup left the resolved credential in the config buffer")
	}
	if bytes.Contains(owned(doc), []byte(canary)) {
		t.Error("cleanup left the resolved credential in the document's own buffer")
	}
}

// TestBuildConfigZeroesTheDocumentWhenTheBuildFails covers the path nobody
// tested: build writes the resolved Authorization header first and fails on the
// cookie after it, so a document that scrubs nothing on failure leaves the
// credential in a buffer no caller can reach to clean.
func TestBuildConfigZeroesTheDocumentWhenTheBuildFails(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	req := bearerReq()
	req.Cookies = []request.Pair{{Name: "session", Value: request.Literal("a\r\nX-Injected: 1")}}

	doc, config, cleanup, err := buildDocument(req, Capture{}, DefaultOptions())
	if err == nil {
		t.Fatal("buildDocument() error = nil, want the CRLF refusal")
	}
	if config != nil {
		t.Errorf("a failed build returned a document:\n%s", config)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil on the failure path")
	}
	if bytes.Contains(owned(doc), []byte(canary)) {
		t.Error("a failed build left the resolved credential in the document's buffer")
	}

	// §5a's cleanup contract: non-nil on every path, and idempotent, because a
	// caller that already ran it on the error path still defers it.
	cleanup()
	cleanup()
}

func TestBuildConfigCleanupIsSafeToCallTwice(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	req := bearerReq()
	req.Method = "POST"
	req.Body = &request.Body{ContentType: "application/json", Data: []byte(`{"a":1}`)}

	doc, config, cleanup, err := buildDocument(req, Capture{}, DefaultOptions())
	if err != nil {
		t.Fatalf("buildDocument() error = %v", err)
	}

	cleanup()
	cleanup()

	if bytes.Contains(config, []byte(canary)) || bytes.Contains(owned(doc), []byte(canary)) {
		t.Error("the credential survived two cleanups")
	}
}

func TestBuildConfigResolvesQueryAndCookieCredentials(t *testing.T) {
	t.Setenv("TALARIA_AUTH_APIKEY_Q", canary+"-query")
	t.Setenv("TALARIA_AUTH_APIKEY_C", canary+"-cookie")

	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query: []request.Pair{
			{Name: "api_key", Value: request.Secret(secret.Env("TALARIA_AUTH_APIKEY_Q"), request.EncodeRaw)},
		},
		Cookies: []request.Pair{
			{Name: "flavour", Value: request.Literal("salty")},
			{Name: "session", Value: request.Secret(secret.Env("TALARIA_AUTH_APIKEY_C"), request.EncodeRaw)},
		},
	}

	config, _, _ := buildConfig(t, req, Capture{})

	if !hasDirective(config, `url = "https://api.example.com/pets?api_key=`+canary+`-query"`) {
		t.Errorf("query credential was not resolved into the url directive:\n%s", config)
	}
	if !hasDirective(config, `cookie = "flavour=salty; session=`+canary+`-cookie"`) {
		t.Errorf("cookie credential was not resolved into the cookie directive:\n%s", config)
	}
}

func TestBuildConfigRendersBasicAuthAsTheUserDirective(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BASIC", "alice:"+canary)

	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Headers: []request.Pair{{
			Name:  "Authorization",
			Value: request.Secret(secret.Env("TALARIA_AUTH_BASIC"), request.EncodeBasic),
		}},
	}

	config, _, _ := buildConfig(t, req, Capture{})

	// curl does the base64, so the raw user:password never has to be encoded here.
	if !hasDirective(config, `user = "alice:`+canary+`"`) {
		t.Errorf("basic auth is not a user directive:\n%s", config)
	}
	if bytes.Contains(config, []byte(`header = "Authorization:`)) {
		t.Errorf("basic auth was also emitted as a header:\n%s", config)
	}
}

// TestBuildConfigRejectsCRLFThatWouldSplitTheRequest is the last gate, and it
// is not redundant with the binder's: not every Request comes from
// request.Build — `history replay` rebuilds one from a stored entry, which is
// untrusted input read back off disk. Escaping is not the answer here, because
// escapeDirective only protects curl's own parser: curl un-escapes \r\n back to
// the two bytes and writes them to the socket, ending the header line early.
func TestBuildConfigRejectsCRLFThatWouldSplitTheRequest(t *testing.T) {
	const injection = "ok\r\nX-Injected: 1"

	base := func() *request.Request {
		return &request.Request{Method: "GET", BaseURL: "https://api.example.com", Path: "/pets"}
	}

	tests := []struct {
		name string
		req  func() *request.Request
	}{
		{"header value", func() *request.Request {
			req := base()
			req.Headers = []request.Pair{{Name: "X-Trace", Value: request.Literal(injection)}}
			return req
		}},
		{"header name", func() *request.Request {
			req := base()
			req.Headers = []request.Pair{{Name: "X-Trace\r\nX-Injected", Value: request.Literal("ok")}}
			return req
		}},
		{"basic auth credential", func() *request.Request {
			req := base()
			req.Headers = []request.Pair{{
				Name:  "Authorization",
				Value: request.Secret(secret.Env("TALARIA_AUTH_BASIC"), request.EncodeBasic),
			}}
			return req
		}},
		{"cookie value", func() *request.Request {
			req := base()
			req.Cookies = []request.Pair{{Name: "flavour", Value: request.Literal(injection)}}
			return req
		}},
		{"cookie name", func() *request.Request {
			req := base()
			req.Cookies = []request.Pair{{Name: "flavour\r\nX-Injected: 1", Value: request.Literal("salty")}}
			return req
		}},
		{"method", func() *request.Request {
			req := base()
			req.Method = "GET /admin HTTP/1.1"
			return req
		}},
		{"method with a CRLF", func() *request.Request {
			req := base()
			req.Method = "GET\r\nX-Injected: 1"
			return req
		}},
		// The body's media type is the header value that does not come from a
		// --header pair: it is a key of the spec's `content:` map, so it is the
		// one Content-Type this package sees without the binder having checked
		// it — and `history replay` rebuilds a Request without the binder at all.
		{"body media type", func() *request.Request {
			req := base()
			req.Body = &request.Body{ContentType: "application/json" + injection, Data: []byte("{}")}
			return req
		}},
		{"body media type ending the header block", func() *request.Request {
			req := base()
			req.Body = &request.Body{
				ContentType: "application/json\r\n\r\nGET /admin HTTP/1.1\r\n",
				Data:        []byte("{}"),
			}
			return req
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TALARIA_AUTH_BASIC", "alice:pw\r\nX-Injected: 1")

			config, _, cleanup, err := BuildConfig(tc.req(), Capture{})
			if cleanup != nil {
				cleanup()
			}

			if err == nil {
				t.Fatalf("BuildConfig() succeeded, want a refusal; document:\n%s", config)
			}
			if code := clierr.From(err).Code; code != clierr.CodeUsage {
				t.Errorf("exit code = %d, want %d", code, clierr.CodeUsage)
			}
			// The document is what curl reads. A refusal that still returns one
			// is a refusal a caller can ignore by accident.
			if config != nil {
				t.Errorf("a document was built despite the refusal:\n%s", config)
			}
		})
	}
}

// bodyFilePath extracts the path from the document's data-binary = "@path"
// directive, failing the test if the body was not diverted to a file.
func bodyFilePath(t *testing.T, config []byte) string {
	t.Helper()

	match := regexp.MustCompile(`(?m)^data-binary = "@(.*)"$`).FindSubmatch(config)
	if match == nil {
		t.Fatalf("config has no temp-file body directive:\n%s", config)
	}

	return string(match[1])
}

// postThroughCurl runs the built document through the real curl binary against a
// test server and returns the body the server received.
func postThroughCurl(t *testing.T, req *request.Request) []byte {
	t.Helper()

	curlPath, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}

	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	req.BaseURL = server.URL
	config, argv, cleanup, err := BuildConfig(req, Capture{})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	defer cleanup()

	cmd := exec.Command(curlPath, argv[1:]...)
	cmd.Stdin = bytes.NewReader(config)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("curl failed: %v\n%s\nconfig:\n%s", err, out, config)
	}

	return received
}
