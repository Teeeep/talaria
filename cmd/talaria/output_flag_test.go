package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// childCmd finds a subcommand of root by name, failing the test if it is absent.
func childCmd(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()

	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("root command has no %q subcommand", name)
	return nil
}

func TestOutputFlagIsPersistentOnRoot(t *testing.T) {
	root := newRootCmd()

	if root.PersistentFlags().Lookup("output") == nil {
		t.Fatal("--output is not registered as a persistent flag on the root command")
	}

	// A subcommand must inherit --output rather than declare its own, so the
	// flag exists exactly once across the whole tree (DESIGN.md §3.1).
	child := childCmd(t, root, "version")
	if child.LocalFlags().Lookup("output") != nil {
		t.Error("version subcommand declares its own --output; it must inherit the persistent one")
	}
	if child.InheritedFlags().Lookup("output") == nil {
		t.Error("version subcommand does not inherit --output from the root command")
	}
}

func TestOutputFlagIsAcceptedBySubcommands(t *testing.T) {
	root := newRootCmd()
	root.SetOut(new(strings.Builder))
	root.SetErr(new(strings.Builder))
	root.SetArgs([]string{"version", "--output", "json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("version --output json returned error: %v", err)
	}
}
