//go:build unix

package curl

import (
	"os/exec"
	"syscall"
)

// isolate puts curl in a process group of its own and makes the context's
// cancellation signal that whole group rather than curl alone.
//
// exec's default Cancel kills the one process it started. curl can have
// children of its own — an ssh for a `socks5h://` proxy, a helper for an
// unusual scheme — and those are what get reparented to init and left running
// with the credential still in flight. Signalling `-pgid` takes the tree.
//
// The group also stops a Ctrl-C in a terminal from reaching curl twice: the
// shell sends SIGINT to the foreground group, and curl is no longer in it, so
// the only thing that stops curl is talaria deciding to stop it — after which
// talaria's own defers still run.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Cancel = func() error {
		// Start failed, so there is no group to signal.
		if cmd.Process == nil {
			return nil
		}

		// Setpgid with no Pgid makes the child its own group leader, so its pid
		// is the group id and the negative of it addresses the group.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// The group is already gone, or was never formed; the single-process
			// kill is what exec would have done anyway.
			return cmd.Process.Kill()
		}

		return nil
	}
}
