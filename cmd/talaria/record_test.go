//go:build unix

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Teeeep/talaria/internal/corpus"
)

// The append lock is bounded, so an append can now fail where it used to wait.
// That cost is one warning line, never a silent loss: recordCall downgrades the
// failure rather than changing the exit code, and this is what makes the
// downgrade visible. An operator who never sees the line has no way to discover
// that history stopped recording.
func TestARecordThatCouldNotTakeTheLockWarns(t *testing.T) {
	t.Setenv(corpus.EnvHistory, "")

	store := corpus.New(filepath.Join(t.TempDir(), "state"), true)
	path, err := store.Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}

	// internal/corpus locks a sibling of the history file. Holding it here is a
	// second talaria process; if that name ever changes, this takes an
	// uncontended lock and the missing warning fails the test.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the state directory: %v", err)
	}
	held, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening the lock file: %v", err)
	}
	defer held.Close() //nolint:errcheck // Closing releases the lock; the test is over by then.
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("locking the lock file: %v", err)
	}

	// Cancelled, so the bounded wait ends at once rather than in five seconds.
	// It is the same give-up either way — the entry is not written.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stderr bytes.Buffer
	recordCall(ctx, &stderr, store, corpus.SourceCall, nil, nil, corpus.Redactors{})

	if !strings.Contains(stderr.String(), "the call was not recorded in history") {
		t.Errorf("stderr = %q, want a warning that the call was not recorded", stderr.String())
	}
}
