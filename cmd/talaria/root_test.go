package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Teeeep/talaria/internal/clierr"
)

// signalChildEnv puts the test binary into the child mode TestASecondSignal
// TerminatesTheProcess drives. It is read in TestMain, before any fixture
// directory exists, because this child is meant to be killed by a signal and
// would never run its own cleanup.
const signalChildEnv = "TALARIA_TEST_SIGNAL_CHILD"

// runSignalChild is the process under test: it installs the same signal context
// run installs, reports that it is ready, and then goes deaf — the shape of a
// talaria stuck in a read no context reaches (os.ReadFile on a FIFO, curl not
// yet reaped). The first signal must cancel; the second must kill, because that
// is the only remaining way out for the human at the terminal.
//
// It sleeps rather than blocking on a channel: a goroutine parked forever on a
// bare channel receive trips Go's deadlock detector, and the child would exit on
// its own with the test proving nothing.
func runSignalChild() {
	ctx, stop := signalContext()
	defer stop()

	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println("cancelled")

	time.Sleep(time.Minute)
	fmt.Println("survived")
}

// TestASecondSignalTerminatesTheProcess is finding 14's other half. The first
// SIGINT cancels the context; signal.NotifyContext then leaves its registration
// live for the rest of the process's life, so every later SIGINT is delivered to
// a channel nobody reads and default termination stays disabled — Ctrl-C twice,
// three times, and the process sits there. Only `kill -9` gets out.
//
// It runs in a child because the assertion is that the signal kills the process,
// and this process is the test suite.
func TestASecondSignalTerminatesTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT delivery and wait-status inspection are POSIX")
	}

	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), signalChildEnv+"=wedge")

	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	// The child that fails this test is one that cannot be signalled at all, so
	// cleanup is the signal it cannot catch.
	t.Cleanup(func() { child.Process.Kill() }) //nolint:errcheck // Already dead on the happy path.

	lines := readLines(stdout)
	awaitLine(t, lines, "ready")

	if err := child.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("first SIGINT: %v", err)
	}
	awaitLine(t, lines, "cancelled")

	// Repeated rather than sent once: the disposition is restored by the
	// goroutine signalContext starts, which races with the child's own print.
	// A human hammering Ctrl-C is the behaviour being reproduced anyway.
	waited := make(chan error, 1)
	go func() { waited <- child.Wait() }()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)

	for {
		select {
		case err := <-waited:
			assertKilledBy(t, err, syscall.SIGINT)
			return
		case <-tick.C:
			child.Process.Signal(os.Interrupt) //nolint:errcheck // A dead child is the pass condition.
		case <-deadline:
			t.Fatal("the child outlived 10s of SIGINTs: the first signal left the handler installed, " +
				"so every later one is swallowed and Ctrl-C cannot end the process")
		}
	}
}

// assertKilledBy asserts that the error a child's Wait returned is that signal
// terminating it, not an ordinary exit. An exit status is the failure to watch
// for here: it would mean the child ran to the end of its sleep, unsignalled.
func assertKilledBy(t *testing.T, err error, want syscall.Signal) {
	t.Helper()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child Wait = %v, want it killed by %v", err, want)
	}

	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("wait status is %T, want syscall.WaitStatus", exit.Sys())
	}
	if !status.Signaled() {
		t.Fatalf("child exited with status %d, want it killed by %v", status.ExitStatus(), want)
	}
	if got := status.Signal(); got != want {
		t.Errorf("child killed by %v, want %v", got, want)
	}
}

// readLines drains r into a channel so a test can wait for what the child said
// with a deadline rather than blocking on a read that may never return.
func readLines(r io.Reader) <-chan string {
	out := make(chan string, 8)
	go func() {
		defer close(out)
		scan := bufio.NewScanner(r)
		for scan.Scan() {
			out <- scan.Text()
		}
	}()

	return out
}

func awaitLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()

	select {
	case got, ok := <-lines:
		if !ok {
			t.Fatalf("the child exited before printing %q", want)
		}
		if got != want {
			t.Fatalf("child said %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the child did not print %q within 10s", want)
	}
}

// TestSIGINTEndsACallWaitingOnStdin is the wiring assertion for the two halves
// above: the context run installs has to reach the one read that waits on
// something outside this process. `--body -` on a pipe nobody writes to and
// nobody closes is a talaria that would otherwise sit there forever — and it is
// what a `kill -TERM` from systemd or CI meets.
//
// It runs the real command in a child because the hang under test is the
// process's, not a package's.
func TestSIGINTEndsACallWaitingOnStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT delivery and wait-status inspection are POSIX")
	}

	// A GET, so the mutation guard does not refuse the call before the body is
	// ever read; --dry-run, so nothing but the binding is under test.
	child := exec.Command(os.Args[0],
		"call", "getPet", "--spec", "testdata/call.yaml", "--param", "petId=42",
		"--dry-run", "--body", "-")
	child.Env = append(os.Environ(), signalChildEnv+"=run")

	// Held open for the lifetime of the test: nothing ever closes the write end,
	// so the child's read can only end by being cancelled. What is written to it
	// is the readiness handshake below, not a body anything reads back.
	stdin, hold, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer hold.Close()  //nolint:errcheck // Test cleanup.
	defer stdin.Close() //nolint:errcheck // Test cleanup.
	child.Stdin = stdin

	var stderr bytes.Buffer
	child.Stderr = &stderr

	if err := child.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	t.Cleanup(func() { child.Process.Kill() }) //nolint:errcheck // Already dead on the happy path.

	// One signal, sent once the child is demonstrably inside the read it has to
	// interrupt. That is strictly later than run installing the handler, and the
	// window matters: run calls curl.SweepStale — an os.ReadDir over TMPDIR and
	// os.RemoveAll calls — *before* signalContext, so a signal timed by a sleep
	// can land while the default disposition is still in force. The child then
	// dies of SIGINT and assertExitedWith reports the same failure a real "the
	// context does not reach the stdin read" regression produces. A second signal
	// would prove nothing here: it kills by default, which is the other test's
	// subject.
	awaitStdinDrain(t, hold)
	if err := child.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- child.Wait() }()

	select {
	case err := <-waited:
		// Exit 1: an interruption is a request that did not complete, which is
		// what AGENT.md promises a Ctrl-C produces. Exit 2 would read as a bad
		// command line.
		assertExitedWith(t, err, int(clierr.CodeRequestFailed))
	case <-time.After(10 * time.Second):
		t.Fatal("the child outlived 10s after SIGINT: --body - blocks in io.ReadAll, " +
			"which no signal reaches")
	}

	if !strings.Contains(stderr.String(), "stdin") {
		t.Errorf("stderr = %q, want it to name the stdin read that was cancelled", stderr.String())
	}
}

// stdinFill is how much awaitStdinDrain writes into the child's stdin. It has
// to exceed the pipe's capacity by enough that a completed write means bytes
// were consumed rather than merely buffered: a Linux pipe holds 64 KiB by
// default, a Darwin one at most that.
const stdinFill = 1 << 20

// awaitStdinDrain blocks until the child is inside the stdin read, and is the
// synchronisation this test used to get from a 500ms sleep. A pipe write of
// stdinFill bytes cannot return until the reader has taken everything past the
// pipe's capacity, so a write that completed is proof the child reached
// io.ReadAll — where a sleep only asserts about the scheduler, and on a loaded
// box either flakes or signals a process that has not installed its handler
// yet. Nothing closes the write end, so the read still cannot end on its own.
func awaitStdinDrain(t *testing.T, w io.Writer) {
	t.Helper()

	wrote := make(chan error, 1)
	go func() {
		_, err := w.Write(make([]byte, stdinFill))
		wrote <- err
	}()

	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("writing %d bytes to the child's stdin: %v", stdinFill, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the child consumed less than %d bytes of stdin in 10s: it never reached "+
			"the --body - read this test signals", stdinFill)
	}
}

// assertExitedWith asserts an ordinary exit with the given code. A signal death
// is a failure even though the process did stop: it would mean the default
// disposition ended the call, not the cancelled context, and the test would pass
// against a stdin read that still cannot be interrupted.
func assertExitedWith(t *testing.T, err error, want int) {
	t.Helper()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child Wait = %v, want exit status %d", err, want)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		t.Fatalf("child was killed by %v, want it to exit %d on its own", status.Signal(), want)
	}
	if got := exit.ExitCode(); got != want {
		t.Errorf("child exited %d, want %d", got, want)
	}
}

// TestACancelledContextEndsASpecFetch is the wiring assertion for the loader's
// half: a remote spec fetch is a wait on something outside this process, exactly
// as the stdin read is, so the context every command runs under has to reach it.
// It fails against a loadSpec that passes context.Background() — the fetch then
// sits on its own thirty-second deadline with the caller already gone.
//
// In-process, driving runContext: the subject is which context loadSpec hands
// the loader, not the signal disposition the child tests cover.
func TestACancelledContextEndsASpecFetch(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	accepted := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(accepted)
		<-release
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runContext(ctx, []string{"list", "--spec", server.URL + "/openapi.yaml"}, &stdout, &stderr)
	}()

	<-accepted
	cancel()

	select {
	case code := <-done:
		// Exit 1, the same code an interrupted call gets: the caller stopped, the
		// spec is not broken. Exit 3 would tell an agent to stop retrying it.
		if want := int(clierr.CodeRequestFailed); code != want {
			t.Errorf("exit = %d, want %d; stderr = %q", code, want, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command outlived 10s after its context was cancelled: the spec fetch " +
			"runs on its own deadline, which no signal reaches")
	}
}

// TestTheSignalContextCancelsOnTheFirstSignal is the in-process half: the
// context signalContext returns is one this process's own SIGINT cancels. It
// asserts nothing about disposition — that is what the child is for — but it is
// what keeps the two halves from both passing on a context nobody cancels.
func TestTheSignalContextCancelsOnTheFirstSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT delivery is POSIX")
	}

	ctx, stop := signalContext()
	defer stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT to self: %v", err)
	}

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SIGINT did not cancel the context")
	}
}
