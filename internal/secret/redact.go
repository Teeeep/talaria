package secret

import (
	"regexp"
	"strings"
)

// Placeholder is what a redacted value renders as when there is no ref behind
// it — a literal the user typed into --header, or a value that arrived from the
// wire. Where a SecretRef exists, its own <redacted:env:NAME> form is more
// useful and is used instead.
const Placeholder = "<redacted>"

// builtinPatterns is the list from the DESIGN.md §5a leak-channel table, plus
// Set-Cookie from the same table's response-header row. Patterns are globs over
// the lower-cased header name where * matches any run of characters; a pattern
// without a * matches the whole name exactly, which is why Cookie does not also
// catch Set-Cookie.
var builtinPatterns = []string{
	"authorization",
	"proxy-authorization",
	"cookie",
	"set-cookie",
	"*api*key*",
	"*token*",
	"*secret*",
}

// builtin is compiled once at startup and shared by every Redactor.
var builtin = compileGlobs(builtinPatterns)

// Redactor decides which header names carry credentials. The built-in list is
// always in effect: a Redactor's extra patterns extend it and there is no way
// to remove one, because a configurable firewall is one a misconfiguration can
// switch off.
//
// The zero value — including a nil *Redactor, which is what an unset struct
// field is — matches the built-in list, so redaction is never accidentally
// opt-in.
type Redactor struct {
	patterns []*regexp.Regexp
}

// NewRedactor returns a Redactor matching the built-in list plus extra, each of
// which is a glob over the header name.
func NewRedactor(extra ...string) *Redactor {
	if len(extra) == 0 {
		return &Redactor{patterns: builtin}
	}

	patterns := make([]*regexp.Regexp, 0, len(builtin)+len(extra))
	patterns = append(patterns, builtin...)
	patterns = append(patterns, compileGlobs(extra)...)

	return &Redactor{patterns: patterns}
}

// IsSensitive reports whether a header by this name carries a credential.
// Matching is case-insensitive, as header names are.
func (r *Redactor) IsSensitive(name string) bool {
	if name == "" {
		return false
	}

	lowered := strings.ToLower(name)
	for _, pattern := range r.match() {
		if pattern.MatchString(lowered) {
			return true
		}
	}

	return false
}

// Value returns value if a header by this name is safe to show, and the
// placeholder otherwise. The name alone decides: a bearer token the user typed
// into --header is as sensitive as one talaria resolved itself.
func (r *Redactor) Value(name, value string) string {
	if r.IsSensitive(name) {
		return Placeholder
	}

	return value
}

// Headers returns a copy of h with sensitive values replaced. The original is
// left untouched — it is still the map the request will be built from, and only
// the copy is bound for an output surface.
func (r *Redactor) Headers(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}

	redacted := make(map[string]string, len(h))
	for name, value := range h {
		redacted[name] = r.Value(name, value)
	}

	return redacted
}

// match returns the patterns in effect, treating a nil Redactor as the
// built-in list.
func (r *Redactor) match() []*regexp.Regexp {
	if r == nil {
		return builtin
	}

	return r.patterns
}

func compileGlobs(patterns []string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		compiled = append(compiled, compileGlob(pattern))
	}

	return compiled
}

// compileGlob turns a header-name glob into an anchored regexp. Everything but
// * is quoted, so a user-supplied pattern cannot be a regexp injection and
// compilation cannot fail.
func compileGlob(pattern string) *regexp.Regexp {
	parts := strings.Split(strings.ToLower(pattern), "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}

	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}
