package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommandPrintsVersion(t *testing.T) {
	var out bytes.Buffer

	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("version command returned error: %v", err)
	}

	got := out.String()
	if strings.TrimSpace(got) == "" {
		t.Fatal("version command wrote nothing to the output writer")
	}
	if !strings.Contains(got, "talaria") {
		t.Errorf("version output %q does not contain %q", got, "talaria")
	}
	if !strings.Contains(got, version) {
		t.Errorf("version output %q does not contain version %q", got, version)
	}
}

func TestRootCmdSilencesUsageAndErrors(t *testing.T) {
	root := newRootCmd()

	if !root.SilenceUsage {
		t.Error("root command must set SilenceUsage: agents parse stderr and the usage dump is noise")
	}
	if !root.SilenceErrors {
		t.Error("root command must set SilenceErrors: errors are reported by the caller, not cobra")
	}
}

func TestRootCmdErrorDoesNotDumpUsage(t *testing.T) {
	var out bytes.Buffer

	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"no-such-command"})

	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for an unknown subcommand, got nil")
	}

	if got := out.String(); strings.Contains(got, "Usage:") {
		t.Errorf("unknown-command error dumped the usage block:\n%s", got)
	}
}
