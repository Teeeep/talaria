package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Teeeep/talaria/internal/spec"
)

// runSpecFile is the fixture every test here drives: seven operations across
// four tags, one of which always 500s and one of which cannot be supplied with
// its path parameter.
const runSpecFile = "testdata/run.yaml"

// runResultJSON is one operation's line in the report.
type runResultJSON struct {
	OperationID string `json:"operation_id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason"`
	Status      int    `json:"status"`
	// A pointer so an absent timing is distinguishable from a call to a local
	// server that legitimately rounded to 0ms.
	TimingMS   *int64 `json:"timing_ms"`
	Validation *struct {
		StatusDocumented bool `json:"status_documented"`
		BodyValid        bool `json:"body_valid"`
		Errors           []struct {
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"validation"`
}

// runJSON is the whole `run` envelope.
type runJSON struct {
	Schema  string          `json:"schema"`
	Results []runResultJSON `json:"results"`
	Summary struct {
		Total   int `json:"total"`
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
	} `json:"summary"`
}

// runServer records every request it serves so a test can assert on what
// actually went out, and answers with a body the fixture spec documents.
type runServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

func newRunServer(t *testing.T) *runServer {
	t.Helper()

	rs := &runServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}

		rs.mu.Lock()
		rs.requests = append(rs.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   string(body),
		})
		rs.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/boom":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"42","name":"Rex"}`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"42","name":"Rex"}`)
		}
	}))
	t.Cleanup(rs.Close)

	return rs
}

func (rs *runServer) received() []recordedRequest {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	return append([]recordedRequest(nil), rs.requests...)
}

// paths is what the server was asked for, in order, as "METHOD /path".
func (rs *runServer) paths() []string {
	var out []string
	for _, req := range rs.received() {
		out = append(out, req.Method+" "+req.Path)
	}

	return out
}

// runRun executes `run` against a fresh, isolated history.
func runRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	return runRunIn(t, t.TempDir(), args...)
}

// runRunIn is runRun with the state directory named, so a test can share one
// history between several commands.
//
// Credentials default to the canary, but one the test has already set — to the
// empty string, say — is left alone: that is how the missing-credential case is
// written.
func runRunIn(t *testing.T, stateDir string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv(spec.EnvSpec, "")
	t.Setenv("XDG_STATE_HOME", stateDir)
	setUnlessSet(t, "TALARIA_AUTH_BEARER", callCanary)
	setUnlessSet(t, "TALARIA_AUTH_APIKEY_PETKEY", callCanary)

	var out, errOut strings.Builder
	code = run(append([]string{"run"}, args...), &out, &errOut)
	stdout, stderr = out.String(), errOut.String()

	if strings.Contains(stdout, callCanary) {
		t.Fatalf("run leaked the credential on stdout:\n%s", stdout)
	}
	if strings.Contains(stderr, callCanary) {
		t.Fatalf("run leaked the credential on stderr:\n%s", stderr)
	}

	return code, stdout, stderr
}

func setUnlessSet(t *testing.T, key, value string) {
	t.Helper()

	if _, ok := os.LookupEnv(key); ok {
		return
	}
	t.Setenv(key, value)
}

func decodeRun(t *testing.T, stdout string) runJSON {
	t.Helper()

	var got runJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}

	return got
}

// result finds one operation's line, failing the test when the report has none.
func (r runJSON) result(t *testing.T, id string) runResultJSON {
	t.Helper()

	for _, res := range r.Results {
		if res.OperationID == id {
			return res
		}
	}

	t.Fatalf("no result for %s in %+v", id, r.Results)

	return runResultJSON{}
}

// ids is every operation the report covered, in order.
func (r runJSON) ids() []string {
	var out []string
	for _, res := range r.Results {
		out = append(out, res.OperationID)
	}

	return out
}

func TestRunExecutesEveryReadOnlyOperation(t *testing.T) {
	srv := newRunServer(t)

	code, stdout, stderr := runRun(t, runSpecFile, "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeRun(t, stdout)
	if len(got.Results) != 7 {
		t.Fatalf("got %d results (%v), want one per operation", len(got.Results), got.ids())
	}

	res := got.result(t, "listPets")
	if res.Outcome != "passed" {
		t.Errorf("listPets outcome = %q (%s), want passed", res.Outcome, res.Reason)
	}
	if res.Method != http.MethodGet || res.Path != "/pets" {
		t.Errorf("listPets = %s %s, want GET /pets", res.Method, res.Path)
	}
	if res.Status != http.StatusOK {
		t.Errorf("listPets status = %d, want 200", res.Status)
	}
	if res.TimingMS == nil {
		t.Error("listPets reported no timing")
	}
	if res.Validation == nil || !res.Validation.BodyValid {
		t.Errorf("listPets validation = %+v, want a valid body", res.Validation)
	}

	if boom := got.result(t, "getBoom"); boom.Outcome != "failed" || boom.Status != http.StatusInternalServerError {
		t.Errorf("getBoom = %q/%d, want failed/500", boom.Outcome, boom.Status)
	}

	if want := "GET /pets"; !strings.Contains(strings.Join(srv.paths(), ","), want) {
		t.Errorf("server saw %v, want it to include %s", srv.paths(), want)
	}
}

func TestRunFiltersAreTheUnionOfTagAndOperation(t *testing.T) {
	srv := newRunServer(t)

	code, stdout, stderr := runRun(t, runSpecFile,
		"--tag", "ops", "--operation", "getPet", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeRun(t, stdout)
	want := []string{"getPet", "getStatus", "getBoom"}
	if diff := strings.Join(got.ids(), ","); diff != strings.Join(want, ",") {
		t.Fatalf("ran %v, want %v", got.ids(), want)
	}
}

func TestRunFilterMatchingNothingIsAUsageError(t *testing.T) {
	srv := newRunServer(t)

	code, _, stderr := runRun(t, runSpecFile, "--tag", "nosuchtag", "--base-url", srv.URL, "--output", "json")
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr: %s", code, stderr)
	}
	if len(srv.received()) != 0 {
		t.Errorf("server saw %v, want nothing", srv.paths())
	}
}

func TestRunSkipsMutationsUnlessAllowed(t *testing.T) {
	srv := newRunServer(t)

	code, stdout, stderr := runRun(t, runSpecFile,
		"--operation", "createPet", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeRun(t, stdout)
	res := got.result(t, "createPet")
	if res.Outcome != "skipped" {
		t.Fatalf("createPet outcome = %q, want skipped", res.Outcome)
	}
	if !strings.Contains(res.Reason, "--allow-mutations") {
		t.Errorf("createPet reason = %q, want it to name --allow-mutations", res.Reason)
	}
	if got.Summary.Skipped != 1 || got.Summary.Failed != 0 {
		t.Errorf("summary = %+v, want the skip counted as skipped, not failed", got.Summary)
	}
	if len(srv.received()) != 0 {
		t.Errorf("server saw %v, want nothing", srv.paths())
	}

	allowed := newRunServer(t)
	code, stdout, stderr = runRun(t, runSpecFile, "--operation", "createPet",
		"--allow-mutations", "--base-url", allowed.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run --allow-mutations = %d, want 0; stderr: %s", code, stderr)
	}
	if res := decodeRun(t, stdout).result(t, "createPet"); res.Outcome != "passed" {
		t.Errorf("createPet outcome = %q (%s), want passed", res.Outcome, res.Reason)
	}
	if want := []string{"POST /pets"}; strings.Join(allowed.paths(), ",") != strings.Join(want, ",") {
		t.Errorf("server saw %v, want %v", allowed.paths(), want)
	}
}

func TestRunReportFlagWinsOverOutput(t *testing.T) {
	srv := newRunServer(t)

	// --report json against --output pretty: an agent that set the persistent
	// flag and then asked run for JSON gets JSON.
	code, stdout, stderr := runRun(t, runSpecFile,
		"--tag", "ops", "--base-url", srv.URL, "--output", "pretty", "--report", "json")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}
	got := decodeRun(t, stdout)
	if got.Schema == "" {
		t.Errorf("--report json printed no schema field: %s", stdout)
	}
	if got.Summary.Total != 2 || got.Summary.Passed != 1 || got.Summary.Failed != 1 {
		t.Errorf("summary = %+v, want 2 total, 1 passed, 1 failed", got.Summary)
	}

	// --report pretty against --output json: the reverse ordering.
	code, stdout, stderr = runRun(t, runSpecFile,
		"--tag", "ops", "--base-url", srv.URL, "--output", "json", "--report", "pretty")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, `"schema"`) {
		t.Errorf("--report pretty printed JSON:\n%s", stdout)
	}
	for _, want := range []string{"1 passed", "1 failed", "0 skipped"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("pretty report is missing %q:\n%s", want, stdout)
		}
	}

	// With only --output set, run follows it.
	code, stdout, stderr = runRun(t, runSpecFile, "--tag", "ops", "--base-url", srv.URL, "--output", "pretty")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, `"schema"`) {
		t.Errorf("--output pretty printed JSON:\n%s", stdout)
	}

	code, stdout, stderr = runRun(t, runSpecFile, "--tag", "ops", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}
	if decodeRun(t, stdout).Schema == "" {
		t.Errorf("--output json printed no schema field:\n%s", stdout)
	}

	code, _, stderr = runRun(t, runSpecFile, "--tag", "ops", "--base-url", srv.URL, "--report", "yaml")
	if code != 2 {
		t.Fatalf("run --report yaml = %d, want 2; stderr: %s", code, stderr)
	}
}

func TestRunFailOnErrorDecidesTheExitCode(t *testing.T) {
	srv := newRunServer(t)

	code, stdout, stderr := runRun(t, runSpecFile,
		"--operation", "getBoom", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run = %d, want 0 without --fail-on-error; stderr: %s", code, stderr)
	}
	if got := decodeRun(t, stdout); got.Summary.Failed != 1 {
		t.Errorf("summary = %+v, want 1 failed reported even at exit 0", got.Summary)
	}

	code, stdout, stderr = runRun(t, runSpecFile,
		"--operation", "getBoom", "--fail-on-error", "--base-url", srv.URL, "--output", "json")
	if code != 4 {
		t.Fatalf("run --fail-on-error = %d, want 4; stderr: %s", code, stderr)
	}
	// The report is still on stdout: the flag changes the exit code, not what
	// the caller gets to read.
	if got := decodeRun(t, stdout); got.result(t, "getBoom").Status != http.StatusInternalServerError {
		t.Errorf("--fail-on-error swallowed the report:\n%s", stdout)
	}
}

func TestRunReportsAnUnsatisfiedCredentialAndExitsFive(t *testing.T) {
	srv := newRunServer(t)
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", "")

	code, stdout, stderr := runRun(t, runSpecFile,
		"--operation", "getSecure", "--base-url", srv.URL, "--output", "json")
	if code != 5 {
		t.Fatalf("run = %d, want 5; stderr: %s", code, stderr)
	}

	res := decodeRun(t, stdout).result(t, "getSecure")
	if res.Outcome != "skipped" {
		t.Errorf("getSecure outcome = %q, want skipped", res.Outcome)
	}
	if !strings.Contains(res.Reason, "TALARIA_AUTH_APIKEY_PETKEY") {
		t.Errorf("getSecure reason = %q, want it to name the variable to export", res.Reason)
	}
	if len(srv.received()) != 0 {
		t.Errorf("server saw %v, want no request without a credential", srv.paths())
	}
}

func TestRunFillsPathParametersFromThePriorityChain(t *testing.T) {
	srv := newRunServer(t)

	code, stdout, stderr := runRun(t, runSpecFile,
		"--operation", "getPet", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}
	if res := decodeRun(t, stdout).result(t, "getPet"); res.Outcome != "passed" {
		t.Errorf("getPet outcome = %q (%s), want passed", res.Outcome, res.Reason)
	}
	// "42" is the spec example, which beats generation.
	if want := []string{"GET /pets/42"}; strings.Join(srv.paths(), ",") != strings.Join(want, ",") {
		t.Errorf("server saw %v, want %v", srv.paths(), want)
	}

	unsuppliable := newRunServer(t)
	code, stdout, stderr = runRun(t, runSpecFile,
		"--operation", "getReport", "--base-url", unsuppliable.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}
	res := decodeRun(t, stdout).result(t, "getReport")
	if res.Outcome != "skipped" {
		t.Fatalf("getReport outcome = %q, want skipped", res.Outcome)
	}
	if !strings.Contains(res.Reason, "reportId") {
		t.Errorf("getReport reason = %q, want it to name the parameter", res.Reason)
	}
	if len(unsuppliable.received()) != 0 {
		t.Errorf("server saw %v, want nothing", unsuppliable.paths())
	}
}

func TestRunRecordsHistoryTaggedRunWithoutEvictingCalls(t *testing.T) {
	srv := newRunServer(t)
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	if code, _, stderr := runCall(t, runSpecFile, "getPet", "--param", "petId=7",
		"--base-url", srv.URL, "--output", "json"); code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	if code, _, stderr := runRunIn(t, stateDir, runSpecFile,
		"--tag", "ops", "--base-url", srv.URL, "--output", "json"); code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}

	code, stdout, stderr := runHistory(t, "--output", "json")
	if code != 0 {
		t.Fatalf("history = %d, want 0; stderr: %s", code, stderr)
	}
	entries := decodeHistoryList(t, stdout).Entries
	if len(entries) != 3 {
		t.Fatalf("history has %d entries, want the call plus two run entries", len(entries))
	}

	sources := map[string]int{}
	for _, entry := range entries {
		sources[entry.Source]++
	}
	if sources["run"] != 2 || sources["call"] != 1 {
		t.Errorf("history sources = %v, want 2 run and 1 call", sources)
	}

	code, stdout, stderr = runHistory(t, "--source", "call", "--output", "json")
	if code != 0 {
		t.Fatalf("history --source call = %d, want 0; stderr: %s", code, stderr)
	}
	filtered := decodeHistoryList(t, stdout).Entries
	if len(filtered) != 1 || filtered[0].Source != "call" {
		t.Fatalf("history --source call = %+v, want the one call entry", filtered)
	}
	// The run's entries did not evict it: the retention cap is per source.
	if filtered[0].OperationID != "getPet" {
		t.Errorf("surviving call entry = %q, want getPet", filtered[0].OperationID)
	}
}

func TestRunFixturesSupplyTheRequestBody(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "createPet.json", `{"body": {"name": "Fixie"}}`)

	srv := newRunServer(t)
	code, stdout, stderr := runRun(t, runSpecFile, "--operation", "createPet", "--allow-mutations",
		"--fixtures", dir, "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run --fixtures = %d, want 0; stderr: %s", code, stderr)
	}
	if res := decodeRun(t, stdout).result(t, "createPet"); res.Outcome != "passed" {
		t.Errorf("createPet outcome = %q (%s), want passed", res.Outcome, res.Reason)
	}

	sent := srv.received()
	if len(sent) != 1 {
		t.Fatalf("server saw %v, want one request", srv.paths())
	}
	if !strings.Contains(sent[0].Body, "Fixie") {
		t.Errorf("request body = %q, want the fixture's", sent[0].Body)
	}

	// Drop the flag and the same run falls through to generation.
	generated := newRunServer(t)
	code, _, stderr = runRun(t, runSpecFile, "--operation", "createPet", "--allow-mutations",
		"--base-url", generated.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}
	sent = generated.received()
	if len(sent) != 1 {
		t.Fatalf("server saw %v, want one request", generated.paths())
	}
	if strings.Contains(sent[0].Body, "Fixie") {
		t.Errorf("request body = %q, want generated data with no fixture loaded", sent[0].Body)
	}
	if !strings.Contains(sent[0].Body, `"name"`) {
		t.Errorf("request body = %q, want a generated body for the declared schema", sent[0].Body)
	}
}

func TestRunFixturesDirectoryMustExist(t *testing.T) {
	srv := newRunServer(t)

	code, _, stderr := runRun(t, runSpecFile, "--operation", "getStatus",
		"--fixtures", filepath.Join(t.TempDir(), "nope"), "--base-url", srv.URL, "--output", "json")
	if code != 2 {
		t.Fatalf("run --fixtures <missing> = %d, want 2; stderr: %s", code, stderr)
	}
	if len(srv.received()) != 0 {
		t.Errorf("server saw %v, want nothing", srv.paths())
	}
}

func writeFixture(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
}
