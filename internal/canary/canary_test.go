package canary_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Teeeep/talaria/internal/canary"
)

// specPath is the fixture declaring one security scheme per auth mechanism.
const specPath = "testdata/canary.yaml"

// binary is the talaria under test, built once by TestMain.
//
// The suite drives a real process rather than calling the command tree
// in-process, for one reason: the command tree lives in package main and cannot
// be imported. Running the binary is also the stronger reading of §5a — it
// greps every byte the process emits, including anything written outside the
// writers a test would otherwise hand it.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "talaria-canary-*")
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
// It has to be said out loud because the dependency is invisible to the tool:
// the suite reaches the code under test by building a binary in a subprocess,
// and `go test` records nothing about that. Without this, a leak introduced in
// internal/curl or cmd/talaria would leave the canary package's own inputs
// untouched, and `go test ./...` would replay a cached pass over code that
// never ran. A gate that can go stale is not a gate. The test cache does track
// files a test reads, so reading them is how the dependency is declared.
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

// mechanism is one way of authenticating, driven end to end with its own
// canary. Received proves the credential actually reached the server, which is
// what makes the absence of it everywhere else mean something: a call that
// quietly sent nothing would pass every leak assertion.
type mechanism struct {
	name string
	op   string
	// env builds the environment carrying the canary, keyed by variable name.
	env func(value string) map[string]string
	// received reports whether the server saw the canary on the request.
	received func(req recordedRequest, value string) bool
}

var mechanisms = []mechanism{
	{
		name: "bearer",
		op:   "getBearer",
		env:  func(v string) map[string]string { return map[string]string{"TALARIA_AUTH_BEARER": v} },
		received: func(req recordedRequest, v string) bool {
			return req.Header.Get("Authorization") == "Bearer "+v
		},
	},
	{
		name: "basic",
		op:   "getBasic",
		env: func(v string) map[string]string {
			return map[string]string{"TALARIA_AUTH_BASIC": "canaryuser:" + v}
		},
		received: func(req recordedRequest, v string) bool {
			// Basic auth is the one mechanism that encodes the credential before
			// it goes on the wire, which is why Scan looks for base64 too.
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte("canaryuser:"+v))
			return req.Header.Get("Authorization") == want
		},
	},
	{
		name: "apikey-header",
		op:   "getHeaderKey",
		env: func(v string) map[string]string {
			return map[string]string{"TALARIA_AUTH_APIKEY_HEADERKEY": v}
		},
		received: func(req recordedRequest, v string) bool {
			return req.Header.Get("X-Api-Key") == v
		},
	},
	{
		name: "apikey-query",
		op:   "getQueryKey",
		env: func(v string) map[string]string {
			return map[string]string{"TALARIA_AUTH_APIKEY_QUERYKEY": v}
		},
		received: func(req recordedRequest, v string) bool {
			return req.Query.Get("api_key") == v
		},
	},
	{
		name: "apikey-cookie",
		op:   "getCookieKey",
		env: func(v string) map[string]string {
			return map[string]string{"TALARIA_AUTH_APIKEY_COOKIEKEY": v}
		},
		received: func(req recordedRequest, v string) bool {
			return req.Cookies[v] || strings.Contains(req.Header.Get("Cookie"), v)
		},
	},
}

// result is one talaria invocation.
type result struct {
	args   []string
	code   int
	stdout string
	stderr string
}

// harness runs talaria against an isolated home, so the only credentials in
// reach are the ones a case injected and the only files it can write are ones
// the case can then read back and grep.
type harness struct {
	t   *testing.T
	env []string
	// home is $HOME for the run: the directory curl looks in for .curlrc, and
	// the parent of the three XDG roots below.
	home  string
	state string
	cache string
	conf  string
}

func newHarness(t *testing.T, vars map[string]string) *harness {
	t.Helper()

	home := t.TempDir()
	h := &harness{
		t:     t,
		home:  home,
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

// run executes talaria and captures both streams.
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

// runOK runs talaria and fails the test unless it exited 0.
func (h *harness) runOK(args ...string) result {
	h.t.Helper()

	res := h.run(args...)
	if res.code != 0 {
		h.t.Fatalf("talaria %s = %d, want 0; stderr: %s",
			strings.Join(args, " "), res.code, res.stderr)
	}

	return res
}

// surfaces returns both streams of a result as scannable surfaces.
func (r result) surfaces() []canary.Surface {
	label := strings.Join(r.args, " ")

	return []canary.Surface{
		canary.Stream("stdout of `talaria "+label+"`", r.stdout),
		canary.Stream("stderr of `talaria "+label+"`", r.stderr),
	}
}

// written returns every file talaria left behind: the history store, the spec
// cache, anything a future version starts writing into the same directories.
// Enumerated by walking, not by name, so a new persistent artifact is covered
// the day it appears.
//
// The config directory is deliberately not among them. Nothing talaria writes
// goes there — it is an input the user wrote, and a case that puts a canary in
// a config file to see how an error path handles it would otherwise be caught
// finding its own fixture.
func (h *harness) written() []canary.Surface {
	h.t.Helper()

	var surfaces []canary.Surface
	for label, root := range map[string]string{
		"history": h.state,
		"cache":   h.cache,
	} {
		found, err := canary.Tree(label, root)
		if err != nil {
			h.t.Fatalf("enumerating the %s surfaces: %v", label, err)
		}
		surfaces = append(surfaces, found...)
	}

	return surfaces
}

// recordedRequest is what arrived at the test server.
type recordedRequest struct {
	Header  http.Header
	Query   url.Values
	Cookies map[string]bool
}

type recordingServer struct {
	*httptest.Server

	mu   sync.Mutex
	last recordedRequest
}

// newServer starts a server that records each request and answers 200 with a
// body carrying no secret of its own.
func newServer(t *testing.T, body string) *recordingServer {
	t.Helper()

	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookies := map[string]bool{}
		for _, c := range r.Cookies() {
			cookies[c.Value] = true
		}

		rs.mu.Lock()
		rs.last = recordedRequest{
			Header:  r.Header.Clone(),
			Query:   r.URL.Query(),
			Cookies: cookies,
		}
		rs.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(rs.Close)

	return rs
}

func (rs *recordingServer) received() recordedRequest {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	return rs.last
}

// TestNoAuthMechanismLeaksIntoAnyOutputSurface is the suite §5a asks for: every
// auth mechanism, every output format, every surface a run produces.
//
// The format loop comes from canary.Formats, which is internal/output's Format
// enum. A fourth format is exercised here the day it is added, and the
// non-empty assertion below means a format that renders nothing fails rather
// than passing vacuously.
func TestNoAuthMechanismLeaksIntoAnyOutputSurface(t *testing.T) {
	for _, mech := range mechanisms {
		for _, format := range canary.Formats() {
			t.Run(mech.name+"/"+format, func(t *testing.T) {
				t.Parallel()

				value := canary.Value(mech.name)
				h := newHarness(t, mech.env(value))
				srv := newServer(t, `{"id":"42","name":"Rex"}`)

				var runs []result
				runs = append(runs,
					h.runOK("call", specPath, mech.op,
						"--base-url", srv.URL, "--output", format, "--dry-run"),
					h.runOK("call", specPath, mech.op,
						"--base-url", srv.URL, "--output", format),
					// Exits 5: the fixture declares every scheme and this case
					// sets one. The report is the surface being checked, and it
					// is written on the way to that exit code.
					h.run("auth", "check", specPath, "--output", format),
					h.runOK("history", "--output", format),
					h.runOK("history", "show", "1", "--output", format),
					h.runOK("history", "replay", "1", "--output", format),
					h.runOK("describe", specPath, mech.op, "--output", format),
					h.runOK("list", specPath, "--output", format),
					// Last, so it disturbs no history index above. `run` sends
					// the same credential `call` does and renders its own
					// report, so it is its own surface.
					h.runOK("run", specPath, "--operation", mech.op,
						"--base-url", srv.URL, "--report", format),
				)

				// The credential reached the server. Without this the rest of
				// the test would pass just as well if talaria sent nothing.
				if !mech.received(srv.received(), value) {
					t.Fatalf("the server never saw the %s credential; the leak assertions below prove nothing", mech.name)
				}

				var surfaces []canary.Surface
				for _, res := range runs {
					if strings.TrimSpace(res.stdout) == "" {
						t.Errorf("`talaria %s` printed nothing; --output %s renders no payload",
							strings.Join(res.args, " "), format)
					}
					surfaces = append(surfaces, res.surfaces()...)
				}
				surfaces = append(surfaces, h.written()...)

				assertNoLeak(t, value, surfaces)
			})
		}
	}
}

// TestNoReportFormatLeaksTheCredential covers §5a's "junit reports" among the
// output surfaces, and every other shape `run --report` writes.
//
// It is a loop of its own rather than a line in the test above because the
// report formats are a superset of the --output ones: junit describes a suite
// of operations, which is something only `run` produces. Driving it from
// canary.ReportFormats means a fifth report format is covered the day it is
// added.
func TestNoReportFormatLeaksTheCredential(t *testing.T) {
	for _, mech := range mechanisms {
		for _, format := range canary.ReportFormats() {
			t.Run(mech.name+"/"+format, func(t *testing.T) {
				t.Parallel()

				value := canary.Value(mech.name)
				h := newHarness(t, mech.env(value))
				srv := newServer(t, `{"id":"42","name":"Rex"}`)

				res := h.runOK("run", specPath, "--operation", mech.op,
					"--base-url", srv.URL, "--report", format)

				if !mech.received(srv.received(), value) {
					t.Fatalf("the server never saw the %s credential; the leak assertions below prove nothing", mech.name)
				}
				// A report that named the operation is a report that rendered.
				// Without this a format writing an empty shell would pass every
				// assertion below by having nothing in it at all.
				if !strings.Contains(res.stdout, mech.op) {
					t.Errorf("`--report %s` does not name the operation it ran:\n%s", format, res.stdout)
				}

				assertNoLeak(t, value, append(res.surfaces(), h.written()...))
			})
		}
	}
}

// TestErrorPathsDoNotLeakTheCredential covers §5a's "error paths are where
// redaction bugs live": a failure at each stage of a call, with a credential
// present throughout.
func TestErrorPathsDoNotLeakTheCredential(t *testing.T) {
	// A response-validation failure is not here because response validation is
	// not built yet (plan tasks 26 and 27). Whoever adds --fail-on-error adds
	// the case; the stages that exist are all covered.
	stages := []struct {
		name string
		args func(serverURL string) []string
		code int
	}{
		{
			name: "spec load",
			args: func(string) []string { return []string{"call", "testdata/absent.yaml", "getBearer"} },
			code: 3,
		},
		{
			name: "operation lookup",
			args: func(string) []string { return []string{"call", specPath, "noSuchOperation"} },
			code: 2,
		},
		{
			name: "parameter binding",
			args: func(url string) []string {
				// getPet's petId is required and unbound.
				return []string{"call", specPath, "getPet", "--base-url", url}
			},
			code: 2,
		},
		{
			name: "mutation gate",
			args: func(url string) []string {
				return []string{"call", specPath, "createThing", "--base-url", url}
			},
			code: 2,
		},
		{
			name: "body read",
			args: func(url string) []string {
				return []string{"call", specPath, "getBearer", "--base-url", url, "--body", "@testdata/absent.json"}
			},
			code: 2,
		},
		{
			name: "curl exec",
			args: func(string) []string {
				// Port 1 on the loopback interface: curl runs, the request fails.
				return []string{"call", specPath, "getBearer", "--base-url", "http://127.0.0.1:1"}
			},
			code: 1,
		},
		{
			name: "history index",
			args: func(string) []string { return []string{"history", "show", "99"} },
			code: 2,
		},
	}

	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			t.Parallel()

			value := canary.Value("error")
			h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": value})
			srv := newServer(t, `{}`)

			res := h.run(stage.args(srv.URL)...)
			if res.code != stage.code {
				t.Errorf("talaria %s = %d, want %d; stderr: %s",
					strings.Join(res.args, " "), res.code, stage.code, res.stderr)
			}
			if strings.TrimSpace(res.stderr) == "" {
				t.Errorf("talaria %s failed silently; there is no error surface to check",
					strings.Join(res.args, " "))
			}

			assertNoLeak(t, value, append(res.surfaces(), h.written()...))
		})
	}
}

// TestAMalformedFlagDoesNotEchoItsValue covers the error path where the
// credential is in the *argv*: the user wrote curl's `Authorization: Bearer …`
// form, or repeated --body, and talaria refuses the invocation.
//
// The canary rides in the rejected argument itself rather than in the
// environment, which is what makes this stage different from every other one in
// this suite — here the value talaria must not print is a value it was handed
// directly, and the message that refuses it is the surface most likely to quote
// it back. §5a puts error paths inside the firewall.
//
// There is no --report surface: `run` takes no --header, --query or --body, so
// a malformed one cannot reach it. --output covers what the failing command
// does render, and h.written() covers the history store.
func TestAMalformedFlagDoesNotEchoItsValue(t *testing.T) {
	cases := []struct {
		name string
		// args builds the flags carrying the canary, appended to a `call`.
		args func(value string) []string
		// wants are the substrings the refusal must carry, so a message that
		// said nothing at all could not pass by leaking nothing.
		wants []string
	}{
		{
			name: "header in curl's colon form",
			args: func(v string) []string {
				return []string{"--header", "Authorization: Bearer " + v}
			},
			wants: []string{"--header 1", "Authorization"},
		},
		{
			name: "header whose name half is not a field name",
			args: func(v string) []string {
				return []string{"--header", "X-Trace: " + v + "=1"}
			},
			wants: []string{"--header 1", "X-Trace"},
		},
		{
			name: "query in the colon form",
			args: func(v string) []string {
				return []string{"--query", "api_key: " + v}
			},
			wants: []string{"--query 1", "api_key"},
		},
		{
			name: "repeated body",
			args: func(v string) []string {
				return []string{
					"--body", `{"client_secret":"` + v + `"}`,
					"--body", "@/tmp/" + v + ".json",
				}
			},
			wants: []string{"--body", "2 times", "literal", "@file"},
		},
	}

	for _, tc := range cases {
		for _, format := range canary.Formats() {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				t.Parallel()

				value := canary.Value("argv")
				h := newHarness(t, nil)
				srv := newServer(t, `{"ok":true}`)

				var runs []result
				for _, extra := range [][]string{{"--dry-run"}, nil} {
					args := append([]string{"call", specPath, "getPublic",
						"--base-url", srv.URL, "--output", format}, tc.args(value)...)
					res := h.run(append(args, extra...)...)
					if res.code != 2 {
						t.Errorf("talaria %s = %d, want 2; stderr: %s",
							strings.Join(res.args, " "), res.code, res.stderr)
					}
					for _, want := range tc.wants {
						if !strings.Contains(res.stderr, want) {
							t.Errorf("the refusal does not mention %q:\n%s", want, res.stderr)
						}
					}
					runs = append(runs, res)
				}
				// The history store is a surface whether or not the refused call
				// reached it, so it is listed and scanned either way.
				runs = append(runs, h.runOK("history", "--output", format))

				var surfaces []canary.Surface
				for _, res := range runs {
					surfaces = append(surfaces, res.surfaces()...)
				}

				assertNoLeak(t, value, append(surfaces, h.written()...))
			})
		}
	}
}

// TestABaseURLCredentialIsRefusedWithoutEchoingIt covers the credential
// position a URL has no symbolic form for: `--base-url http://user:pass@host`.
//
// Accepting it would write the value into request.url, the emitted curl and
// history.jsonl in cleartext, so talaria refuses it and points at
// TALARIA_AUTH_BASIC. That makes the refusal an error surface holding a
// credential it was handed directly — the case §5a calls out — so it must name
// the host and nothing else.
func TestABaseURLCredentialIsRefusedWithoutEchoingIt(t *testing.T) {
	for _, format := range canary.Formats() {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			value := canary.Value("userinfo")
			h := newHarness(t, nil)
			srv := newServer(t, `{"ok":true}`)
			base := "http://canaryuser:" + value + "@" + strings.TrimPrefix(srv.URL, "http://")

			var runs []result
			for _, extra := range [][]string{{"--dry-run"}, nil} {
				args := append([]string{"call", specPath, "getPublic",
					"--base-url", base, "--output", format}, extra...)
				res := h.run(args...)
				if res.code != 2 {
					t.Errorf("talaria %s = %d, want 2; stderr: %s",
						strings.Join(res.args, " "), res.code, res.stderr)
				}
				if !strings.Contains(res.stderr, "TALARIA_AUTH_BASIC") {
					t.Errorf("the refusal does not point at the supported path:\n%s", res.stderr)
				}
				runs = append(runs, res)
			}
			// The history store is a surface whether or not the refused call
			// reached it, so it is listed and scanned either way.
			runs = append(runs, h.runOK("history", "--output", format))

			var surfaces []canary.Surface
			for _, res := range runs {
				surfaces = append(surfaces, res.surfaces()...)
			}

			assertNoLeak(t, value, append(surfaces, h.written()...))
		})
	}
}

// TestACredentialResolutionFailureNamesNoValue covers the one error path where
// the credential is in a *configuration file* rather than the environment: a
// profile with a literal value is refused, and the refusal must not quote what
// it refused (§5a).
func TestACredentialResolutionFailureNamesNoValue(t *testing.T) {
	t.Parallel()

	value := canary.Value("profile")
	h := newHarness(t, nil)
	writeConfig(t, h, "profiles:\n  staging:\n    auth:\n      bearerAuth: "+value+"\n")

	res := h.run("call", specPath, "getBearer", "--profile", "staging", "--dry-run")
	if res.code != 2 {
		t.Errorf("a literal profile credential = %d, want 2; stderr: %s", res.code, res.stderr)
	}

	assertNoLeak(t, value, append(res.surfaces(), h.written()...))
}

// TestAMissingCredentialIsReportedWithoutAValue exercises the exit-5 path with
// a canary set for a *different* scheme, so the report is built while a
// credential is in reach.
func TestAMissingCredentialIsReportedWithoutAValue(t *testing.T) {
	t.Parallel()

	value := canary.Value("authcheck")
	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": value})

	// Every other scheme in the fixture is unset, so `auth check` reports the
	// gap and exits 5.
	res := h.run("auth", "check", specPath, "--output", "json")
	if res.code != 5 {
		t.Errorf("auth check with schemes unset = %d, want 5; stderr: %s", res.code, res.stderr)
	}

	assertNoLeak(t, value, append(res.surfaces(), h.written()...))
}

// TestAUserSuppliedHeaderIsRedactedLikeASpecCredential holds the line the
// Redactor already claims: "a bearer token the user typed into --header is as
// sensitive as one talaria resolved itself". The value's origin does not change
// what it is.
//
// The --query case is here rather than in a test of its own because it is the
// same claim about a different flag: §5a names query-string API keys as a
// credential location, so `--query api_key=…` is covered by the built-in name
// matcher exactly as `--header X-Api-Key: …` is.
func TestAUserSuppliedHeaderIsRedactedLikeASpecCredential(t *testing.T) {
	cases := []struct {
		name string
		flag string
		// arg builds the flag's name=value argument around the canary.
		arg func(value string) string
		// sent returns what the server actually received under that name, so
		// each case can prove the value still went out before asserting that
		// no surface printed it.
		sent func(name string, req recordedRequest) string
	}{
		{
			name: "authorization",
			flag: "--header",
			arg:  func(v string) string { return "Authorization=Bearer " + v },
			sent: func(name string, req recordedRequest) string { return req.Header.Get(name) },
		},
		{
			name: "api-key",
			flag: "--header",
			arg:  func(v string) string { return "X-Api-Key=" + v },
			sent: func(name string, req recordedRequest) string { return req.Header.Get(name) },
		},
		{
			name: "token",
			flag: "--header",
			arg:  func(v string) string { return "X-Session-Token=" + v },
			sent: func(name string, req recordedRequest) string { return req.Header.Get(name) },
		},
		{
			name: "query-api-key",
			flag: "--query",
			arg:  func(v string) string { return "api_key=" + v },
			sent: func(name string, req recordedRequest) string { return req.Query.Get(name) },
		},
	}

	for _, tc := range cases {
		for _, format := range canary.Formats() {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				t.Parallel()

				value := canary.Value("header")
				h := newHarness(t, nil)
				srv := newServer(t, `{"ok":true}`)

				runs := []result{
					h.runOK("call", specPath, "getPublic", "--base-url", srv.URL,
						tc.flag, tc.arg(value), "--output", format, "--dry-run"),
					h.runOK("call", specPath, "getPublic", "--base-url", srv.URL,
						tc.flag, tc.arg(value), "--output", format),
					h.runOK("history", "--output", format),
					h.runOK("history", "show", "1", "--output", format),
				}

				// It still has to arrive: redaction is about what talaria prints,
				// not about dropping what the user asked to send.
				name := strings.SplitN(tc.arg(value), "=", 2)[0]
				if got := tc.sent(name, srv.received()); !strings.Contains(got, value) {
					t.Fatalf("the server never saw the %s value; the leak assertions below prove nothing", tc.flag)
				}

				var surfaces []canary.Surface
				for _, res := range runs {
					surfaces = append(surfaces, res.surfaces()...)
				}

				assertNoLeak(t, value, append(surfaces, h.written()...))
			})
		}
	}
}

// TestAnUnconfiguredResponseBodySecretIsNotRedacted documents the boundary
// rather than pretending it is elsewhere. §5a redacts response headers and the
// RFC 6749 token fields by name; a secret under an API-specific name is the
// known uncovered threat, because talaria cannot tell a credential from any
// other string by looking at it.
//
// If this ever fails because the value was redacted, the boundary moved and the
// documentation — AGENT.md, DESIGN.md §5a — moves with it.
func TestAnUnconfiguredResponseBodySecretIsNotRedacted(t *testing.T) {
	t.Parallel()

	value := canary.Value("response")
	h := newHarness(t, nil)
	srv := newServer(t, `{"access_token":"issued","data":{"session":"`+value+`"}}`)

	res := h.runOK("call", specPath, "getPublic", "--base-url", srv.URL, "--output", "json")
	if !strings.Contains(res.stdout, value) {
		t.Errorf("an unconfigured response body secret was redacted; the documented boundary has moved:\n%s", res.stdout)
	}
	// The names talaria *can* recognise are covered without configuration, which
	// is what makes the above a boundary rather than an absence of redaction.
	if strings.Contains(res.stdout, `"issued"`) {
		t.Errorf("the built-in access_token body path was not redacted:\n%s", res.stdout)
	}

	// And the configured countermeasure closes the rest of the gap.
	writeConfig(t, h, "redact:\n  body-paths:\n    - data.session\n")

	res = h.runOK("call", specPath, "getPublic", "--base-url", srv.URL, "--output", "json")
	if strings.Contains(res.stdout, value) {
		t.Errorf("redact.body-paths did not redact the configured path:\n%s", res.stdout)
	}
}

// TestNoCommandExposesAVerboseOrDebugFlag is §5a's "no --verbose that bypasses
// [redaction]" in its strongest form: there is no such flag to bypass it.
// Whoever adds one inherits this test, and with it the obligation to route it
// through the redacted representation.
func TestNoCommandExposesAVerboseOrDebugFlag(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	commands := listCommands(t, h)

	// A parse that found nothing would pass silently, so the shape of the help
	// output is asserted before anything is concluded from it.
	for _, want := range []string{"call", "list", "describe", "search", "uses", "auth", "history", "version"} {
		if !contains(commands, want) {
			t.Fatalf("parsed the command list as %v, which is missing %q", commands, want)
		}
	}

	for _, name := range commands {
		for _, flag := range []string{"--verbose", "--debug"} {
			res := h.run(name, flag)
			if res.code != 2 {
				t.Errorf("`talaria %s %s` = %d, want 2 (unknown flag); stdout: %s stderr: %s",
					name, flag, res.code, res.stdout, res.stderr)
			}
		}

		// A hidden flag would not fail above but also would not be advertised,
		// so the help text is checked too.
		help := h.run(name, "--help")
		for _, flag := range []string{"--verbose", "--debug"} {
			if strings.Contains(help.stdout, flag) {
				t.Errorf("`talaria %s --help` advertises %s:\n%s", name, flag, help.stdout)
			}
		}
	}
}

// TestASpecFetchedOverHTTPLeavesNoCredentialInTheCache covers the spec cache as
// a surface with something actually in it — a cache that was never written is
// not evidence of anything.
func TestASpecFetchedOverHTTPLeavesNoCredentialInTheCache(t *testing.T) {
	t.Parallel()

	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	value := canary.Value("cache")
	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": value})

	srv := newServer(t, `{"ok":true}`)
	specSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(spec)
	}))
	t.Cleanup(specSrv.Close)

	res := h.runOK("call", specSrv.URL+"/openapi.yaml", "getBearer",
		"--base-url", srv.URL, "--output", "json")

	cached, err := canary.Tree("cache", h.cache)
	if err != nil {
		t.Fatalf("enumerating the cache: %v", err)
	}
	if len(cached) == 0 {
		t.Fatal("the spec cache is empty; the fetch did not exercise the surface this test is about")
	}

	assertNoLeak(t, value, append(res.surfaces(), h.written()...))
}

// TestAPlantedCurlrcCannotCaptureTheCredential covers the capability §5a does
// not concede: an attacker who can write one file under $HOME but cannot read
// the environment.
//
// curl parses $HOME/.curlrc before the `-K -` document, so without curl's -q a
// `trace-ascii` line there writes the plaintext Authorization header — the one
// place in the process the credential is resolved — to a path of the writer's
// choosing, at whatever mode their umask gives. The redacted output surfaces
// stay clean throughout, which is why this needs a case of its own: every other
// assertion in this suite would pass while the token sat in the trace file.
func TestAPlantedCurlrcCannotCaptureTheCredential(t *testing.T) {
	t.Parallel()

	value := canary.Value("curlrc")
	h := newHarness(t, map[string]string{"TALARIA_AUTH_BEARER": value})
	srv := newServer(t, `{"ok":true}`)

	trace := filepath.Join(h.home, "trace.txt")
	curlrc := filepath.Join(h.home, ".curlrc")
	if err := os.WriteFile(curlrc, []byte("trace-ascii = "+trace+"\n"), 0o600); err != nil {
		t.Fatalf("planting the .curlrc: %v", err)
	}

	res := h.runOK("call", specPath, "getBearer", "--base-url", srv.URL, "--output", "json")

	// The call has to have happened with the credential on it, or a missing
	// trace file would mean nothing.
	if got := srv.received().Header.Get("Authorization"); got != "Bearer "+value {
		t.Fatalf("the server never saw the credential; the assertions below prove nothing")
	}

	surfaces := append(res.surfaces(), h.written()...)
	switch _, err := os.Stat(trace); {
	case err == nil:
		// The file existing is already the finding, but scanning it says whether
		// the credential is in it — the difference between a lost -q and a lost
		// firewall.
		found, err := canary.Tree("curlrc trace", trace)
		if err != nil {
			t.Fatalf("reading the trace file: %v", err)
		}
		surfaces = append(surfaces, found...)
		t.Errorf("curl honoured %s and wrote %s; the -q that disables it is gone from argv", curlrc, trace)
	case !errors.Is(err, fs.ErrNotExist):
		t.Fatalf("stating the trace file: %v", err)
	}

	assertNoLeak(t, value, surfaces)
}

// assertNoLeak fails the test naming every surface the canary reached.
func assertNoLeak(t *testing.T, value string, surfaces []canary.Surface) {
	t.Helper()

	leaks := canary.Scan(value, surfaces...)
	if len(leaks) == 0 {
		return
	}

	var names []string
	for _, leak := range leaks {
		names = append(names, leak.String())
	}
	t.Errorf("the canary reached %d surface(s):\n  %s", len(leaks), strings.Join(names, "\n  "))
}

// listCommands reads the command names out of the root help, so a command
// added later is checked without anyone remembering to list it here.
func listCommands(t *testing.T, h *harness) []string {
	t.Helper()

	help := h.runOK("--help")

	var names []string
	var inList bool
	for _, line := range strings.Split(help.stdout, "\n") {
		switch {
		case strings.HasPrefix(line, "Available Commands:"):
			inList = true
		case inList && strings.TrimSpace(line) == "":
			inList = false
		case inList:
			if fields := strings.Fields(line); len(fields) > 0 {
				names = append(names, fields[0])
			}
		}
	}

	return names
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}

// writeConfig puts a config file in the harness's isolated config home.
func writeConfig(t *testing.T, h *harness, yaml string) {
	t.Helper()

	dir := filepath.Join(h.conf, "talaria")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating the config directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing the config file: %v", err)
	}
}
