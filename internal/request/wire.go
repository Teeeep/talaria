package request

// This file holds the wire-safety charset rules: what a name, a value, a method
// or a media type may be made of before it can go on the wire. They live
// together because they are one concern — a request must not be able to say
// more than its caller wrote — and because most of them share the HTTP token
// charset.

import (
	"fmt"
	"strings"
)

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

// crlfProblem completes the messages that reject a request-splitting value. It
// describes the fault without echoing what caused it, for the reason `pairs`
// gives: the value may be a credential.
const crlfProblem = "carries a carriage return or newline, which would end its line early and " +
	"append a header the caller did not write (its value is not echoed)"

// SplitsRequest reports whether a name=value pair would break out of the field
// it sits in. A CR or an LF ends a header line on the wire, so a value holding
// either turns one field into two — or, with a bare CRLF pair, ends the header
// block and starts a second request on the same connection.
//
// Both halves are checked because both go on the wire: a cookie's name is
// joined to its value in one Cookie header, and a query name is only safe
// because QueryString escapes it.
//
// Escaping is deliberately not the answer. curl's config document has escapes
// for both characters (see internal/curl's escapeDirective), but they protect
// curl's *parser* — curl un-escapes them back to the literal bytes and writes
// those to the socket. A value carrying a CRLF is a value the caller cannot
// have meant, so it is a usage error, not something to sanitise into a request
// they did not ask for.
//
// It is exported for the same reason IsMethod is: internal/curl re-checks every
// pair as the last gate before the document, and a credential is only a string
// there — it is resolved after this package has finished with the Request.
func SplitsRequest(name, value string) bool {
	return strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n")
}

// IsMethod reports whether s can go in a request line as its method. An HTTP
// method is a token (RFC 9110 §9), the same production a field name is drawn
// from, so it is the same check under the name that says what it guards.
//
// It is exported for internal/curl, which re-checks the method as the last gate
// before the `request` directive: not every Request is built by this package —
// `history replay` rebuilds one from a stored entry — and the charset should
// not be spelled out twice.
func IsMethod(s string) bool { return isFieldName(s) }

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

// hasControl reports whether s holds a space or a control character — neither
// belongs in a URL, and a CR or LF in one would reach the wire.
func hasControl(s string) bool {
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return true
		}
	}

	return false
}

// isMediaType reports whether s can be sent as a Content-Type value: a
// type/subtype pair of tokens, followed by any number of `; parameter=value`
// parameters (RFC 9110 §8.3.1).
//
// Checking the whole grammar rather than only for a CR or an LF is the cheaper
// rule, not the stricter one: the token charset refuses the two split
// characters along with the NUL, the whitespace and the colon that would name a
// second header, all in one place. The parameter form has to be spelled out
// either way, or `text/plain; charset=utf-8` would fail it.
func isMediaType(s string) bool {
	parts := strings.Split(s, ";")

	kind, subtype, ok := strings.Cut(parts[0], "/")
	if !ok || !isFieldName(kind) || !isFieldName(subtype) {
		return false
	}

	for _, p := range parts[1:] {
		name, value, ok := strings.Cut(strings.TrimLeft(p, " \t"), "=")
		if !ok || !isFieldName(name) || !isParameterValue(value) {
			return false
		}
	}

	return true
}

// isParameterValue reports whether v can follow a media type parameter's `=`:
// a token, or the quoted string a multipart boundary uses to carry characters a
// token cannot (RFC 9110 §5.6.6).
func isParameterValue(v string) bool {
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		return isQuotedText(v[1 : len(v)-1])
	}

	return isFieldName(v)
}

// isQuotedText reports whether s can sit between a quoted string's quotes:
// printable ASCII and the horizontal tab, with neither the quote that would end
// it early nor the backslash that would escape whatever follows it.
func isQuotedText(s string) bool {
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			return false
		case r != '\t' && (r < ' ' || r > '~'):
			return false
		}
	}

	return true
}
