package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
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
const readmePath = "../../README.md"
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

// TestAgentDocInvocationsAreAcceptedAsWritten runs every command the doc shows
// in a fenced block, exactly as written, and requires it not to be a usage
// error. The tests above read command names, exit codes and variable names;
// none of them read a flag's argument, which is how the headline `call` example
// came to say `--header 'X-Trace: abc'` when the parser takes `X-Trace=abc`.
// That line exits 2, and it is the one line a model copies verbatim.
func TestAgentDocInvocationsAreAcceptedAsWritten(t *testing.T) {
	fixture := filepath.Join("testdata", "call.yaml")

	// The doc writes the spec as `./openapi.yaml`, and omits it entirely where it
	// is showing that $TALARIA_SPEC replaces the argument. Both have to land on a
	// real spec for the invocation to get as far as its flags.
	t.Setenv(spec.EnvSpec, fixture)

	for _, invocation := range fencedInvocations(t, readAgentDoc(t)) {
		t.Run(invocation, func(t *testing.T) {
			if reason := notRunnable[invocation]; reason != "" {
				t.Skip(reason)
			}

			args := shellFields(t, invocation)[1:]
			for i, arg := range args {
				if arg == docSpecPath {
					args[i] = fixture
				}
			}
			// Appended rather than assumed: --dry-run is `call`'s flag, and adding
			// it to `list` or `auth check` would itself be the usage error under
			// test. Every command that has it is one that would otherwise send a
			// request to the fixture's deliberately unroutable server.
			if hasDryRun(newRootCmd(), args) && !slices.Contains(args, "--dry-run") {
				args = append(args, "--dry-run")
			}

			var stdout, stderr strings.Builder
			if code := run(args, &stdout, &stderr); code == int(clierr.CodeUsage) {
				t.Errorf("AGENT.md shows `%s`, which exits 2: %s", invocation, stderr.String())
			}
		})
	}
}

// docSpecPath is the placeholder path the doc uses for the user's own spec.
const docSpecPath = "./openapi.yaml"

// TestDocsWriteHeadersAsNameEqualsValue covers the prose that the test above
// cannot execute. An inline `--header 'X-Api-Key: sk-live-…'` sits outside every
// fenced block and is copied just as readily as one inside — and a header the
// parser rejects is a header whose value reached an error message.
func TestDocsWriteHeadersAsNameEqualsValue(t *testing.T) {
	for _, path := range []string{agentDocPath, readmePath} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}

		for _, match := range headerColonForm.FindAllString(string(body), -1) {
			t.Errorf("%s writes `%s…`; talaria's --header takes name=value, and this exits 2",
				path, strings.TrimSpace(match))
		}
	}
}

// headerColonForm matches a `--header` whose argument is written curl's way,
// `Name: value`, quoted or not. `--header` with no argument after it — the way
// both docs refer to the flag in passing — has no colon and does not match.
var headerColonForm = regexp.MustCompile(`--header\s+['"]?[A-Za-z][A-Za-z0-9-]*\s*:`)

// notRunnable holds the fenced invocations that cannot be executed here, keyed
// by the line as the doc writes it so that editing the line brings the reason
// back for review rather than silently disabling the check. Narrowing the
// extraction instead would have hidden them.
var notRunnable = map[string]string{
	"talaria run ./openapi.yaml --tag pets --operation getPet --base-url http://localhost:9000": "" +
		"`run` has no --dry-run — sending the requests is what it is for — and this example " +
		"points at a host that is not listening",
	"talaria history show 2026-08-02T21:40:11.183204Z": "" +
		"names one recorded entry by id; the id is an illustration and the isolated history " +
		"these tests run against is empty",
}

// fencedInvocations extracts the commands the doc shows in fenced blocks, which
// unlike the inline code spans docInvocations reads are written to be run: they
// carry real arguments rather than `<operationId>` placeholders.
func fencedInvocations(t *testing.T, doc string) []string {
	t.Helper()

	var out []string
	fenced := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced && strings.HasPrefix(line, "talaria ") {
			out = append(out, strings.TrimSpace(line))
		}
	}

	if len(out) == 0 {
		t.Fatal("AGENT.md shows no runnable command in a fenced block")
	}

	return out
}

// hasDryRun reports whether the command args name accepts --dry-run. It resolves
// the command rather than matching on its name so that a second command growing
// the flag is covered without a change here.
func hasDryRun(root *cobra.Command, args []string) bool {
	cmd, _, err := root.Find(args)
	return err == nil && cmd.Flags().Lookup("dry-run") != nil
}

// shellFields splits an invocation into arguments the way a shell would, so an
// example is executed as the reader's shell would execute it. Splitting on
// whitespace alone would turn `--header 'X-Trace: abc'` into two mangled
// arguments, and would pass the doc's aligned trailing comments to cobra as
// positional arguments — both failures of the extractor rather than of the doc.
func shellFields(t *testing.T, invocation string) []string {
	t.Helper()

	var (
		fields  []string
		current strings.Builder
		quote   rune
		open    bool
	)
	for _, r := range invocation {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '#' && current.Len() == 0 && !open:
			// A `#` starting a word begins a comment; the rest of the line is not
			// part of the command.
			return fields
		case r == '\'' || r == '"':
			quote, open = r, true
		case r == ' ' || r == '\t':
			if current.Len() > 0 || open {
				fields = append(fields, current.String())
				current.Reset()
				open = false
			}
		default:
			current.WriteRune(r)
		}
	}
	if quote != 0 {
		t.Fatalf("AGENT.md shows `%s`, which has an unterminated quote", invocation)
	}
	if current.Len() > 0 || open {
		fields = append(fields, current.String())
	}

	return fields
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
