//go:build unix

package corpus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This file is the append lock: what happens when another writer is holding it.
// The store's own tests cover what Append writes once it has it.
//
// Every wait here is asserted from a goroutine against a timer rather than
// inline, because the defect these tests exist for is an *unbounded* wait: an
// inline assertion on a lock that never comes back does not fail, it hangs the
// package until go test's ten-minute deadline kills it. A bound that is missing
// has to cost one bounded test, not the suite.
//
// The deadline cases go through lockWith rather than Append so that watching a
// wait give up costs a fraction of a second instead of lockTimeout. What Append
// contributes is the wiring — that it reaches a wait with a way out at all —
// and the cancellation case asserts that in milliseconds.
const (
	// testTimeout is the deadline the give-up cases run with: long enough that a
	// loaded machine cannot trip it before the assertion runs, short enough that
	// the suite does not notice.
	testTimeout = 300 * time.Millisecond

	// waitSlack is how long a bounded wait is given before the test calls it
	// unbounded.
	waitSlack = 20 * time.Second
)

// holdLock takes the append lock through a descriptor of its own, exactly as a
// second talaria process would, and returns the call that releases it. It is
// released at the end of the test if the test has not released it itself.
func holdLock(t *testing.T, path string) func() {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		t.Fatalf("creating the state directory: %v", err)
	}

	f, err := os.OpenFile(path+lockSuffix, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		t.Fatalf("opening the lock file: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close() //nolint:errcheck // The Flock failure is the one worth reporting.
		t.Fatalf("locking the lock file: %v", err)
	}

	// sync.Once rather than a bool: a test that releases early is releasing from
	// its own goroutine, and t.Cleanup runs the same call from another.
	var once sync.Once
	release := func() {
		once.Do(func() {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // The Close below releases it either way.
			f.Close()                                   //nolint:errcheck // Nothing was written through this descriptor.
		})
	}
	t.Cleanup(release)

	return release
}

// awaited runs one lock attempt off the test goroutine and reports what it
// returned and how long it took, or fails the test if it did not return at all.
func awaited(t *testing.T, ctx context.Context, path string, timeout time.Duration) (func(), error, time.Duration) {
	t.Helper()

	type result struct {
		unlock func()
		err    error
		spent  time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		unlock, err := lockWith(ctx, path, timeout)
		done <- result{unlock, err, time.Since(start)}
	}()

	select {
	case r := <-done:
		return r.unlock, r.err, r.spent
	case <-time.After(waitSlack):
		t.Fatalf("the lock attempt was still waiting after %s; the wait is unbounded", waitSlack)
		return nil, nil, 0
	}
}

func TestTheLockGivesUpOnAHolderThatNeverReleases(t *testing.T) {
	_, path := newStore(t)
	holdLock(t, path)

	_, err, spent := awaited(t, context.Background(), path, testTimeout)

	if err == nil {
		t.Fatal("the lock was taken while another descriptor held it")
	}
	if !strings.Contains(err.Error(), "cannot lock the history file") {
		t.Errorf("error = %q, want it to name the history lock", err)
	}
	// The message names the deadline, so an operator reading a warning can tell
	// "someone else is writing" from "the store is unreachable".
	if !strings.Contains(err.Error(), testTimeout.String()) {
		t.Errorf("error = %q, want it to name the %s deadline it gave up at", err, testTimeout)
	}
	if spent < testTimeout {
		t.Errorf("gave up after %s, want it to wait out the whole %s deadline", spent, testTimeout)
	}
}

// Ordinary contention still serialises. An append that gave up on the first
// refusal would be bounded too, and would drop history whenever two talaria
// processes overlapped — which is the case the lock exists for.
func TestTheLockWaitsOutAHolderThatReleases(t *testing.T) {
	_, path := newStore(t)
	release := holdLock(t, path)

	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()

	unlock, err, spent := awaited(t, context.Background(), path, waitSlack)
	if err != nil {
		t.Fatalf("lockWith: %v", err)
	}
	unlock()

	if spent < 50*time.Millisecond {
		t.Errorf("the lock was taken after %s, before the holder released it", spent)
	}
}

// A wait that gave up may still be granted the lock afterwards, and the
// descriptor it was granted on has to be closed. One that was not would leave
// this process holding the append lock for the rest of its life, having already
// reported that it could not take it: the next append blocks on a holder that
// is not writing anything and never will.
func TestALockGrantedAfterTheWaitGaveUpIsReleased(t *testing.T) {
	_, path := newStore(t)
	release := holdLock(t, path)

	if _, err, _ := awaited(t, context.Background(), path, testTimeout); err == nil {
		t.Fatal("the lock was taken while another descriptor held it")
	}

	// The abandoned wait is now the only thing queued on the lock. Releasing
	// hands it the lock; nothing is left to release it but the abandonment.
	release()

	unlock, err, _ := awaited(t, context.Background(), path, waitSlack)
	if err != nil {
		t.Fatalf("taking the lock after the holder released it: %v", err)
	}
	unlock()
}

func TestAppendStopsWaitingWhenTheContextIsCancelled(t *testing.T) {
	store, path := newStore(t)
	holdLock(t, path)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	type result struct {
		err   error
		spent time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		err := store.Append(ctx, Entry{Source: SourceCall, Method: "GET", URL: "https://api.example.com/pets/1"})
		done <- result{err, time.Since(start)}
	}()

	var r result
	select {
	case r = <-done:
	case <-time.After(waitSlack):
		t.Fatalf("Append was still waiting for the lock %s after the cancellation", waitSlack)
	}

	if r.err == nil {
		t.Fatal("Append succeeded while another descriptor held the lock")
	}
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("Append error = %v, want it to wrap context.Canceled", r.err)
	}
	// Ctrl-C during a contended append has to be quicker than lockTimeout, or it
	// is indistinguishable from no Ctrl-C at all.
	if r.spent >= lockTimeout {
		t.Errorf("Append took %s to notice the cancellation, want well under the %s deadline",
			r.spent, lockTimeout)
	}
}

// A cancelled context does not stop an append nothing is contending for.
// `call` records the request it was interrupted in the middle of — that entry is
// what says the request was tried — and it does so after cmd.Context() has
// already been cancelled. So the uncontended acquisition is attempted before the
// context is consulted, and only the wait is cancellable.
func TestAppendRecordsUnderACancelledContextWhenNothingHoldsTheLock(t *testing.T) {
	store, _ := newStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Append(ctx, Entry{Source: SourceCall, Method: "GET", URL: "https://api.example.com/pets/1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("store holds %d entries, want the interrupted call recorded", len(entries))
	}
}

// The lock file lives in a user-writable state directory, so what is at that
// path is not this process's decision. A directory there is refused as an
// entry-level failure naming the lock, never a panic and never a silent success.
func TestAppendReportsALockFileItCannotOpen(t *testing.T) {
	store, path := newStore(t)

	if err := os.MkdirAll(path+lockSuffix, dirMode); err != nil {
		t.Fatalf("creating the directory in the lock's place: %v", err)
	}

	err := store.Append(context.Background(),
		Entry{Source: SourceCall, Method: "GET", URL: "https://api.example.com/pets/1"})
	if err == nil {
		t.Fatal("Append succeeded with a directory where the lock file belongs")
	}
	if !strings.Contains(err.Error(), "history lock") {
		t.Errorf("Append error = %q, want it to name the history lock", err)
	}
}
