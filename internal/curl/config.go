package curl

import (
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/request"
)

// maxInlineBody is the size above which a body stops being a directive and
// becomes a 0600 temp file. DESIGN.md §5a calls the temp file "the exception":
// it is a real file on disk for the length of one exec, so the threshold is set
// where the config document stops being a reasonable place to put bytes rather
// than at any hard limit of curl's.
const maxInlineBody = 1 << 20

// Capture names the files curl writes the response to. Both are optional; the
// executor allocates them and passes them here rather than appending them to
// argv, because §5a's rule is that *nothing* about the call is an argument, not
// merely nothing sensitive.
type Capture struct {
	// BodyPath receives the response body, via the output directive.
	BodyPath string
	// HeaderPath receives the response headers, via the dump-header directive.
	HeaderPath string
}

// BuildConfig renders req as a curl config document for `curl -K -`.
//
// This is the one place in talaria where a resolved credential exists. The
// document is written to curl's stdin and holds the real values; the returned
// argv is exactly `curl -K -` and holds nothing, because /proc/*/cmdline is
// readable by any process on the host — including ones an agent spawns (§5a).
//
// cleanup must be called once curl has exited. It removes any temp body file
// and zeroes the document, so the resolved values do not linger in a buffer the
// rest of the process can still reach. It is safe to call more than once, and
// is non-nil even when BuildConfig fails.
//
// A credential whose environment variable is unset fails with exit code 5 and
// no document, rather than sending an empty header — an unauthenticated request
// that looks authenticated is the worse outcome (§4).
func BuildConfig(req *request.Request, capture Capture) (config []byte, argv []string, cleanup func(), err error) {
	argv = []string{"curl", "-K", "-"}
	if req == nil {
		return nil, argv, func() {}, clierr.RequestFailed("no request to execute")
	}

	var doc document
	if err := doc.build(req, capture); err != nil {
		doc.discard()
		return nil, argv, doc.cleanup, err
	}

	config = []byte(doc.b.String())
	doc.b.Reset()

	return config, argv, doc.cleanupWith(config), nil
}

// document accumulates the directives and the temp files they point at, so a
// failure part-way through still knows what to remove.
type document struct {
	b     strings.Builder
	files []string
}

func (d *document) build(req *request.Request, capture Capture) error {
	url, err := req.URL(resolve)
	if err != nil {
		return err
	}
	d.directive("url", url)

	// head rather than `request = "HEAD"`: -X HEAD leaves curl waiting for a body
	// of Content-Length bytes that a compliant server never sends, so the call
	// hangs (or exits 18 when the connection closes first). HEAD is a safe method
	// that `run` includes without --allow-mutations, so one such operation would
	// stall an entire report.
	head := strings.EqualFold(req.Method, http.MethodHead)
	switch {
	case head:
		d.flag("head")
	case req.Method != "":
		d.directive("request", req.Method)
	}

	if err := d.auth(req); err != nil {
		return err
	}
	if err := d.cookies(req); err != nil {
		return err
	}
	if err := d.body(req); err != nil {
		return err
	}

	// Defence in depth behind request.IsHTTPScheme: curl speaks file, gopher,
	// dict and smb, and a redirect chooses the scheme of the next hop without
	// asking talaria. The leading `=` makes each list absolute rather than
	// additive, so these two are the only protocols this process can use.
	d.directive("proto", "=http,https")
	d.directive("proto-redir", "=http,https")

	// silent suppresses the progress meter, which would otherwise be interleaved
	// with the write-out payload on stdout; show-error keeps real failures
	// visible despite it.
	d.flag("silent")
	d.flag("show-error")
	// %{json} is the whole response metadata in one parseable object, and is the
	// reason DESIGN.md sets a curl >= 7.70 floor.
	d.directive("write-out", "%{json}")

	if capture.BodyPath != "" {
		// head makes curl write the header block to the output file, which at
		// capture.BodyPath would be reported as the response body. dump-header
		// still captures the headers, and the staged empty body file keeps
		// readResponse reading a real path.
		path := capture.BodyPath
		if head {
			path = os.DevNull
		}
		d.directive("output", path)
	}
	if capture.HeaderPath != "" {
		d.directive("dump-header", capture.HeaderPath)
	}

	return nil
}

// auth writes the request headers, diverting basic auth to curl's own user
// directive: the header text is base64(user:password), and letting curl encode
// it means this package never has to.
func (d *document) auth(req *request.Request) error {
	for _, h := range req.Headers {
		value, err := resolve(h.Value)
		if err != nil {
			return err
		}

		if h.Value.Encoding() == request.EncodeBasic {
			d.directive("user", strings.TrimPrefix(value, h.Value.Prefix()))
			continue
		}

		d.directive("header", h.Name+": "+value)
	}

	return nil
}

// cookies writes every cookie as one directive. curl keeps only the last of
// several, so they are joined rather than repeated.
func (d *document) cookies(req *request.Request) error {
	if len(req.Cookies) == 0 {
		return nil
	}

	var b strings.Builder
	for i, c := range req.Cookies {
		value, err := resolve(c.Value)
		if err != nil {
			return err
		}

		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name + "=" + value)
	}

	d.directive("cookie", b.String())

	return nil
}

// body writes the request body, inline when it can be and via a temp file when
// it cannot.
func (d *document) body(req *request.Request) error {
	if req.Body == nil {
		return nil
	}

	if req.Body.ContentType != "" && !hasHeader(req, "Content-Type") {
		d.directive("header", "Content-Type: "+req.Body.ContentType)
	}

	if inlinable(req.Body.Data) {
		// data-raw rather than data or data-binary: those two read a value
		// starting with @ as a filename, and a JSON body legitimately can. Verified
		// against curl 8.14.1 — data-raw sends the bytes as given and strips
		// nothing, where `data = "@path"` also strips newlines.
		d.directive("data-raw", string(req.Body.Data))
		return nil
	}

	path, err := d.tempFile(req.Body.Data)
	if err != nil {
		return err
	}
	// The file's bytes are read verbatim, so data-binary rather than data: data
	// strips newlines and carriage returns out of a file, which corrupts any
	// pretty-printed JSON and breaks any signed payload.
	d.directive("data-binary", "@"+path)

	return nil
}

// inlinable reports whether a body can live in the config document. Valid UTF-8
// under the size limit can; anything else takes the temp-file path, because a
// NUL byte truncates a quoted value and arbitrary bytes have no escape sequence
// that survives curl's parser.
func inlinable(data []byte) bool {
	if len(data) > maxInlineBody || !utf8.Valid(data) {
		return false
	}

	return !strings.ContainsRune(string(data), 0)
}

// tempFile writes data to a 0600 file and records it for cleanup.
func (d *document) tempFile(data []byte) (string, error) {
	f, err := os.CreateTemp("", "talaria-body-*")
	if err != nil {
		return "", clierr.RequestFailed("cannot stage the request body: %w", err)
	}
	d.files = append(d.files, f.Name())

	// CreateTemp already makes the file 0600; setting it explicitly keeps that a
	// guarantee of this code rather than of the standard library's current
	// behaviour, since the body may carry credentials the user put in it.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return "", clierr.RequestFailed("cannot secure the request body file: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", clierr.RequestFailed("cannot write the request body: %w", err)
	}

	if err := f.Close(); err != nil {
		return "", clierr.RequestFailed("cannot write the request body: %w", err)
	}

	return f.Name(), nil
}

// directive writes one `name = "value"` line, escaped for curl's parser.
func (d *document) directive(name, value string) {
	d.b.WriteString(name + ` = "` + escapeDirective(value) + "\"\n")
}

// flag writes one bare directive, the config form of a boolean option.
func (d *document) flag(name string) { d.b.WriteString(name + "\n") }

// discard drops a partially built document, so a failure returns nothing a
// caller could accidentally send or print.
func (d *document) discard() { d.b.Reset() }

// cleanup removes the temp files this document points at.
func (d *document) cleanup() {
	for _, path := range d.files {
		os.Remove(path) //nolint:errcheck // Best effort; the file is 0600 and in TMPDIR.
	}
	d.files = nil
}

// cleanupWith also zeroes the finished document, so the resolved credentials in
// it stop being readable from the buffer once curl has exited.
func (d *document) cleanupWith(config []byte) func() {
	return func() {
		d.cleanup()
		for i := range config {
			config[i] = 0
		}
	}
}

// resolve is the renderer that reads real credential values. It is the single
// crossing of the firewall described in DESIGN.md §5a: every other renderer in
// talaria produces a redacted or symbolic form, and this one exists only to
// feed the config document.
func resolve(v request.Value) (string, error) {
	if !v.IsSecret() {
		// Reveal rather than String: a literal the user typed under a
		// credential-shaped name displays redacted everywhere else, and the
		// wire is the one place it must not.
		return v.Reveal(), nil
	}

	value, err := v.Ref().Resolve()
	if err != nil {
		return "", err
	}

	return v.Prefix() + value, nil
}

// configEscape covers the escape sequences curl's config parser understands, in
// the order a single pass must apply them. strings.Replacer scans once and
// picks the longest match at each position, which is what keeps the backslash
// rule from re-escaping the backslashes the other rules just introduced — the
// double-escaping bug that ordered sequential replaces invite.
var configEscape = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	"\t", `\t`,
	"\n", `\n`,
	"\r", `\r`,
)

func escapeDirective(s string) string { return configEscape.Replace(s) }
