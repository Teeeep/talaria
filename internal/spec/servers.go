package spec

import (
	"slices"
	"strings"

	v3high "github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/orderedmap"
)

// maxServerURL bounds a substituted server URL. A variable's default is
// spec-controlled text spliced into the URL once per placeholder, so
// substitution multiplies attacker-chosen length by attacker-chosen count.
// A server over the bound is omitted, never truncated: a truncated URL names a
// different authority than the one the spec declared.
const maxServerURL = 2048

// Servers returns the document's server URLs with each {variable} replaced by
// its declared default. A server whose URL cannot be fully substituted is
// omitted rather than returned half-done — an undeclared placeholder, a
// default outside its own enum, an unterminated brace, or a result over
// maxServerURL. Callers treat every URL returned here as a usable base URL, so
// a template must never reach one.
//
// What comes back is not validated as a URL: a default can rewrite the
// authority, and judging that is the caller's job. binder.baseURL applies the
// same userinfo and absolute-http(s) checks to a substituted server as to a
// hand-typed --base-url.
func Servers(doc *Document) []string {
	if doc == nil || doc.Model == nil {
		return nil
	}

	urls := make([]string, 0, len(doc.Model.Servers))
	for _, server := range doc.Model.Servers {
		if server == nil || server.URL == "" {
			continue
		}

		if url, ok := substitute(server.URL, server.Variables); ok {
			urls = append(urls, url)
		}
	}

	return urls
}

// substitute replaces every {name} in raw with that variable's default,
// reporting false when the URL cannot be substituted completely.
//
// It is single-pass by construction: the scan reads raw and writes to a
// separate builder, so replacement text is never rescanned. A default that
// itself names a variable therefore stays literal, and the leftover-brace check
// drops that server instead of expanding it forever.
func substitute(raw string, vars *orderedmap.Map[string, *v3high.ServerVariable]) (string, bool) {
	defaults := map[string]string{}
	for name, v := range vars.FromOldest() {
		if v == nil || (len(v.Enum) > 0 && !slices.Contains(v.Enum, v.Default)) {
			// A default outside its own enum contradicts the spec that
			// declared it; leaving it out of the lookup drops the server.
			continue
		}

		defaults[name] = v.Default
	}

	var out strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] != '{' {
			out.WriteByte(raw[i])
			i++
		} else {
			end := strings.IndexByte(raw[i:], '}')
			if end < 0 {
				return "", false
			}

			value, ok := defaults[raw[i+1:i+end]]
			if !ok {
				return "", false
			}

			out.WriteString(value)
			i += end + 1
		}

		if out.Len() > maxServerURL {
			return "", false
		}
	}

	url := out.String()
	if strings.Contains(url, "{") {
		return "", false
	}

	return url, true
}
