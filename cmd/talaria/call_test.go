package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/spec"
)

// callResponseSecret is a credential-shaped value the *server* sends back. It
// stands in for a session cookie an API hands out: the pretty renderer must not
// print it, because a human reading a call is not asking to have a token pasted
// into their scrollback.
const callResponseSecret = "server-side-session-token"

// callExecJSON is the §4 envelope for an executed call: the dry run's request
// block plus the response block only a real call has.
type callExecJSON struct {
	Schema  string `json:"schema"`
	DryRun  bool   `json:"dry_run"`
	Request struct {
		Curl    string            `json:"curl"`
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response *struct {
		Status  int                 `json:"status"`
		Headers map[string][]string `json:"headers"`
		Body    json.RawMessage     `json:"body"`
		// A pointer so an absent timing_ms is distinguishable from a call to a
		// local server that legitimately rounded to 0ms.
		TimingMS *int64 `json:"timing_ms"`
	} `json:"response"`
}

// recordedRequest is what actually arrived at the test server, credential
// included. Asserting on it is how a test proves the real value went on the
// wire while the output carried only the reference.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

type callServer struct {
	*httptest.Server

	mu   sync.Mutex
	last recordedRequest
}

// newCallServer starts a server that records each request before responding.
func newCallServer(t *testing.T, respond http.HandlerFunc) *callServer {
	t.Helper()

	cs := &callServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}

		cs.mu.Lock()
		cs.last = recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   string(body),
		}
		cs.mu.Unlock()

		respond(w, r)
	}))
	t.Cleanup(cs.Close)

	return cs
}

func (cs *callServer) received() recordedRequest {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	return cs.last
}

// jsonPet is the handler most of these tests call: a 200 with a JSON body.
func jsonPet(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"id":"42","name":"Rex"}`)
}

// decodeCall decodes stdout as the call envelope.
func decodeCall(t *testing.T, stdout string) callExecJSON {
	t.Helper()

	var got callExecJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}

	return got
}

func TestCallExecutesAndReturnsTheResponseEnvelope(t *testing.T) {
	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, allowHost(t, srv), "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeCall(t, stdout)

	if got.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", got.Schema)
	}
	if got.DryRun {
		t.Error("dry_run = true, want false for an executed call")
	}
	if got.Request.Method != "GET" {
		t.Errorf("request.method = %q, want GET", got.Request.Method)
	}
	if want := srv.URL + "/pets/42"; got.Request.URL != want {
		t.Errorf("request.url = %q, want %q", got.Request.URL, want)
	}
	if got.Request.Curl == "" {
		t.Error("request.curl is empty")
	}

	if got.Response == nil {
		t.Fatalf("response block is absent from an executed call:\n%s", stdout)
	}
	if got.Response.Status != http.StatusOK {
		t.Errorf("response.status = %d, want 200", got.Response.Status)
	}
	if got.Response.TimingMS == nil {
		t.Error("response.timing_ms is absent")
	}
	if ct := got.Response.Headers["Content-Type"]; len(ct) == 0 || !strings.Contains(ct[0], "application/json") {
		t.Errorf("response.headers[Content-Type] = %v, want it to name application/json", ct)
	}

	if rec := srv.received(); rec.Path != "/pets/42" {
		t.Errorf("server saw path %q, want /pets/42", rec.Path)
	}
}

func TestCallSendsTheCredentialButReportsTheReference(t *testing.T) {
	// The whole product in one test: the real token reaches the server, and the
	// output an agent reads names the environment variable instead (§5a).
	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, allowHost(t, srv), "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	if got, want := srv.received().Header.Get("Authorization"), "Bearer "+callCanary; got != want {
		t.Errorf("server saw Authorization %q, want %q", got, want)
	}

	got := decodeCall(t, stdout)
	if want := "Bearer <redacted:env:TALARIA_AUTH_BEARER>"; got.Request.Headers["Authorization"] != want {
		t.Errorf("request.headers[Authorization] = %q, want %q", got.Request.Headers["Authorization"], want)
	}
}

func TestCallCurlMatchesTheDryRunForTheSameInvocation(t *testing.T) {
	srv := newCallServer(t, jsonPet)

	args := []string{
		"testdata/call.yaml", "getPet", "--param", "petId=42", "--query", "verbose=true",
		"--base-url", srv.URL, "--output", "json",
	}

	code, executed, stderr := runCall(t, args...)
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	code, dry, stderr := runCall(t, append(args, "--dry-run")...)
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	if got, want := decodeCall(t, executed).Request.Curl, decodeCall(t, dry).Request.Curl; got != want {
		t.Errorf("request.curl differs between execution and dry run:\n executed: %s\n dry run:  %s", got, want)
	}
}

func TestCallBaseURLOverridesTheSpecServer(t *testing.T) {
	// The spec's server is https://api.invalid, which does not resolve. A call
	// that reached the test server proves --base-url replaced it rather than
	// being merged with it.
	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call --base-url = %d, want 0; stderr: %s", code, stderr)
	}

	if got := decodeCall(t, stdout).Request.URL; !strings.HasPrefix(got, srv.URL) {
		t.Errorf("request.url = %q, want it under %q", got, srv.URL)
	}
	if rec := srv.received(); rec.Path != "/public" {
		t.Errorf("server saw path %q, want /public", rec.Path)
	}
}

func TestCallEmbedsAJSONResponseBodyAsJSON(t *testing.T) {
	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	body := decodeCall(t, stdout).Response.Body

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("response.body is not embedded JSON (%s): %v", body, err)
	}
	if decoded["name"] != "Rex" {
		t.Errorf("response.body.name = %v, want Rex", decoded["name"])
	}
}

func TestCallRendersANonJSONResponseBodyAsAString(t *testing.T) {
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "not json at all")
	})

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	var body string
	if err := json.Unmarshal(decodeCall(t, stdout).Response.Body, &body); err != nil {
		t.Fatalf("response.body is not a JSON string: %v", err)
	}
	if body != "not json at all" {
		t.Errorf("response.body = %q, want the text verbatim", body)
	}
}

func TestCallPrettyOutputShowsStatusAndTimingWithoutResponseHeaders(t *testing.T) {
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session="+callResponseSecret)
		jsonPet(w, nil)
	})

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "pretty")
	if code != 0 {
		t.Fatalf("call --output pretty = %d, want 0; stderr: %s", code, stderr)
	}

	for _, want := range []string{"200", "ms"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("pretty output does not contain %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, callResponseSecret) {
		t.Errorf("pretty output printed a response credential:\n%s", stdout)
	}
}

func TestCallTreatsAnHTTPErrorAsASuccessfulObservation(t *testing.T) {
	// §4: a 404 is something talaria observed, not something that went wrong.
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call against a 404 = %d, want 0; stderr: %s", code, stderr)
	}

	if got := decodeCall(t, stdout).Response.Status; got != http.StatusNotFound {
		t.Errorf("response.status = %d, want 404", got)
	}
}

func TestCallSendsABodyFromTheFlag(t *testing.T) {
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations",
		"--body", `{"name":"Rex"}`, "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call --body = %d, want 0; stderr: %s", code, stderr)
	}

	if got := srv.received().Body; got != `{"name":"Rex"}` {
		t.Errorf("server saw body %q, want the literal --body value", got)
	}
}

func TestCallReportsAnUnreachableServer(t *testing.T) {
	// A closed port: curl completes, the request does not. That is exit 1, not a
	// response with a status.
	srv := newCallServer(t, jsonPet)
	url := srv.URL
	srv.Close()

	code, _, stderr := runCall(t, "testdata/call.yaml", "getPublic", "--base-url", url)
	if code != 1 {
		t.Fatalf("call against a closed port = %d, want 1; stderr: %s", code, stderr)
	}

	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "curl exited") {
		t.Errorf("error message %q does not report curl's exit status", msg)
	}
}

func TestCallStopsWhenTheProcessIsCancelled(t *testing.T) {
	t.Setenv(spec.EnvSpec, "")
	t.Setenv("TALARIA_AUTH_BEARER", callCanary)

	srv := newCallServer(t, jsonPet)

	// Cancelled before the call rather than during it: the wiring under test is
	// whether the process's cancellation reaches curl at all, and a context that
	// is already done makes that question deterministic.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr strings.Builder
	code := runContext(ctx, []string{
		"call", "testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, allowHost(t, srv), "--output", "json",
	}, &stdout, &stderr)

	if code != int(clierr.CodeRequestFailed) {
		t.Fatalf("call = %d, want %d; stderr: %s", code, clierr.CodeRequestFailed, stderr.String())
	}
	if msg := decodeErr(t, stderr.String()).Error.Message; !strings.Contains(msg, "cancel") {
		t.Errorf("error message = %q, want it to say the request was cancelled", msg)
	}
	if rec := srv.received(); rec.Path != "" {
		t.Errorf("server saw %s %s, want a cancelled call to have sent nothing", rec.Method, rec.Path)
	}
}

// allowHost is the argument that lets a credential reach a test server.
//
// The fixtures declare an unroutable servers[0] on purpose, so a --base-url at
// a test server is an *off-spec* host and a credential is withheld from it
// (internal/request.AllowedHosts). Every case that means to put a credential on
// the wire therefore has to name the host it is going to; a case that omits
// this is asserting the withholding instead.
func allowHost(t *testing.T, srv *callServer) string {
	t.Helper()

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing %q: %v", srv.URL, err)
	}

	return "--allow-host=" + parsed.Host
}
