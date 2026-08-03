package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
)

func TestExitCodeTranslatesErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"usage", clierr.Usage("unknown operation"), 2},
		{"spec load", clierr.SpecLoad("parse error"), 3},
		{"wrapped spec load", fmt.Errorf("context: %w", clierr.SpecLoad("parse error")), 3},
		{"unrecognised", errors.New("something else went wrong"), 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr strings.Builder

			if got := exitCode(tt.err, &stderr); got != tt.want {
				t.Errorf("exitCode = %d, want %d", got, tt.want)
			}

			if tt.err == nil && stderr.Len() != 0 {
				t.Errorf("a nil error wrote %q to stderr, want nothing", stderr.String())
			}
			if tt.err != nil && stderr.Len() == 0 {
				t.Error("a non-nil error wrote nothing to stderr, want structured JSON")
			}
		})
	}
}

func TestRunReturnsZeroOnSuccess(t *testing.T) {
	var stdout, stderr strings.Builder

	if got := run([]string{"version"}, &stdout, &stderr); got != 0 {
		t.Errorf("run(version) = %d, want 0; stderr: %s", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), "talaria") {
		t.Errorf("run(version) stdout = %q, want the version line", stdout.String())
	}
}

func TestRunRejectsAnUnknownOutputFormat(t *testing.T) {
	// The other half of the internal/output parse-error assertion: the bare error
	// that ParseFormat returns has to arrive here as a usage error, exit code 2,
	// listing every valid value on stderr.
	var stdout, stderr strings.Builder

	if got := run([]string{"version", "--output", "xml"}, &stdout, &stderr); got != 2 {
		t.Fatalf("run(--output xml) = %d, want 2; stderr: %s", got, stderr.String())
	}

	var payload struct {
		Schema string `json:"schema"`
		Error  struct {
			Code         int      `json:"code"`
			Message      string   `json:"message"`
			Alternatives []string `json:"valid_alternatives"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr.String()), &payload); err != nil {
		t.Fatalf("stderr is not valid JSON: %v\ngot: %s", err, stderr.String())
	}

	if payload.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", payload.Schema)
	}
	if payload.Error.Code != 2 {
		t.Errorf("error.code = %d, want 2", payload.Error.Code)
	}
	if !strings.Contains(payload.Error.Message, "xml") {
		t.Errorf("error.message = %q, want it to name the rejected value", payload.Error.Message)
	}
	if got := strings.Join(payload.Error.Alternatives, ","); got != "json,pretty,tsv" {
		t.Errorf("error.valid_alternatives = %v, want [json pretty tsv]", payload.Error.Alternatives)
	}
}

func TestRunTreatsAnUnknownCommandAsAUsageError(t *testing.T) {
	// A typo is a bad invocation, not a failed request. Exit 1 is the code
	// AGENT.md tells an agent is transient and worth retrying, so an
	// unclassified "unknown command" sends it into a retry loop; DESIGN.md §4
	// assigns 2.
	var stdout, stderr strings.Builder

	if got := run([]string{"bogus"}, &stdout, &stderr); got != 2 {
		t.Fatalf("run(bogus) = %d, want 2; stderr: %s", got, stderr.String())
	}

	var payload struct {
		Schema string `json:"schema"`
		Error  struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr.String()), &payload); err != nil {
		t.Fatalf("stderr is not valid JSON: %v\ngot: %s", err, stderr.String())
	}

	if payload.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", payload.Schema)
	}
	if payload.Error.Code != 2 {
		t.Errorf("error.code = %d, want 2", payload.Error.Code)
	}
	if !strings.Contains(payload.Error.Message, "bogus") {
		t.Errorf("error.message = %q, want it to name the unknown command", payload.Error.Message)
	}
}

func TestRunOffersTheNearMissForAMistypedCommand(t *testing.T) {
	var stdout, stderr strings.Builder

	if got := run([]string{"vrsion"}, &stdout, &stderr); got != 2 {
		t.Fatalf("run(vrsion) = %d, want 2; stderr: %s", got, stderr.String())
	}

	var payload struct {
		Error struct {
			Alternatives []string `json:"valid_alternatives"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr.String()), &payload); err != nil {
		t.Fatalf("stderr is not valid JSON: %v\ngot: %s", err, stderr.String())
	}

	if got := strings.Join(payload.Error.Alternatives, ","); got != "version" {
		t.Errorf("error.valid_alternatives = %v, want [version]", payload.Error.Alternatives)
	}
}

func TestRunWithNoArgumentsPrintsHelpAndSucceeds(t *testing.T) {
	// Classifying the unknown-command case must not turn the bare invocation
	// into an error: `talaria` on its own is how a human finds the commands.
	var stdout, stderr strings.Builder

	if got := run(nil, &stdout, &stderr); got != 0 {
		t.Fatalf("run() = %d, want 0; stderr: %s", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Available Commands:") {
		t.Errorf("run() stdout = %q, want the help text", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("run() wrote %q to stderr, want nothing", stderr.String())
	}
}

func TestRunTreatsACommandGroupWithNoSubcommandAsAUsageError(t *testing.T) {
	// `talaria auth` holds subcommands and does nothing itself, so naming it
	// alone is an incomplete invocation. Cobra's default — help on stdout, exit
	// 0 — tells an agent that asked for JSON that it succeeded, and hands it
	// prose to parse.
	var stdout, stderr strings.Builder

	if got := run([]string{"auth"}, &stdout, &stderr); got != 2 {
		t.Fatalf("run(auth) = %d, want 2; stdout: %s", got, stdout.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("run(auth) wrote %q to stdout, want the failure on stderr only", stdout.String())
	}

	var payload struct {
		Schema string `json:"schema"`
		Error  struct {
			Code         int      `json:"code"`
			Message      string   `json:"message"`
			Alternatives []string `json:"valid_alternatives"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr.String()), &payload); err != nil {
		t.Fatalf("stderr is not valid JSON: %v\ngot: %s", err, stderr.String())
	}

	if payload.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", payload.Schema)
	}
	if payload.Error.Code != 2 {
		t.Errorf("error.code = %d, want 2", payload.Error.Code)
	}
	if !strings.Contains(payload.Error.Message, "talaria auth") {
		t.Errorf("error.message = %q, want it to name the command", payload.Error.Message)
	}
	if got := strings.Join(payload.Error.Alternatives, ","); got != "check" {
		t.Errorf("error.valid_alternatives = %v, want [check]", payload.Error.Alternatives)
	}
}

func TestRunTreatsAnUnknownSubcommandAsAUsageError(t *testing.T) {
	// The same contract one level down: `talaria bogus` already exits 2, and an
	// agent has no way to know that `talaria auth chekc` is a different kind of
	// typo.
	var stdout, stderr strings.Builder

	if got := run([]string{"auth", "chekc"}, &stdout, &stderr); got != 2 {
		t.Fatalf("run(auth chekc) = %d, want 2; stdout: %s", got, stdout.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("run(auth chekc) wrote %q to stdout, want the failure on stderr only", stdout.String())
	}

	var payload struct {
		Error struct {
			Code         int      `json:"code"`
			Message      string   `json:"message"`
			Alternatives []string `json:"valid_alternatives"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr.String()), &payload); err != nil {
		t.Fatalf("stderr is not valid JSON: %v\ngot: %s", err, stderr.String())
	}

	if payload.Error.Code != 2 {
		t.Errorf("error.code = %d, want 2", payload.Error.Code)
	}
	if !strings.Contains(payload.Error.Message, "chekc") {
		t.Errorf("error.message = %q, want it to name the unknown subcommand", payload.Error.Message)
	}
	if got := strings.Join(payload.Error.Alternatives, ","); got != "check" {
		t.Errorf("error.valid_alternatives = %v, want [check], the near miss", payload.Error.Alternatives)
	}
}

func TestRunTreatsAnUnknownFlagAsAUsageError(t *testing.T) {
	var stdout, stderr strings.Builder

	if got := run([]string{"version", "--nope"}, &stdout, &stderr); got != 2 {
		t.Errorf("run(--nope) = %d, want 2; stderr: %s", got, stderr.String())
	}
}
