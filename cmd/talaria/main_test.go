package main

import (
	"fmt"
	"os"
	"testing"
)

// TestMain isolates the whole package from the developer's own configuration
// and state. config.DefaultPath reads $XDG_CONFIG_HOME, so a real
// ~/.config/talaria/config.yaml would otherwise decide which profiles exist and
// what `call` redacts — and a suite whose result depends on the machine it runs
// on is not a suite. $XDG_STATE_HOME is isolated for the stronger reason that
// `call` *writes* there: a test run must not append to the developer's own
// history file. Tests that want a config file, or that assert on what was
// recorded, point the variables at their own directory with t.Setenv.
func TestMain(m *testing.M) {
	// The signal tests re-exec this binary. Both children are meant to be ended
	// by a signal, so neither reaches the cleanup below; they branch before
	// anything is created, so there is nothing to leave behind. "run" is the
	// real command tree with the arguments the parent passed, which is the only
	// way to assert on the process's own signal handling.
	switch os.Getenv(signalChildEnv) {
	case "wedge":
		runSignalChild()
		return
	case "run":
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}

	dirs := map[string]string{
		"XDG_CONFIG_HOME": "talaria-config-*",
		"XDG_STATE_HOME":  "talaria-state-*",
	}

	var created []string
	for name, pattern := range dirs {
		dir, err := os.MkdirTemp("", pattern)
		if err != nil {
			fmt.Fprintf(os.Stderr, "isolating %s: %v\n", name, err)
			os.Exit(1)
		}
		created = append(created, dir)

		if err := os.Setenv(name, dir); err != nil {
			fmt.Fprintf(os.Stderr, "isolating %s: %v\n", name, err)
			os.Exit(1)
		}
	}

	code := m.Run()

	// Cleanup is explicit rather than deferred: os.Exit does not run defers.
	for _, dir := range created {
		os.RemoveAll(dir) //nolint:errcheck // Best effort; it is a temp dir.
	}
	os.Exit(code)
}
