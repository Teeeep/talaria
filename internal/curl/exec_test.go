package curl

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
)

// requireCurl skips a test that needs the real binary. curl is talaria's
// execution engine (§3.4), so these tests drive it rather than a stub: a fake
// would only prove the parser agrees with itself.
func requireCurl(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
}

// getFrom returns a GET request against a test server's URL.
func getFrom(base string) *request.Request {
	return &request.Request{Method: http.MethodGet, BaseURL: base, Path: "/pets"}
}

// serve starts a test server and stops it when the test ends.
func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server
}

// requireCLIError unwraps err to the *clierr.Error the exit-code contract is
// carried on, failing the test if it is not one.
func requireCLIError(t *testing.T, err error) *clierr.Error {
	t.Helper()

	if err == nil {
		t.Fatal("Execute() error = nil, want a failure")
	}

	var cerr *clierr.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("Execute() error = %T (%v), want *clierr.Error", err, err)
	}

	return cerr
}

func TestExecuteReturnsStatusBodyHeadersAndTiming(t *testing.T) {
	requireCurl(t)

	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		// The sleep is what makes timing observable: a loopback round trip can
		// otherwise finish inside curl's reporting resolution and round to zero,
		// which would make the assertion below a coin flip rather than a test.
		time.Sleep(15 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-42")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":1,"name":"rex"}`) //nolint:errcheck // Test server.
	})

	resp, err := Execute(t.Context(), getFrom(server.URL))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if resp.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", resp.Status)
	}
	if got, want := string(resp.Body), `{"id":1,"name":"rex"}`; got != want {
		t.Errorf("Body = %q, want %q", got, want)
	}
	if got := resp.Headers.Get("X-Request-Id"); got != "req-42" {
		t.Errorf("Headers[X-Request-Id] = %q, want %q", got, "req-42")
	}
	if got := resp.Headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Headers[Content-Type] = %q, want %q", got, "application/json")
	}
	if resp.TimingMS <= 0 {
		t.Errorf("TimingMS = %d, want > 0", resp.TimingMS)
	}
}

func TestExecuteKeepsBodyAndMetadataOnSeparateChannels(t *testing.T) {
	requireCurl(t)

	// A response body that impersonates curl's own --write-out payload, closing
	// braces and all. If the body ever shared a stream with the metadata, the
	// status below would come back as 599 rather than 200.
	body := `{"http_code":599,"time_total":42.0,"note":"} not the real metadata {"}`
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body) //nolint:errcheck // Test server.
	})

	resp, err := Execute(t.Context(), getFrom(server.URL))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if resp.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200 — the body was parsed as metadata", resp.Status)
	}
	if string(resp.Body) != body {
		t.Errorf("Body = %q, want %q", resp.Body, body)
	}
	if resp.TimingMS > 1000 {
		t.Errorf("TimingMS = %d, want the real timing, not the body's 42s", resp.TimingMS)
	}
}

func TestExecuteCompletesAHEADRequest(t *testing.T) {
	requireCurl(t)

	// A compliant server answers HEAD with the Content-Length the GET would have
	// carried and no body at all. `-X HEAD` makes curl wait for those bytes
	// forever (or exit 18 when the connection closes); `--head` is what makes
	// this return. There is no max-time yet, so the deadline lives here rather
	// than letting a regression hang the suite.
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "42")
		w.Header().Set("X-Request-Id", "req-42")
		w.WriteHeader(http.StatusOK)
	})

	req := getFrom(server.URL)
	req.Method = http.MethodHead

	type outcome struct {
		resp *Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := Execute(t.Context(), req)
		done <- outcome{resp, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Execute() error = %v", got.err)
		}
		if got.resp.Status != http.StatusOK {
			t.Errorf("Status = %d, want 200", got.resp.Status)
		}
		if id := got.resp.Headers.Get("X-Request-Id"); id != "req-42" {
			t.Errorf("Headers[X-Request-Id] = %q, want %q", id, "req-42")
		}
		// The header block goes to the dump, not to the body: a HEAD response has
		// no body, and reporting the headers as one would be a fabricated body.
		if len(got.resp.Body) != 0 {
			t.Errorf("Body = %q, want empty for a HEAD response", got.resp.Body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute() on HEAD did not return in 10s — curl is waiting for a body the server never sends")
	}
}

// hangingServer listens, accepts, and never answers — the shape of a wedged API
// or a dropped packet filter. A raw listener rather than httptest.Server: a
// handler that blocks also blocks the server's own Close, so the test would hang
// in cleanup instead of in the call it is measuring.
func hangingServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	accepted := make(chan struct{})
	go func() {
		defer close(accepted)

		var conns []net.Conn
		defer func() {
			for _, conn := range conns {
				conn.Close() //nolint:errcheck // Test teardown.
			}
		}()

		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Held rather than closed: a closed connection is a failure curl
			// reports at once, and this test is about the one it never would.
			conns = append(conns, conn)
		}
	}()
	t.Cleanup(func() {
		ln.Close() //nolint:errcheck // Test teardown.
		<-accepted
	})

	return "http://" + ln.Addr().String()
}

func TestBuildConfigBoundsTheCallInTime(t *testing.T) {
	config, _, _ := buildConfig(t, getFrom("https://api.example.com"), Capture{})

	// The defaults, in the document rather than in Go: curl enforces both itself
	// and exits 28, so nothing on the happy path has to watch the clock.
	for _, want := range []string{`connect-timeout = "10"`, `max-time = "30"`} {
		if !hasDirective(config, want) {
			t.Errorf("document has no %q directive:\n%s", want, config)
		}
	}
}

func TestBuildConfigWithUsesTheGivenTimeouts(t *testing.T) {
	opts := Options{ConnectTimeout: 250 * time.Millisecond, MaxTime: 1500 * time.Millisecond}

	config, _, cleanup, err := BuildConfigWith(getFrom("https://api.example.com"), Capture{}, opts)
	if err != nil {
		t.Fatalf("BuildConfigWith() error = %v", err)
	}
	t.Cleanup(cleanup)

	// Fractional seconds, because a sub-second bound is exactly what a test or a
	// tight CI budget asks for and curl accepts a decimal here.
	for _, want := range []string{`connect-timeout = "0.25"`, `max-time = "1.5"`} {
		if !hasDirective(config, want) {
			t.Errorf("document has no %q directive:\n%s", want, config)
		}
	}
}

func TestExecuteWithGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	requireCurl(t)

	opts := Options{ConnectTimeout: 500 * time.Millisecond, MaxTime: 500 * time.Millisecond}
	req := getFrom(hangingServer(t))

	type outcome struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		_, err := ExecuteWith(t.Context(), req, opts)
		done <- outcome{err, time.Since(start)}
	}()

	select {
	case got := <-done:
		cerr := requireCLIError(t, got.err)
		if cerr.Code != clierr.CodeRequestFailed {
			t.Errorf("Code = %d, want %d", cerr.Code, clierr.CodeRequestFailed)
		}
		// 28 is curl's own timeout status. Asserting it rather than any non-zero
		// exit is what distinguishes curl honouring max-time from talaria killing
		// a curl that ignored it — the fallback, not the mechanism.
		if !strings.Contains(cerr.Message, "curl exited 28") {
			t.Errorf("Message = %q, want curl's timeout status (28) in it", cerr.Message)
		}
		if got.elapsed > 5*time.Second {
			t.Errorf("ExecuteWith() took %s with a 500ms max-time, want the timeout to bound it",
				got.elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ExecuteWith() never returned against a server that never answers")
	}
}

func TestExecuteReportsAConnectionFailureAsRequestFailed(t *testing.T) {
	requireCurl(t)

	// A server that has been stopped leaves a port nothing is listening on,
	// without this test having to guess an unused one.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := server.URL
	server.Close()

	_, err := Execute(t.Context(), getFrom(base))

	cerr := requireCLIError(t, err)
	if cerr.Code != clierr.CodeRequestFailed {
		t.Errorf("Code = %d, want %d", cerr.Code, clierr.CodeRequestFailed)
	}
	// The exact status is curl's to choose (7 for a refused connection); that one
	// is reported at all is what an operator needs to look it up.
	if !regexp.MustCompile(`curl exited [1-9][0-9]*`).MatchString(cerr.Message) {
		t.Errorf("Message = %q, want curl's non-zero exit status in it", cerr.Message)
	}
}

func TestExecuteTreatsHTTPErrorsAsSuccessfulObservations(t *testing.T) {
	requireCurl(t)

	// §4: a 404 is a successful observation, not a failed request. Only
	// --fail-on-error changes that, and it is not this layer's decision.
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, `{"error":"nope"}`) //nolint:errcheck // Test server.
			})

			resp, err := Execute(t.Context(), getFrom(server.URL))
			if err != nil {
				t.Fatalf("Execute() error = %v, want nil for HTTP %d", err, status)
			}
			if resp.Status != status {
				t.Errorf("Status = %d, want %d", resp.Status, status)
			}
			if string(resp.Body) != `{"error":"nope"}` {
				t.Errorf("Body = %q, want the error body", resp.Body)
			}
		})
	}
}

func TestCheckVersionEnforcesTheFloor(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{
			name:    "below the floor",
			output:  "curl 7.69.1 (x86_64-pc-linux-gnu) libcurl/7.69.1 OpenSSL/1.1.1",
			wantErr: true,
		},
		{
			name:   "exactly the floor",
			output: "curl 7.70.0 (x86_64-pc-linux-gnu) libcurl/7.70.0 OpenSSL/1.1.1",
		},
		{
			name:   "above the floor",
			output: "curl 8.14.1 (x86_64-pc-linux-gnu) libcurl/8.14.1 OpenSSL/3.5.0\nRelease-Date: 2025-06-04",
		},
		{
			name:   "a newer major with a lower minor",
			output: "curl 8.0.1 (x86_64-pc-linux-gnu) libcurl/8.0.1",
		},
		{
			name:    "unparseable",
			output:  "not curl at all",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkVersion(tt.output)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkVersion() error = %v, wantErr = %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), minVersion) {
				t.Errorf("error = %q, want the %s floor named in it", err, minVersion)
			}
		})
	}
}

func TestParseHeadersTakesTheLastBlock(t *testing.T) {
	// A redirect leaves the intermediate response's headers in front of the real
	// one. Taking the last block is what keeps a stale Location or a 100 Continue
	// out of the reported response.
	dump := "HTTP/1.1 301 Moved Permanently\r\n" +
		"Location: /pets/42\r\n" +
		"X-Stale: yes\r\n" +
		"\r\n" +
		"HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/json\r\n" +
		"X-Fresh: yes\r\n" +
		"\r\n"

	headers := parseHeaders([]byte(dump))

	if got := headers.Get("X-Fresh"); got != "yes" {
		t.Errorf("X-Fresh = %q, want %q", got, "yes")
	}
	if got := headers.Get("X-Stale"); got != "" {
		t.Errorf("X-Stale = %q, want it dropped with the redirect block", got)
	}
	if got := headers.Get("Location"); got != "" {
		t.Errorf("Location = %q, want it dropped with the redirect block", got)
	}
}

func TestRequestFailedRedactsCredentialsInCurlStderr(t *testing.T) {
	const canary = "canary-9f3a-not-in-any-output"
	t.Setenv("TALARIA_AUTH_APIKEY", canary)

	req := &request.Request{
		Method:  http.MethodGet,
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query: []request.Pair{{
			Name:  "api_key",
			Value: request.Secret(secret.Env("TALARIA_AUTH_APIKEY"), request.EncodeRaw),
		}},
	}
	// curl echoes the URL it was given into several of its errors, and the URL is
	// where an API-key credential lives. §5a: errors are built from the redacted
	// representation.
	stderr := "curl: (60) SSL certificate problem for https://api.example.com/pets?api_key=" + canary

	err := requestFailed(req, 60, stderr)

	if strings.Contains(err.Error(), canary) {
		t.Fatalf("error leaked the credential: %q", err)
	}
	if !strings.Contains(err.Error(), "<redacted:env:TALARIA_AUTH_APIKEY>") {
		t.Errorf("error = %q, want the credential shown as its redacted ref", err)
	}
	if !strings.Contains(err.Error(), "curl exited 60") {
		t.Errorf("error = %q, want curl's exit status in it", err)
	}
}

func TestRequestFailedRedactsSensitiveLiteralsInCurlStderr(t *testing.T) {
	// A literal the user typed under a credential-shaped name — `--query
	// api_key=…` — is marked Sensitive at bind time and shows as <redacted>
	// everywhere else. curl's own error text is an output surface like any
	// other, so it has to be scrubbed the same way (§5a).
	const canary = "canary-9f3a/not-in-any-output"

	req := &request.Request{
		Method:  http.MethodGet,
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query: []request.Pair{{
			Name:  "api_key",
			Value: request.Literal(canary).Sensitive(),
		}},
	}
	// Both forms: curl echoes the URL it was given, which carries the value
	// percent-encoded, and its other messages quote what it was handed.
	stderr := "curl: (60) SSL certificate problem for https://api.example.com/pets?api_key=" +
		url.QueryEscape(canary) + " (value " + canary + ")"

	err := requestFailed(req, 60, stderr)

	if strings.Contains(err.Error(), canary) {
		t.Fatalf("error leaked the literal: %q", err)
	}
	if strings.Contains(err.Error(), url.QueryEscape(canary)) {
		t.Fatalf("error leaked the percent-encoded literal: %q", err)
	}
	if !strings.Contains(err.Error(), secret.Placeholder) {
		t.Errorf("error = %q, want the literal shown as %s", err, secret.Placeholder)
	}
}

func TestExecuteDoesNotLeakASensitiveLiteralWhenCurlRejectsTheURL(t *testing.T) {
	requireCurl(t)

	const canary = "canary-4c71-not-in-any-output"

	// A `[` in the path is a glob curl parses itself: it exits 3 and prints the
	// whole URL back, credential included, before any request is made.
	req := &request.Request{
		Method:  http.MethodGet,
		BaseURL: "https://api.example.invalid",
		Path:    "/pets[/1",
		Query: []request.Pair{{
			Name:  "api_key",
			Value: request.Literal(canary).Sensitive(),
		}},
	}

	_, err := Execute(t.Context(), req)
	if err == nil {
		t.Fatal("Execute() error = nil, want curl to reject the URL")
	}

	if strings.Contains(err.Error(), canary) {
		t.Fatalf("error leaked the literal: %q", err)
	}
	if !strings.Contains(err.Error(), secret.Placeholder) {
		t.Errorf("error = %q, want the literal shown as %s", err, secret.Placeholder)
	}
}

// waitFor blocks on signal until it fires or the test gives up, so a wiring
// mistake fails with the description rather than hanging the package.
func waitFor(t *testing.T, signal <-chan struct{}, within time.Duration, describe string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(within):
		t.Fatalf("timed out after %s waiting for %s", within, describe)
	}
}

// assertNoTalariaTemp fails if talaria left any of its own temp files behind in
// dir. It is the assertion behind the whole of Task 6: a call that ended early
// must not leave the unredacted response, or a request body, on disk.
func assertNoTalariaTemp(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), capturePrefix) || strings.HasPrefix(entry.Name(), bodyPrefix) {
			t.Errorf("%s survived the call: the response or request body is still on disk", entry.Name())
		}
	}
}

func TestExecuteWithKillsCurlWhenTheContextIsCancelled(t *testing.T) {
	requireCurl(t)

	// Every temp file this call makes lands here and nowhere else, which is what
	// makes "left nothing behind" assertable rather than approximate.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	started, dropped := make(chan struct{}, 1), make(chan struct{}, 1)
	server := serve(t, func(_ http.ResponseWriter, r *http.Request) {
		// Drained first: net/http only watches a connection for the peer going
		// away once the handler has consumed the request body, so an unread body
		// would leave the assertion below unable to observe anything at all.
		io.Copy(io.Discard, r.Body) //nolint:errcheck // Test server.
		started <- struct{}{}
		select {
		// The server sees curl's connection go away only if curl actually died.
		// An orphaned curl reparented to init would hold this open and finish
		// the request long after talaria returned, which is the bug.
		case <-r.Context().Done():
			dropped <- struct{}{}
		case <-time.After(10 * time.Second):
		}
	})

	req := getFrom(server.URL)
	req.Method = http.MethodPost
	// A NUL byte cannot be inlined into the config document, so this body takes
	// the temp-file path — the second thing a killed talaria used to leave behind.
	req.Body = &request.Body{ContentType: "application/octet-stream", Data: []byte{'a', 0, 'b'}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := ExecuteWith(ctx, req, Options{MaxTime: 30 * time.Second})
		done <- err
	}()

	waitFor(t, started, 10*time.Second, "curl to reach the server")
	cancel()

	select {
	case err := <-done:
		cerr := requireCLIError(t, err)
		if cerr.Code != clierr.CodeRequestFailed {
			t.Errorf("Code = %d, want %d", cerr.Code, clierr.CodeRequestFailed)
		}
		// Distinct from the max-time message: a cancelled request is the caller's
		// doing, and reporting it as a timeout would send an agent looking for a
		// slow API that does not exist.
		if !strings.Contains(cerr.Message, "cancel") {
			t.Errorf("Message = %q, want it to say the request was cancelled", cerr.Message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ExecuteWith() did not return after its context was cancelled")
	}

	waitFor(t, dropped, 10*time.Second, "the server to see curl go away")
	assertNoTalariaTemp(t, tmp)
}

func TestSweepStaleRemovesOnlyTalariaTempFilesPastTheGrace(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	aged := time.Now().Add(-2 * staleGrace)
	stale := []string{capturePrefix + "old", bodyPrefix + "old"}
	kept := []string{capturePrefix + "live", bodyPrefix + "live", "someone-elses-old"}

	for _, name := range []string{capturePrefix + "old", capturePrefix + "live"} {
		path := filepath.Join(tmp, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("staging %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(path, "body"), []byte("secret"), 0o600); err != nil {
			t.Fatalf("staging %s: %v", name, err)
		}
	}
	for _, name := range []string{bodyPrefix + "old", bodyPrefix + "live", "someone-elses-old"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("body"), 0o600); err != nil {
			t.Fatalf("staging %s: %v", name, err)
		}
	}
	for _, name := range []string{capturePrefix + "old", bodyPrefix + "old", "someone-elses-old"} {
		if err := os.Chtimes(filepath.Join(tmp, name), aged, aged); err != nil {
			t.Fatalf("ageing %s: %v", name, err)
		}
	}

	SweepStale()

	for _, name := range stale {
		if _, err := os.Lstat(filepath.Join(tmp, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep, err = %v", name, err)
		}
	}
	for _, name := range kept {
		if _, err := os.Lstat(filepath.Join(tmp, name)); err != nil {
			t.Errorf("%s was swept but should have been left alone: %v", name, err)
		}
	}
}
