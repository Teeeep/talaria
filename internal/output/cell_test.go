package output

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEscapeCellLeavesPrintableTextAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"getUser",
		"/users/{id}",
		"a summary with spaces, punctuation & symbols",
		"héllo — naïve ✓", // multi-byte UTF-8 is text, not structure
	} {
		if got := escapeCell(s); got != s {
			t.Errorf("escapeCell(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestEscapeCellEscapesTheCharactersThatBreakARow(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"tab is the column separator", "a\tb", `a\tb`},
		{"newline is the row separator", "line one\nline two", `line one\nline two`},
		{"carriage return overwrites the printed line", "safe\rspoofed", `safe\rspoofed`},
		{"backslash doubles so the escape is unambiguous", `a\tb`, `a\\tb`},
		{"NUL", "a\x00b", `a\x00b`},
		{"ANSI escape sequence", "a\x1b[31mred\x1b[0m", `a\x1b[31mred\x1b[0m`},
		{"DEL", "a\x7fb", `a\x7fb`},
		// The scan for work to do must not stop at the first multi-byte rune.
		{"a tab after a multi-byte rune", "héllo\tworld", `héllo\tworld`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeCell(tt.in); got != tt.want {
				t.Errorf("escapeCell(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEscapeCellEscapesInvalidUTF8(t *testing.T) {
	// A recorded response body is raw bytes; a lone 0xff is not a rune and
	// makes every downstream width calculation wrong.
	got := escapeCell("a\xffb")
	if want := `a\xffb`; got != want {
		t.Errorf("escapeCell of invalid UTF-8 = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Errorf("escapeCell returned invalid UTF-8: %q", got)
	}
}

func TestEscapeCellIsUnambiguousForTheSeparatorRepeated(t *testing.T) {
	// 1,000 tabs must not become 1,000 column breaks.
	got := escapeCell(strings.Repeat("\t", 1000))
	if strings.ContainsRune(got, '\t') {
		t.Fatal("escapeCell left a literal tab in the cell")
	}
	if want := strings.Repeat(`\t`, 1000); got != want {
		t.Errorf("escapeCell of 1,000 tabs = %q, want 1,000 escaped tabs", got)
	}
}

func TestCellWidthCountsWhatIsPrinted(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"getUser", 7},
		{"héllo", 5},   // runes, not bytes
		{"a\tb", 4},    // the tab prints as two characters
		{"\x1b[0m", 7}, // the four characters of \x1b plus three printable
		{"\xff", 4},    // an invalid byte prints as \xff
		{`a\b`, 4},     // the backslash doubles
	}

	for _, tt := range tests {
		if got := CellWidth(tt.in); got != tt.want {
			t.Errorf("CellWidth(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestTruncateCellFitsTheEscapedForm(t *testing.T) {
	// The renderer escapes after the caller truncates, so a budget spent on the
	// raw text overflows the line it was supposed to bound.
	for _, s := range []string{
		strings.Repeat("\t", 40),
		strings.Repeat("a", 40),
		"line one\nline two\twith tab, and a long tail of ordinary words",
		strings.Repeat("\xff", 40),
		"héllo " + strings.Repeat("naïve ", 20),
	} {
		for _, max := range []int{1, 2, 12, 30, 100} {
			got := TruncateCell(s, max)
			if w := CellWidth(got); w > max {
				t.Errorf("TruncateCell(%q, %d) has escaped width %d: %q", s, max, w, got)
			}
		}
	}
}

func TestTruncateCellLeavesAFittingCellAlone(t *testing.T) {
	for _, s := range []string{"", "getUser", "héllo"} {
		if got := TruncateCell(s, 20); got != s {
			t.Errorf("TruncateCell(%q, 20) = %q, want it unchanged", s, got)
		}
	}
}

func TestTruncateCellMarksTheCut(t *testing.T) {
	got := TruncateCell(strings.Repeat("a", 40), 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("TruncateCell = %q, want a trailing ellipsis so the cut is visible", got)
	}
	if !strings.HasPrefix(got, "aaa") {
		t.Errorf("TruncateCell = %q, want the start of the value to survive", got)
	}
}

func TestEscapeCellNeverEmitsAStructuralByte(t *testing.T) {
	// Every byte value, one cell each: nothing that survives may be a tab, a
	// newline, a carriage return or a byte outside printable UTF-8.
	for b := 0; b < 256; b++ {
		cell := escapeCell(string([]byte{byte(b)}))
		if strings.ContainsAny(cell, "\t\r\n") {
			t.Errorf("escapeCell(%#v) = %q, which still carries a structural byte", byte(b), cell)
		}
		if !utf8.ValidString(cell) {
			t.Errorf("escapeCell(%#v) = %q, which is not valid UTF-8", byte(b), cell)
		}
	}
}
