package spec

import (
	"strings"
	"testing"
)

// serverDoc loads a minimal 3.0 document whose only interesting content is the
// servers block, written verbatim so a test reads as the spec an author would
// have written. The block is indented two spaces under `servers:`.
func serverDoc(t *testing.T, serversBlock string) *Document {
	t.Helper()

	src := "openapi: 3.0.3\n" +
		"info:\n  title: servers\n  version: \"1\"\n" +
		"servers:\n" + serversBlock +
		"paths: {}\n"

	doc, err := LoadBytes([]byte(src))
	if err != nil {
		t.Fatalf("LoadBytes: %v\n%s", err, src)
	}

	return doc
}

func TestServersSubstitutesAVariableDefault(t *testing.T) {
	doc := serverDoc(t, `  - url: https://{region}.api.example.com/v1
    variables:
      region:
        default: eu
`)

	got := Servers(doc)
	want := []string{"https://eu.api.example.com/v1"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Servers = %q, want %q", got, want)
	}
}

func TestServersSubstitutesEveryVariableInOnePass(t *testing.T) {
	doc := serverDoc(t, `  - url: https://{region}.api.example.com/{version}
    variables:
      region:
        default: eu
      version:
        default: v2
`)

	got := Servers(doc)
	if len(got) != 1 || got[0] != "https://eu.api.example.com/v2" {
		t.Errorf("Servers = %q, want [https://eu.api.example.com/v2]", got)
	}
}

// A default outside its own enum is a spec that contradicts itself. Emitting
// the bad default would send the request somewhere the spec never declared, so
// the server is dropped instead.
func TestServersOmitsAServerWhoseDefaultIsNotInItsEnum(t *testing.T) {
	doc := serverDoc(t, `  - url: https://{region}.api.example.com/v1
    variables:
      region:
        default: mars
        enum:
          - eu
          - us
`)

	if got := Servers(doc); len(got) != 0 {
		t.Errorf("Servers = %q, want none: the default is not in the enum", got)
	}
}

func TestServersOmitsAServerNamingAnUndeclaredVariable(t *testing.T) {
	doc := serverDoc(t, `  - url: https://{region}.api.example.com/v1
    variables:
      unrelated:
        default: eu
`)

	if got := Servers(doc); len(got) != 0 {
		t.Errorf("Servers = %q, want none: {region} is not declared", got)
	}
}

func TestServersReturnsEverySubstitutableServerInOrder(t *testing.T) {
	doc := serverDoc(t, `  - url: https://{region}.a.example.com
    variables:
      region:
        default: eu
  - url: https://{region}.broken.example.com
    variables:
      other:
        default: eu
  - url: https://b.example.com
  - url: https://{region}.c.example.com
    variables:
      region:
        default: us
`)

	got := Servers(doc)
	want := []string{
		"https://eu.a.example.com",
		"https://b.example.com",
		"https://us.c.example.com",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Servers = %q, want %q", got, want)
	}
}

// Substitution must be single-pass: replacement text is never rescanned. Two
// variables whose defaults name each other would otherwise expand forever.
func TestServersDoesNotRescanSubstitutedText(t *testing.T) {
	doc := serverDoc(t, `  - url: https://{a}.example.com
    variables:
      a:
        default: "{b}"
      b:
        default: "{a}"
`)

	got := Servers(doc)
	if len(got) != 0 {
		t.Errorf("Servers = %q, want none: the substituted text still holds a placeholder", got)
	}
}

// A default is spec-controlled text spliced into a URL, so it can rewrite the
// authority. Servers does not judge the result — that is the base-URL check's
// job — but it must not silently drop the evidence either: what it returns has
// to still contain the injected text so the caller's userinfo and scheme
// checks see it.
func TestServersDoesNotSanitiseAnAuthorityRewritingDefault(t *testing.T) {
	tests := []struct {
		name    string
		block   string
		wantURL string
	}{
		{
			"path escape",
			`  - url: https://{sub}.example.com
    variables:
      sub:
        default: "evil.com/"
`,
			"https://evil.com/.example.com",
		},
		{
			"userinfo",
			`  - url: https://{sub}.example.com
    variables:
      sub:
        default: "x@attacker.com"
`,
			"https://x@attacker.com.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Servers(serverDoc(t, tt.block))
			if len(got) != 1 || got[0] != tt.wantURL {
				t.Errorf("Servers = %q, want [%s]", got, tt.wantURL)
			}
		})
	}
}

// Substitution is a growth primitive: every placeholder is replaced by
// arbitrary spec-controlled text, so an unbounded implementation turns a short
// spec into gigabytes. Both shapes must come back bounded, with the oversized
// server omitted rather than truncated — a truncated URL is a different
// authority.
func TestServersBoundsTheSubstitutedLength(t *testing.T) {
	t.Run("one enormous default", func(t *testing.T) {
		doc := serverDoc(t, `  - url: https://{sub}.example.com
    variables:
      sub:
        default: "`+strings.Repeat("a", 1<<20)+`"
`)

		if got := Servers(doc); len(got) != 0 {
			t.Errorf("Servers returned %d URLs of length %d, want none", len(got), len(got[0]))
		}
	})

	t.Run("a thousand variables", func(t *testing.T) {
		var url, vars strings.Builder
		url.WriteString("https://")
		for range 1000 {
			url.WriteString("{v}")
		}
		url.WriteString(".example.com")
		vars.WriteString("    variables:\n      v:\n        default: \"" + strings.Repeat("b", 4096) + "\"\n")

		doc := serverDoc(t, "  - url: "+url.String()+"\n"+vars.String())

		if got := Servers(doc); len(got) != 0 {
			t.Errorf("Servers returned %d URLs of length %d, want none", len(got), len(got[0]))
		}
	})
}

func TestServersOnDocumentsWithNoUsableServer(t *testing.T) {
	tests := []struct {
		name string
		doc  func(*testing.T) *Document
	}{
		{"no servers block", func(t *testing.T) *Document {
			doc, err := LoadBytes([]byte("openapi: 3.0.3\ninfo:\n  title: t\n  version: \"1\"\npaths: {}\n"))
			if err != nil {
				t.Fatalf("LoadBytes: %v", err)
			}
			return doc
		}},
		{"empty url", func(t *testing.T) *Document {
			return serverDoc(t, "  - url: \"\"\n")
		}},
		{"nil variables map", func(t *testing.T) *Document {
			return serverDoc(t, "  - url: https://{region}.example.com\n")
		}},
		{"unterminated brace", func(t *testing.T) *Document {
			return serverDoc(t, "  - url: https://{region.example.com\n")
		}},
		{"nil document", func(*testing.T) *Document { return nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Servers(tt.doc(t)); len(got) != 0 {
				t.Errorf("Servers = %q, want none", got)
			}
		})
	}
}
