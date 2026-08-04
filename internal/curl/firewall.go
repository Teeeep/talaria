package curl

// This file is the §5a firewall inside internal/curl: the one place a resolved
// credential exists, the last gate a value passes before it is written, and the
// cleanup that stops it lingering once curl has exited. The document builder in
// config.go is the other half — it decides the shape of the call, this half
// decides what may be in it and for how long.

import (
	"os"
	"strings"
	"unicode/utf8"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/request"
)

// checkSplit is the last gate before a pair is written into the document, and
// it is not redundant with the binder's: `history replay` rebuilds a Request
// from a stored entry rather than from request.Build, and a credential is only
// a string here, after resolve. escapeDirective is not the answer — it protects
// curl's parser, and curl un-escapes \r\n back to the two bytes on the wire.
//
// The value arrives in the pieces it will be written in rather than joined,
// because joining a resolved credential is the copy this package cannot zero
// (see resolveParts). Scanning the pieces is the same test as scanning the
// join: neither CR nor LF can straddle two of them.
//
// Only the name is quoted back, and never the value: the value may be the
// resolved credential this package exists to keep off every other surface.
func checkSplit(kind, name string, values ...string) error {
	// The charset stays in request.SplitsRequest rather than being respelled
	// here: the name is checked against an empty value, and each piece against
	// an empty name.
	split := request.SplitsRequest(name, "")
	for _, value := range values {
		split = split || request.SplitsRequest("", value)
	}
	if !split {
		return nil
	}

	return clierr.Usage("%s %q carries a carriage return or newline, which would end its line "+
		"early and append a header the caller did not write (its value is not echoed)", kind, name)
}

// basicPair is the last gate on a basic-auth credential: it refuses a value
// curl would not read as a user:password pair. The `Basic ` prefix is not here
// to be stripped — resolveParts hands the prefix back separately, and the
// caller drops it rather than writing it — so what arrives is the pair itself.
//
// curl treats a -u value with no colon as a username and asks for the password
// on /dev/tty, which is not the config pipe and never answers — the call blocks
// until its own max-time and then reports that curl outlived its timeout, which
// sends the reader looking for a slow API. A tool whose §3.1 promise is "never
// prompt, never page" refuses the value instead. An empty username is the same
// mistake read from the other end (`:password`, or a bare `:`), and no request
// talaria builds wants it.
//
// Only the variable is named, never the value: this function is downstream of
// resolveParts, so what it holds is the credential itself.
func basicPair(v request.Value, pair string) (string, error) {
	if user, _, ok := strings.Cut(pair, ":"); ok && user != "" {
		return pair, nil
	}

	if name := v.Ref().Name; name != "" {
		return "", clierr.CredentialMissing(
			"$%s must be user:password; without a username and a colon curl asks for the "+
				"password on the terminal, and talaria never prompts (its value is not echoed)", name)
	}

	return "", clierr.CredentialMissing(
		"the basic-auth credential must be user:password (its value is not echoed)")
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

// discard zeroes the document's buffer and empties it, so a failure part-way
// through leaves nothing in it a caller could send or print. The whole capacity
// is cleared, not the current length: the bytes past len are the ones the last
// append walked over. Together with grow, which clears each array the buffer
// outgrows, this covers every array the document allocated.
//
// It does not cover every copy of the credential in the process, and no comment
// here should be read as claiming otherwise. The string os.Getenv hands back is
// immutable and lives until the collector reclaims it, as does any copy an
// escaper is forced to make; what this package zeroes is what it can address,
// which is these arrays and the one it hands to the caller.
func (d *document) discard() {
	clear(d.b[:cap(d.b)])
	d.b = d.b[:0]
}

// cleanup removes the temp files this document points at.
func (d *document) cleanup() {
	for _, path := range d.files {
		os.Remove(path) //nolint:errcheck // Best effort; the file is 0600 and in TMPDIR.
	}
	d.files = nil
}

// cleanupWith zeroes the copy of the document a caller was handed, alongside the
// document's own buffer, so the resolved credentials in either stop being
// readable once curl has exited. Both halves are idempotent, and config may be
// nil — the failure path has no copy to scrub.
func (d *document) cleanupWith(config []byte) func() {
	return func() {
		d.cleanup()
		d.discard()
		clear(config)
	}
}

// resolveParts is the reader of real credential values. It is the single
// crossing of the firewall described in DESIGN.md §5a: every other renderer in
// talaria produces a redacted or symbolic form, and this one exists only to
// feed the config document.
//
// It hands back the scheme prefix and the value as two pieces rather than
// joined, because the join is a Go string holding the credential and a string
// is the one buffer this package can neither address nor zero. What it returns
// is the environment's own string — immutable and outliving this call whatever
// this package does, which is the residue §5a concedes; the concatenation added
// a second copy of exactly the same kind, on top of one that was unavoidable.
func resolveParts(v request.Value) (prefix, value string, err error) {
	if !v.IsSecret() {
		// Reveal rather than String: a literal the user typed under a
		// credential-shaped name displays redacted everywhere else, and the
		// wire is the one place it must not.
		return "", v.Reveal(), nil
	}

	value, err = v.Ref().Resolve()
	if err != nil {
		return "", "", err
	}

	return v.Prefix(), value, nil
}

// resolve is resolveParts joined, for the request.Render function Request.URL
// takes. The join is the copy resolveParts exists to avoid, so it is confined
// to the URL path, which assembles the query string in a strings.Builder that
// has the same residue and no seam at which to avoid it. Nothing that writes a
// directive may call this.
func resolve(v request.Value) (string, error) {
	prefix, value, err := resolveParts(v)
	if err != nil {
		return "", err
	}

	return prefix + value, nil
}
