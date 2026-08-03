package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Teeeep/talaria/internal/clierr"
)

// EnvSpec names the environment variable consulted when neither the positional
// argument nor --spec supplies a spec. Setting it once is what lets an agent
// run a dozen commands against the same API without repeating itself.
const EnvSpec = "TALARIA_SPEC"

// fetchTimeout bounds a remote spec fetch. A spec that never arrives has to
// become an exit code rather than a hung process an agent cannot interpret.
const fetchTimeout = 30 * time.Second

// maxSpecBytes bounds a remote spec read, and maxSpecRedirects the chain of
// hops it will follow to get there.
//
// The size is an order of magnitude past the 2 MB DESIGN.md cites for a real
// swagger.json, and it is applied to the bytes actually read: Content-Length is
// a claim the server makes, and the body may arrive chunked or compressed. The
// clock bound alone is not enough — fetchTimeout lets thirty seconds of full
// bandwidth into memory. The redirect bound is tight because the fetched bytes
// are cached under a hash of the URL the *caller* named, so every hop is a
// server choosing what talaria remembers as that URL's spec.
const (
	maxSpecBytes     = 32 << 20
	maxSpecRedirects = 5
)

// cacheDirMode and cacheFileMode keep the cache private to its owner. Specs are
// other people's API descriptions and routinely carry example credentials and
// internal hostnames, so they are not world-readable.
const (
	cacheDirMode  os.FileMode = 0o700
	cacheFileMode os.FileMode = 0o600
)

// Resolve picks the spec source from the three ways of supplying one, in
// precedence order: positional argument, then --spec, then $TALARIA_SPEC. The
// result is a file path or a URL; classifying it is Load's job.
func Resolve(arg, flag string) (string, error) {
	for _, candidate := range []string{arg, flag, os.Getenv(EnvSpec)} {
		if candidate != "" {
			return candidate, nil
		}
	}

	return "", clierr.Usage(
		"no spec given: pass one as the positional argument, with --spec, or in $%s", EnvSpec)
}

// Loader loads a spec from a path or a URL, caching remote specs on disk. The
// zero value is usable and caches under the user cache dir; tests set CacheDir
// to a temporary directory so they never touch the real one.
type Loader struct {
	// CacheDir holds cached remote specs. Empty means
	// $XDG_CACHE_HOME/talaria/specs (falling back to ~/.cache/talaria/specs).
	CacheDir string
	// Client fetches remote specs. Nil means a default client with
	// fetchTimeout. Spec fetches deliberately use net/http: the rule that API
	// traffic goes through curl is about calls the agent makes, not about
	// reading the description of them.
	Client *http.Client
}

// Load reads the spec named by ref, which is either a URL or a file path.
func Load(ref string) (*Document, error) {
	return (&Loader{}).Load(ref)
}

// Load reads the spec named by ref. A remote spec is served from the cache when
// one is present, so a session that makes a dozen calls downloads it once.
func (l *Loader) Load(ref string) (*Document, error) {
	if !isURL(ref) {
		return LoadFile(ref)
	}

	return l.loadURL(ref)
}

// isURL classifies a spec reference. Anything that is not plainly http(s) is a
// path, so Windows drive letters and relative paths stay paths.
func isURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

func (l *Loader) loadURL(url string) (*Document, error) {
	// A cache path that cannot be computed is not fatal: the fetch still works,
	// it just will not be remembered.
	cachePath, _ := l.cachePath(url)
	if cachePath != "" {
		if data, err := os.ReadFile(cachePath); err == nil {
			return loadBytes(data, url)
		}
	}

	data, err := l.fetch(url)
	if err != nil {
		return nil, err
	}

	// Parse before caching. Caching first would make one bad response poison
	// every later run, and the cache has no expiry an agent could reach.
	doc, err := loadBytes(data, url)
	if err != nil {
		return nil, err
	}

	if cachePath != "" {
		writeCache(cachePath, data)
	}

	return doc, nil
}

func (l *Loader) fetch(url string) ([]byte, error) {
	// A copy, never the caller's client: the redirect policy below belongs to
	// this fetch, and writing it into a client the caller still holds would
	// change how their other requests behave.
	client := http.Client{Timeout: fetchTimeout}
	if l.Client != nil {
		client = *l.Client
	}
	client.CheckRedirect = checkRedirect

	resp, err := client.Get(url)
	if err != nil {
		return nil, clierr.SpecLoad("fetching spec %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, clierr.SpecLoad("fetching spec %s: HTTP %s", url, resp.Status)
	}

	// One byte past the limit, so a spec of exactly maxSpecBytes still loads and
	// anything larger is distinguishable from it without a second read.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
	if err != nil {
		return nil, clierr.SpecLoad("reading spec %s: %w", url, err)
	}
	if len(data) > maxSpecBytes {
		return nil, clierr.SpecLoad("reading spec %s: larger than the %d byte limit", url, maxSpecBytes)
	}

	return data, nil
}

// checkRedirect bounds the hops a spec fetch follows and keeps every one of
// them on http(s). Both matter because the bytes are cached under a hash of the
// URL the caller named: a redirect is a server choosing what talaria will
// remember as that URL's spec, and a hop to file:// or another scheme would let
// it choose something the caller never made a request to.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxSpecRedirects {
		return fmt.Errorf("stopped after %d redirects", maxSpecRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect to scheme %q", req.URL.Scheme)
	}

	return nil
}

// cachePath is where the spec at url is cached. The name is the SHA-256 of the
// URL, so query strings and paths that would be illegal or colliding as
// filenames all get one stable, distinct entry.
func (l *Loader) cachePath(url string) (string, error) {
	dir := l.CacheDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "talaria", "specs")
	}

	sum := sha256.Sum256([]byte(url))

	return filepath.Join(dir, hex.EncodeToString(sum[:])), nil
}

// writeCache stores data at path. Caching is an optimisation, so every failure
// is dropped: a spec that loaded is not going to be reported as broken because
// it could not be written to disk afterwards.
func writeCache(path string, data []byte) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, cacheDirMode); err != nil {
		return
	}

	// Written to a temp file and renamed so a concurrent talaria never reads a
	// half-written spec. CreateTemp already opens at 0600; the Chmod makes that
	// a stated guarantee rather than an inherited default.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Chmod(cacheFileMode); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}

	os.Rename(tmp.Name(), path)
}
