package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/spec"
)

// AGENT.md is the operating manual for the one reader who cannot check the
// source when the manual is wrong (DESIGN.md §3.7). These tests hold the three
// facts a model will act on directly — exit codes, command names, credential
// variable names — against the code that implements them, so the doc fails the
// build rather than drifting.

const agentDocPath = "../../AGENT.md"
const clierrPath = "../../internal/clierr/clierr.go"

func TestAgentDocDocumentsEveryExitCode(t *testing.T) {
	doc := readAgentDoc(t)

	documented := documentedExitCodes(t, doc)
	implemented := implementedExitCodes(t)

	for code, name := range implemented {
		if !documented[code] {
			t.Errorf("clierr.%s is exit code %d and AGENT.md's table does not list it", name, code)
		}
	}
	for code := range documented {
		if _, ok := implemented[code]; !ok {
			t.Errorf("AGENT.md documents exit code %d, which no clierr constant defines", code)
		}
	}
}

func TestAgentDocDocumentsEveryCommand(t *testing.T) {
	doc := readAgentDoc(t)

	for _, path := range commandPaths(newRootCmd()) {
		if !strings.Contains(doc, path) {
			t.Errorf("`%s` is a registered command and AGENT.md never names it", path)
		}
	}
}

func TestAgentDocNamesOnlyRegisteredCommands(t *testing.T) {
	root := newRootCmd()

	for _, invocation := range docInvocations(readAgentDoc(t)) {
		if err := resolves(root, invocation); err != "" {
			t.Errorf("AGENT.md shows `%s`: %s", invocation, err)
		}
	}
}

func TestAgentDocNamesOnlyRealEnvVars(t *testing.T) {
	// The doc's whole job on this subject is to tell an agent which variable to
	// ask a human to export. A name that no code reads sends that human to set
	// something that will never be looked at.
	known := map[string]bool{
		spec.EnvSpec:      true,
		corpus.EnvHistory: true,
		config.EnvBearer:  true,
		config.EnvBasic:   true,
	}

	for _, name := range regexp.MustCompile(`TALARIA_[A-Z0-9_]*`).FindAllString(readAgentDoc(t), -1) {
		if known[name] || strings.HasPrefix(name, config.EnvAPIKeyPrefix) {
			continue
		}

		t.Errorf("AGENT.md names $%s, which talaria never reads", name)
	}
}

func readAgentDoc(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile(agentDocPath)
	if err != nil {
		t.Fatalf("reading AGENT.md: %v", err)
	}

	return string(body)
}

// exitCodeRow matches a markdown table row whose first cell is a bare integer,
// which is the shape of the exit-code table and of nothing else in the doc.
var exitCodeRow = regexp.MustCompile(`(?m)^\|\s*(\d+)\s*\|`)

func documentedExitCodes(t *testing.T, doc string) map[int]bool {
	t.Helper()

	codes := map[int]bool{}
	for _, match := range exitCodeRow.FindAllStringSubmatch(doc, -1) {
		code, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("parsing exit-code row %q: %v", match[0], err)
		}
		codes[code] = true
	}

	if len(codes) == 0 {
		t.Fatal("AGENT.md has no exit-code table")
	}

	return codes
}

// implementedExitCodes reads the clierr.Code constants out of the source rather
// than listing them here: a test that names them would itself be a second copy
// of the contract, drifting alongside the doc it is meant to police.
func implementedExitCodes(t *testing.T) map[int]string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), clierrPath, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", clierrPath, err)
	}

	codes := map[int]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}

		for _, s := range gen.Specs {
			spec, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := spec.Type.(*ast.Ident); !ok || ident.Name != "Code" {
				continue
			}

			for i, name := range spec.Names {
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("clierr.%s is not a literal exit code", name.Name)
				}

				value, err := strconv.Atoi(lit.Value)
				if err != nil {
					t.Fatalf("parsing clierr.%s = %s: %v", name.Name, lit.Value, err)
				}
				codes[value] = name.Name
			}
		}
	}

	if len(codes) == 0 {
		t.Fatalf("found no Code constants in %s", clierrPath)
	}

	return codes
}

// commandPaths lists every invocable command in the tree as it would be typed,
// e.g. "talaria auth check". A command with subcommands and no Run of its own —
// `auth` — is a namespace rather than something to document.
func commandPaths(root *cobra.Command) []string {
	var paths []string

	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Runnable() {
			paths = append(paths, cmd.CommandPath())
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)

	return paths
}

// docInvocation matches a code span or fenced line beginning with the binary's
// name, which is how the doc shows a command.
var docInvocation = regexp.MustCompile("(?m)(?:`|^\\s*)talaria ([^`\n]+)")

// docInvocations extracts every `talaria …` the doc shows, so the reverse of
// TestAgentDocDocumentsEveryCommand can check that each one exists.
func docInvocations(doc string) []string {
	var out []string
	for _, match := range docInvocation.FindAllStringSubmatch(doc, -1) {
		out = append(out, "talaria "+strings.TrimSpace(match[1]))
	}

	return out
}

// resolves walks an invocation's leading words down the command tree and
// reports why it does not name a command, or "" when it does. Words stop being
// command names at the first flag, placeholder or path, which is where the
// arguments begin.
func resolves(root *cobra.Command, invocation string) string {
	cmd := root
	for _, word := range strings.Fields(invocation)[1:] {
		if strings.HasPrefix(word, "-") || strings.HasPrefix(word, "<") ||
			strings.HasPrefix(word, "[") || strings.HasPrefix(word, "$") ||
			strings.ContainsAny(word, "/.=") {
			break
		}

		child := childNamed(cmd, word)
		if child == nil {
			// A word that names no subcommand is an argument once the command it
			// follows can run — `history show 3` — and a mistake otherwise.
			if cmd.Runnable() {
				break
			}

			return "`" + word + "` is not a subcommand of `" + cmd.CommandPath() + "`"
		}
		cmd = child
	}

	if cmd == root {
		return "it names no command"
	}
	if !cmd.Runnable() {
		return "`" + cmd.CommandPath() + "` is a group of subcommands, not a command"
	}

	return ""
}

func childNamed(cmd *cobra.Command, name string) *cobra.Command {
	for _, child := range cmd.Commands() {
		if child.Name() == name {
			return child
		}
	}

	return nil
}
