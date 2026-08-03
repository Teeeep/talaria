package main

import (
	"slices"
	"testing"

	"github.com/spf13/pflag"
)

// Every command in DESIGN.md §4 that takes --profile, --base-url or
// --allow-host — call, auth check, history replay — gets them by inheritance.
// Registering any of them per-command would shadow the persistent flag
// silently, so the test is that the root owns them and no subcommand declares
// its own.
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

// --allow-host names one host each time it appears; DESIGN.md §5a calls it
// repeatable, so a second occurrence has to add to the set rather than replace
// the first. That is a property of how it is registered, not of the parse.
func TestAllowHostIsRepeatable(t *testing.T) {
	root := newRootCmd()

	flag := root.PersistentFlags().Lookup("allow-host")
	if flag == nil {
		t.Fatal("--allow-host is not registered")
	}
	if _, ok := flag.Value.(pflag.SliceValue); !ok {
		t.Fatalf("--allow-host has value type %T, which takes a single value; it must be repeatable",
			flag.Value)
	}

	if err := root.PersistentFlags().Parse([]string{
		"--allow-host", "localhost:9000", "--allow-host", "twin.internal",
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, err := root.PersistentFlags().GetStringArray("allow-host")
	if err != nil {
		t.Fatalf("GetStringArray: %v", err)
	}
	if want := []string{"localhost:9000", "twin.internal"}; !slices.Equal(got, want) {
		t.Errorf("--allow-host = %q, want %q", got, want)
	}
}
