//go:build unix

package corpus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockTimeout bounds the wait for the append lock. It is the way out that does
// not need anybody at the keyboard: a holder that never releases — a wedged
// talaria, a stale NFS mount — used to block every later append forever, with no
// output and, because Go installs its signal handlers with SA_RESTART, no way to
// Ctrl-C out of it either.
//
// A minute, because giving up costs a history entry and the legitimate holder
// can be slow: one append over a store at maxStoreBytes reads it, rewrites it
// and scans it again, so a few seconds under the lock is ordinary and several
// writers multiply it. Reaching this means the holder is not making progress,
// not that it is busy — and a human has the faster way out either way.
const lockTimeout = time.Minute

// abandonTimeout bounds the second wait: how long a wait that already gave up
// keeps its descriptor open hoping the flock it queued will still be granted.
const abandonTimeout = time.Minute

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
//
// An append that has to wait for another writer still waits: one that gave up
// on the first refusal would drop history the caller was told had been
// recorded, which is the bug this lock exists to prevent. What the wait now has
// is two ways out — ctx and lockTimeout — so waiting is not the same as
// blocking forever.
func lock(ctx context.Context, path string) (func(), error) {
	return lockWith(ctx, path, lockTimeout)
}

// lockWith is lock with the deadline named rather than defaulted, so a test can
// watch a wait give up without waiting out lockTimeout to see it.
func lockWith(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, fmt.Errorf("cannot create the history directory: %w", err)
	}

	f, err := os.OpenFile(path+lockSuffix, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, fmt.Errorf("cannot open the history lock: %w", err)
	}

	release := func() {
		// Close alone would release the lock; the unlock is spelled out so the
		// release does not rest on that being remembered.
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // The Close below releases it either way.
		f.Close()                                   //nolint:errcheck // Nothing was written through this descriptor.
	}

	// The uncontended acquisition happens before ctx is consulted, and this is
	// the reason the non-blocking attempt is separate from the wait: `call`
	// records the request it was interrupted in the middle of, so recordCall
	// runs with the signal context already cancelled. Refusing the lock there
	// would turn every Ctrl-C into a lost entry for a call that was made.
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return release, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close() //nolint:errcheck // The Flock failure is the one worth reporting.
		return nil, fmt.Errorf("cannot lock the history file: %w", err)
	}

	if err := waitForLock(ctx, f, timeout); err != nil {
		return nil, fmt.Errorf("cannot lock the history file: %w", err)
	}

	return release, nil
}

// waitForLock blocks in flock until it is granted, ctx is cancelled or the
// timeout expires, and closes f on every path but the first.
//
// The flock runs in its own goroutine because one already in progress cannot be
// interrupted — the kernel returns when the holder releases, which may be never,
// and SA_RESTART means a signal will not shake it loose. Polling LOCK_NB against
// a sleep would need no goroutine, but it is unfair in exactly the shape this
// store sees: a writer appending in a loop re-takes the lock in microseconds
// while every other waiter is mid-sleep, and the starved one hits the deadline
// during ordinary contention.
//
// A wait that gave up may still be granted the lock afterwards, so the descriptor
// is handed to abandon, which closes it — releasing the lock — when the flock
// returns or when abandonTimeout expires, whichever comes first. The channel is
// buffered so neither goroutine can be left blocked on the send, and the caller
// is a process on its way to reporting a failure.
func waitForLock(ctx context.Context, f *os.File, timeout time.Duration) error {
	// The descriptor is resolved here rather than inside the goroutine: abandon
	// closes f on a deadline now, and os.File.Fd reads the state Close writes, so
	// leaving the call in the goroutine is a data race the race detector sees.
	// The blocking flock only ever needs the number.
	fd := int(f.Fd())

	taken := make(chan error, 1)
	go func() { taken <- syscall.Flock(fd, syscall.LOCK_EX) }()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	select {
	case err := <-taken:
		if err != nil {
			f.Close() //nolint:errcheck // The Flock failure is the one worth reporting.
			return err
		}
		return nil
	case <-ctx.Done():
		go abandon(f, taken)
		return ctx.Err()
	case <-deadline.C:
		go abandon(f, taken)
		return fmt.Errorf("another process has held it for longer than %s", timeout)
	}
}

// abandon waits out a flock nothing is listening for any more and drops the
// descriptor, so a lock granted after the wait gave up is not one this process
// holds until it exits.
//
// The wait itself is bounded, because the flock may be granted late, at exit, or
// never: a holder that is wedged for good leaves this queued behind it forever,
// and a long-lived embedder appending on a schedule would accumulate one
// descriptor per abandoned wait until it ran out of them. Past abandonTimeout
// the descriptor is dropped anyway —
// TestALockGrantedOnADroppedDescriptorIsNotHeld is why that is safe: the flock
// is still queued on the open file description, and the grant does not outlive
// the syscall that was waiting for it, so a later grant releases as immediately
// as the timely one does.
//
// What is not reclaimed is the goroutine. A flock already in progress cannot be
// cancelled — closing the descriptor does not wake it, and SA_RESTART means a
// signal will not either — so it stays parked until the kernel answers. That
// costs a stack rather than a descriptor, and nothing this package can write
// makes it shorter.
func abandon(f *os.File, taken <-chan error) {
	abandonWith(f, taken, abandonTimeout)
}

// abandonWith is abandon with the deadline named rather than defaulted, exactly
// as lockWith is to lock: a test watches the second wait give up without waiting
// out abandonTimeout to see it.
func abandonWith(f *os.File, taken <-chan error, timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	select {
	case <-taken:
	case <-deadline.C:
	}
	f.Close() //nolint:errcheck // Closing is what releases the lock; there is nobody left to report to.
}
