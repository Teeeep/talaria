package curl

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
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

	resp, err := Execute(getFrom(server.URL))
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

	resp, err := Execute(getFrom(server.URL))
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

func TestExecuteReportsAConnectionFailureAsRequestFailed(t *testing.T) {
	requireCurl(t)

	// A server that has been stopped leaves a port nothing is listening on,
	// without this test having to guess an unused one.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := server.URL
	server.Close()

	_, err := Execute(getFrom(base))

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

			resp, err := Execute(getFrom(server.URL))
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
