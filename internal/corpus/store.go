package corpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// EnvHistory switches recording off wherever talaria runs. It is read here
// rather than layered in with the profile settings because it is the operator's
// override: `history.enabled` is what a user configured, and this is what
// someone auditing a machine sets, so it wins over both.
const EnvHistory = "TALARIA_HISTORY"

const (
	// fileName is the store, one JSON entry per line, under the state directory.
	fileName = "history.jsonl"
	// maxPerSource is the retention cap, applied per Source rather than over the
	// whole file: a single `run` over a large spec would otherwise evict every
	// interactive call a session had made.
	maxPerSource = 1000

	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600
)

// Store is the append-only history file.
//
// Failures from its methods are deliberately unclassified: a call that reached
// the server and came back is a success, and whether talaria managed to write
// the fact down afterwards is the caller's decision to report, not a reason to
// change the command's exit code.
type Store struct {
	dir     string
	enabled bool
}

// New returns a store writing under dir, recording only when enabled.
//
// An empty dir means the default state directory; tests pass a temporary one.
// The enabled flag is the profile's `history.enabled` as the caller resolved it
// — this package never reads the config itself, so that the twin can use the
// same store without one.
func New(dir string, enabled bool) *Store {
	return &Store{dir: dir, enabled: enabled}
}

// Path is the history file's location.
func (s *Store) Path() (string, error) {
	dir := ""
	if s != nil {
		dir = s.dir
	}

	if dir == "" {
		base, err := stateDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate the talaria state directory: %w", err)
		}
		dir = base
	}

	return filepath.Join(dir, fileName), nil
}

// Recording reports whether Append will write anything, so a caller can skip
// building an entry it is only going to throw away.
func (s *Store) Recording() bool {
	if s == nil || !s.enabled {
		return false
	}

	return !off(os.Getenv(EnvHistory))
}

// Append records one entry and applies the retention cap.
//
// With recording off it does nothing at all — no file, no directory. An opt-out
// that still left a history file behind would not be one.
func (s *Store) Append(e Entry) error {
	if !s.Recording() {
		return nil
	}

	path, err := s.Path()
	if err != nil {
		return err
	}

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("cannot encode the history entry: %w", err)
	}

	if err := write(path, append(line, '\n')); err != nil {
		return err
	}

	return trim(path)
}

// Read returns every readable entry, oldest first.
//
// It does not consult the recording setting: history written before recording
// was switched off is still history, and reading it puts nothing new on disk.
// A store that was never written is empty rather than an error.
func (s *Store) Read() ([]Entry, error) {
	path, err := s.Path()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the history file: %w", err)
	}

	var entries []Entry
	for _, line := range lines(data) {
		var entry Entry
		// A line that will not parse is skipped rather than fatal. The file is
		// appended to by every call, so a process killed mid-write leaves a
		// partial last line, and losing every earlier entry to it would make the
		// store fragile exactly when something has already gone wrong.
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

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
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read the history file: %w", err)
	}

	all := lines(data)
	sources := make([]Source, len(all))
	readable := make([]bool, len(all))
	counts := map[Source]int{}
	for i, line := range all {
		source, ok := lineSource(line)
		sources[i], readable[i] = source, ok
		if ok {
			counts[source]++
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

// lineSource reads just the source off a stored line, reporting whether the
// line parses at all. Trimming works from the raw bytes rather than from decoded
// entries so a rewrite reproduces what was written, byte for byte.
func lineSource(line []byte) (Source, bool) {
	var head struct {
		Source Source `json:"source"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return "", false
	}

	return head.Source, true
}

// lines splits the store into its non-empty lines.
func lines(data []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
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

// off reports whether a TALARIA_HISTORY value switches recording off. The
// variable has no other job, so every plausible spelling of "no" counts;
// anything else — including an empty value — leaves the caller's setting in
// force. Guessing wrong in this direction records nothing, which is the safe
// way to be wrong about a store of credentials-adjacent data.
func off(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "0", "false", "no":
		return true
	}

	return false
}
