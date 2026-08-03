// Package ci holds no code. It exists for one test: the CI workflow has to keep
// running the commands this project actually uses.
//
// The workflow is what turns the Task 24 canary suite from a file somebody could
// remember to run into a build gate, which §5a of the design requires ("redaction
// regressions fail the build"). A gate that runs invented commands, or a stale Go
// version, gates nothing — so the test reads the workflow and the two files it is
// supposed to agree with, and compares them.
package ci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Paths are relative to this package directory.
const (
	workflowPath = "../../.github/workflows/ci.yml"
	stackPath    = "../../.ralph/stack.json"
	goModPath    = "../../go.mod"
)

// workflow is the subset of GitHub Actions syntax this test reads.
type workflow struct {
	On   map[string]yaml.Node `yaml:"on"`
	Jobs map[string]struct {
		RunsOn string `yaml:"runs-on"`
		Steps  []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			With map[string]string `yaml:"with"`
			Run  string            `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// stack is the subset of .ralph/stack.json that records shell commands.
type stack struct {
	BuildCommand string `json:"build_command"`
	LintCommand  string `json:"lint_command"`
	TestCommand  string `json:"test_command"`
}

func loadWorkflow(t *testing.T) workflow {
	t.Helper()

	raw, err := os.ReadFile(filepath.FromSlash(workflowPath))
	if err != nil {
		t.Fatalf("reading the workflow: %v", err)
	}

	var w workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("parsing %s as YAML: %v", workflowPath, err)
	}

	return w
}

func loadStack(t *testing.T) stack {
	t.Helper()

	raw, err := os.ReadFile(filepath.FromSlash(stackPath))
	if err != nil {
		t.Fatalf("reading the stack file: %v", err)
	}

	var s stack
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parsing %s as JSON: %v", stackPath, err)
	}

	return s
}

// runCommands returns every shell command the workflow executes, in order.
func runCommands(w workflow) []string {
	var cmds []string

	for _, job := range w.Jobs {
		for _, step := range job.Steps {
			if run := strings.TrimSpace(step.Run); run != "" {
				cmds = append(cmds, run)
			}
		}
	}

	return cmds
}

// TestWorkflowTriggersOnPushAndPullRequest keeps the gate on both paths into
// main: a direct push and a pull request.
func TestWorkflowTriggersOnPushAndPullRequest(t *testing.T) {
	w := loadWorkflow(t)

	if len(w.Jobs) == 0 {
		t.Fatalf("the workflow declares no jobs")
	}

	for _, trigger := range []string{"push", "pull_request"} {
		if _, ok := w.On[trigger]; !ok {
			t.Errorf("the workflow has no %q trigger; it has %v", trigger, triggerNames(w))
		}
	}
}

func triggerNames(w workflow) []string {
	names := make([]string, 0, len(w.On))
	for name := range w.On {
		names = append(names, name)
	}

	return names
}

// TestWorkflowRunsOnlyStackCommands is the anti-drift check. Every command the
// workflow runs has to be one .ralph/stack.json already records, so CI cannot
// quietly start testing something other than what this project builds, lints and
// tests locally.
func TestWorkflowRunsOnlyStackCommands(t *testing.T) {
	w := loadWorkflow(t)
	s := loadStack(t)

	recorded := map[string]string{
		s.BuildCommand: "build_command",
		s.LintCommand:  "lint_command",
		s.TestCommand:  "test_command",
	}

	for _, field := range []struct{ name, value string }{
		{"build_command", s.BuildCommand},
		{"lint_command", s.LintCommand},
		{"test_command", s.TestCommand},
	} {
		if field.value == "" {
			t.Fatalf("%s records no %s", stackPath, field.name)
		}
	}

	ran := make(map[string]bool)

	for _, cmd := range runCommands(w) {
		field, ok := recorded[cmd]
		if !ok {
			t.Errorf("the workflow runs %q, which appears in no command field of %s", cmd, stackPath)

			continue
		}

		ran[field] = true
	}

	for _, field := range []string{"build_command", "lint_command", "test_command"} {
		if !ran[field] {
			t.Errorf("the workflow never runs the %s from %s", field, stackPath)
		}
	}
}

// TestWorkflowRunsTheFullTestSuite states the canary gate directly rather than
// leaving it implied by the stack file: `go test ./...` runs internal/canary, so
// a redaction regression fails the build.
func TestWorkflowRunsTheFullTestSuite(t *testing.T) {
	const suite = "go test ./..."

	for _, cmd := range runCommands(loadWorkflow(t)) {
		if cmd == suite {
			return
		}
	}

	t.Errorf("the workflow never runs %q, so the canary suite gates nothing", suite)
}

// TestWorkflowPinsTheGoModVersion catches the workflow testing against a
// toolchain the module does not target.
func TestWorkflowPinsTheGoModVersion(t *testing.T) {
	want := goDirective(t)

	got, ok := setupGoVersion(loadWorkflow(t))
	if !ok {
		t.Fatalf("the workflow has no actions/setup-go step with a go-version")
	}

	if got != want {
		t.Errorf("the workflow pins Go %q; go.mod requires %q", got, want)
	}
}

// goDirective returns the version from the `go` line of go.mod.
func goDirective(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.FromSlash(goModPath))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}

	for _, line := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			return fields[1]
		}
	}

	t.Fatalf("go.mod has no `go` directive")

	return ""
}

// setupGoVersion returns the version the setup-go step pins.
func setupGoVersion(w workflow) (string, bool) {
	for _, job := range w.Jobs {
		for _, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "actions/setup-go@") {
				continue
			}

			if version, ok := step.With["go-version"]; ok {
				return strings.TrimSpace(version), true
			}
		}
	}

	return "", false
}
