//go:build unix

package corpus

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lock takes the exclusive advisory lock covering the history file at path and
// returns the call that releases it.
//
// The lock lives on a sibling file rather than on the store itself because trim
// replaces the store by rename. A descriptor held on the store would end up
// guarding an inode that is no longer at that path, and the next appender would
// open the new one and find no contention at all. The sibling is never renamed,
// so every appender contends on the same inode.
//
// The lock is advisory and per open file description, so goroutines sharing a
// Store serialise against each other exactly as separate talaria processes do.
func lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, fmt.Errorf("cannot create the history directory: %w", err)
	}

	f, err := os.OpenFile(path+lockSuffix, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, fmt.Errorf("cannot open the history lock: %w", err)
	}

	// LOCK_EX without LOCK_NB: an append that has to wait for another writer is
	// correct, and one that gave up would drop history the caller was told had
	// been recorded, which is the bug this lock exists to prevent.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close() //nolint:errcheck // The Flock failure is the one worth reporting.
		return nil, fmt.Errorf("cannot lock the history file: %w", err)
	}

	return func() {
		// Close alone would release the lock; the unlock is spelled out so the
		// release does not rest on that being remembered.
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // The Close below releases it either way.
		f.Close()                                   //nolint:errcheck // Nothing was written through this descriptor.
	}, nil
}
