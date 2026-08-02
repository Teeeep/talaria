package request

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/secret"
	"github.com/Teeeep/talaria/internal/spec"
)

// Parameter locations. Three of them are also credential locations and are
// aliased from internal/config so the two packages cannot drift; `path` is the
// one place a credential never goes, which is why config does not name it.
const (
	inPath   = "path"
	inQuery  = config.InQuery
	inHeader = config.InHeader
	inCookie = config.InCookie
)

// Inputs is everything one call needs: the operation and the document it came
// from, the resolved profile and credentials, and the raw `name=value` flag
// strings exactly as the CLI received them. Parsing them here rather than in
// cmd/ keeps the binding rules — and their error messages — testable without a
// command tree.
type Inputs struct {
	Op      operation.Operation
	Doc     *spec.Document
	Profile *config.Profile
	// Creds are the credentials config.Resolve produced for Op. They name
	// credentials; they never carry one.
	Creds []config.Credential
	// BaseURL is --base-url. It beats the profile, which beats the spec.
	BaseURL string
	// Params are --param name=value for parameters the operation declares, in
	// any location.
	Params []string
	// Query are --query name=value, for query parameters the spec may not
	// declare.
	Query []string
	// Headers are --header name=value.
	Headers []string
	// Body holds --body exactly as given: a literal, @file, or "-" for stdin.
	// It is a slice so a repeated flag is a reportable mistake rather than a
	// silent last-one-wins.
	Body []string
	// Stdin is the reader `--body -` consumes. It is injected rather than read
	// from os.Stdin so the ownership rule is testable: the Go process reads the
	// body in full before curl exists, and curl's own stdin carries the config
	// document (DESIGN.md §5a).
	Stdin io.Reader
}

// Build binds inputs to an operation and returns the request to make.
//
// Every binding problem is collected before returning rather than reported one
// at a time: an agent that gets "petId is required; --header malformed" fixes
// both in one round trip, where a fail-fast builder would cost it two.
func Build(in Inputs) (*Request, error) {
	b := &binder{in: in}

	req := &Request{
		OperationID: in.Op.ID,
		Method:      in.Op.Method,
		BaseURL:     b.baseURL(),
	}

	bound := b.params()
	req.Path = b.path(bound)

	// Query goes through hide for the same reason headers and cookies do: §5a
	// names query-string API keys (`?api_key=`) as a credential location, and a
	// value's origin — spec, profile, --param or --query — does not change what
	// the name says it is.
	req.Query = hide(append(b.located(bound, inQuery), b.pairs(in.Query, "--query", nil)...))
	req.Headers = hide(b.headers(bound))
	req.Cookies = hide(b.located(bound, inCookie))
	// After the headers, because the body's content type defers to a
	// Content-Type the user set; before the credentials, which never set one.
	req.Body = b.body(req)

	b.credentials(req)

	if err := b.err(); err != nil {
		return nil, err
	}

	return req, nil
}

// binder accumulates the problems found while binding so they can all be
// reported together.
type binder struct {
	in       Inputs
	problems []string
	// unknown records that some --param named a parameter the operation does
	// not declare, so the error can carry the declared names as alternatives.
	unknown bool
}

func (b *binder) fail(format string, a ...any) {
	b.problems = append(b.problems, clierr.Usage(format, a...).Message)
}

// err folds the collected problems into one usage error, naming the operation
// so the message stands alone in a log.
func (b *binder) err() error {
	if len(b.problems) == 0 {
		return nil
	}

	err := clierr.Usage("cannot build a request for %s: %s",
		operationName(b.in.Op), strings.Join(b.problems, "; "))
	if b.unknown {
		err = err.WithAlternatives(declaredNames(b.in.Op)...)
	}

	return err
}

// baseURL resolves where the request goes: --base-url, then the profile, then
// the spec's first server (DESIGN.md §4). A spec whose server URL is relative
// — common for specs that expect a host to be supplied — counts as no server.
func (b *binder) baseURL() string {
	candidates := []struct{ source, raw string }{
		{"--base-url", b.in.BaseURL},
	}
	if b.in.Profile != nil {
		candidates = append(candidates, struct{ source, raw string }{
			"profile " + b.in.Profile.Name, b.in.Profile.BaseURL})
	}
	candidates = append(candidates, struct{ source, raw string }{"the spec's servers[0].url", firstServer(b.in.Doc)})

	for _, c := range candidates {
		if c.raw == "" {
			continue
		}

		// Before the parse, so a URL that fails to parse cannot have its
		// userinfo quoted back by the message below.
		if host, ok := Userinfo(c.raw); ok {
			b.fail("base URL from %s carries a credential in its userinfo (user:password@%s); "+
				"remove it and set %s=user:password instead, which keeps the value out of the "+
				"request, the emitted curl and the history", c.source, host, config.EnvBasic)
			return ""
		}

		parsed, err := url.Parse(c.raw)
		if err != nil || parsed.Host == "" || !IsHTTPScheme(parsed.Scheme) {
			b.fail("base URL %q from %s is not an absolute http(s) URL", c.raw, c.source)
			return ""
		}

		return strings.TrimSuffix(c.raw, "/")
	}

	b.fail("no base URL: the spec declares no server, so pass --base-url or set one in a profile")

	return ""
}

// firstServer returns the document's first server URL, or "" when it has none.
func firstServer(doc *spec.Document) string {
	if doc == nil || doc.Model == nil || len(doc.Model.Servers) == 0 || doc.Model.Servers[0] == nil {
		return ""
	}

	return doc.Model.Servers[0].URL
}

// params parses --param flags against the parameters the operation declares,
// keyed by the declared name so path substitution and location routing both
// read from one place.
func (b *binder) params() map[string]string {
	declared := map[string]operation.Param{}
	for _, p := range b.in.Op.Params {
		declared[p.Name] = p
	}

	bound := map[string]string{}
	for i, raw := range b.in.Params {
		name, value, ok := strings.Cut(raw, "=")
		if !ok || name == "" {
			// Reported like every other rejected name=value flag: by position,
			// with the value elided. A --param can be a query-string API key.
			b.fail("--param %d is not name=value%s", i+1, elided(raw))
			continue
		}

		if _, ok := declared[name]; !ok {
			b.unknown = true
			b.fail("%s declares no parameter %q", operationName(b.in.Op), name)
			continue
		}

		bound[name] = value
	}

	// Reported after parsing so a missing parameter and a misspelt one surface
	// together rather than one run apart.
	for _, p := range b.in.Op.Params {
		if _, ok := bound[p.Name]; p.Required && !ok {
			b.fail("--param %s is required (%s parameter)", p.Name, p.In)
		}
	}

	return bound
}

// path substitutes the bound path parameters into the operation's template.
//
// Values are escaped with url.PathEscape, so a value containing / or ? stays
// inside its own segment instead of rewriting the request's target — the same
// reason SQL uses placeholders rather than string concatenation.
func (b *binder) path(bound map[string]string) string {
	path := b.in.Op.Path
	for _, p := range b.in.Op.Params {
		if p.In != inPath {
			continue
		}

		value, ok := bound[p.Name]
		if !ok {
			continue
		}

		path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(value))
	}

	// A leftover placeholder means the spec templated a segment it never
	// declared a parameter for; without this the request would go to a literal
	// "/pets/{petId}".
	if strings.ContainsAny(path, "{}") {
		b.fail("path %q has a placeholder the operation declares no parameter for", b.in.Op.Path)
	}

	return path
}

// located returns the bound parameters that belong in one location, in the
// order the spec declares them so the result is stable across runs.
func (b *binder) located(bound map[string]string, in string) []Pair {
	var out []Pair
	for _, p := range b.in.Op.Params {
		if p.In != in {
			continue
		}
		if value, ok := bound[p.Name]; ok {
			out = append(out, Pair{Name: p.Name, Value: Literal(value)})
		}
	}

	return out
}

// headers merges the three sources of request headers. The profile is the least
// specific and only contributes names nothing else supplied, so `--header
// X-Env=explicit` overrides a profile's X-Env instead of sending both.
func (b *binder) headers(bound map[string]string) []Pair {
	out := append(b.located(bound, inHeader), b.pairs(b.in.Headers, "--header", httpFieldName)...)

	if b.in.Profile == nil {
		return out
	}

	set := map[string]bool{}
	for _, p := range out {
		set[strings.ToLower(p.Name)] = true
	}

	names := make([]string, 0, len(b.in.Profile.Headers))
	for name := range b.in.Profile.Headers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if !set[strings.ToLower(name)] {
			out = append(out, Pair{Name: name, Value: Literal(b.in.Profile.Headers[name])})
		}
	}

	return out
}

// hide marks every literal sitting under a credential-shaped name — the §5a
// built-in list: Authorization, Cookie, Proxy-Authorization, *api*key*, *token*,
// *secret* — as sensitive, so it renders `<redacted>` on every surface that
// displays a Value while still going on the wire.
//
// The name is all there is to go on. talaria cannot tell a token the user typed
// into --header from an ordinary string by looking at the string, and §5a's
// answer is that it does not have to: the name decides, whether the value came
// from a spec's security scheme, a profile, a bound parameter or the flag.
func hide(pairs []Pair) []Pair {
	// A nil *Redactor is the built-in list, which is the whole point here: the
	// user-extensible patterns are a display concern the config file adds
	// downstream, and this floor is not configurable.
	var builtin *secret.Redactor

	for i, p := range pairs {
		if !p.Value.IsSecret() && builtin.IsSensitive(p.Name) {
			pairs[i].Value = p.Value.Sensitive()
		}
	}

	return pairs
}

// pairs parses repeatable name=value flags. Only the first = separates, because
// header and query values legitimately contain more.
//
// A rejected argument is reported by position and never by content. The shell
// expanded it before talaria saw it, so `--header "Authorization: Bearer
// $TOKEN"` — the form a user reaches for — holds the real token, and quoting it
// back would publish the credential in the exit-2 message on stderr. §5a puts
// error paths inside the firewall, not outside it.
//
// rule, when non-nil, is the extra constraint this flag holds its names to.
func (b *binder) pairs(raws []string, flag string, rule *nameRule) []Pair {
	out := make([]Pair, 0, len(raws))
	for i, raw := range raws {
		name, value, ok := strings.Cut(raw, "=")
		if !ok || name == "" {
			b.fail("%s %d is not name=value%s", flag, i+1, elided(raw))
			continue
		}

		if rule != nil && !rule.ok(name) {
			b.fail("%s %d %s%s", flag, i+1, rule.want, elided(name))
			continue
		}

		out = append(out, Pair{Name: name, Value: Literal(value)})
	}

	return out
}

// nameRule constrains what the name half of a name=value flag may be. Only
// --header has one: a header name goes on the wire as an HTTP field name, while
// a query parameter name is an ordinary string an API is free to spell
// `filter[status]`.
type nameRule struct {
	// ok reports whether the name is usable.
	ok func(string) bool
	// want completes "--header 2 …" when it is not.
	want string
}

// httpFieldName holds --header names to what a header name can actually be.
var httpFieldName = &nameRule{ok: isFieldName, want: "is not a valid HTTP header name"}

// fieldNameChars is the token character set an HTTP field name is drawn from
// (RFC 9110 §5.1, via §5.6.2).
const fieldNameChars = "!#$%&'*+-.^_`|~" +
	"0123456789" +
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"abcdefghijklmnopqrstuvwxyz"

// isFieldName reports whether name can be sent as a header name.
//
// The character that fails this in practice is the colon, from curl's `Name:
// value` form: `--header 'X-Trace: abc=1'` parses as the name "X-Trace: abc",
// which curl's config document renders as the malformed wire header
// `X-Trace: abc: 1`. Refusing it is the difference between a request the user
// did not write and a clean exit 2.
func isFieldName(name string) bool {
	for _, r := range name {
		if !strings.ContainsRune(fieldNameChars, r) {
			return false
		}
	}

	return name != ""
}

// elided describes a rejected argument without echoing what may be a
// credential: the text before its first `=` or `:`, which is a name, and
// nothing at all when it holds neither separator — an argument with no
// separator in it is indistinguishable from a bare token. Not even a truncated
// prefix of the value: part of a credential is still part of a credential.
func elided(raw string) string {
	if cut := strings.IndexAny(raw, "=:"); cut > 0 {
		return fmt.Sprintf(" (it starts %q; the rest is not echoed)", raw[:cut])
	}

	return " (its text is not echoed)"
}

// credentials puts each resolved credential where its scheme says it goes, as a
// reference. This is the §5a boundary: what lands on the request is the name of
// a credential and how to encode it, never the credential.
func (b *binder) credentials(req *Request) {
	for _, cred := range b.in.Creds {
		pair := Pair{Name: cred.Name, Value: Secret(cred.Ref, encodingFor(cred.Kind))}

		switch cred.In {
		case inHeader:
			req.Headers = append(req.Headers, pair)
		case inQuery:
			req.Query = append(req.Query, pair)
		case inCookie:
			req.Cookies = append(req.Cookies, pair)
		default:
			b.fail("scheme %q wants its credential in %q, which is not a place a request has",
				cred.Scheme, cred.In)
		}
	}
}

func encodingFor(kind config.Kind) Encoding {
	switch kind {
	case config.KindBearer:
		return EncodeBearer
	case config.KindBasic:
		return EncodeBasic
	default:
		return EncodeRaw
	}
}

// declaredNames lists the parameters the operation accepts, for the
// valid_alternatives an agent corrects itself from.
func declaredNames(op operation.Operation) []string {
	names := make([]string, 0, len(op.Params))
	for _, p := range op.Params {
		names = append(names, p.Name)
	}

	return names
}

// operationName is the operation's ID, falling back to method and path for a
// spec that sets no operationId.
func operationName(op operation.Operation) string {
	if op.ID != "" {
		return op.ID
	}

	return op.Method + " " + op.Path
}
