package curl

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The prefixes talaria gives its own temp files, matching what newCapture and
// document.tempFile pass to the os package. They are the only names the sweep
// will remove: everything else in TMPDIR belongs to somebody else.
const (
	capturePrefix = "talaria-call-"
	bodyPrefix    = "talaria-body-"
)

// staleGrace is how old one of talaria's temp files has to be before the sweep
// takes it.
//
// A day is far longer than any request: the default max-time is 30s and even a
// generous --timeout is minutes. The margin is deliberate, because the cost of
// the two errors is not symmetric — sweeping a live call's capture out from
// under it breaks a request that was working, while leaving a dead one an extra
// hour costs nothing that the SIGKILL had not already cost.
const staleGrace = 24 * time.Hour

// SweepStale removes talaria's own leftover temp files from TMPDIR.
//
// It is the backstop for SIGKILL, the one signal no handler can catch: cleanup
// there cannot run at all, so a capture directory holding the *raw, unredacted*
// response — and the request body, which may carry a credential the user put in
// it — would otherwise sit in TMPDIR until the machine was rebooted. Every
// other way of dying is handled by the context reaching curl and the defers
// running.
//
// It is silent and best-effort by design. Nobody ran talaria to have their temp
// directory tidied, so a permission error on somebody else's file, or a TMPDIR
// that cannot be read at all, is not worth a word on stderr.
func SweepStale() {
	dir := os.TempDir()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-staleGrace)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, capturePrefix) && !strings.HasPrefix(name, bodyPrefix) {
			continue
		}

		// Info is lstat here, so a symlink somebody planted under one of these
		// names is judged and removed as the link it is, never followed.
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}

		os.RemoveAll(filepath.Join(dir, name)) //nolint:errcheck // Best effort.
	}
}
