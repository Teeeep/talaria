package main

import (
	"fmt"
	"os"
	"testing"
)

// TestMain isolates the whole package from the developer's own configuration.
// config.DefaultPath reads $XDG_CONFIG_HOME, so a real ~/.config/talaria/config.yaml
// would otherwise decide which profiles exist and what `call` redacts — and a
// suite whose result depends on the machine it runs on is not a suite. Tests
// that want a config file point the variable at one with t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "talaria-config-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolating the config directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		fmt.Fprintf(os.Stderr, "isolating the config directory: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	os.RemoveAll(dir) //nolint:errcheck // Best effort; it is a temp dir.
	os.Exit(code)
}
