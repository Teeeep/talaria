// Package curl turns a bound request into curl: the displayable command
// `--dry-run` prints and every executed call reports, the config document curl
// actually reads, and the executor that spawns it (DESIGN.md §3.4).
//
// This file is symbolic about credentials. It reads request.Value's Symbolic
// and redacted forms and never calls SecretRef.Resolve, so every header, cookie
// and query value in an emitted command references $TALARIA_AUTH_BEARER rather
// than a token: "runnable in a shell where the env var is set, useless to
// exfiltrate" (§5a). Resolution happens in exactly one place, config.go's
// resolve, and nothing outside this package can reach it.
//
// The body is the exception, because it is raw bytes and never becomes a
// request.Value at all. A body the caller typed is printed as typed — it is
// already in that caller's hands (§3.4) — and one read from a file or stdin is
// referenced instead of inlined; see bodyArgs.
package curl

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/Teeeep/talaria/internal/request"
)

// Render returns the curl command that reproduces req: a portable, symbolic
// reproduction for bug reports, docs and scripts (§3.4).
//
// The URL comes last, the way the design's sketch and most hand-written curl
// put it, so a reader sees what is being called without scanning past the
// flags.
func Render(req *request.Request) string {
	if req == nil {
		return ""
	}

	// -q mirrors the executed command (config.go's argv), so a pasted
	// reproduction behaves like the call talaria made rather than like the
	// reader's ~/.curlrc — which would otherwise apply its directives to a
	// request the reader has just expanded a real credential into.
	args := []string{"curl", "-q", "-s"}
	switch {
	// -I rather than -X HEAD, matching the config document: pasted, -X HEAD waits
	// for a body the server never sends, so the reproduction would hang where the
	// call it reproduces did not.
	case strings.EqualFold(req.Method, http.MethodHead):
		args = append(args, "-I")
	// Naming GET is noise only while curl's default is what actually goes out.
	// A body changes that: --data-raw makes curl send POST unless the method is
	// named, and the config document the real call reads always writes
	// `request = "GET"` (config.go's build). Left out, the printed command would
	// send a different method than the call it reproduces. Anything else is
	// worth seeing, and for a mutation it is the most important token in the
	// line.
	case req.Method != "" && !(strings.EqualFold(req.Method, http.MethodGet) && req.Body == nil):
		args = append(args, "-X", req.Method)
	}

	args = append(args, headerArgs(req)...)
	if cookies := cookieWord(req); cookies != nil {
		args = append(args, "-b", cookies.String())
	}
	args = append(args, bodyArgs(req)...)
	args = append(args, urlWord(req, request.Value.Symbolic).String())

	return strings.Join(args, " ")
}

// URL renders req's URL the way a reader consumes it: percent-encoded like the
// wire form, but with credentials shown as <redacted:env:NAME> rather than
// percent-encoded into illegibility. It is what the `request.url` field of
// call's output carries (§5a: symbolic in emitted curl, redacted in output URL
// fields).
//
// request.Request.URL is the wire-correct counterpart, and is what the executor
// uses once it has resolved the credentials.
func URL(req *request.Request) string {
	if req == nil {
		return ""
	}

	return urlWord(req, request.Value.String).plain()
}

// headerArgs renders the request headers, with basic auth diverted to curl's
// own -u.
func headerArgs(req *request.Request) []string {
	var args []string
	for _, h := range req.Headers {
		// A basic-auth header's text is base64(user:password), which cannot be
		// produced without the credential. -u takes the raw user:password, so
		// the command stays symbolic and still works when it is pasted.
		if h.Value.Encoding() == request.EncodeBasic {
			args = append(args, "-u", (&word{}).credential(h.Value.Ref().Symbolic(), h.Value).String())
			continue
		}

		args = append(args, "-H", (&word{}).literal(h.Name+": ").value(h.Value).String())
	}

	return args
}

// cookieWord renders every cookie into one -b argument. curl keeps only the
// last -b of several, so the cookies are joined rather than repeated.
func cookieWord(req *request.Request) *word {
	if len(req.Cookies) == 0 {
		return nil
	}

	w := &word{}
	for i, c := range req.Cookies {
		if i > 0 {
			w.literal("; ")
		}
		w.literal(c.Name + "=").value(c.Value)
	}

	return w
}

// bodyArgs renders the request body and the content type that describes it.
//
// Only a body the caller typed is printed. A body read from a file or from
// stdin was written by someone other than whoever reads this command — a human,
// a CI job — and §3 principle 0 puts stdout first among the surfaces such a
// value must not reach, so those two are referenced rather than inlined. The
// bytes on the wire are the same either way; this changes only what is shown.
func bodyArgs(req *request.Request) []string {
	if req.Body == nil {
		return nil
	}

	var args []string
	// A media type that would split the request is left out rather than
	// printed. This command is documentation a human pastes into a shell, and a
	// single-quoted word spans a raw newline happily, so printing it would hand
	// the reader the injection the call itself refuses to make — BuildConfig
	// returns the error for the same value and builds no document.
	if ct, err := bodyContentType(req); err == nil && ct != "" {
		args = append(args, "-H", (&word{}).literal(contentTypeHeader+": "+ct).String())
	}

	flag, value := bodyDirective(req.Body)

	return append(args, flag, (&word{}).literal(value).String())
}

// bodyDirective picks the curl option that carries the body and the value it
// takes.
//
// --data-raw for a typed body, matching the config document for the same reason
// (config.go's body): --data and --data-binary read a value starting with @ as
// a filename, so a JSON body beginning with @ would make the emitted command
// read a local file and send it to the API. --data-binary for the two
// references, because that is the option that reads a file verbatim — --data
// strips newlines and carriage returns out of it, which would send different
// bytes than the call sent. TestThePreviewedCommandSendsWhatTheCallSends fails
// on a pretty-printed body file if this is --data.
func bodyDirective(body *request.Body) (flag, value string) {
	if ref, ok := BodyReference(body); ok {
		return "--data-binary", ref
	}

	return "--data-raw", string(body.Data)
}

// BodyReference is how a body the caller did not type is *named* rather than
// shown: `@path` for a file, `@-` for stdin, and ok false for a body given as
// the --body value, which the caller already holds.
//
// It is exported because the reproduction is not the only display surface the
// rule reaches (DESIGN.md §3.4): the envelope's request.body field answers the
// same question one field over, and callPayload asks here rather than branching
// on Origin itself, so the two cannot come to disagree about which spelling a
// referenced body has. fileRef is the whole reason that matters — a file named
// `-` is `@./-` in both places or in neither.
func BodyReference(body *request.Body) (ref string, ok bool) {
	switch body.Origin {
	case request.BodyFile:
		return "@" + fileRef(body.Path), true
	case request.BodyStdin:
		return "@-", true
	default:
		return "", false
	}
}

// fileRef is a path spelled so curl reads it as a file. A bare `-` after the @
// is curl's spelling of stdin, and `--body @-` asked for a file called `-`, so
// that one path is made relative to say which it meant.
func fileRef(path string) string {
	if path == stdinPath {
		return "./" + path
	}

	return path
}

// stdinPath is the filename curl reads as standard input rather than as a file.
const stdinPath = "-"

// contentTypeHeader is the header a body's media type travels in.
const contentTypeHeader = "Content-Type"

// bodyContentType returns the media type the body needs its own Content-Type
// directive for, empty when the request already carries the header explicitly,
// and an error when the media type would end the header line early.
//
// It is one function because both surfaces read the same field: the config
// document curl executes and the command Render prints. The media type is the
// one header value that arrives without passing the binder's checks — it is a
// key of the spec's `content:` map, and `history replay` rebuilds a Request
// with no binder at all.
func bodyContentType(req *request.Request) (string, error) {
	if req.Body == nil || req.Body.ContentType == "" || hasHeader(req, contentTypeHeader) {
		return "", nil
	}

	if err := checkSplit("header", contentTypeHeader, req.Body.ContentType); err != nil {
		return "", err
	}

	return req.Body.ContentType, nil
}

func hasHeader(req *request.Request, name string) bool {
	for _, h := range req.Headers {
		if strings.EqualFold(h.Name, name) {
			return true
		}
	}

	return false
}

// urlWord builds the request URL as one shell word. render chooses how a
// credential in the query string appears — Symbolic for the emitted command,
// String for the redacted display form.
//
// Literal values are percent-encoded here rather than by request.QueryString
// because a percent-encoded $NAME would no longer be a reference the shell can
// expand. Encoding per value, instead of over the finished string, is what lets
// the two live in one URL.
func urlWord(req *request.Request, render func(request.Value) string) *word {
	w := (&word{}).literal(req.BaseURL + req.Path)

	for i, q := range req.Query {
		separator := "&"
		if i == 0 {
			separator = "?"
		}
		w.literal(separator + url.QueryEscape(q.Name) + "=")

		// Sensitive rather than secret: a literal hidden by name has no value to
		// render here either, and percent-encoding its placeholder would print
		// %3Credacted%3E where a reader expects <redacted>. Skipping the encode
		// is safe because render is Symbolic or String — display forms, never a
		// resolving renderer.
		if q.Value.IsSensitive() {
			w.credential(render(q.Value), q.Value)
			continue
		}
		w.literal(url.QueryEscape(q.Value.String()))
	}

	return w
}

// shellName matches an environment variable name a shell will expand from a
// bare $NAME. A ref whose name is not one of these is rendered redacted
// instead: an emitted command that stops being copy-pasteable is a better
// outcome than one that expands into something unintended.
var shellName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// part is one piece of a shell word. expand marks the pieces the shell is
// meant to act on — the credential references, and nothing else.
type part struct {
	text   string
	expand bool
}

// word is one argument of the rendered command, assembled from literal text and
// credential references.
//
// The quoting rule follows from what each word contains. A word of pure literal
// text is single-quoted, which needs no escaping beyond the quote itself and
// leaves every $ and backtick inert. A word carrying a reference has to be
// double-quoted so the shell expands it, so its literal parts are escaped
// against the four characters double quotes still interpret.
type word struct{ parts []part }

func (w *word) literal(s string) *word {
	if s != "" {
		w.parts = append(w.parts, part{text: s})
	}

	return w
}

// value appends a request value: the reference symbolically, or the literal
// text for a literal value.
func (w *word) value(v request.Value) *word {
	if v.IsSecret() {
		return w.credential(v.Symbolic(), v)
	}

	return w.literal(v.String())
}

// credential appends a rendered credential, marking it for expansion only when
// the shell can actually expand it back into the value. A basic-auth value, a
// redacted rendering, or a ref from a source that is not the environment all
// render as inert text.
func (w *word) credential(rendered string, v request.Value) *word {
	if rendered == v.String() || !shellName.MatchString(v.Ref().Name) {
		return w.literal(rendered)
	}

	w.parts = append(w.parts, part{text: rendered, expand: true})

	return w
}

// plain returns the word's text with no quoting, for the display fields that
// are read rather than run.
func (w *word) plain() string {
	var b strings.Builder
	for _, p := range w.parts {
		b.WriteString(p.text)
	}

	return b.String()
}

// String returns the word quoted for a shell.
func (w *word) String() string {
	expands := false
	for _, p := range w.parts {
		if p.expand {
			expands = true
			break
		}
	}

	if !expands {
		return "'" + strings.ReplaceAll(w.plain(), "'", `'\''`) + "'"
	}

	var b strings.Builder
	b.WriteByte('"')
	for _, p := range w.parts {
		if p.expand {
			b.WriteString(p.text)
			continue
		}
		b.WriteString(escapeInDoubleQuotes(p.text))
	}
	b.WriteByte('"')

	return b.String()
}

// doubleQuoted escapes the characters a shell still interprets inside double
// quotes: command substitution, variable expansion, the quote itself and the
// escape.
var doubleQuoted = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	"`", "\\`",
	`$`, `\$`,
)

func escapeInDoubleQuotes(s string) string { return doubleQuoted.Replace(s) }
