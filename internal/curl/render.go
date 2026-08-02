// Package curl turns a bound request into curl: the displayable command
// `--dry-run` prints and every executed call reports, the config document curl
// actually reads, and the executor that spawns it (DESIGN.md §3.4).
//
// This file is symbolic. It reads request.Value's Symbolic and redacted forms
// and never calls SecretRef.Resolve, so an emitted command references
// $TALARIA_AUTH_BEARER rather than a token: "runnable in a shell where the env
// var is set, useless to exfiltrate" (§5a). Resolution happens in exactly one
// place, config.go's resolve, and nothing outside this package can reach it.
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

	args := []string{"curl", "-s"}
	switch {
	// -I rather than -X HEAD, matching the config document: pasted, -X HEAD waits
	// for a body the server never sends, so the reproduction would hang where the
	// call it reproduces did not.
	case strings.EqualFold(req.Method, http.MethodHead):
		args = append(args, "-I")
	// GET is curl's default and naming it adds noise; anything else is worth
	// seeing, and for a mutation it is the most important token in the line.
	case req.Method != "" && !strings.EqualFold(req.Method, http.MethodGet):
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
// --data-raw rather than --data or --data-binary, matching the config document
// for the same reason (config.go's body): those two read a value starting with
// @ as a filename, so the emitted command would read a local file and send it
// to the API where the call sent the text. --data also strips newlines, which
// changes the bytes a signed or whitespace-sensitive payload carries.
func bodyArgs(req *request.Request) []string {
	if req.Body == nil {
		return nil
	}

	var args []string
	if req.Body.ContentType != "" && !hasHeader(req, "Content-Type") {
		args = append(args, "-H", (&word{}).literal("Content-Type: "+req.Body.ContentType).String())
	}

	return append(args, "--data-raw", (&word{}).literal(string(req.Body.Data)).String())
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
