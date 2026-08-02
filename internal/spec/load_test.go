package spec

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
