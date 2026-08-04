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
	"unsafe"

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
//
// It can only ever see the array the document ended on. The arrays abandoned on
// the way there are TestBuildZeroesTheArrayItAbandonsWhenTheBufferGrows.
func owned(d *document) []byte { return d.b[:cap(d.b)] }

// cookieReq is a GET whose credential goes in a cookie: the other place in the
// document a resolved credential is written, and the one the builder assembles
// by joining several values into a single directive. The secret is last so the
// directive ends with it.
func cookieReq() *request.Request {
	return &request.Request{
		OperationID: "getPet",
		Method:      "GET",
		BaseURL:     "https://api.example.com/v1",
		Path:        "/pets/42",
		Cookies: []request.Pair{
			{Name: "flavour", Value: request.Literal("salty")},
			{Name: "session", Value: request.Secret(secret.Env("TALARIA_AUTH_APIKEY"), request.EncodeRaw)},
		},
	}
}

// twoCookieReq is cookieReq with a second credential, so the joined directive
// carries more than one.
func twoCookieReq() *request.Request {
	req := cookieReq()
	req.Cookies = append(req.Cookies, request.Pair{
		Name:  "sso",
		Value: request.Secret(secret.Env("TALARIA_AUTH_SSO"), request.EncodeRaw),
	})

	return req
}

// longCanary is a token several times the size of the buffer it is written
// into, so its own directive forces the reallocation rather than the ones after
// it. It contains canary at both ends, which is what lets every assertion below
// search for canary alone.
var longCanary = canary + strings.Repeat("-x", 4096) + canary

// credentialShapes are the ways a resolved credential reaches the document,
// hostile sizes and counts included. Every value contains canary, and last is
// the one whose text ends its directive — which is where a test that sizes a
// buffer to the credential has to measure to.
var credentialShapes = []struct {
	name string
	env  map[string]string
	last string
	req  func() *request.Request
}{
	{
		name: "bearer header",
		env:  map[string]string{"TALARIA_AUTH_BEARER": canary},
		last: canary,
		req:  bearerReq,
	},
	{
		name: "cookie",
		env:  map[string]string{"TALARIA_AUTH_APIKEY": canary},
		last: canary,
		req:  cookieReq,
	},
	{
		name: "two cookie credentials in one directive",
		env: map[string]string{
			"TALARIA_AUTH_APIKEY": canary,
			"TALARIA_AUTH_SSO":    canary + "-sso",
		},
		last: canary + "-sso",
		req:  twoCookieReq,
	},
	{
		name: "a bearer token far larger than the buffer",
		env:  map[string]string{"TALARIA_AUTH_BEARER": longCanary},
		last: longCanary,
		req:  bearerReq,
	},
}

// setenv installs a case's credentials for the duration of the test.
func setenv(t *testing.T, env map[string]string) {
	t.Helper()
	for name, value := range env {
		t.Setenv(name, value)
	}
}

func TestBuildConfigCleanupZeroesTheDocument(t *testing.T) {
	for _, tc := range credentialShapes {
		t.Run(tc.name, func(t *testing.T) {
			setenv(t, tc.env)

			doc, config, cleanup, err := buildDocument(tc.req(), Capture{}, DefaultOptions())
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
		})
	}
}

// TestBuildZeroesTheArrayItAbandonsWhenTheBufferGrows is the half owned() is
// blind to. A buffer that grows by plain append allocates, copies and drops the
// old array with the resolved credential still in it: discard() then scrubs the
// surviving array while several earlier ones stay readable for as long as the
// collector leaves them alone. A minimal request reallocates five times.
//
// The seeded capacity stops exactly where the credential's directive ends, so
// that directive lands in the seeded array and the ones after it do not fit —
// which is the abandonment, made deterministic. The precondition is positional
// rather than a read of the array afterwards, because a build that zeroes what
// it abandons has already cleared it by the time the test could look.
func TestBuildZeroesTheArrayItAbandonsWhenTheBufferGrows(t *testing.T) {
	for _, tc := range credentialShapes {
		t.Run(tc.name, func(t *testing.T) {
			setenv(t, tc.env)

			_, config, cleanup, err := buildDocument(tc.req(), Capture{}, DefaultOptions())
			if err != nil {
				t.Fatalf("buildDocument() error = %v", err)
			}
			at := bytes.Index(config, []byte(tc.last))
			if at < 0 {
				t.Fatalf("the document never carried the credential:\n%s", config)
			}
			// The last credential is the last thing on its line, so its directive
			// ends two bytes later: the closing quote and the newline.
			end, full := at+len(tc.last)+2, len(config)
			cleanup()

			seed := make([]byte, 0, end)
			held := seed[:cap(seed)]

			d := &document{b: seed}
			if err := d.build(tc.req(), Capture{}, DefaultOptions()); err != nil {
				t.Fatalf("build() error = %v", err)
			}
			t.Cleanup(d.cleanup)

			if full <= cap(seed) || cap(d.b) <= cap(seed) {
				t.Fatalf("the document (%d bytes) never outgrew the seeded array (cap %d), "+
					"so nothing was abandoned and this test proves nothing", full, cap(seed))
			}

			d.cleanup()
			d.discard()

			if bytes.Contains(held, []byte(canary)) {
				t.Error("the array the buffer outgrew still holds a resolved credential")
			}
			if bytes.Contains(owned(d), []byte(canary)) {
				t.Error("discard left a resolved credential in the surviving array")
			}
		})
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

// TestEscapingAValueNeedingNoEscapeDoesNotCopyIt holds the reasoning in
// document.directive honest. The escaped value is written as its own piece so
// that no concatenation carrying the credential is built, which only helps if
// the escaper itself does not copy: configEscape's olds are all single bytes,
// so strings.NewReplacer returns a byteStringReplacer, which returns the string
// it was given when nothing in it matched. Give it a multi-byte old and it
// becomes a generic replacer that allocates on every call, credential included.
func TestEscapingAValueNeedingNoEscapeDoesNotCopyIt(t *testing.T) {
	value := canary

	if got := escapeDirective(value); unsafe.StringData(got) != unsafe.StringData(value) {
		t.Error("escapeDirective copied a value that needed no escaping, into a string " +
			"nothing can zero")
	}
	// The other half: a value that does need escaping is a copy, necessarily.
	if got := escapeDirective(`a"b`); got != `a\"b` {
		t.Errorf("escapeDirective(`a\"b`) = %q, want %q", got, `a\"b`)
	}
}

// TestResolvingACredentialDoesNotCopyItOutOfTheEnvironment is the other half,
// at the crossing itself. `v.Prefix() + value` was a Go string holding the
// resolved credential, allocated on every resolve and readable for as long as
// the collector left it alone — the residue §5a concedes is os.Getenv's own
// immutable string, and a second copy of the same kind is not covered by that
// concession. Returning the two pieces means the value that comes back *is* the
// environment's string rather than a copy of it, which pointer identity is the
// only way to state.
func TestResolvingACredentialDoesNotCopyItOutOfTheEnvironment(t *testing.T) {
	const name = "TALARIA_AUTH_BEARER"
	t.Setenv(name, canary)

	prefix, value, err := resolveParts(request.Secret(secret.Env(name), request.EncodeBearer))
	if err != nil {
		t.Fatalf("resolveParts() error = %v", err)
	}
	if prefix != "Bearer " || value != canary {
		t.Fatalf("resolveParts() = %q, %q, want %q, %q", prefix, value, "Bearer ", canary)
	}

	if unsafe.StringData(value) != unsafe.StringData(os.Getenv(name)) {
		t.Error("resolveParts copied the credential out of the environment's own string, " +
			"into one nothing can address or zero")
	}
}

// TestWritingACredentialAllocatesNothingToHoldIt is the same reasoning one
// level up, over the two places a resolved credential is written: document.auth
// and document.cookies. `h.Name + ": " + value` and `v.Prefix() + value` are
// each one allocation whose bytes are the credential, in a Go string this
// package can neither address nor zero — the hazard directive() is written in
// four pieces to avoid, reintroduced at the call site and inside resolve.
//
// Allocation count is what makes that visible: every piece these two write is
// either a constant or a string the caller already held (the environment's own,
// via escapeDirective, which returns its argument when nothing needs escaping),
// so writing into a buffer with room for them copies the credential nowhere and
// allocates nothing. One allocation is one copy.
func TestWritingACredentialAllocatesNothingToHoldIt(t *testing.T) {
	for _, tc := range credentialShapes {
		t.Run(tc.name, func(t *testing.T) {
			setenv(t, tc.env)

			req := tc.req()
			// Sized past the longest document these shapes build, so a write never
			// grows the buffer: grow's allocation is the one this test must not
			// count, and it is TestBuildZeroesTheArrayItAbandonsWhenTheBufferGrows'
			// subject rather than this one's.
			d := &document{b: make([]byte, 0, 4*len(longCanary))}
			t.Cleanup(d.discard)

			allocs := testing.AllocsPerRun(10, func() {
				d.b = d.b[:0]
				if err := d.auth(req); err != nil {
					t.Fatalf("auth() error = %v", err)
				}
				if err := d.cookies(req); err != nil {
					t.Fatalf("cookies() error = %v", err)
				}
			})

			if !bytes.Contains(owned(d), []byte(canary)) {
				t.Fatal("nothing wrote the credential, so this test proves nothing")
			}
			if allocs != 0 {
				t.Errorf("writing the credential allocated %v times; every allocation here is "+
					"a copy of it in a string nothing can zero", allocs)
			}
		})
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
