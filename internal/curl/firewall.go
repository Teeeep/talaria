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
// Only the name is quoted back, and never the value: the value may be the
// resolved credential this package exists to keep off every other surface.
func checkSplit(kind, name, value string) error {
	if !request.SplitsRequest(name, value) {
		return nil
	}

	return clierr.Usage("%s %q carries a carriage return or newline, which would end its line "+
		"early and append a header the caller did not write (its value is not echoed)", kind, name)
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
