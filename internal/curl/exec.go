package curl

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/request"
)

// Response is one observed HTTP response. It is the `response` block of call's
// output (DESIGN.md §4) before any redaction is applied — redaction of what came
// back off the wire is Task 20's job, and doing it here would leave no
// unredacted copy for validation to check against the spec.
type Response struct {
	Status   int         `json:"status"`
	Headers  http.Header `json:"headers"`
	Body     []byte      `json:"body"`
	TimingMS int64       `json:"timing_ms"`
}

// Execute runs req through the system curl and returns what came back.
//
// An HTTP 4xx or 5xx is a successful observation and returns a Response with no
// error (§4); only a request that could not be completed at all — a refused
// connection, a TLS failure, curl itself failing — is an error, and it carries
// exit code 1.
//
// The credential values live in the config document written to curl's stdin and
// nowhere else: not in argv, which /proc/*/cmdline exposes to any process on
// the host, and not in the environment curl inherits (§5a).
func Execute(req *request.Request) (*Response, error) {
	path, err := exec.LookPath("curl")
	if err != nil {
		return nil, clierr.RequestFailed("curl is not installed or not on PATH: %w", err)
	}
	if err := preflight(path); err != nil {
		return nil, err
	}

	capture, cleanupCapture, err := newCapture()
	if err != nil {
		return nil, err
	}
	defer cleanupCapture()

	config, argv, cleanupConfig, err := BuildConfig(req, capture)
	defer cleanupConfig()
	if err != nil {
		return nil, err
	}

	// argv[0] is the command name BuildConfig reports; the resolved path is what
	// actually gets executed, so a PATH change mid-run cannot swap the binary
	// between the preflight and the call.
	cmd := exec.Command(path, argv[1:]...)
	// The document goes in over a pipe rather than through the process's own
	// stdin, which belongs to the user and may be a request body (Task 18).
	cmd.Stdin = bytes.NewReader(config)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, runFailure(req, err, stderr.String())
	}

	return readResponse(req, capture, stdout.Bytes())
}

// writeOut is curl's `--write-out '%{json}'` payload, of which talaria reads
// two fields. It arrives as the whole of curl's stdout, so it is unmarshalled
// as one object rather than scanned for out of a mixed stream — the split that
// makes a response body impersonating this shape a non-event.
type writeOut struct {
	HTTPCode  int     `json:"http_code"`
	TimeTotal float64 `json:"time_total"`
	ErrorMsg  string  `json:"errormsg"`
}

// readResponse assembles the Response from curl's three output channels.
func readResponse(req *request.Request, capture Capture, stdout []byte) (*Response, error) {
	var meta writeOut
	if err := json.Unmarshal(stdout, &meta); err != nil {
		return nil, clierr.RequestFailed("cannot parse curl's response metadata: %w", err)
	}

	body, err := os.ReadFile(capture.BodyPath)
	if err != nil {
		return nil, clierr.RequestFailed("cannot read the response body: %w", err)
	}

	dump, err := os.ReadFile(capture.HeaderPath)
	if err != nil {
		return nil, clierr.RequestFailed("cannot read the response headers: %w", err)
	}

	// http_code is 0 when curl never got a response. That is a completed process
	// with nothing observed, which is a failed request rather than an empty one.
	if meta.HTTPCode == 0 {
		return nil, requestFailed(req, 0, meta.ErrorMsg)
	}

	return &Response{
		Status:   meta.HTTPCode,
		Headers:  parseHeaders(dump),
		Body:     body,
		TimingMS: int64(math.Round(meta.TimeTotal * 1000)),
	}, nil
}

// newCapture allocates the two files curl writes the response to, inside a 0700
// directory of their own. They hold response data, which may carry a session
// cookie or a token the API handed back, so they are never world-readable and
// never outlive the exec.
func newCapture() (Capture, func(), error) {
	dir, err := os.MkdirTemp("", "talaria-call-*")
	if err != nil {
		return Capture{}, func() {}, clierr.RequestFailed("cannot stage the response: %w", err)
	}

	cleanup := func() { os.RemoveAll(dir) } //nolint:errcheck // Best effort; 0700 and in TMPDIR.

	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return Capture{}, func() {}, clierr.RequestFailed("cannot secure the response directory: %w", err)
	}

	capture := Capture{
		BodyPath:   filepath.Join(dir, "body"),
		HeaderPath: filepath.Join(dir, "headers"),
	}

	// Creating both up front means curl writes into a file that is already 0600,
	// rather than one whose mode comes from curl's umask.
	for _, path := range []string{capture.BodyPath, capture.HeaderPath} {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			cleanup()
			return Capture{}, func() {}, clierr.RequestFailed("cannot stage the response: %w", err)
		}
		if err := f.Close(); err != nil {
			cleanup()
			return Capture{}, func() {}, clierr.RequestFailed("cannot stage the response: %w", err)
		}
	}

	return capture, cleanup, nil
}

// parseHeaders turns curl's dump-header file into an http.Header, keeping only
// the last response block. Redirects and a 100 Continue each leave a block in
// front of the real one, and a stale Location reported as the response's own is
// worse than no Location at all.
func parseHeaders(dump []byte) http.Header {
	headers := http.Header{}

	blocks := strings.Split(strings.ReplaceAll(string(dump), "\r\n", "\n"), "\n\n")
	last := ""
	for _, block := range blocks {
		if strings.TrimSpace(block) != "" {
			last = block
		}
	}

	for _, line := range strings.Split(last, "\n") {
		// The status line is not a header; the Response's Status comes from
		// %{json}, which does not need re-deriving from text.
		if line == "" || strings.HasPrefix(line, "HTTP/") {
			continue
		}

		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		headers.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}

	return headers
}

// runFailure classifies a curl process that did not exit 0.
func runFailure(req *request.Request, err error, stderr string) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return requestFailed(req, exitErr.ExitCode(), stderr)
	}

	return clierr.RequestFailed("cannot run curl: %w", err)
}

// requestFailed builds the error for a request curl could not complete, with
// curl's own exit status in it so an operator can look the code up.
//
// The detail passes through the request's scrubber first: curl echoes the URL
// it was given into several of its messages, and for an API-key scheme the URL
// is exactly where the credential is. §5a — errors are built from the redacted
// representation.
func requestFailed(req *request.Request, exitCode int, detail string) error {
	detail = strings.TrimSpace(scrubber(req).Replace(detail))
	if detail == "" {
		return clierr.RequestFailed("the request could not be completed: curl exited %d", exitCode)
	}

	return clierr.RequestFailed("the request could not be completed: curl exited %d: %s", exitCode, detail)
}

// scrubber returns a replacer that rewrites any credential this request
// resolved back into its <redacted:env:NAME> form.
//
// It works from the request's own refs rather than from a pattern over the
// text: talaria knows exactly which values it put on the wire, and matching
// those is neither a guess nor defeatable by an unusual message format. A
// credential that will not resolve was never sent and so cannot be echoed.
func scrubber(req *request.Request) *strings.Replacer {
	if req == nil {
		return strings.NewReplacer()
	}

	var pairs []string
	for _, group := range [][]request.Pair{req.Headers, req.Query, req.Cookies} {
		for _, p := range group {
			if !p.Value.IsSecret() {
				continue
			}

			resolved, err := p.Value.Ref().Resolve()
			if err != nil || resolved == "" {
				continue
			}

			redacted := p.Value.Ref().String()
			pairs = append(pairs, resolved, redacted)
			// A credential echoed back as part of a URL is percent-encoded, which
			// the literal form would walk straight past.
			if encoded := url.QueryEscape(resolved); encoded != resolved {
				pairs = append(pairs, encoded, redacted)
			}
		}
	}

	return strings.NewReplacer(pairs...)
}
