package curl

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/Teeeep/talaria/internal/clierr"
)

// minVersion is the hard floor from DESIGN.md §3.4. `--write-out '%{json}'`
// landed in curl 7.70, and the executor's whole three-channel design rests on
// it, so an older curl is refused rather than worked around.
const minVersion = "7.70"

const (
	minMajor = 7
	minMinor = 70
)

// versionLine matches the `curl 8.14.1 (…) libcurl/…` banner. Only the first
// two components decide the floor; distributions append their own suffixes to
// the patch component and none of them matter here.
var versionLine = regexp.MustCompile(`(?m)^curl\s+(\d+)\.(\d+)`)

// preflightTimeout bounds the version check on its own, independently of the
// call's max-time: this subprocess runs before talaria has done anything, and a
// `curl` on PATH that blocks — a wrapper script on a stalled NFS mount, one
// waiting on a terminal — must not be able to wedge the process (§3.1, "never
// prompt, never page"). Five seconds is far past what printing a banner costs
// and far short of the thirty a request gets.
const preflightTimeout = 5 * time.Second

// maxVersionBytes bounds what `curl --version` may put in memory. A real banner
// is a few hundred bytes; anything past this is discarded rather than buffered,
// so a `curl` that writes gigabytes on --version cannot exhaust the process
// through the one channel it is trusted with.
const maxVersionBytes = 64 << 10

// preflightCache memoises the verdict per binary, keyed by path so that a
// PATH change between calls cannot be answered with the previous curl's
// verdict. Only a verdict *about the binary* is cached — a version that passed
// or failed the floor. A run that produced no banner at all is not: a timeout,
// a cancelled context or a binary caught mid-upgrade is transient, and the
// sync.Once this replaced let one of them poison every later call in the
// process.
var (
	preflightMu    sync.Mutex
	preflightCache = map[string]error{}
)

// preflight verifies that the curl at path is new enough, once per binary.
//
// Checking the version rather than mere presence is what turns "curl: option
// --write-out: is unknown" on an older machine into a sentence that names the
// requirement — and the distribution story targets machines that are not this
// one.
func preflight(ctx context.Context, path string) error {
	return preflightWith(ctx, path, preflightTimeout)
}

// preflightWith is preflight with the deadline named rather than defaulted.
func preflightWith(ctx context.Context, path string, timeout time.Duration) error {
	preflightMu.Lock()
	cached, ok := preflightCache[path]
	preflightMu.Unlock()
	if ok {
		return cached
	}

	out, runErr := runVersion(ctx, path, timeout)

	// The banner is the answer, so how the process ended only matters when it
	// did not give one: a wrapper that exits non-zero after printing it, or
	// leaves a child holding the stdout pipe open past WaitDelay, has still said
	// which curl this is.
	if !versionLine.MatchString(out) {
		if runErr != nil {
			return runErr
		}

		return checkVersion(out)
	}

	verdict := checkVersion(out)

	preflightMu.Lock()
	preflightCache[path] = verdict
	preflightMu.Unlock()

	return verdict
}

// runVersion executes `path --version` and returns what it wrote, bounded in
// time by timeout and in memory by maxVersionBytes.
func runVersion(ctx context.Context, path string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, path, "--version")
	// The same three bounds every other curl in this package gets: the context
	// kills it, WaitDelay bounds the reaping when a grandchild still holds the
	// pipe, and the kill goes to the whole process group.
	cmd.WaitDelay = killGrace
	isolate(cmd)

	var out boundedBuffer
	cmd.Stdout = &out

	err := cmd.Run()
	if err == nil {
		return out.String(), nil
	}

	// Which deadline ended it is what the reader needs: the caller's own
	// cancellation is reported in the sentence ExecuteWith already uses for it,
	// so a Ctrl-C during the preflight does not read as a slow binary.
	switch {
	case ctx.Err() != nil:
		return out.String(), clierr.RequestFailed("the request was cancelled before it completed")
	case runCtx.Err() != nil:
		return out.String(), clierr.RequestFailed(
			"cannot check the curl version: %s --version did not answer within %s", path, timeout)
	default:
		return out.String(), clierr.RequestFailed("cannot run %s --version: %w", path, err)
	}
}

// boundedBuffer keeps the first maxVersionBytes written to it and discards the
// rest, reporting every write as accepted so the subprocess is never handed an
// EPIPE for writing more than talaria wanted. The banner is the first line, so
// truncation loses nothing a working curl needed to say — and a curl whose
// version line is past the bound is reported as one whose version cannot be
// determined.
type boundedBuffer struct{ b []byte }

func (w *boundedBuffer) Write(p []byte) (int, error) {
	if room := maxVersionBytes - len(w.b); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		w.b = append(w.b, p[:room]...)
	}

	return len(p), nil
}

func (w *boundedBuffer) String() string { return string(w.b) }

// checkVersion reports whether the output of `curl --version` describes a curl
// at or above the floor.
func checkVersion(output string) error {
	match := versionLine.FindStringSubmatch(output)
	if match == nil {
		return clierr.RequestFailed(
			"cannot determine the curl version; talaria needs curl >= %s for --write-out '%%{json}'",
			minVersion,
		)
	}

	// The regexp matched \d+, so these cannot fail on anything but an
	// implausibly long number, which strconv reports as out of range — and a
	// curl with a 20-digit major version is above the floor either way.
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return nil
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return nil
	}

	if major > minMajor || (major == minMajor && minor >= minMinor) {
		return nil
	}

	return clierr.RequestFailed(
		"curl %d.%d is too old; talaria needs curl >= %s for --write-out '%%{json}'",
		major, minor, minVersion,
	)
}
