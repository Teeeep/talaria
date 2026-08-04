// Package e2e drives the whole tool as one loop: a real binary, a real server,
// a real spec, in the order DESIGN.md §4 documents an agent working an API.
//
// Every other suite in this module tests one component against its own
// fixtures. This is the only one that proves component N's output actually
// reaches component N+1 — that the id `list` prints is the id `describe`
// accepts, that the curl `--dry-run` shows is the curl `call` runs, that a
// Swagger 2.0 document converted at load time survives all the way to the
// validator, and that the credential firewall holds across a whole session
// rather than one command at a time.
package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Teeeep/talaria/internal/canary"
)

// The two specs the suite drives, describing the same API: one OpenAPI 3.0,
// one Swagger 2.0. The same test server answers both, which is what makes the
// 2.0 run a test of the conversion rather than of a second API.
const (
	specPath  = "testdata/e2e-api.yaml"
	spec2Path = "testdata/e2e-api-2.0.json"
)

// reproSpecPath describes the same API in the shapes whose rendered curl could
// diverge from the executed one — a GET with a body, a HEAD, a cookie, a form
// body. It is separate so the pair above stays symmetrical.
const reproSpecPath = "testdata/e2e-reproduce.yaml"

// binary is the talaria under test, built once by TestMain.
//
// The suite drives a real process rather than the command tree in-process, for
// the reason the canary suite does: the tree lives in package main and cannot
// be imported. It is also the stronger reading of an end-to-end test — the exit
// codes asserted here are the ones a shell would see, and the streams scanned
// for a credential include anything written outside the writers a test would
// otherwise hand it.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "talaria-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating the build directory: %v\n", err)
		os.Exit(1)
	}

	if err := readSources(); err != nil {
		fmt.Fprintf(os.Stderr, "reading the sources under test: %v\n", err)
		os.Exit(1)
	}

	binary = filepath.Join(dir, "talaria")
	build := exec.Command("go", "build", "-o", binary, "./cmd/talaria")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building talaria: %v\n%s", err, out)
		os.RemoveAll(dir) //nolint:errcheck // Best effort; it is a temp dir.
		os.Exit(1)
	}

	code := m.Run()

	// Explicit rather than deferred: os.Exit does not run defers.
	os.RemoveAll(dir) //nolint:errcheck // Best effort; it is a temp dir.
	os.Exit(code)
}

// readSources opens every Go source file in the module so the test cache knows
// this suite depends on all of them.
//
// The dependency is real but invisible to the tool: the suite reaches the code
// under test by building a binary in a subprocess, and `go test` records
// nothing about that. Without this, a break anywhere in cmd/ or internal/ would
// leave this package's own inputs untouched and `go test ./...` would replay a
// cached pass over code that never ran.
func readSources() error {
	root := filepath.Join("..", "..")

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}

		_, err = os.ReadFile(path)

		return err
	})
}

// result is one talaria invocation.
type result struct {
	args   []string
	code   int
	stdout string
	stderr string
}

// label renders the invocation for a failure message.
func (r result) label() string { return "talaria " + strings.Join(r.args, " ") }

// surfaces returns both streams of a result as scannable surfaces.
func (r result) surfaces() []canary.Surface {
	return []canary.Surface{
		canary.Stream("stdout of `"+r.label()+"`", r.stdout),
		canary.Stream("stderr of `"+r.label()+"`", r.stderr),
	}
}

// harness runs talaria against an isolated home, so history, cache and config
// belong to the test rather than to the developer, and the only credentials in
// reach are the ones the case injected.
type harness struct {
	t     *testing.T
	env   []string
	state string
	cache string
	conf  string
}

func newHarness(t *testing.T, vars map[string]string) *harness {
	t.Helper()

	home := t.TempDir()
	h := &harness{
		t:     t,
		state: filepath.Join(home, "state"),
		cache: filepath.Join(home, "cache"),
		conf:  filepath.Join(home, "config"),
	}

	// Built from nothing rather than from os.Environ: a developer's own
	// TALARIA_AUTH_BEARER or TALARIA_SPEC must not be able to change what this
	// suite tests, and PATH is all curl needs to be found.
	h.env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_STATE_HOME=" + h.state,
		"XDG_CACHE_HOME=" + h.cache,
		"XDG_CONFIG_HOME=" + h.conf,
	}
	for name, value := range vars {
		h.env = append(h.env, name+"="+value)
	}

	return h
}

// run executes talaria and captures both streams and the exit code.
func (h *harness) run(args ...string) result {
	h.t.Helper()

	cmd := exec.Command(binary, args...)
	cmd.Env = h.env

	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			h.t.Fatalf("running talaria %s: %v", strings.Join(args, " "), err)
		}
		code = exit.ExitCode()
	}

	return result{args: args, code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// runOK runs talaria and fails the test unless it exited 0. A step of a
// workflow that did not succeed makes every assertion after it meaningless, so
// this is fatal rather than an error.
func (h *harness) runOK(args ...string) result {
	h.t.Helper()

	res := h.run(args...)
	if res.code != 0 {
		h.t.Fatalf("`%s` = %d, want 0; stderr: %s", res.label(), res.code, res.stderr)
	}

	return res
}

// reproduce runs the command `--dry-run` printed, in a shell holding the same
// environment talaria had, so a $TALARIA_AUTH_BEARER in it expands to the same
// credential the real call resolved.
//
// This is the only way to test what --dry-run claims. Comparing the rendered
// string against another rendering of the same request compares talaria with
// itself; only curl can say what the printed command actually sends.
func (h *harness) reproduce(command string) {
	h.t.Helper()

	cmd := exec.Command("sh", "-c", command)
	cmd.Env = h.env

	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("running the rendered command: %v\n%s\n%s", err, command, out)
	}
}

// written returns every file talaria left behind — the history store, the spec
// cache — as surfaces to scan. Enumerated by walking rather than by name, so a
// new persistent artifact is covered the day it appears.
func (h *harness) written() []canary.Surface {
	h.t.Helper()

	var surfaces []canary.Surface
	for label, root := range map[string]string{"history": h.state, "cache": h.cache} {
		found, err := canary.Tree(label, root)
		if err != nil {
			h.t.Fatalf("enumerating the %s surfaces: %v", label, err)
		}
		surfaces = append(surfaces, found...)
	}

	return surfaces
}

// The bodies the test server answers with. brokenPet is the one that matters:
// it is a 200 the spec documents, carrying a body the spec's Pet schema does
// not allow — an integer id and no name — so it separates "the server is down"
// from "the server disagrees with its own documentation".
const (
	onePet     = `{"id":"42","name":"Rex"}`
	brokenPet  = `{"id":7}`
	createdPet = `{"id":"43","name":"Rex"}`
)

// recordedRequest is one request as it arrived at the test server.
type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

// server is the API both specs describe.
type server struct {
	*httptest.Server

	mu       sync.Mutex
	received []recordedRequest
}

// newServer starts the API. The routes are exactly the paths both fixtures
// declare, so a request talaria built from either spec lands on the same
// handler and gets the same body.
func newServer(t *testing.T) *server {
	t.Helper()

	srv := &server{}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /pets", srv.answer(http.StatusOK, `[`+onePet+`]`))
	mux.HandleFunc("POST /pets", srv.answer(http.StatusCreated, createdPet))
	mux.HandleFunc("GET /pets/{petId}", srv.answer(http.StatusOK, onePet))
	mux.HandleFunc("GET /broken", srv.answer(http.StatusOK, brokenPet))
	mux.HandleFunc("GET /secure", srv.answer(http.StatusOK, onePet))

	srv.Server = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

// answer records the request and writes a fixed JSON response.
func (s *server) answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		read, _ := io.ReadAll(r.Body)

		s.mu.Lock()
		s.received = append(s.received, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   string(read),
		})
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// requests returns everything the server has been asked for so far.
func (s *server) requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]recordedRequest(nil), s.received...)
}

// sawHeader reports whether any request carried the given header value. It is
// what makes the leak assertions mean something: a talaria that quietly sent no
// credential at all would pass every one of them.
func (s *server) sawHeader(name, value string) bool {
	for _, req := range s.requests() {
		if req.Header.Get(name) == value {
			return true
		}
	}

	return false
}

// The JSON payloads the suite reads. They are deliberately partial: each holds
// the fields one step hands to the next, and nothing else, so a field added
// elsewhere does not have to be mirrored here.
type listView struct {
	Operations []struct {
		ID      string `json:"id"`
		Method  string `json:"method"`
		Path    string `json:"path"`
		Summary string `json:"summary"`
	} `json:"operations"`
}

type searchView struct {
	Query   string `json:"query"`
	Results []struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Where string `json:"where"`
	} `json:"results"`
}

type describeView struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Params []struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	} `json:"params"`
	Responses []struct {
		Status string `json:"status"`
	} `json:"responses"`
}

type callVw struct {
	DryRun  bool `json:"dry_run"`
	Request struct {
		Curl   string `json:"curl"`
		Method string `json:"method"`
		URL    string `json:"url"`
		Body   string `json:"body"`
	} `json:"request"`
	CredentialsWithheld []struct {
		Scheme string `json:"scheme"`
		Reason string `json:"reason"`
		Host   string `json:"host"`
	} `json:"credentials_withheld"`
	Response *struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	} `json:"response"`
	Validation *struct {
		StatusDocumented      bool `json:"status_documented"`
		ContentTypeDocumented bool `json:"content_type_documented"`
		BodyValid             bool `json:"body_valid"`
		Errors                []struct {
			Message string `json:"message"`
			Field   string `json:"field"`
		} `json:"errors"`
	} `json:"validation"`
}

type historyView struct {
	Entries []struct {
		Index       int    `json:"index"`
		Source      string `json:"source"`
		OperationID string `json:"operation_id"`
		Method      string `json:"method"`
		Path        string `json:"path"`
		URL         string `json:"url"`
		Status      int    `json:"status"`
	} `json:"entries"`
}

// decode reads one command's JSON output into the view it renders.
func decode[T any](t *testing.T, res result) T {
	t.Helper()

	var out T
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatalf("decoding the output of `%s`: %v\nstdout: %s", res.label(), err, res.stdout)
	}

	return out
}

// TestTheDocumentedAgentWorkflowRunsEndToEnd walks the sequence from DESIGN.md
// §4 against one spec and one server, asserting at each step that what came out
// is what the next step needs.
//
// The steps are not independent and are deliberately not subtests: history is
// built by the calls before it, and a `describe` that took an id `list` did not
// print would prove nothing about either.
func TestTheDocumentedAgentWorkflowRunsEndToEnd(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": "e2e-workflow-token"})
	srv := newServer(t)

	// 1. list — the agent's entry point: what can this API do?
	listed := decode[listView](t, h.runOK("list", specPath, "--output", "json"))
	id := ""
	for _, op := range listed.Operations {
		if op.Method == http.MethodGet && op.Path == "/pets" {
			id = op.ID
		}
	}
	if id == "" {
		t.Fatalf("`list` printed no id for GET /pets: %+v", listed.Operations)
	}

	// 2. search — the same operation, found by a word rather than by reading
	// the whole list. It must name the same id, because that is what the agent
	// carries to the next command.
	found := decode[searchView](t, h.runOK("search", specPath, "pet", "--output", "json"))
	if !namesOperation(found, id) {
		t.Fatalf("`search pet` does not name %q as an operation: %+v", id, found.Results)
	}

	// 3. describe — the id from step 1 is accepted verbatim.
	described := decode[describeView](t, h.runOK("describe", specPath, id, "--output", "json"))
	if described.ID != id || described.Path != "/pets" {
		t.Fatalf("`describe %s` describes %s %s, not GET /pets", id, described.Method, described.Path)
	}
	if len(described.Responses) == 0 {
		t.Errorf("`describe %s` reports no responses, so there is no contract to call against", id)
	}

	// 4. call --dry-run — the request, sent nowhere.
	dry := decode[callVw](t, h.runOK("call", specPath, id,
		"--base-url", srv.URL, "--output", "json", "--dry-run"))
	if !dry.DryRun || dry.Response != nil {
		t.Fatalf("`call --dry-run` reported dry_run=%v with a response block=%v",
			dry.DryRun, dry.Response != nil)
	}
	if len(srv.requests()) != 0 {
		t.Fatalf("`call --dry-run` reached the server %d time(s)", len(srv.requests()))
	}

	// 5. call — the same request, sent. The claim of --dry-run is that the
	// command it printed is the call, so the command is run and the two are
	// compared as the server saw them. Comparing the printed string against
	// another rendering of it would only compare talaria with itself.
	h.reproduce(dry.Request.Curl)
	called := decode[callVw](t, h.runOK("call", specPath, id, "--base-url", srv.URL, "--output", "json"))
	if got := srv.requests(); len(got) != 2 {
		t.Fatalf("the server saw %d requests, want 2 (the previewed command, then the call): %+v", len(got), got)
	} else if diff := requestDiff(got[0], got[1]); diff != "" {
		t.Errorf("the previewed command sent something else than the call did: %s\npreviewed: %s",
			diff, dry.Request.Curl)
	}
	if called.Response == nil || called.Response.Status != http.StatusOK {
		t.Fatalf("`call %s` did not observe a 200: %s", id, called.stdoutOf())
	}
	if called.Validation == nil || !called.Validation.BodyValid {
		t.Errorf("the server's documented response did not validate: %+v", called.Validation)
	}

	// 6. history — the call is on the record, under the id it was made with.
	listedHistory := decode[historyView](t, h.runOK("history", "--output", "json"))
	if len(listedHistory.Entries) != 1 {
		t.Fatalf("history holds %d entries after one call and one dry run, want 1: %+v",
			len(listedHistory.Entries), listedHistory.Entries)
	}
	entry := listedHistory.Entries[0]
	if entry.Index != 1 || entry.OperationID != id || entry.Status != http.StatusOK {
		t.Fatalf("history entry 1 is %+v, want index 1, operation %q, status 200", entry, id)
	}

	// 7. history replay — the index `history` printed is the index `replay`
	// takes. It re-derives the call through the spec rather than re-sending the
	// stored line, so the spec and the base URL are named again.
	replayed := decode[callVw](t, h.runOK("history", "replay", specPath, "1",
		"--base-url", srv.URL, "--output", "json"))
	if replayed.Response == nil || replayed.Response.Status != http.StatusOK {
		t.Fatalf("`history replay 1` did not observe a 200: %s", replayed.stdoutOf())
	}
	if replayed.Request.URL != called.Request.URL {
		t.Errorf("the replay called %s, the original called %s", replayed.Request.URL, called.Request.URL)
	}

}

// stdoutOf renders a decoded call for a failure message. The view is what was
// parsed rather than the raw bytes, which is enough to see what came back and
// carries no field the parse dropped.
func (v callVw) stdoutOf() string {
	out, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}

	return string(out)
}

// namesOperation reports whether a search result set names id as an operation.
func namesOperation(found searchView, id string) bool {
	for _, res := range found.Results {
		if res.Kind == "operation" && res.Name == id {
			return true
		}
	}

	return false
}

// requestDiff names everything on which two requests the server recorded
// disagree, or "" when they are the same request twice.
//
// The headers are compared in full rather than by an allow-list: talaria and
// the rendered command run the same curl against the same server, so anything
// curl adds on its own it adds to both, and a difference is a real one.
func requestDiff(previewed, called recordedRequest) string {
	var diffs []string
	if previewed.Method != called.Method {
		diffs = append(diffs, fmt.Sprintf("method: previewed %s, call %s", previewed.Method, called.Method))
	}
	if previewed.Path != called.Path {
		diffs = append(diffs, fmt.Sprintf("path: previewed %s, call %s", previewed.Path, called.Path))
	}
	if previewed.Body != called.Body {
		diffs = append(diffs, fmt.Sprintf("body: previewed %q, call %q", previewed.Body, called.Body))
	}

	names := map[string]bool{}
	for name := range previewed.Header {
		names[name] = true
	}
	for name := range called.Header {
		names[name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if a, b := previewed.Header.Values(name), called.Header.Values(name); !slices.Equal(a, b) {
			diffs = append(diffs, fmt.Sprintf("header %s: previewed %v, call %v", name, a, b))
		}
	}

	return strings.Join(diffs, "; ")
}

// TestThePreviewedCommandSendsWhatTheCallSends executes the curl `--dry-run`
// printed and compares what the server received with what the real call sent.
//
// It covers the request shapes where the two can drift, because the rendered
// command and the config document curl reads are written by different code and
// spell the same request differently. A GET with a body is the case that
// motivated it: --data-raw makes curl send POST unless the method is named,
// while the config document always names it, so the divergence is invisible
// anywhere but the wire.
func TestThePreviewedCommandSendsWhatTheCallSends(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		// file, when set, is written to disk and passed as --body @path. The
		// emitted command references that file rather than inlining it, so what
		// curl reads back off disk is part of what "the same request" means.
		// Single-line by necessity: `--data @path` strips newlines, which
		// TestAPreviewedFileBodyLosesTheNewlinesTheCallSends below pins.
		file string
	}{
		{name: "a GET carrying a body", args: []string{"searchPets", "--body", `{"name":"Rex"}`}},
		// -I rather than -X HEAD, which pasted would wait for a body the server
		// never sends.
		{name: "a HEAD", args: []string{"checkPets"}},
		// One -b holding every cookie, because curl keeps only the last of several.
		{name: "a cookie parameter", args: []string{"sessionPets", "--param", "petId=42", "--param", "session=s3ss10n"}},
		// A media type that is not the JSON curl would otherwise assume.
		{name: "a form body", args: []string{"createPetForm", "--body", "name=Rex", "--allow-mutations"}},
		{name: "a file body", args: []string{"searchPets"}, file: `{"name":"Rex"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": "reproduce-token"})
			srv := newServer(t)

			args := []string{"call", reproSpecPath}
			args = append(args, tc.args...)
			if tc.file != "" {
				args = append(args, "--body", "@"+writeBodyFile(t, tc.file))
			}
			args = append(args, "--base-url", srv.URL, "--output", "json")

			dry := decode[callVw](t, h.runOK(append(slices.Clone(args), "--dry-run")...))
			h.reproduce(dry.Request.Curl)
			h.runOK(args...)

			got := srv.requests()
			if len(got) != 2 {
				t.Fatalf("the server saw %d requests, want 2 (the previewed command, then the call): %+v",
					len(got), got)
			}
			if diff := requestDiff(got[0], got[1]); diff != "" {
				t.Errorf("the previewed command sent something else than the call did: %s\npreviewed: %s",
					diff, dry.Request.Curl)
			}
		})
	}
}

// writeBodyFile writes a request body to a file and returns its path, for the
// cases that pass --body @path.
func writeBodyFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the body file: %v", err)
	}

	return path
}

// TestAPreviewedFileBodyLosesTheNewlinesTheCallSends pins a divergence rather
// than a guarantee: `--data @path` is what the emitted command uses for a file
// body, and curl strips newlines out of a file read that way, while the call
// itself sends the file's bytes exactly as they are. So for a pretty-printed or
// signed payload the preview and the call send different bytes.
//
// It is asserted here because the alternative is that nobody knows. DESIGN.md
// §3.4 names --data, so widening this to --data-binary is a design decision and
// not a bug fix. If this test ever fails because the two now agree, the decision
// was taken — delete the test and the note in .ralph/refactor-backlog.md with
// it.
func TestAPreviewedFileBodyLosesTheNewlinesTheCallSends(t *testing.T) {
	t.Parallel()

	const body = "{\n  \"name\": \"Rex\"\n}\n"

	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": "reproduce-token"})
	srv := newServer(t)

	args := []string{"call", reproSpecPath, "searchPets",
		"--body", "@" + writeBodyFile(t, body),
		"--base-url", srv.URL, "--output", "json"}

	dry := decode[callVw](t, h.runOK(append(slices.Clone(args), "--dry-run")...))
	h.reproduce(dry.Request.Curl)
	h.runOK(args...)

	got := srv.requests()
	if len(got) != 2 {
		t.Fatalf("the server saw %d requests, want 2 (the previewed command, then the call): %+v", len(got), got)
	}
	if got[1].Body != body {
		t.Errorf("the call sent %q, want the file's bytes unchanged", got[1].Body)
	}
	if want := strings.ReplaceAll(body, "\n", ""); got[0].Body != want {
		t.Errorf("the previewed command sent %q, want %q — curl's own newline stripping.\n"+
			"If it now matches the call, --data was widened and this test has served its purpose",
			got[0].Body, want)
	}
}

// TestNoStepOfTheWorkflowLeaksTheCredential runs the whole sequence with a
// canary in the environment and greps every surface it produced.
//
// The canary suite proves each command redacts. This proves the *session* does:
// a credential that survived one command's redaction and was written down by
// the next would pass there and fail here.
func TestNoStepOfTheWorkflowLeaksTheCredential(t *testing.T) {
	t.Parallel()

	value := canary.Value("e2e")
	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": value})
	srv := newServer(t)
	// The spec declares api.invalid, so the credential only reaches the test
	// server because the host is allowed explicitly. That is the point of the
	// flag, and without it the leak assertions below would be scanning a
	// workflow that never carried a credential at all.
	allow, host := "--allow-host", hostOf(t, srv.URL)

	runs := []result{
		h.runOK("list", specPath, "--output", "json"),
		h.runOK("search", specPath, "pet", "--output", "json"),
		h.runOK("describe", specPath, "listPets", "--output", "json"),
		h.runOK("call", specPath, "listPets", "--base-url", srv.URL, allow, host, "--dry-run", "--output", "json"),
		h.runOK("call", specPath, "listPets", "--base-url", srv.URL, allow, host, "--output", "json"),
		h.runOK("history", "--output", "json"),
		h.runOK("history", "show", "1", "--output", "json"),
		h.runOK("history", "replay", specPath, "1", "--base-url", srv.URL, allow, host, "--output", "json"),
		// Exits 5: secureKey is unset. The report is written on the way to that
		// exit code, with the bearer canary in reach the whole time.
		h.run("auth", "check", specPath, "--output", "json"),
		// A failure surface, because §5a's rule is that error paths are where
		// redaction bugs live.
		h.run("call", specPath, "getBroken", "--base-url", srv.URL, allow, host, "--fail-on-error", "--output", "json"),
	}

	// The credential reached the server. Without this the assertions below
	// would pass just as well against a talaria that sent nothing.
	if !srv.sawHeader("Authorization", "Bearer "+value) {
		t.Fatalf("the server never saw the bearer credential; the leak assertions below prove nothing")
	}

	var surfaces []canary.Surface
	for _, res := range runs {
		if strings.TrimSpace(res.stdout) == "" {
			t.Errorf("`%s` printed nothing; there is no surface here to check", res.label())
		}
		surfaces = append(surfaces, res.surfaces()...)
	}
	surfaces = append(surfaces, h.written()...)

	leaks := canary.Scan(value, surfaces...)
	if len(leaks) == 0 {
		return
	}

	var names []string
	for _, leak := range leaks {
		names = append(names, leak.String())
	}
	t.Errorf("the canary reached %d surface(s) across the workflow:\n  %s",
		len(leaks), strings.Join(names, "\n  "))
}

// TestAMutationIsRefusedWithoutAllowMutationsAtBothLevels holds the safety rail
// DESIGN.md §4 puts in the tool rather than in the prompt, at each of the two
// places a request can be issued from.
func TestAMutationIsRefusedWithoutAllowMutations(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": "e2e-mutation-token"})
	srv := newServer(t)

	// `call`: refused before anything is built, so nothing reaches the server.
	refused := h.run("call", specPath, "createPet", "--base-url", srv.URL,
		"--body", `{"name":"Rex"}`, "--output", "json")
	if refused.code != 2 {
		t.Errorf("`%s` = %d, want 2; stderr: %s", refused.label(), refused.code, refused.stderr)
	}
	if !strings.Contains(refused.stderr, "--allow-mutations") {
		t.Errorf("the refusal does not name the flag that lifts it: %s", refused.stderr)
	}
	if len(srv.requests()) != 0 {
		t.Fatalf("a refused mutation still reached the server %d time(s)", len(srv.requests()))
	}

	// `call --allow-mutations`: the same request, sent.
	allowed := decode[callVw](t, h.runOK("call", specPath, "createPet", "--base-url", srv.URL,
		"--body", `{"name":"Rex"}`, "--allow-mutations", "--output", "json"))
	if allowed.Response == nil || allowed.Response.Status != http.StatusCreated {
		t.Fatalf("the allowed mutation did not observe a 201: %s", allowed.stdoutOf())
	}
	if got := srv.requests(); len(got) != 1 || got[0].Method != http.MethodPost {
		t.Fatalf("the server saw %+v, want one POST", got)
	}

	if got := srv.requests(); len(got) != 1 {
		t.Fatalf("the server saw %d requests, want 1 from the allowed call", len(got))
	}
}

// TestTheExitCodeContractIsObservableAcrossOneSpec covers the codes an agent
// branches on, against the same spec and the same server, so the mapping is
// proved to be a property of what happened rather than of which fixture was
// loaded (DESIGN.md §4).
//
// Codes 1 and 3 are absent by design: a request that cannot complete and a spec
// that cannot be read are failures *before* the loop this suite is about, and
// both are covered where they happen.
func TestTheExitCodeContractIsObservableAcrossOneSpec(t *testing.T) {
	t.Parallel()

	// bearerAuth is set and secureKey is not, so the same spec produces both a
	// successful call and an unsatisfiable operation.
	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": "e2e-exitcode-token"})
	srv := newServer(t)

	cases := []struct {
		name string
		args []string
		code int
		why  string
	}{
		{
			name: "0 the call succeeded",
			args: []string{"call", specPath, "listPets", "--base-url", srv.URL, "--output", "json"},
			code: 0,
			why:  "a documented 200 that matches its schema",
		},
		{
			name: "2 the invocation was wrong",
			args: []string{"call", specPath, "noSuchOperation", "--base-url", srv.URL, "--output", "json"},
			code: 2,
			why:  "an operationId the spec does not declare",
		},
		{
			name: "4 the response violates the spec",
			args: []string{"call", specPath, "getBroken", "--base-url", srv.URL,
				"--fail-on-error", "--output", "json"},
			code: 4,
			why:  "a documented 200 carrying a body the schema forbids",
		},
		{
			name: "5 a credential is missing",
			args: []string{"auth", "check", specPath, "--output", "json"},
			code: 5,
			why:  "getSecure needs secureKey and nothing sets it",
		},
	}

	for _, tc := range cases {
		res := h.run(tc.args...)
		if res.code != tc.code {
			t.Errorf("`%s` = %d, want %d (%s); stderr: %s",
				res.label(), res.code, tc.code, tc.why, res.stderr)
		}
		// Every non-zero code is an error an agent reads, so it comes with a
		// structured message rather than a bare code.
		if tc.code != 0 && !strings.Contains(res.stderr, `"code":`) {
			t.Errorf("`%s` exited %d with no structured error on stderr: %q",
				res.label(), res.code, res.stderr)
		}
	}

	// Code 4 is a verdict about the body, not about the transport: the same
	// call without --fail-on-error is a successful observation, and the
	// violations are on stdout either way.
	observed := decode[callVw](t, h.runOK("call", specPath, "getBroken",
		"--base-url", srv.URL, "--output", "json"))
	if observed.Validation == nil || len(observed.Validation.Errors) == 0 {
		t.Fatalf("the broken response validated clean, so exit 4 above was not about the body: %s",
			observed.stdoutOf())
	}
}

// TestASwagger2SpecFlowsThroughTheWholeLoop repeats the core of the workflow
// against the same API described as Swagger 2.0.
//
// Conversion happens once, at load time (DESIGN.md §5). This is what proves its
// output actually reaches everything downstream: the operation model reads it,
// the curl builder binds a path parameter and a converted security scheme from
// it, the executor sends the result, and the validator checks the response
// against the converted response schema — including catching a violation of it.
func TestASwagger2SpecFlowsThroughTheWholeLoop(t *testing.T) {
	t.Parallel()

	value := canary.Value("swagger2")
	h := newHarness(t, map[string]string{"TALARIA_AUTH_APIKEY_APIKEY": value})
	srv := newServer(t)

	// The operation model, built from the converted document.
	listed := decode[listView](t, h.runOK("list", spec2Path, "--output", "json"))
	if len(listed.Operations) != 4 {
		t.Fatalf("the converted 2.0 spec has %d operations, want 4: %+v",
			len(listed.Operations), listed.Operations)
	}

	// The converted parameter: `type: string` in 2.0 becomes a schema in 3.x,
	// and `describe` reads it as one.
	described := decode[describeView](t, h.runOK("describe", spec2Path, "getPet", "--output", "json"))
	if described.Path != "/pets/{petId}" || len(described.Params) != 1 {
		t.Fatalf("`describe getPet` on the converted spec reports %s with %d params",
			described.Path, len(described.Params))
	}
	if described.Params[0].Name != "petId" || !described.Params[0].Required {
		t.Errorf("the converted path parameter is %+v, want a required petId", described.Params[0])
	}

	// The curl builder: the converted securityDefinition is a header API key
	// referenced by name, and the path template is filled from --param.
	called := decode[callVw](t, h.runOK("call", spec2Path, "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--allow-host", hostOf(t, srv.URL), "--output", "json"))
	if !strings.HasSuffix(called.Request.URL, "/pets/42") {
		t.Errorf("the converted path template produced %q", called.Request.URL)
	}
	if !strings.Contains(called.Request.Curl, "$TALARIA_AUTH_APIKEY_APIKEY") {
		t.Errorf("the emitted curl does not reference the converted scheme's variable: %s",
			called.Request.Curl)
	}

	// The executor: the credential the converted scheme describes went out on
	// the wire, in the header the 2.0 document named.
	if !srv.sawHeader("X-Api-Key", value) {
		t.Fatalf("the server never saw the converted API key: %+v", srv.requests())
	}

	// The validator: the converted response schema is what the body is checked
	// against, and it accepts a valid pet.
	if called.Response == nil || called.Response.Status != http.StatusOK {
		t.Fatalf("`call getPet` on the converted spec did not observe a 200: %s", called.stdoutOf())
	}
	if called.Validation == nil || !called.Validation.BodyValid {
		t.Errorf("a valid pet failed validation against the converted schema: %+v", called.Validation)
	}

	// …and rejects an invalid one, which is what proves the converted schema is
	// being enforced rather than merely carried.
	broken := h.run("call", spec2Path, "getBroken", "--base-url", srv.URL,
		"--fail-on-error", "--output", "json")
	if broken.code != 4 {
		t.Errorf("`%s` = %d, want 4; the converted schema is not being enforced. stderr: %s",
			broken.label(), broken.code, broken.stderr)
	}

	// The canary rode the converted path the whole way and came out nowhere.
	leaks := canary.Scan(value, append(called.surfacesOf(), h.written()...)...)
	if len(leaks) > 0 {
		t.Errorf("the converted spec's credential leaked: %+v", leaks)
	}
}

// surfacesOf renders a decoded call as a surface, so the 2.0 path's output is
// scanned along with everything the harness wrote.
func (v callVw) surfacesOf() []canary.Surface {
	return []canary.Surface{canary.Stream("call output (swagger 2.0)", v.stdoutOf())}
}

// hostOf is a test server's authority, for --allow-host.
func hostOf(t *testing.T, rawURL string) string {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}

	return parsed.Host
}

// A credential goes to a host the spec declares, and to no other, whatever
// --base-url says.
//
// This is the wire-level assertion the finding needs and the only kind that can
// catch it. Redaction answers *does it print*; every output surface here is
// already clean, and the canary suite passes today while a production bearer
// token is delivered to any host the caller names. The question this test asks
// is *who received it*, and only the listener can answer that.
func TestACredentialNeverReachesAHostTheSpecDoesNotDeclare(t *testing.T) {
	t.Parallel()

	value := canary.Value("host-binding")
	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": value})
	srv := newServer(t)

	// The spec declares api.invalid; srv is somewhere else entirely.
	res := h.runOK("call", specPath, "listPets", "--base-url", srv.URL, "--output", "json")

	got := srv.requests()
	if len(got) != 1 {
		t.Fatalf("the server saw %d requests, want 1: %+v", len(got), got)
	}
	if auth := got[0].Header.Get("Authorization"); auth != "" {
		t.Errorf("the off-spec host received Authorization: %q", auth)
	}
	// Not just the one header: the canary must be absent from everything that
	// arrived, whatever position it might have travelled in.
	for name, values := range got[0].Header {
		for _, v := range values {
			if strings.Contains(v, value) {
				t.Errorf("the off-spec host received the credential in header %s: %q", name, v)
			}
		}
	}
	if strings.Contains(got[0].Body, value) || strings.Contains(got[0].Path, value) {
		t.Errorf("the off-spec host received the credential in the request: %+v", got[0])
	}

	// Withheld, not refused: pointing at a local twin is the common case and
	// must not need a flag (DESIGN.md §5a). The call ran and exited 0 — runOK
	// above — and says what it left out.
	called := decode[callVw](t, res)
	if len(called.CredentialsWithheld) != 1 {
		t.Fatalf("credentials_withheld = %+v, want one entry", called.CredentialsWithheld)
	}
	withheld := called.CredentialsWithheld[0]
	if withheld.Scheme != "bearerAuth" || withheld.Host != hostOf(t, srv.URL) || withheld.Reason == "" {
		t.Errorf("credentials_withheld[0] = %+v, want bearerAuth withheld from %s with a reason",
			withheld, hostOf(t, srv.URL))
	}
	if !strings.Contains(res.stderr, "bearerAuth") || !strings.Contains(res.stderr, "--allow-host") {
		t.Errorf("stderr does not name the withheld scheme and the flag that permits it: %s", res.stderr)
	}

	// The deliberate override: the same call with the host allowed delivers it.
	h.runOK("call", specPath, "listPets", "--base-url", srv.URL,
		"--allow-host", hostOf(t, srv.URL), "--output", "json")
	if !srv.sawHeader("Authorization", "Bearer "+value) {
		t.Errorf("--allow-host did not put the credential on the wire: %+v", srv.requests())
	}
}

// `history replay` re-derives where it sends from the spec, so a hand-edited
// entry cannot redirect a credential-bearing call at a host of its choosing.
// The store is a plain file: an agent that can write it could otherwise turn
// replay into "send this to me".
func TestAnEditedHistoryEntryCannotRetargetAReplay(t *testing.T) {
	t.Parallel()

	value := canary.Value("replay-retarget")
	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": value})
	srv := newServer(t)
	steal := newServer(t)

	h.runOK("call", specPath, "getPet", "--param", "petId=42", "--base-url", srv.URL,
		"--allow-host", hostOf(t, srv.URL), "--output", "json")

	// The one field an attacker with write access to the store would change.
	rewriteHistory(t, h, func(entry map[string]any) {
		entry["url"] = steal.URL + "/pets/42"
	})

	// The stored host is allowed, so this is not the refusal path — it is the
	// re-derivation path, and it still must not go there.
	h.runOK("history", "replay", specPath, "1", "--base-url", srv.URL,
		"--allow-host", hostOf(t, srv.URL), "--allow-host", hostOf(t, steal.URL), "--output", "json")

	if got := steal.requests(); len(got) != 0 {
		t.Fatalf("the replay went to the host the stored line named: %+v", got)
	}
	if len(srv.requests()) != 2 {
		t.Errorf("the replay did not re-issue against the spec's target: %+v", srv.requests())
	}

	// And with the stored host outside the allowed set, replay refuses rather
	// than silently retargeting (DESIGN.md:407). The replay above appended an
	// entry of its own, so the edit is applied again to cover it.
	rewriteHistory(t, h, func(entry map[string]any) {
		entry["url"] = steal.URL + "/pets/42"
	})

	res := h.run("history", "replay", specPath, "1", "--base-url", srv.URL, "--output", "json")
	if res.code != 2 {
		t.Errorf("`%s` = %d, want 2", res.label(), res.code)
	}
	if got := steal.requests(); len(got) != 0 {
		t.Errorf("the refused replay still reached the stored host: %+v", got)
	}
}

// rewriteHistory applies edit to every line of the store, standing in for
// whatever else on the machine can write that file.
func rewriteHistory(t *testing.T, h *harness, edit func(map[string]any)) {
	t.Helper()

	path := filepath.Join(h.state, "talaria", "history.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the history store: %v", err)
	}

	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decoding a history line: %v", err)
		}
		edit(entry)

		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("encoding a history line: %v", err)
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}

	if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
		t.Fatalf("rewriting the history store: %v", err)
	}
}
