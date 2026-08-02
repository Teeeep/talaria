package main

import (
	"bytes"
	"encoding/json"
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

func TestVersionCommandRendersTheJSONEnvelope(t *testing.T) {
	// DESIGN.md §3.1 promises --output json on every command, and version is the
	// one command an agent calls to check compatibility: a bare line it has to
	// scrape defeats the point.
	var stdout, stderr strings.Builder

	if got := run([]string{"version", "--output", "json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("run(version --output json) = %d, want 0; stderr: %s", got, stderr.String())
	}

	var payload struct {
		Schema  string `json:"schema"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot: %s", err, stdout.String())
	}

	if payload.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", payload.Schema)
	}
	if payload.Version != version {
		t.Errorf("version = %q, want %q", payload.Version, version)
	}
	if got := strings.Count(strings.TrimSpace(stdout.String()), "\n"); got != 0 {
		t.Errorf("stdout spans %d lines, want one object on one line:\n%s", got+1, stdout.String())
	}
}

func TestVersionCommandPrettyIsTheBareLine(t *testing.T) {
	var stdout, stderr strings.Builder

	if got := run([]string{"version", "--output", "pretty"}, &stdout, &stderr); got != 0 {
		t.Fatalf("run(version --output pretty) = %d, want 0; stderr: %s", got, stderr.String())
	}

	if want := "talaria " + version + "\n"; stdout.String() != want {
		t.Errorf("pretty output = %q, want %q", stdout.String(), want)
	}
}

func TestVersionCommandRendersTSV(t *testing.T) {
	var stdout, stderr strings.Builder

	if got := run([]string{"version", "--output", "tsv"}, &stdout, &stderr); got != 0 {
		t.Fatalf("run(version --output tsv) = %d, want 0; stderr: %s", got, stderr.String())
	}

	if !strings.Contains(stdout.String(), version) {
		t.Errorf("tsv output = %q, want it to carry the version", stdout.String())
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
