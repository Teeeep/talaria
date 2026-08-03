package corpus

// This file is the file mechanics under the Store: how the history file is
// found, read under a bound, appended to, trimmed and replaced. store.go is the
// API above it — Append, Read, Recording, Path and id assignment — and calls
// into here. The split is the one internal/curl makes between config.go and
// firewall.go: what the thing does, and how the bytes are handled.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// fileName is the store, one JSON entry per line, under the state directory.
	fileName = "history.jsonl"
	// maxPerSource is the retention cap, applied per Source rather than over the
	// whole file: a single `run` over a large spec would otherwise evict every
	// interactive call a session had made.
	maxPerSource = 1000

	// maxEntryBytes is the longest line a reader will hold. An entry newBody
	// wrote carries at most two MaxBody-sized bodies, base64 costing a third
	// more, plus a URL and headers, so this is roughly four times the largest
	// line this tool produces — and a line past it is skipped rather than
	// allocated, because its length is a number in a file anyone may edit.
	maxEntryBytes = 256 << 10
	// maxStoreBytes is the whole file the reader will consume. Past it the file
	// has stopped being the store trim maintains and the read is refused, which
	// leaves a file to move aside; loading it and being killed for the memory
	// leaves nothing.
	maxStoreBytes = 64 << 20

	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600
)

// write appends one line, creating the directory and the file with the modes
// DESIGN.md §5a requires of talaria's highest-risk artifact.
func write(path string, line []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("cannot create the history directory: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone and gives a new one
	// dirMode masked by the umask. The Chmod makes 0700 a guarantee rather than
	// an inherited default.
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("cannot secure the history directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("cannot open the history file: %w", err)
	}
	if err := f.Chmod(fileMode); err != nil {
		f.Close() //nolint:errcheck // The Chmod failure is the one worth reporting.
		return fmt.Errorf("cannot secure the history file: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close() //nolint:errcheck // The Write failure is the one worth reporting.
		return fmt.Errorf("cannot write the history entry: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("cannot write the history entry: %w", err)
	}

	return nil
}

// trim enforces the per-source cap, rewriting the file only when some source is
// over it. The common append leaves the file alone.
func trim(path string) error {
	data, err := readStore(path)
	if err != nil {
		return err
	}

	all := lines(data)
	sources := make([]Source, len(all))
	readable := make([]bool, len(all))
	counts := map[Source]int{}
	for i, line := range all {
		head, ok := lineHead(line)
		sources[i], readable[i] = head.Source, ok
		if ok {
			counts[head.Source]++
		}
	}

	drop := map[Source]int{}
	for source, n := range counts {
		if n > maxPerSource {
			drop[source] = n - maxPerSource
		}
	}
	if len(drop) == 0 {
		return nil
	}

	// Oldest first: the file is in append order, so dropping the first n lines
	// of an over-cap source keeps the newest maxPerSource of it, and every other
	// source stays exactly where it was.
	var kept bytes.Buffer
	for i, line := range all {
		if !readable[i] {
			// The rewrite is the one moment a line nothing can parse can be
			// dropped without losing anything Read would have returned.
			continue
		}
		if drop[sources[i]] > 0 {
			drop[sources[i]]--
			continue
		}
		kept.Write(line)
		kept.WriteByte('\n')
	}

	return replace(path, kept.Bytes())
}

// replace swaps the file's contents atomically, so a reader never sees a
// half-written store and a crash mid-trim leaves the old file intact.
func replace(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".history-*")
	if err != nil {
		return fmt.Errorf("cannot stage the trimmed history: %w", err)
	}
	// A no-op once the rename has happened, and the cleanup for every path where
	// it has not.
	defer os.Remove(tmp.Name()) //nolint:errcheck // Best effort; the file is 0600 in the state dir.

	// CreateTemp already opens at 0600; the Chmod makes that a stated guarantee
	// rather than an inherited default.
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close() //nolint:errcheck // The Chmod failure is the one worth reporting.
		return fmt.Errorf("cannot secure the trimmed history: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // The Write failure is the one worth reporting.
		return fmt.Errorf("cannot write the trimmed history: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write the trimmed history: %w", err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("cannot replace the history file: %w", err)
	}

	return nil
}

// entryHead is the part of a stored line the store itself consults: the source
// the retention cap counts by, and the id a new one must not collide with.
type entryHead struct {
	ID     string `json:"id"`
	Source Source `json:"source"`
}

// lineHead reads that head off a stored line, reporting whether the line parses
// at all. Trimming and id assignment work from the raw bytes rather than from
// decoded entries so a rewrite reproduces what was written, byte for byte.
func lineHead(line []byte) (entryHead, bool) {
	var head entryHead
	if err := json.Unmarshal(line, &head); err != nil {
		return entryHead{}, false
	}

	return head, true
}

// readStore reads the whole history file, refusing one past maxStoreBytes.
//
// The bound is on the bytes actually read, not on what os.Stat reports: a store
// that is a symlink to /dev/zero or a FIFO stats as empty and reads forever, and
// this file is user-writable by design. A caller that only wants what it can
// parse — storedIDs — treats the refusal like any other unreadable file; the
// ones that report to a human pass the error up, because a store this size has
// stopped being the file trim maintains and moving it aside is a decision only
// its owner can make.
func readStore(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // Read-only; the read error is the one worth reporting.

	data, err := io.ReadAll(io.LimitReader(f, maxStoreBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read the history file: %w", err)
	}
	if len(data) > maxStoreBytes {
		return nil, fmt.Errorf("the history file at %s is larger than the %d bytes talaria will read; move it aside", path, maxStoreBytes)
	}

	return data, nil
}

// lines splits the store into the non-empty lines a reader will hold. A line
// past maxEntryBytes is left out exactly as an unparseable one is: its length is
// a number in a file anyone may edit, and the entries recorded after it must
// still come back (§5a — a hostile entry fails that entry, never the process).
func lines(data []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 || len(line) > maxEntryBytes {
			continue
		}
		out = append(out, line)
	}

	return out
}

// stateDir is $XDG_STATE_HOME/talaria, falling back to ~/.local/state/talaria.
// The lookup is spelled out because Go's os package has no UserStateDir; the
// fallback is the one the XDG spec itself names. A relative XDG_STATE_HOME is
// ignored, as the spec requires.
func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "talaria"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".local", "state", "talaria"), nil
}
