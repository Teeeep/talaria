// Package request holds the HTTP request talaria is about to make. It is the
// type DESIGN.md §5a says the credential firewall shapes: every field that
// could carry a credential holds a Value, and a Value is either a literal
// string or a secret.SecretRef — never a resolved credential.
//
// Everything downstream reads this type: symbolic curl rendering, the curl
// config document, JSON output, history and the corpus. If it could hold a bare
// secret string, each of those would become a leak channel, so it cannot.
// Reading the actual credential is one call to Value.Resolve, made by
// internal/curl at exec time and nowhere else.
package request

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/Teeeep/talaria/internal/secret"
)

// Encoding says how a Value's credential becomes the text of the field it sits
// in. It exists because a bearer token is not sent verbatim — the header reads
// `Bearer <token>` — and the prefix has to survive into rendering without the
// renderer having to know which scheme produced the value.
type Encoding string

const (
	// EncodeRaw sends the value as it stands. Literals and API keys use it.
	EncodeRaw Encoding = ""
	// EncodeBearer sends `Bearer ` followed by the value (RFC 6750).
	EncodeBearer Encoding = "bearer"
	// EncodeBasic sends `Basic ` followed by base64(user:password). The base64
	// needs the value, so only a resolving renderer can produce the header text;
	// internal/curl renders it as curl's own -u instead, which keeps it symbolic.
	EncodeBasic Encoding = "basic"
)

// Value is one header, query or cookie value: either a literal or a reference
// to a credential, never both and never a resolved secret. It is a struct
// rather than an interface so it marshals, compares and prints as one thing.
//
// The zero Value is an empty literal.
type Value struct {
	literal string
	ref     secret.SecretRef
	enc     Encoding
	// sensitive hides a literal from every display form. A referenced
	// credential needs no such flag: it has no value to hide.
	sensitive bool
}

// Literal returns a Value holding text the user supplied directly.
func Literal(s string) Value { return Value{literal: s} }

// Secret returns a Value naming a credential, encoded per enc.
func Secret(ref secret.SecretRef, enc Encoding) Value { return Value{ref: ref, enc: enc} }

// Sensitive returns a copy of v that displays as `<redacted>` everywhere a
// Value is displayed, while still going on the wire unchanged.
//
// It is what a literal sitting under a credential-shaped name becomes: a token
// the user typed into `--header Authorization: …` is as sensitive as one
// talaria resolved itself (§5a's built-in list, which "applies to pretty output
// too"). Marking the Value rather than scrubbing each renderer is the point —
// §5a's architecture consequence is that a surface has to *ask* for a secret to
// leak one, and there is no way to ask a Value for a hidden literal except
// Reveal.
func (v Value) Sensitive() Value {
	v.sensitive = true

	return v
}

// IsSecret reports whether this value names a credential.
func (v Value) IsSecret() bool { return !v.ref.IsZero() }

// IsSensitive reports whether this value is hidden from display: every secret
// is, and so is a literal marked by Sensitive.
func (v Value) IsSensitive() bool { return v.sensitive || v.IsSecret() }

// Ref returns the credential this value names, or the zero SecretRef for a
// literal. The ref is safe to print: it holds a name, not a value.
func (v Value) Ref() secret.SecretRef { return v.ref }

// Encoding returns how this value becomes field text, so internal/curl can pick
// the right directive without re-deriving it from the scheme.
func (v Value) Encoding() Encoding { return v.enc }

// String is the display form: the literal as-is, or the encoding prefix
// followed by `<redacted:env:NAME>`. It is what JSON output, pretty output and
// history show, and it is safe under every printf verb because SecretRef is.
func (v Value) String() string {
	if !v.IsSecret() {
		if v.sensitive {
			return secret.Placeholder
		}

		return v.literal
	}

	return v.Prefix() + v.ref.String()
}

// Reveal returns the literal text this value carries, hidden or not. It exists
// for the one renderer that builds curl's config document: a sensitive literal
// still has to go on the wire, and String no longer returns it.
//
// For a value naming a credential it returns the redacted display form.
// Reading a real credential is SecretRef.Resolve, in internal/curl at exec
// time, and this is not that.
func (v Value) Reveal() string {
	if v.IsSecret() {
		return v.String()
	}

	return v.literal
}

// GoString keeps %#v — the verb reached for when debugging, i.e. exactly when a
// leak would be least expected — as safe as %v.
func (v Value) GoString() string { return v.String() }

// MarshalJSON renders the display form, so any JSON carrying a credential
// position carries the redacted name instead of a value.
func (v Value) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

// Symbolic is the form for emitted and dry-run curl: `Bearer $TALARIA_AUTH_BEARER`,
// runnable wherever the variable is set and useless to exfiltrate (§5a).
//
// EncodeBasic has no symbolic header form — the value must be base64-encoded
// before it goes in the header — so it falls back to the redacted form. The
// emitted command stays correct because internal/curl renders basic auth with
// curl's -u, which takes the raw `user:password` and can stay symbolic.
func (v Value) Symbolic() string {
	if !v.IsSecret() || v.enc == EncodeBasic {
		return v.String()
	}

	return v.Prefix() + v.ref.Symbolic()
}

// Prefix is the literal text that precedes the credential in the field, e.g.
// "Bearer " for a token. It is exported because internal/curl builds the
// resolved field text at exec time and would otherwise have to re-derive the
// prefix from the scheme — the duplication §5a's single-crossing rule exists to
// avoid.
func (v Value) Prefix() string {
	switch v.enc {
	case EncodeBearer:
		return "Bearer "
	case EncodeBasic:
		return "Basic "
	default:
		return ""
	}
}

// Render turns a Value into the text that goes on the wire, in a URL, or on
// screen. Making it a parameter rather than a method is what keeps resolution
// out of this package: Redacted and Symbolic live here, and the one renderer
// that reads credentials is internal/curl's, at exec time.
type Render func(Value) (string, error)

// Redacted renders credentials as `<redacted:env:NAME>`. It is the default for
// every surface a user or agent reads.
func Redacted(v Value) (string, error) { return v.String(), nil }

// Symbolic renders credentials as `$NAME`, for copy-pasteable curl.
func Symbolic(v Value) (string, error) { return v.Symbolic(), nil }

// Pair is one named value on the request: a header, a query parameter or a
// cookie. Pairs are ordered slices rather than maps because repetition is legal
// for all three and because a stable order makes emitted curl reproducible.
type Pair struct {
	Name  string `json:"name"`
	Value Value  `json:"value"`
}

// Body is the request body: the bytes --body resolved to, and the media type
// they will be sent as. Data is already the final bytes — the Go process reads
// a file or stdin before curl exists, so nothing downstream has to open
// anything (DESIGN.md §5a).
type Body struct {
	ContentType string
	Data        []byte
}

// MarshalJSON renders the body as text alongside its content type.
func (b Body) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ContentType string `json:"content_type"`
		Data        string `json:"data"`
	}{b.ContentType, string(b.Data)})
}

// IsHTTPScheme reports whether scheme is one talaria will make a request with.
//
// curl also speaks file, gopher, dict, smb and a dozen others, so an unchecked
// scheme turns "call this operation" into a local file read or a raw write to
// an arbitrary TCP port. Every URL talaria assembles comes from untrusted input
// — a spec's servers[0].url, a profile, a stored history entry — so the check
// belongs at each of those boundaries, not only on the flag a user typed.
//
// Schemes are case-insensitive per RFC 3986 §3.1, so HTTPS:// is accepted.
func IsHTTPScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https":
		return true
	}

	return false
}

// Userinfo reports whether raw carries a `user:password@` before its host, and
// returns the host it precedes so a refusal can name where the URL pointed
// without quoting what it carried.
//
// Userinfo is the one credential position a URL has no symbolic form for: a
// base URL is copied whole into request.url, into the emitted curl and into the
// history store, all of which are read back later, so a credential written
// there is a credential in cleartext forever (§5a). Every source is untrusted —
// a flag, a profile, a spec's servers[0].url, a stored entry — so the check
// belongs at each of those boundaries.
//
// It reads the text rather than url.Parse's User field so that a URL too
// malformed to parse is covered too: the parse error quotes the whole string,
// and a check that ran after it would have nothing left to protect. Only a URL
// with an authority can have userinfo, hence the `://` requirement; anything
// else is left to IsHTTPScheme to refuse.
func Userinfo(raw string) (host string, present bool) {
	_, authority, ok := strings.Cut(raw, "://")
	if !ok {
		return "", false
	}
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}

	// Last, not first: a userinfo may itself contain an escaped separator, and
	// the host is always what follows the final one.
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return "", false
	}

	return authority[at+1:], true
}

// Request is a fully bound HTTP request, still symbolic about credentials.
//
// BaseURL and Path are kept apart from Query because the query string cannot be
// materialised without deciding how to render secrets, and that decision belongs
// to the caller: see URL.
type Request struct {
	// OperationID names the operation this came from, for output and history.
	OperationID string `json:"operation_id"`
	Method      string `json:"method"`
	// BaseURL is the scheme://host[/prefix] the call goes to, without a trailing
	// slash.
	BaseURL string `json:"base_url"`
	// Path is the operation's path with its path parameters substituted and
	// percent-encoded, e.g. /pets/42.
	Path    string `json:"path"`
	Query   []Pair `json:"query,omitempty"`
	Headers []Pair `json:"headers,omitempty"`
	Cookies []Pair `json:"cookies,omitempty"`
	Body    *Body  `json:"body,omitempty"`
	// Withheld names the credentials this request will *not* carry because
	// BaseURL's host is outside the allowed set. The request is still runnable:
	// §5a withholds rather than refuses, because pointing --base-url at a local
	// twin is the common case and must not need a flag.
	Withheld []Withheld `json:"credentials_withheld,omitempty"`
}

// Withheld is one credential the host-binding rule kept off a request
// (DESIGN.md §5a). It names the scheme and the host so an agent can act on the
// omission rather than infer it from a downstream 401.
type Withheld struct {
	Scheme string `json:"scheme"`
	Reason string `json:"reason"`
	Host   string `json:"host"`
}

// URL builds the full request URL, rendering any credential in the query string
// with render. Pass Redacted for anything a user reads, Symbolic for emitted
// curl; internal/curl passes a resolving renderer at exec time and nothing else
// does.
func (r Request) URL(render Render) (string, error) {
	query, err := r.QueryString(render)
	if err != nil {
		return "", err
	}

	full := r.BaseURL + r.Path
	if query == "" {
		return full, nil
	}

	return full + "?" + query, nil
}

// QueryString renders the query parameters in the order they were bound.
// url.QueryEscape does the encoding so it is not hand-rolled, and the order is
// preserved rather than sorted so emitted curl matches what was asked for.
func (r Request) QueryString(render Render) (string, error) {
	var b strings.Builder
	for _, p := range r.Query {
		value, err := render(p.Value)
		if err != nil {
			return "", err
		}

		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p.Name))
		b.WriteByte('=')

		// A placeholder is text to read, not bytes to send, so encoding it
		// would only turn <redacted> into %3Credacted%3E. The test is what was
		// rendered, not whether the value is sensitive: this function is shared
		// with the wire path (internal/curl resolves through it), and skipping
		// the escape for a resolved credential would put unescaped bytes in the
		// request URL. Same comparison word.credential makes.
		if p.Value.IsSensitive() && (value == p.Value.String() || value == p.Value.Symbolic()) {
			b.WriteString(value)
			continue
		}
		b.WriteString(url.QueryEscape(value))
	}

	return b.String(), nil
}
