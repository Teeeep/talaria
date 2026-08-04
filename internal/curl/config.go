package curl

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/request"
)

// maxInlineBody is the size above which a body stops being a directive and
// becomes a 0600 temp file. DESIGN.md §5a calls the temp file "the exception":
// it is a real file on disk for the length of one exec, so the threshold is set
// where the config document stops being a reasonable place to put bytes rather
// than at any hard limit of curl's.
const maxInlineBody = 1 << 20

// The default bounds on one call. They are generous enough that a slow but
// working API still answers, and short enough that a wedged one becomes an exit
// code rather than a process an agent cannot interpret — the reasoning
// internal/spec/source.go already applies to fetching a spec, applied to the
// higher-risk path.
const (
	DefaultConnectTimeout = 10 * time.Second
	DefaultMaxTime        = 30 * time.Second
)

// Options bound one invocation of curl in time. They are enforced by curl
// itself, which exits 28 when either runs out; the executor's own deadline is
// only the backstop for a curl that ignores them.
type Options struct {
	// ConnectTimeout limits establishing the connection.
	ConnectTimeout time.Duration
	// MaxTime limits the whole operation, connection included.
	MaxTime time.Duration
}

// DefaultOptions returns the bounds a caller gets without asking.
func DefaultOptions() Options {
	return Options{ConnectTimeout: DefaultConnectTimeout, MaxTime: DefaultMaxTime}
}

// withDefaults fills in the timeouts a caller left unset.
//
// A non-positive value means "unset" rather than "no limit": an unbounded call
// is exactly what these options exist to prevent, so there is deliberately no
// way to ask for one. A connect timeout longer than the total is clamped, since
// curl would enforce the total against it anyway and the pair reads as a lie.
func (o Options) withDefaults() Options {
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = DefaultConnectTimeout
	}
	if o.MaxTime <= 0 {
		o.MaxTime = DefaultMaxTime
	}
	if o.ConnectTimeout > o.MaxTime {
		o.ConnectTimeout = o.MaxTime
	}

	return o
}

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
// and zeroes the two buffers this package controls: the returned config and the
// builder's own. It is safe to call more than once, and is non-nil even when
// BuildConfig fails. It says nothing about copies made downstream of the return
// — the bytes handed to curl's stdin are outside what this package can scrub.
//
// A credential whose environment variable is unset fails with exit code 5 and
// no document, rather than sending an empty header — an unauthenticated request
// that looks authenticated is the worse outcome (§4).
func BuildConfig(req *request.Request, capture Capture) (config []byte, argv []string, cleanup func(), err error) {
	return BuildConfigWith(req, capture, DefaultOptions())
}

// BuildConfigWith is BuildConfig with the timeouts named rather than defaulted.
func BuildConfigWith(
	req *request.Request,
	capture Capture,
	opts Options,
) (config []byte, argv []string, cleanup func(), err error) {
	// -q first, where curl reads it: it disables the default config file, which
	// curl would otherwise parse *before* the -K document. A `trace-ascii` or
	// `proxy` line in a $HOME/.curlrc anyone can write applies to the one request
	// carrying the resolved credential, and copies it straight back out. Writing
	// that file is a weaker capability than the env-var read §5a concedes, so
	// this flag is what keeps the firewall inside its claimed boundary.
	argv = []string{"curl", "-q", "-K", "-"}
	if req == nil {
		return nil, argv, func() {}, clierr.RequestFailed("no request to execute")
	}

	_, config, cleanup, err = buildDocument(req, capture, opts.withDefaults())

	return config, argv, cleanup, err
}

// buildDocument is BuildConfigWith with the document itself handed back, so a
// caller can reach the buffer the document owns rather than only the copy. The
// tests for the zeroing are that caller: the defect this seam exists to make
// visible is a scrubbed copy sitting beside an unscrubbed original.
func buildDocument(
	req *request.Request,
	capture Capture,
	opts Options,
) (doc *document, config []byte, cleanup func(), err error) {
	doc = &document{}
	if err := doc.build(req, capture, opts); err != nil {
		doc.discard()
		return doc, nil, doc.cleanupWith(nil), err
	}

	config = append([]byte(nil), doc.b...)
	doc.discard()

	return doc, config, doc.cleanupWith(config), nil
}

// document accumulates the directives and the temp files they point at, so a
// failure part-way through still knows what to remove.
//
// b is a []byte the document owns rather than a strings.Builder: the resolved
// credential is written into it, and only a buffer this package can address is
// a buffer it can zero. Builder.Reset drops the array without touching it, and
// Builder.String aliases it into an immutable string that can never be zeroed.
type document struct {
	b     []byte
	files []string
}

func (d *document) build(req *request.Request, capture Capture, opts Options) error {
	url, err := req.URL(resolve)
	if err != nil {
		return err
	}
	d.directive("url", url)

	if req.Method != "" && !request.IsMethod(req.Method) {
		// Quoted where a value never is: a method is a verb, not a credential,
		// and %q renders any CR or LF in it as an escape rather than a real one.
		return clierr.Usage("method %q is not a valid HTTP method", req.Method)
	}

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

	// The bounds curl enforces on itself. Emitted as directives rather than
	// watched from Go because curl exits 28 of its own accord, which runFailure
	// already classifies as a failed request — so the happy path never has to
	// know a clock exists. A `run` without these stalls on operation k of n and
	// emits no report at all.
	d.directive("connect-timeout", seconds(opts.ConnectTimeout))
	d.directive("max-time", seconds(opts.MaxTime))

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

		if err := checkSplit("header", h.Name, value); err != nil {
			return err
		}

		if h.Value.Encoding() == request.EncodeBasic {
			pair, err := basicPair(h.Value, value)
			if err != nil {
				return err
			}
			d.directive("user", pair)
			continue
		}

		d.directive("header", h.Name+": "+value)
	}

	return nil
}

// cookies writes every cookie as one directive. curl keeps only the last of
// several, so they are joined rather than repeated.
//
// The joining happens in d.b, one piece at a time, rather than in a
// strings.Builder handed to directive: a cookie value can be a resolved
// credential, and Builder.String aliases the builder's array into an immutable
// string — a second copy, in the one kind of buffer this package can neither
// address nor zero. Escaping per piece is the same as escaping the join,
// because none of `;`, ` ` or `=` is a character configEscape touches.
func (d *document) cookies(req *request.Request) error {
	if len(req.Cookies) == 0 {
		return nil
	}

	d.write(`cookie = "`)
	for i, c := range req.Cookies {
		value, err := resolve(c.Value)
		if err != nil {
			return err
		}

		if err := checkSplit("cookie", c.Name, value); err != nil {
			return err
		}

		if i > 0 {
			d.write("; ")
		}
		d.write(escapeDirective(c.Name))
		d.write("=")
		d.write(escapeDirective(value))
	}
	d.write("\"\n")

	return nil
}

// body writes the request body, inline when it can be and via a temp file when
// it cannot.
func (d *document) body(req *request.Request) error {
	if req.Body == nil {
		return nil
	}

	ct, err := bodyContentType(req)
	if err != nil {
		return err
	}
	if ct != "" {
		d.directive("header", contentTypeHeader+": "+ct)
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

// tempFile writes data to a 0600 file and records it for cleanup.
//
// cleanup unlinks the file; it does not overwrite the bytes first, so a body
// the user put a credential in is protected by the mode and by its lifetime,
// not by scrubbing the way the in-memory buffers are. Stated rather than fixed
// because overwriting is a behaviour change with its own failure modes on a
// copy-on-write filesystem, and it belongs in a task of its own.
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

// seconds renders a duration the way curl's timeout options read it: decimal
// seconds, with no trailing zeroes, so a sub-second bound survives the trip.
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

// directive writes one `name = "value"` line, escaped for curl's parser.
//
// Written in four pieces rather than as one concatenation because a concatenated
// `name + ` = "` + value + …` is a new Go string holding the resolved
// credential, and a string is exactly what this package cannot zero. What
// escapeDirective returns is the same hazard, and is avoided the same way: its
// replacer hands back the value unchanged when nothing needs escaping, which a
// credential almost never does.
func (d *document) directive(name, value string) {
	d.write(name)
	d.write(` = "`)
	d.write(escapeDirective(value))
	d.write("\"\n")
}

// flag writes one bare directive, the config form of a boolean option.
func (d *document) flag(name string) {
	d.write(name)
	d.write("\n")
}

// write appends s to the document's buffer, growing it first so that append
// never reallocates on its own.
func (d *document) write(s string) {
	d.grow(len(s))
	d.b = append(d.b, s...)
}

// grow makes room for n more bytes, zeroing the array it leaves behind.
//
// This is the half discard() cannot do. append reallocates by allocating,
// copying and dropping the old array — with whatever resolved credential had
// been written into it still there, in memory nothing holds a reference to and
// so nothing can clear. A minimal request reallocates five times, and each
// abandoned array holds the prefix of the document written so far, credential
// included. Zeroing on the way past is the only moment the reference still
// exists.
func (d *document) grow(n int) {
	if cap(d.b)-len(d.b) >= n {
		return
	}

	old := d.b
	size := 2 * cap(old)
	if need := len(old) + n; size < need {
		size = need
	}

	b := make([]byte, len(old), size)
	copy(b, old)
	clear(old[:cap(old)])
	d.b = b
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
