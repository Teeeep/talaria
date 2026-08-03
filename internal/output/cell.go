package output

import (
	"strings"
	"unicode/utf8"
)

// hexDigits indexes the lowercase hex digits used by the \xNN escape.
const hexDigits = "0123456789abcdef"

// escapeCell renders one table cell so that it cannot alter the row and column
// structure around it. Every cell is untrusted: spec summaries and descriptions
// carry real newlines, and a `history show` cell is a recorded response body,
// which is raw bytes.
//
// Escaping, not stripping: a caller running `cut -f3` must be able to tell a
// tab inside a value from a column break, and dropping the byte would leave the
// two indistinguishable. The scheme is Go's own — \t, \r, \n, \\ and \xNN for
// every other C0 control, DEL, and every byte that is not part of a valid UTF-8
// sequence — so it is unambiguous to decode and safe to print to a terminal.
func escapeCell(s string) string {
	if !needsEscape(s) {
		return s
	}

	// Cells that need escaping are usually mostly printable; grow from the
	// input length rather than assuming the worst case.
	var b strings.Builder
	b.Grow(len(s) + 8)

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
			i++
		case c == '\t':
			b.WriteString(`\t`)
			i++
		case c == '\r':
			b.WriteString(`\r`)
			i++
		case c == '\n':
			b.WriteString(`\n`)
			i++
		case c < 0x20 || c == 0x7f:
			writeHexEscape(&b, c)
			i++
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				writeHexEscape(&b, c)
				i++

				continue
			}
			b.WriteString(s[i : i+size])
			i += size
		}
	}

	return b.String()
}

// needsEscape reports whether s contains a byte escapeCell would rewrite. It is
// the fast path: almost every cell is plain text, and the common case should
// not allocate.
func needsEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || c == '\\' {
			return true
		}
	}

	return !utf8.ValidString(s)
}

func writeHexEscape(b *strings.Builder, c byte) {
	b.WriteString(`\x`)
	b.WriteByte(hexDigits[c>>4])
	b.WriteByte(hexDigits[c&0x0f])
}

// CellWidth is how many characters a cell occupies once a renderer has escaped
// it. A caller sizing a column must measure this rather than the raw string: a
// single tab is one rune in and two out, so a budget spent on the raw text
// overflows the line it was meant to bound.
func CellWidth(s string) int {
	if !needsEscape(s) {
		return utf8.RuneCountInString(s)
	}

	return utf8.RuneCountInString(escapeCell(s))
}

// TruncateCell shortens s so that CellWidth of the result is at most max,
// marking the cut with an ellipsis so a reader can tell the value continues.
// Cells are cut on rune boundaries — including the single-byte boundary of an
// invalid UTF-8 byte, which escapes to four characters of its own.
func TruncateCell(s string, max int) string {
	if CellWidth(s) <= max {
		return s
	}
	if max < 1 {
		return ""
	}

	// One character of the budget goes to the ellipsis.
	budget := max - 1

	width := 0
	end := 0
	for end < len(s) {
		_, size := utf8.DecodeRuneInString(s[end:])
		w := CellWidth(s[end : end+size])
		if width+w > budget {
			break
		}
		width += w
		end += size
	}

	return s[:end] + "…"
}

// escapeRow returns row with every cell escaped. It returns a new slice: the
// rows a command builds are also the rows it may still hold a reference to.
func escapeRow(row []string) []string {
	out := make([]string, len(row))
	for i, cell := range row {
		out[i] = escapeCell(cell)
	}

	return out
}
