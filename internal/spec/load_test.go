package spec

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi/orderedmap"

	"github.com/Teeeep/talaria/internal/clierr"
)

// operationIDs collects every operationId in the document, sorted, so two
// documents can be compared regardless of map iteration order.
func operationIDs(t *testing.T, doc *Document) []string {
	t.Helper()

	var ids []string
	for pair := doc.Model.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
		for opPair := pair.Value().GetOperations().First(); opPair != nil; opPair = opPair.Next() {
			ids = append(ids, opPair.Value().OperationId)
		}
	}
	sort.Strings(ids)

	return ids
}

func TestLoadFileReadsOpenAPI30YAML(t *testing.T) {
	doc, err := LoadFile(filepath.Join("testdata", "petstore-3.0.yaml"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if !strings.HasPrefix(doc.Version, "3.0") {
		t.Errorf("Version = %q, want a 3.0.x version", doc.Version)
	}
	if got, want := doc.Model.Paths.PathItems.Len(), 2; got != want {
		t.Errorf("path item count = %d, want %d", got, want)
	}
}

// The 3.1 JSON fixture describes the same API as the 3.0 YAML one. Downstream
// packages read operations, so proving the operationIds match is what makes
// format and minor version invisible past this package.
func TestLoadIsIndifferentToFormatAndMinorVersion(t *testing.T) {
	v30, err := LoadFile(filepath.Join("testdata", "petstore-3.0.yaml"))
	if err != nil {
		t.Fatalf("LoadFile 3.0: %v", err)
	}
	v31, err := LoadFile(filepath.Join("testdata", "petstore-3.1.json"))
	if err != nil {
		t.Fatalf("LoadFile 3.1: %v", err)
	}

	if !strings.HasPrefix(v31.Version, "3.1") {
		t.Errorf("Version = %q, want a 3.1.x version", v31.Version)
	}

	got, want := operationIDs(t, v31), operationIDs(t, v30)
	if len(got) != len(want) {
		t.Fatalf("operationIds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("operationIds = %v, want %v", got, want)
		}
	}
}

func TestLoadBytesAcceptsRawSpecBytes(t *testing.T) {
	doc, err := LoadBytes([]byte(`{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{}}`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if doc.Version != "3.1.0" {
		t.Errorf("Version = %q, want 3.1.0", doc.Version)
	}
}

// A server URL's variables are what internal/request substitutes to get the URL
// a request actually goes to, and §5a defines the allowed host set as the
// servers *after* substitution. Both read Servers[].Variables off the loaded
// model, so the loader has to carry defaults and enums through intact.
func TestLoadFileKeepsServerVariables(t *testing.T) {
	doc, err := LoadFile(filepath.Join("testdata", "server-variables.yaml"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if got, want := len(doc.Model.Servers), 2; got != want {
		t.Fatalf("server count = %d, want %d", got, want)
	}
	if got, want := doc.Model.Servers[0].URL, "https://{region}.api.example.com/{version}"; got != want {
		t.Errorf("servers[0].url = %q, want %q — the loader does not substitute, internal/request does", got, want)
	}

	vars := doc.Model.Servers[0].Variables
	if vars == nil {
		t.Fatal("servers[0].variables is nil")
	}

	region := vars.GetOrZero("region")
	if region == nil {
		t.Fatal("servers[0].variables has no region")
	}
	if got, want := region.Default, "eu"; got != want {
		t.Errorf("region.default = %q, want %q", got, want)
	}
	if got, want := strings.Join(region.Enum, ","), "eu,us"; got != want {
		t.Errorf("region.enum = %q, want %q", got, want)
	}

	if version := vars.GetOrZero("version"); version == nil || version.Default != "v1" {
		t.Errorf("version variable = %+v, want a default of v1", version)
	}
	if orderedmap.Len(doc.Model.Servers[1].Variables) != 0 {
		t.Errorf("servers[1] declares variables it does not have")
	}
}

// requireSpecLoad asserts err carries exit code 3 rather than panicking or
// falling through unclassified to code 1.
func requireSpecLoad(t *testing.T, err error) *clierr.Error {
	t.Helper()

	if err == nil {
		t.Fatal("want an error, got nil")
	}

	var cerr *clierr.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error is not a *clierr.Error: %v", err)
	}
	if cerr.Code != clierr.CodeSpecLoad {
		t.Errorf("Code = %d, want %d", cerr.Code, clierr.CodeSpecLoad)
	}

	return cerr
}

func TestLoadFileRejectsMalformedYAML(t *testing.T) {
	path := filepath.Join("testdata", "malformed.yaml")

	_, err := LoadFile(path)
	cerr := requireSpecLoad(t, err)

	if !strings.Contains(cerr.Message, path) {
		t.Errorf("message = %q, want it to name %q", cerr.Message, path)
	}
}

func TestLoadFileReportsMissingPath(t *testing.T) {
	path := filepath.Join("testdata", "does-not-exist.yaml")

	_, err := LoadFile(path)
	cerr := requireSpecLoad(t, err)

	if !strings.Contains(cerr.Message, path) {
		t.Errorf("message = %q, want it to name %q", cerr.Message, path)
	}
}

func TestLoadBytesRejectsNonSpecInput(t *testing.T) {
	_, err := LoadBytes([]byte("this is not a spec at all"))
	requireSpecLoad(t, err)
}

// captureStdout swaps the process's real os.Stdout for a pipe around fn and
// returns everything written to it. The assertion has to cover the file
// descriptor rather than a cobra writer, because that is the channel a
// dependency's default logger reaches past the envelope on.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()

	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	// Drain concurrently: a pipe with nobody reading blocks the writer once its
	// buffer fills, which would hang the load instead of failing the test.
	read := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		read <- string(out)
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe writer: %v", err)
	}

	return <-read
}

// stdout belongs to the talaria/v1 envelope. libopenapi's default document
// configuration logs build problems as JSON to the real os.Stdout, which
// bypasses cmd.OutOrStdout, the envelope and every redaction path at once — a
// spec with an unresolvable $ref would otherwise put stray documents on stdout.
func TestLoadKeepsSpecDiagnosticsOffStdout(t *testing.T) {
	path := filepath.Join("testdata", "unresolvable-remote-ref.yaml")

	out := captureStdout(t, func() {
		_, _ = LoadFile(path)
	})

	if out != "" {
		t.Errorf("stdout = %q, want nothing written to it", out)
	}
}

// Passing an explicit document configuration is exactly where the defaults that
// stop a hostile spec reading local files or fetching URLs could be dropped, so
// both stay pinned by a test.
func TestLoadDoesNotResolveRemoteReferences(t *testing.T) {
	doc, err := LoadFile(filepath.Join("testdata", "unresolvable-remote-ref.yaml"))
	if err != nil {
		requireSpecLoad(t, err)

		return
	}

	if got := responseDescription(t, doc); got != "" {
		t.Errorf("remote $ref resolved to %q, want it left unresolved", got)
	}
}

func TestLoadDoesNotResolveFileReferences(t *testing.T) {
	doc, err := LoadFile(filepath.Join("testdata", "local-file-ref.yaml"))
	if err != nil {
		requireSpecLoad(t, err)

		return
	}

	if got := responseDescription(t, doc); got != "" {
		t.Errorf("file $ref resolved to %q, want it left unresolved", got)
	}
}

// responseDescription returns the description of the sole 200 response in the
// $ref fixtures, or "" if the reference never resolved to one.
func responseDescription(t *testing.T, doc *Document) string {
	t.Helper()

	pair := doc.Model.Paths.PathItems.First()
	if pair == nil || pair.Value().Get == nil || pair.Value().Get.Responses == nil {
		return ""
	}
	resp := pair.Value().Get.Responses.FindResponseByCode(200)
	if resp == nil {
		return ""
	}

	return resp.Description
}
