// Package canary supports talaria's leak suite: the check that injects a known
// secret through every auth mechanism and greps every output surface for it
// (DESIGN.md §5a, "A CI suite injects canary secrets through every auth
// mechanism and greps every output surface … Redaction regressions fail the
// build").
//
// The enumeration lives in non-test code on purpose. A hand-written list of
// output surfaces rots the moment someone adds a format or a file talaria
// writes; these helpers derive what they can from the code itself — Formats
// from internal/output's Format enum, Tree from whatever is actually on disk —
// so the suite grows with the tool instead of quietly covering less of it.
package canary

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Teeeep/talaria/internal/output"
)

// Surface is one place a secret could come out: a captured stream, a file
// talaria wrote, a report it rendered. Content is bytes rather than a string
// because a surface may well be a binary artifact, and a leak in one is still
// a leak.
type Surface struct {
	// Name identifies the surface in a failure message, e.g. `stdout(json)`.
	Name    string
	Content []byte
}

// Stream returns a Surface holding a captured stdout or stderr.
func Stream(name, content string) Surface {
	return Surface{Name: name, Content: []byte(content)}
}

// Tree returns one Surface per file under root, named label/relative-path.
// It is how the persistent surfaces — the history store, the spec cache — are
// enumerated: by walking what talaria actually wrote rather than by naming the
// files the suite happens to know about.
//
// A root that does not exist yields no surfaces and no error. Not every run
// writes to every directory, and a suite that failed because a cache was cold
// would teach nobody anything.
func Tree(label, root string) ([]Surface, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}

	var surfaces []Surface
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}

		surfaces = append(surfaces, Surface{Name: label + "/" + rel, Content: content})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", root, err)
	}

	return surfaces, nil
}

// Formats returns every --output value the CLI accepts, straight from the
// Format enum. The leak suite iterates this rather than a list of its own, so a
// fourth format is exercised by every case the day it is added — that is the
// point of the indirection.
func Formats() []string { return output.Formats() }

// escapable is spliced into the middle of every canary so the value differs
// from its percent-encoded form.
//
// Without it a canary is label plus hex, url.QueryEscape is the identity on it,
// and the percent needle in needles below is never constructed for any value
// this package generates — so a credential that reached a URL field encoded
// would go unseen. Both characters are sub-delims: QueryEscape escapes them,
// and they are legal unquoted in a header value, a cookie-octet, a URL's
// userinfo and a YAML scalar, which is where the suite's cases put a canary.
const escapable = "!*"

// Value returns a fresh canary: the label, so a failure says which mechanism
// leaked, plus 128 bits of entropy, so a match in an output stream cannot be a
// coincidence or a value some fixture happened to contain.
func Value(label string) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail on any platform talaria runs on, and a leak
		// suite that silently fell back to a predictable value would be worse
		// than one that stopped.
		panic("canary: reading random bytes: " + err.Error())
	}

	encoded := hex.EncodeToString(buf)

	return label + "-" + encoded[:len(encoded)/2] + escapable + encoded[len(encoded)/2:]
}

// Leak is one canary found on one surface.
type Leak struct {
	// Surface is the Name of the surface the value was found on.
	Surface string
	// Encoding names the form it was found in: raw, base64, percent.
	Encoding string
}

func (l Leak) String() string { return l.Surface + " (" + l.Encoding + ")" }

// Scan reports every surface carrying value, in any encoding talaria could
// plausibly have written it in. An empty result is the assertion the suite
// makes: the secret went to the server and nowhere else.
func Scan(value string, surfaces ...Surface) []Leak {
	if value == "" {
		return nil
	}

	var leaks []Leak
	for _, surface := range surfaces {
		content := string(surface.Content)
		for _, needle := range needles(value) {
			if strings.Contains(content, needle.text) {
				leaks = append(leaks, Leak{Surface: surface.Name, Encoding: needle.encoding})
			}
		}
	}

	return leaks
}

// needle is one form of a canary to search for.
type needle struct {
	encoding string
	text     string
}

// needles returns the forms a canary could appear in. Raw covers every output
// surface talaria writes directly. Percent-encoding covers a value that reached
// a URL field, and Value carries escapable so that form is always distinct from
// the raw one. Base64 covers basic auth, where the credential is encoded before
// it is anything else — and is checked at all three alignments, because where
// the value starts inside the encoded string decides which of the three
// encodings its interior matches.
func needles(value string) []needle {
	out := []needle{{encoding: "raw", text: value}}

	if escaped := url.QueryEscape(value); escaped != value {
		out = append(out, needle{encoding: "percent", text: escaped})
	}

	for shift := 0; shift < 3 && shift < len(value); shift++ {
		encoded := base64.StdEncoding.EncodeToString([]byte(value[shift:]))
		// The first and last few characters of an encoding depend on what
		// surrounds the value; the interior does not. Trimming both ends keeps
		// the needle exact at the cost of a few characters of a value that is
		// long precisely so it can afford them.
		if len(encoded) <= 12 {
			continue
		}

		out = append(out, needle{
			encoding: fmt.Sprintf("base64+%d", shift),
			text:     encoded[4 : len(encoded)-4],
		})
	}

	return out
}
