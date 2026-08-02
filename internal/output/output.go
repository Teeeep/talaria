// Package output renders command results in the three shapes talaria speaks:
// machine-readable JSON carrying a versioned schema field, aligned pretty text
// for humans, and bare TSV for shell pipelines (DESIGN.md §3.1).
package output

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// SchemaVersion is the value of the top-level "schema" field on every JSON
// payload. Agent prompts key off it, so it changes only on a breaking change to
// the output shape.
const SchemaVersion = "talaria/v1"

// Format is an output rendering mode selected by --output.
type Format string

const (
	FormatJSON   Format = "json"
	FormatPretty Format = "pretty"
	FormatTSV    Format = "tsv"
)

// formats lists every valid Format in the order shown to users.
var formats = []Format{FormatJSON, FormatPretty, FormatTSV}

// ParseFormat converts a --output value into a Format. The error names every
// valid value so an agent can correct itself without reading the help text.
func ParseFormat(s string) (Format, error) {
	for _, f := range formats {
		if Format(s) == f {
			return f, nil
		}
	}

	valid := make([]string, len(formats))
	for i, f := range formats {
		valid[i] = string(f)
	}
	return "", fmt.Errorf("unknown output format %q: valid values are %s", s, strings.Join(valid, ", "))
}

// Resolve picks the output format. An explicit --output value always wins; with
// no explicit value the default is pretty on a terminal and JSON when piped,
// because piped output is being read by a program.
func Resolve(explicit string, isTTY bool) (Format, error) {
	if explicit != "" {
		return ParseFormat(explicit)
	}
	if isTTY {
		return FormatPretty, nil
	}
	return FormatJSON, nil
}

// IsTTY reports whether w is a character device, i.e. an interactive terminal.
// A writer that is not an *os.File (a test buffer, a pipe wrapper) is never a
// TTY, so tests and pipelines both land on the machine-readable default.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
