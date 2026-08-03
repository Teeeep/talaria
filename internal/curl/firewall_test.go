package curl

// This file tests the §5a firewall half of the package (firewall.go): the one
// crossing where a resolved credential exists, the CRLF gate every pair passes
// before it is written, and the zeroing that stops either lingering once curl
// has exited. config_test.go tests the other half — the shape of the document.

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
)

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
