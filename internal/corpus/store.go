package corpus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EnvHistory switches recording off wherever talaria runs. It is read here
// rather than layered in with the profile settings because it is the operator's
// override: `history.enabled` is what a user configured, and this is what
// someone auditing a machine sets, so it wins over both.
const EnvHistory = "TALARIA_HISTORY"

// lockSuffix names the sibling file appends hold their advisory lock on. It is
// a separate file so the lock outlives trim's rename of the store.
const lockSuffix = ".lock"

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

// Append records one entry, gives it its stable id, and applies the retention
// cap.
//
// The id, the write and the trim are one critical section. Trim rewrites the
// whole file from a snapshot it read, so an entry appended between that read and
// the rename would be dropped silently — Append had already returned nil for it,
// so nothing would ever report it missing. The id is chosen against the same
// snapshot for the same reason: two processes that picked one concurrently could
// pick the same one. Holding the lock across all three is what makes a nil
// return mean the entry is in the store under an id nothing else holds.
//
// With recording off it does nothing at all — no file, no directory, no lock.
// An opt-out that still left a history file behind would not be one.
func (s *Store) Append(e Entry) error {
	if !s.Recording() {
		return nil
	}

	path, err := s.Path()
	if err != nil {
		return err
	}

	unlock, err := lock(path)
	if err != nil {
		return err
	}
	defer unlock()

	e.ID = uniqueID(path, e)

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("cannot encode the history entry: %w", err)
	}

	if err := write(path, append(line, '\n')); err != nil {
		return err
	}

	return trim(path)
}

// uniqueID is the id the entry goes to disk under. The caller holds the append
// lock, so what this reads off the store cannot change under it.
//
// The timestamp is the id: it needs no counter kept anywhere, it is already in
// append order, and trim rewriting the file around it cannot change it. Two
// entries can still land on the same instant — a second process, or a clock with
// less than nanosecond resolution — and a duplicate id would resolve to whichever
// entry came first and silently replay the wrong request, which is the thing the
// field exists to stop. So a taken id gets a counting suffix instead.
func uniqueID(path string, e Entry) string {
	candidate := e.ID
	if candidate == "" {
		candidate = e.Timestamp.UTC().Format(time.RFC3339Nano)
	}

	taken := storedIDs(path)
	if !taken[candidate] {
		return candidate
	}
	for n := 2; ; n++ {
		suffixed := candidate + "#" + strconv.Itoa(n)
		if !taken[suffixed] {
			return suffixed
		}
	}
}

// storedIDs is the set of ids already in the store. A file that cannot be read —
// it usually does not exist yet — holds no ids, which makes every candidate
// free. Lines written before ids existed contribute none, so an old store's
// entries never make a new id look taken.
func storedIDs(path string) map[string]bool {
	data, err := readStore(path)
	if err != nil {
		return nil
	}

	ids := map[string]bool{}
	for _, line := range lines(data) {
		if head, ok := lineHead(line); ok && head.ID != "" {
			ids[head.ID] = true
		}
	}

	return ids
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

	data, err := readStore(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
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
