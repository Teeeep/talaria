//go:build !unix

package curl

import "os/exec"

// isolate is a no-op where there is no process group to put curl in.
//
// Cancellation still kills curl itself, through exec's own default Cancel;
// what is missing is only the reach into anything curl spawned. Keeping this a
// build-tagged no-op rather than a new dependency means the package still
// compiles everywhere, as internal/corpus does for flock.
func isolate(*exec.Cmd) {}
