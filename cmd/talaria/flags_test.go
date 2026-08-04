package main

import "testing"

// Every command in DESIGN.md §4 that takes --profile or --base-url — call, run,
// auth check — gets them by inheritance. Registering either one per-command
// would shadow the persistent flag silently, so the test is that the root owns
// them and no subcommand declares its own.
func TestProfileAndBaseURLArePersistentOnRoot(t *testing.T) {
	root := newRootCmd()

	for _, flag := range []string{"profile", "base-url", "allow-host"} {
		if root.PersistentFlags().Lookup(flag) == nil {
			t.Errorf("--%s is not registered as a persistent flag on the root command", flag)
		}

		for _, child := range root.Commands() {
			if child.LocalFlags().Lookup(flag) != nil {
				t.Errorf("%s subcommand declares its own --%s; it must inherit the persistent one",
					child.Name(), flag)
			}
			if child.InheritedFlags().Lookup(flag) == nil {
				t.Errorf("%s subcommand does not inherit --%s from the root command",
					child.Name(), flag)
			}
		}
	}
}
