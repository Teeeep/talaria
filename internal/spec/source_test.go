package spec

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
)

// requireUsage asserts err carries exit code 2 — a bad invocation the caller
// can correct, not a spec that failed to load.
func requireUsage(t *testing.T, err error) *clierr.Error {
	t.Helper()

	if err == nil {
		t.Fatal("want an error, got nil")
	}

	var cerr *clierr.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error is not a *clierr.Error: %v", err)
	}
	if cerr.Code != clierr.CodeUsage {
		t.Errorf("Code = %d, want %d", cerr.Code, clierr.CodeUsage)
	}

	return cerr
}

func TestResolvePrefersPositionalArgOverFlagAndEnv(t *testing.T) {
	t.Setenv(EnvSpec, "from-env.yaml")

	got, err := Resolve("from-arg.yaml", "from-flag.yaml")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "from-arg.yaml"; got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestResolvePrefersFlagOverEnv(t *testing.T) {
	t.Setenv(EnvSpec, "from-env.yaml")

	got, err := Resolve("", "from-flag.yaml")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "from-flag.yaml"; got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestResolveFallsBackToEnv(t *testing.T) {
	t.Setenv(EnvSpec, "from-env.yaml")

	got, err := Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "from-env.yaml"; got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

// With no spec anywhere the agent needs to be told every way it could have
// supplied one, in the one message it gets.
func TestResolveWithNoSpecNamesAllThreeSources(t *testing.T) {
	t.Setenv(EnvSpec, "")

	_, err := Resolve("", "")
	cerr := requireUsage(t, err)

	for _, want := range []string{"--spec", EnvSpec} {
		if !strings.Contains(cerr.Message, want) {
			t.Errorf("message = %q, want it to mention %q", cerr.Message, want)
		}
	}
	if !strings.Contains(cerr.Message, "argument") {
		t.Errorf("message = %q, want it to mention the positional argument", cerr.Message)
	}
}

// specServer serves the 3.0 fixture and reports how many times it was asked
// for, so a cache hit is observable as a request that never happened.
func specServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", "petstore-3.0.yaml"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(body)
	}))
	t.Cleanup(server.Close)

	return server, &requests
}

func TestLoaderFetchesSpecOverHTTP(t *testing.T) {
	server, requests := specServer(t)
	loader := &Loader{CacheDir: t.TempDir()}

	doc, err := loader.Load(server.URL + "/openapi.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !strings.HasPrefix(doc.Version, "3.0") {
		t.Errorf("Version = %q, want a 3.0.x version", doc.Version)
	}
	if doc.Source != server.URL+"/openapi.yaml" {
		t.Errorf("Source = %q, want the URL it was fetched from", doc.Source)
	}
	if *requests != 1 {
		t.Errorf("server saw %d requests, want 1", *requests)
	}
}

// The second load must not touch the network at all — proven by taking the
// server away between the two calls.
func TestLoaderServesSecondLoadFromCache(t *testing.T) {
	server, _ := specServer(t)
	url := server.URL + "/openapi.yaml"
	loader := &Loader{CacheDir: t.TempDir()}

	if _, err := loader.Load(url); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	server.Close()

	doc, err := loader.Load(url)
	if err != nil {
		t.Fatalf("second Load after the server went away: %v", err)
	}
	if !strings.HasPrefix(doc.Version, "3.0") {
		t.Errorf("Version = %q, want a 3.0.x version", doc.Version)
	}
}

// Cached specs are other people's API descriptions and sometimes carry example
// credentials; they are not world-readable.
func TestLoaderWritesCacheFilesPrivately(t *testing.T) {
	server, _ := specServer(t)
	cacheDir := filepath.Join(t.TempDir(), "cache", "specs")
	loader := &Loader{CacheDir: cacheDir}

	if _, err := loader.Load(server.URL + "/openapi.yaml"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	info, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatalf("stat cache dir: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Errorf("cache dir mode = %04o, want %04o", got, want)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache holds %d entries, want 1", len(entries))
	}
	entryInfo, err := entries[0].Info()
	if err != nil {
		t.Fatalf("stat cache file: %v", err)
	}
	if got, want := entryInfo.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("cache file mode = %04o, want %04o", got, want)
	}
}

func TestLoaderReportsHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such spec", http.StatusNotFound)
	}))
	defer server.Close()

	loader := &Loader{CacheDir: t.TempDir()}

	_, err := loader.Load(server.URL + "/missing.yaml")
	cerr := requireSpecLoad(t, err)

	if !strings.Contains(cerr.Message, "404") {
		t.Errorf("message = %q, want it to carry the HTTP status", cerr.Message)
	}
}

// A failed fetch is not cached: the next load must try the network again.
func TestLoaderDoesNotCacheFailedFetches(t *testing.T) {
	cacheDir := t.TempDir()
	loader := &Loader{CacheDir: cacheDir}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such spec", http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := loader.Load(server.URL + "/missing.yaml"); err == nil {
		t.Fatal("want an error from a 404 spec, got nil")
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cache holds %d entries after a failed fetch, want 0", len(entries))
	}
}

func TestLoaderReadsLocalPaths(t *testing.T) {
	path := filepath.Join("testdata", "petstore-3.0.yaml")

	doc, err := (&Loader{CacheDir: t.TempDir()}).Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if doc.Source != path {
		t.Errorf("Source = %q, want %q", doc.Source, path)
	}
}
