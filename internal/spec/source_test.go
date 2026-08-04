package spec

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

	doc, err := loader.Load(context.Background(), server.URL+"/openapi.yaml")
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

	if _, err := loader.Load(context.Background(), url); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	server.Close()

	doc, err := loader.Load(context.Background(), url)
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

	if _, err := loader.Load(context.Background(), server.URL+"/openapi.yaml"); err != nil {
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

	_, err := loader.Load(context.Background(), server.URL+"/missing.yaml")
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

	if _, err := loader.Load(context.Background(), server.URL+"/missing.yaml"); err == nil {
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

// paddedSpec returns the 3.0 fixture grown to exactly n bytes with a trailing
// YAML comment, so a size bound can be tested against a spec that is otherwise
// valid and would load if it fit.
func paddedSpec(t *testing.T, n int) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", "petstore-3.0.yaml"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	const comment = "\n# "
	pad := n - len(body) - len(comment)
	if pad < 0 {
		t.Fatalf("the fixture is %d bytes, larger than the %d wanted", len(body), n)
	}

	out := append(append([]byte{}, body...), comment...)

	return append(out, bytes.Repeat([]byte("x"), pad)...)
}

// serveBytes answers every request with body, counting the requests so a cache
// hit stays observable.
func serveBytes(t *testing.T, body []byte) (*httptest.Server, *int) {
	t.Helper()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(body)
	}))
	t.Cleanup(server.Close)

	return server, &requests
}

// A 2 MB spec is the size DESIGN.md cites for a real swagger.json; the bound
// exists to stop a hostile server, not to stop a large API.
func TestLoaderAcceptsARealisticallyLargeSpec(t *testing.T) {
	server, _ := serveBytes(t, paddedSpec(t, 2<<20))
	loader := &Loader{CacheDir: t.TempDir()}

	if _, err := loader.Load(context.Background(), server.URL+"/openapi.yaml"); err != nil {
		t.Fatalf("Load of a 2 MB spec: %v", err)
	}
}

func TestLoaderAcceptsASpecAtTheReadBound(t *testing.T) {
	server, _ := serveBytes(t, paddedSpec(t, maxSpecBytes))
	loader := &Loader{CacheDir: t.TempDir()}

	if _, err := loader.Load(context.Background(), server.URL+"/openapi.yaml"); err != nil {
		t.Fatalf("Load of a spec at exactly the bound: %v", err)
	}
}

// requireOverBound asserts that err is the read bound refusing, and not some
// later failure that happens to look like one. It matters because every
// oversized body in these tests is `x` repeated, which does not parse as a
// spec: an unbounded read reaches loadBytes and returns an error too, so
// asserting only `err != nil` would pass against no bound at all.
func requireOverBound(t *testing.T, err error) {
	t.Helper()

	cerr := requireSpecLoad(t, err)
	if !strings.Contains(cerr.Message, strconv.Itoa(maxSpecBytes)) {
		t.Errorf("message = %q, want the read bound naming %d bytes", cerr.Message, maxSpecBytes)
	}
}

func TestLoaderRefusesASpecOneBytePastTheReadBound(t *testing.T) {
	server, _ := serveBytes(t, paddedSpec(t, maxSpecBytes+1))
	loader := &Loader{CacheDir: t.TempDir()}

	_, err := loader.Load(context.Background(), server.URL+"/openapi.yaml")

	requireOverBound(t, err)
}

// The bytes of an oversized fetch are never written down: the cache has no
// expiry, so one hostile response would otherwise poison the URL for good.
func TestLoaderDoesNotCacheAnOversizedFetch(t *testing.T) {
	server, _ := serveBytes(t, paddedSpec(t, maxSpecBytes+1))
	cacheDir := t.TempDir()
	loader := &Loader{CacheDir: cacheDir}

	if _, err := loader.Load(context.Background(), server.URL+"/openapi.yaml"); err == nil {
		t.Fatal("want an error from an oversized spec, got nil")
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cache holds %d entries after an oversized fetch, want 0", len(entries))
	}
}

// A server that keeps talking is bounded by volume, not only by fetchTimeout:
// the read has to end long before the clock does.
func TestLoaderSurvivesAServerThatNeverStopsSending(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		for written := 0; written < 4*maxSpecBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	loader := &Loader{CacheDir: t.TempDir()}

	_, err := loader.Load(context.Background(), server.URL+"/openapi.yaml")

	requireOverBound(t, err)
}

// endlessBody stands in for a server whose Content-Length is a claim rather
// than a fact: it keeps answering long past the header it arrived behind.
//
// It stops at four times the bound rather than running forever. An unbounded
// reader would be the more faithful adversary, but it makes the *failure* mode
// of this test an out-of-memory kill of whatever machine runs the suite —
// which is how a build of this task died once already. Four times the bound is
// past every limit under test, so a fetch that stops reading fails the
// assertion below, and one that does not allocates 128 MiB and still fails.
type endlessBody struct{ read int }

func (b *endlessBody) Read(p []byte) (int, error) {
	if b.read >= 4*maxSpecBytes {
		return 0, io.EOF
	}

	for i := range p {
		p[i] = 'x'
	}
	b.read += len(p)

	return len(p), nil
}

func (*endlessBody) Close() error { return nil }

type lyingTransport struct{}

func (lyingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Header:        make(http.Header),
		ContentLength: 100,
		Body:          &endlessBody{},
	}, nil
}

// The bound is on the bytes actually read. Content-Length is chosen by the
// server, so a truthful-looking small one in front of an endless body must not
// buy any allocation.
func TestLoaderDoesNotTrustContentLength(t *testing.T) {
	loader := &Loader{
		CacheDir: t.TempDir(),
		Client:   &http.Client{Transport: lyingTransport{}},
	}

	_, err := loader.Load(context.Background(), "http://example.invalid/openapi.yaml")

	requireOverBound(t, err)
}

// A compressed body is decompressed by the transport, so the cap has to apply
// to what comes out of it rather than to what arrived.
func TestLoaderBoundsADecompressedSpec(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for written := 0; written <= maxSpecBytes; written += len(chunk) {
		if _, err := zw.Write(chunk); err != nil {
			t.Fatalf("compressing: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the gzip writer: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed.Bytes())
	}))
	defer server.Close()

	loader := &Loader{CacheDir: t.TempDir()}

	_, err := loader.Load(context.Background(), server.URL+"/openapi.yaml")

	requireOverBound(t, err)
}

// The bytes are cached under a hash of the URL the caller named, so every hop
// is a server choosing what talaria will remember as that URL's spec.
func TestLoaderBoundsTheRedirectChain(t *testing.T) {
	requests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, server.URL+"/next"+strconv.Itoa(requests), http.StatusFound)
	}))
	defer server.Close()

	loader := &Loader{CacheDir: t.TempDir()}

	if _, err := loader.Load(context.Background(), server.URL+"/openapi.yaml"); err == nil {
		t.Fatal("Load followed an endless redirect chain, want a refusal")
	}
	if requests > maxSpecRedirects+1 {
		t.Errorf("server saw %d requests, want at most %d", requests, maxSpecRedirects+1)
	}
}

func TestLoaderRefusesARedirectAwayFromHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer server.Close()

	loader := &Loader{CacheDir: t.TempDir()}

	_, err := loader.Load(context.Background(), server.URL+"/openapi.yaml")
	cerr := requireSpecLoad(t, err)

	// The message, not just the failure: net/http's transport refuses an
	// unsupported scheme on its own, so an error alone would pass with no
	// redirect policy at all. The refusal has to come from the policy, which is
	// what also bounds the hops a supported scheme could take.
	if !strings.Contains(cerr.Message, "refusing a redirect") {
		t.Errorf("message = %q, want the redirect policy's refusal", cerr.Message)
	}
}

// The redirect policy belongs to the fetch, not to the caller's client: a
// Loader handed a client must not find its own CheckRedirect rewritten.
func TestLoaderLeavesACallersRedirectPolicyAlone(t *testing.T) {
	server, _ := specServer(t)
	client := &http.Client{}
	loader := &Loader{CacheDir: t.TempDir(), Client: client}

	if _, err := loader.Load(context.Background(), server.URL+"/openapi.yaml"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if client.CheckRedirect != nil {
		t.Error("the caller's client had its CheckRedirect set by a spec fetch")
	}
}

func TestLoaderReadsLocalPaths(t *testing.T) {
	path := filepath.Join("testdata", "petstore-3.0.yaml")

	doc, err := (&Loader{CacheDir: t.TempDir()}).Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if doc.Source != path {
		t.Errorf("Source = %q, want %q", doc.Source, path)
	}
}

// cancelGrace bounds how long a cancelled fetch may take to come back. It is
// short enough that fetchTimeout cannot satisfy it — a fetch that ignores the
// context fails these tests by waiting out the full thirty seconds — and long
// enough that a loaded machine does not fail them for being slow.
const cancelGrace = 5 * time.Second

// requireCancelled asserts that err is the caller's own cancellation, reported
// the way an interrupted call is: exit 1, not the spec-load code. Ctrl-C during
// a fetch does not mean the spec is broken, and an agent branching on 3 would
// stop retrying a spec that is fine. The errors.Is is what distinguishes a real
// cancellation from a fetch that failed for some other reason and happened to
// be running under a cancelled context.
func requireCancelled(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("want an error from a cancelled fetch, got nil")
	}

	var cerr *clierr.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error is not a *clierr.Error: %v", err)
	}
	if cerr.Code != clierr.CodeRequestFailed {
		t.Errorf("Code = %d, want %d — an interruption, not a broken spec", cerr.Code, clierr.CodeRequestFailed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

// loadAsync runs Load in a goroutine and returns the channel its error arrives
// on, so a test can assert on *when* it came back as well as on what it said.
func loadAsync(l *Loader, ctx context.Context, ref string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := l.Load(ctx, ref)
		done <- err
	}()

	return done
}

// requirePromptly asserts the fetch came back inside cancelGrace, which only a
// fetch the context reached can do.
func requirePromptly(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(cancelGrace):
		t.Fatalf("Load did not return within %s of the cancellation; fetchTimeout is %s", cancelGrace, fetchTimeout)

		return nil
	}
}

// A server that accepts the connection and never answers. Without the context
// reaching the fetch, Ctrl-C here waits out fetchTimeout's full thirty seconds
// — the one case §3.1's "never page" rule exists for.
func TestLoaderStopsAFetchTheContextCancels(t *testing.T) {
	accepted := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(accepted)
		<-release
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loader := &Loader{CacheDir: t.TempDir()}
	done := loadAsync(loader, ctx, server.URL+"/openapi.yaml")

	<-accepted
	cancel()

	requireCancelled(t, requirePromptly(t, done))
}

// A context already cancelled on entry: the fetch must not be made at all.
func TestLoaderRefusesAFetchUnderAnAlreadyCancelledContext(t *testing.T) {
	server, requests := specServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	loader := &Loader{CacheDir: t.TempDir()}

	_, err := loader.Load(ctx, server.URL+"/openapi.yaml")

	requireCancelled(t, err)
	if *requests != 0 {
		t.Errorf("server saw %d requests under a cancelled context, want 0", *requests)
	}
}

// Cancellation after the bound has started reading. The handler flushes a chunk
// before signalling, so the response headers are on the wire and the read is
// the wait the cancellation lands in — the size bound and the clock are
// independent, and neither is what ends this fetch.
func TestLoaderStopsAReadTheContextCancels(t *testing.T) {
	flushed := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(bytes.Repeat([]byte("x"), 1<<16))
		w.(http.Flusher).Flush()
		close(flushed)
		<-release
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loader := &Loader{CacheDir: t.TempDir()}
	done := loadAsync(loader, ctx, server.URL+"/openapi.yaml")

	<-flushed
	cancel()

	requireCancelled(t, requirePromptly(t, done))
}

// Cancellation during the redirect chain. Each hop is a fresh request, so a
// fetch that carries the context only into the first one would follow the rest
// to maxSpecRedirects after the caller has already given up.
func TestLoaderStopsFollowingRedirectsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	requests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 2 {
			cancel()
		}
		http.Redirect(w, r, server.URL+"/next"+strconv.Itoa(requests), http.StatusFound)
	}))
	defer server.Close()

	loader := &Loader{CacheDir: t.TempDir()}

	requireCancelled(t, requirePromptly(t, loadAsync(loader, ctx, server.URL+"/openapi.yaml")))

	if requests > 3 {
		t.Errorf("server saw %d requests after the cancellation on the second, want at most 3", requests)
	}
}

// The cache is a read, not a wait, so a cancelled context does not stop it:
// recordCall's ordering argument applies here too — the answer is already on
// disk, and refusing to return it would turn a Ctrl-C at the wrong moment into
// a failure for work that was already done.
func TestLoaderServesTheCacheUnderACancelledContext(t *testing.T) {
	server, _ := specServer(t)
	url := server.URL + "/openapi.yaml"
	loader := &Loader{CacheDir: t.TempDir()}

	if _, err := loader.Load(context.Background(), url); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := loader.Load(ctx, url); err != nil {
		t.Errorf("Load from cache under a cancelled context: %v", err)
	}
}
