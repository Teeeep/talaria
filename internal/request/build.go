package request

import (
	"context"
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

// Parameter locations, aliased from the package that owns the `In` field they
// describe so the spellings here cannot drift from the ones that produced them.
const (
	inPath   = operation.InPath
	inQuery  = operation.InQuery
	inHeader = operation.InHeader
	inCookie = operation.InCookie
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
	// Hosts is the set of hosts a resolved credential may be sent to (§5a).
	// It is default-deny: the zero HostSet allows nothing, so a caller that
	// forgets to build one withholds every credential rather than delivering
	// one to a host nobody vouched for.
	Hosts HostSet
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
	// Redactor is the config file's user-extensible display patterns
	// (`redact.headers`), layered over the built-in credential names §5a fixes
	// as the non-configurable floor. Nil means that floor and nothing more.
	Redactor *secret.Redactor
	// Stdin is the reader `--body -` consumes. It is injected rather than read
	// from os.Stdin so the ownership rule is testable: the Go process reads the
	// body in full before curl exists, and curl's own stdin carries the config
	// document (DESIGN.md §5a).
	Stdin io.Reader
	// Ctx bounds the one part of binding that waits on something outside this
	// process: the read of Stdin. It lives beside the reader rather than
	// arriving as a first argument because it is that read's lifetime and
	// nothing else's — Build itself computes, and a caller with no stdin has
	// nothing to cancel. Nil means context.Background(); the read is then
	// uninterruptible, which is why cmd/talaria passes cmd.Context().
	Ctx context.Context
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
		Method:      b.method(in.Op.Method),
		BaseURL:     b.baseURL(),
	}

	bound := b.params()
	req.Path = b.path(bound)

	// Query goes through hide for the same reason headers and cookies do: §5a
	// names query-string API keys (`?api_key=`) as a credential location, and a
	// value's origin — spec, profile, --param or --query — does not change what
	// the name says it is.
	req.Query = hide(in.Redactor, append(b.located(bound, inQuery), b.pairs(in.Query, "--query", nil)...))
	req.Headers = hide(in.Redactor, b.headers(bound))
	req.Cookies = hide(in.Redactor, b.located(bound, inCookie))
	// After the headers, because the body's content type defers to a
	// Content-Type the user set; before the credentials, which never set one.
	req.Body = b.body(req)

	b.credentials(req)

	if b.fatal != nil {
		return nil, b.fatal
	}
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
	// fatal is a failure that ends the binding rather than being collected with
	// the others; see stop.
	fatal error
	// unknown records that some --param named a parameter the operation does
	// not declare, so the error can carry the declared names as alternatives.
	unknown bool
}

// ctx is Inputs.Ctx with the nil case filled in. A background context's Done
// channel is nil and blocks forever, so an Inputs built without one waits on the
// read exactly as it did before the context existed.
func (b *binder) ctx() context.Context {
	if b.in.Ctx == nil {
		return context.Background()
	}

	return b.in.Ctx
}

func (b *binder) fail(format string, a ...any) {
	b.problems = append(b.problems, clierr.Usage(format, a...).Message)
}

// stop records a failure that is not the caller's invocation and so cannot join
// the usage problems: binding was interrupted, and there is nothing to correct.
// It carries its own exit code and beats whatever else was collected — a
// half-bound request produces missing-parameter complaints that are artefacts of
// the interruption, and reporting those as usage errors would tell an agent to
// fix a command line that was fine.
func (b *binder) stop(format string, a ...any) {
	if b.fatal == nil {
		b.fatal = clierr.RequestFailed(format, a...)
	}
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
//
// The template itself is held to isPathTemplate first. A path is joined to the
// base URL as text, so one that is not absolute moves the URL's authority and
// takes the credential with it; a spec shaped like that is broken rather than
// unauthorised, which is why it is exit 2 here as well as a withholding in
// credentials.
func (b *binder) path(bound map[string]string) string {
	if !isPathTemplate(b.in.Op.Path) {
		b.fail("%s", badPathTemplate(b.in.Op))

		return ""
	}

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

// method holds the operation's method to the HTTP token charset before it can
// become curl's `request` directive.
//
// The request line is `METHOD SP target SP HTTP/1.1`, so a method carrying a
// space rewrites the target and one carrying a CRLF appends a request of the
// caller's choosing. The method comes from the spec, which is untrusted input
// like every other source here.
//
// An empty method is left alone: it is the absence of a method rather than a
// bad one, and internal/curl already reads it as "let curl default to GET".
func (b *binder) method(method string) string {
	if method == "" || isFieldName(method) {
		return method
	}

	// Quoted, unlike a rejected value: a method is a verb, never a credential,
	// and %q renders any CR or LF in it as an escape rather than a real one.
	b.fail("method %q is not a valid HTTP method", method)

	return ""
}

// located returns the bound parameters that belong in one location, in the
// order the spec declares them so the result is stable across runs.
func (b *binder) located(bound map[string]string, in string) []Pair {
	var out []Pair
	for _, p := range b.in.Op.Params {
		if p.In != in {
			continue
		}

		value, ok := bound[p.Name]
		if !ok {
			continue
		}

		// Only header names are field names. A query or cookie parameter is an
		// ordinary string an API is free to spell `filter[status]`.
		if in == inHeader && !isFieldName(p.Name) {
			b.fail("%s declares header parameter %q, which is not a valid HTTP header name",
				operationName(b.in.Op), p.Name)
			continue
		}

		if SplitsRequest(p.Name, value) {
			b.fail("--param %s %s (%s parameter)", p.Name, crlfProblem, p.In)
			continue
		}

		out = append(out, Pair{Name: p.Name, Value: Literal(value)})
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
		if set[strings.ToLower(name)] {
			continue
		}

		value := b.in.Profile.Headers[name]
		if !isFieldName(name) {
			b.fail("profile %s sets header %q, which is not a valid HTTP header name", b.in.Profile.Name, name)
			continue
		}
		if SplitsRequest(name, value) {
			b.fail("profile %s header %s %s", b.in.Profile.Name, name, crlfProblem)
			continue
		}

		out = append(out, Pair{Name: name, Value: Literal(value)})
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
//
// red is the config file's extension of that list. A nil one is the built-in
// floor alone, which is what it has to be: redaction is never opt-in, so a
// caller that supplies no patterns gets the same protection as one that does.
func hide(red *secret.Redactor, pairs []Pair) []Pair {
	for i, p := range pairs {
		if !p.Value.IsSecret() && red.IsSensitive(p.Name) {
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

		if SplitsRequest(name, value) {
			b.fail("%s %d %s%s", flag, i+1, crlfProblem, elided(raw))
			continue
		}

		out = append(out, Pair{Name: name, Value: Literal(value)})
	}

	return out
}

// WithheldOffSpec is the reason credentials_withheld[] carries when the
// request's host is not one the spec declared or a human allowed. It is the
// only reason today; the field exists so a later one can be told apart without
// parsing prose.
const WithheldOffSpec = "host not in spec servers[]"

// credentials puts each resolved credential where its scheme says it goes, as a
// reference. This is the §5a boundary: what lands on the request is the name of
// a credential and how to encode it, never the credential.
//
// The host decides first. Redaction answers *does it print*; the host set
// answers *who receives it*, and a credential bound for a host outside that set
// is diverted onto req.Withheld instead of onto the wire.
//
// The question is asked about `BaseURL + Path`, the string Request.URL hands to
// the executor, and never about BaseURL alone: the join is textual, so a path
// beginning `@` makes the base URL the userinfo of a request to somewhere else
// entirely. Asking about the base is asking about a host that is no longer the
// one being talked to.
func (b *binder) credentials(req *Request) {
	target := req.BaseURL + req.Path
	allowed := b.in.Hosts.Allows(target)

	for _, cred := range b.in.Creds {
		if !b.credentialName(cred) {
			continue
		}

		if !allowed {
			req.Withheld = append(req.Withheld, Withheld{
				Scheme: cred.Scheme,
				Reason: WithheldOffSpec,
				Host:   b.in.Hosts.Key(target),
			})
			continue
		}

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

// credentialName reports whether the credential's name can go where its scheme
// says it goes, failing the binding when it cannot. The rule and its wording
// are credentialNameProblem's, which `auth check` reads through Refuses so the
// pre-flight cannot pass a document the call refuses.
//
// It is checked before the host set is consulted, so a hostile document is exit
// 2 wherever the call was pointed rather than exit 0 with a credentials_withheld
// entry.
func (b *binder) credentialName(cred config.Credential) bool {
	if problem := credentialNameProblem(cred); problem != "" {
		b.fail("%s", problem)
		return false
	}

	return true
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
