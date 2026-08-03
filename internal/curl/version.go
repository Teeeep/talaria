package curl

import (
	"os/exec"
	"regexp"
	"strconv"
	"sync"

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

// preflightOnce keeps the version check to one subprocess per talaria run: it
// is the same binary every time, and `run` executes hundreds of calls.
var (
	preflightOnce sync.Once
	preflightErr  error
)

// preflight verifies that the curl at path is new enough, once per process.
//
// Checking the version rather than mere presence is what turns "curl: option
// --write-out: is unknown" on an older machine into a sentence that names the
// requirement — and the distribution story targets machines that are not this
// one.
func preflight(path string) error {
	preflightOnce.Do(func() {
		out, err := exec.Command(path, "--version").Output()
		if err != nil {
			preflightErr = clierr.RequestFailed("cannot run %s --version: %w", path, err)
			return
		}

		preflightErr = checkVersion(string(out))
	})

	return preflightErr
}

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
