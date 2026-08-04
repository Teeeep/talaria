package replay

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/spec"
)

// replayed builds the request an entry replays into. It is the whole seam these
// tests need: everything a stored entry can get wrong shows up in the Request.
func replayed(t *testing.T, e corpus.Entry) (*request.Request, error) {
	t.Helper()

	doc, err := spec.LoadFile(filepath.Join("testdata", "replay.yaml"))
	if err != nil {
		t.Fatalf("LoadFile(replay.yaml): %v", err)
	}

	index := operation.NewIndex(operation.Extract(doc))

	return Build(Inputs{
		Entry:  e,
		Doc:    doc,
		Index:  index,
		Stderr: io.Discard,
	})
}

// entry is a recorded GET of getThings, as `call` would have written it.
func entry(rawURL string, headers, cookies map[string]string) corpus.Entry {
	return corpus.Entry{
		ID:          "2026-08-04T00:00:00Z",
		Timestamp:   time.Unix(0, 0).UTC(),
		Source:      corpus.SourceCall,
		OperationID: "getThings",
		Method:      "GET",
		URL:         rawURL,
		Request:     corpus.EntryRequest{Headers: headers, Cookies: cookies},
	}
}

// A repeated declared query parameter is the default OpenAPI array encoding
// (style: form, explode: true) and is what `call` emits from repeated --query.
// Routing every occurrence through Inputs.Params collapsed them, because
// binder.params is a map[string]string: ?tag=a&tag=b replayed as ?tag=b, exit 0,
// nothing on stderr, while `history show` displayed both.
func TestReplayKeepsEveryValueOfARepeatedDeclaredQueryParameter(t *testing.T) {
	req, err := replayed(t, entry(
		"https://api.example.com/things?tag=a&tag=b",
		map[string]string{"X-Tenant": "acme"}, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, err := req.URL(request.Symbolic)
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	if want := "tag=a&tag=b"; !strings.Contains(got, want) {
		t.Errorf("replayed URL = %q, want it to carry %q", got, want)
	}
}

// A declared header parameter took neither branch: it went out as a --header
// flag rather than binding as the parameter the operation declares, so an entry
// talaria had just written came back "--param X-Tenant is required".
func TestReplayBindsADeclaredHeaderParameter(t *testing.T) {
	req, err := replayed(t, entry(
		"https://api.example.com/things",
		map[string]string{"X-Tenant": "acme"}, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var found bool
	for _, h := range req.Headers {
		if strings.EqualFold(h.Name, "X-Tenant") {
			found = true
		}
	}
	if !found {
		t.Error("the replayed request carries no X-Tenant header")
	}
}

// The silent limb: a declared cookie was warned about and then dropped, so the
// replay exited 0 and the wire carried no Cookie header at all — a different
// request from the one `history show` displays.
func TestReplaySendsADeclaredCookieParameter(t *testing.T) {
	req, err := replayed(t, entry(
		"https://api.example.com/things",
		map[string]string{"X-Tenant": "acme"},
		map[string]string{"sess": "abc123"}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var found bool
	for _, c := range req.Cookies {
		if c.Name == "sess" {
			found = true
		}
	}
	if !found {
		t.Error("the replayed request carries no sess cookie")
	}
}

// The hostile half: a redacted value is not recoverable and must not be
// invented, whatever the operation declares about it.
func TestReplayDoesNotResurrectARedactedDeclaredParameter(t *testing.T) {
	var warned strings.Builder

	doc, err := spec.LoadFile(filepath.Join("testdata", "replay.yaml"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	req, err := Build(Inputs{
		Entry: entry("https://api.example.com/things",
			map[string]string{"X-Tenant": "acme"},
			map[string]string{"sess": "<redacted>"}),
		Doc:    doc,
		Index:  operation.NewIndex(operation.Extract(doc)),
		Stderr: &warned,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, c := range req.Cookies {
		if c.Name == "sess" {
			t.Error("a redacted cookie was replayed as a value")
		}
	}
	if !strings.Contains(warned.String(), "sess") {
		t.Errorf("stderr = %q, want it to name the cookie it could not replay", warned.String())
	}
}
