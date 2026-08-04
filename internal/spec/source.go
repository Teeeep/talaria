package spec

import (
	"crypto/sha256"
	"encoding/hex"
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
	client := l.Client
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}

	// TRACKED DEBT (phase-2a task 1): this is cycle-2 finding 8 — a spec fetch is a
	// wait the signal context cannot reach, so Ctrl-C during one costs the full
	// fetchTimeout. Fixing it threads a context through Load and is a signature
	// change across every caller, so it gets a task and a test, not a drive-by.
	// Remove this waiver in the commit that fixes it; the linter is the checker.
	resp, err := client.Get(url) //nolint:noctx // phase-2a task 1
	if err != nil {
		return nil, clierr.SpecLoad("fetching spec %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, clierr.SpecLoad("fetching spec %s: HTTP %s", url, resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, clierr.SpecLoad("reading spec %s: %w", url, err)
	}

	return data, nil
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
	defer os.Remove(tmp.Name()) //nolint:errcheck // Best effort: the rename below usually wins the race with it.

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

	// Caching is an optimisation, as the doc comment says: a spec that loaded is
	// not reported broken because it could not be written down.
	os.Rename(tmp.Name(), path) //nolint:errcheck // Deliberate; see above.
}
