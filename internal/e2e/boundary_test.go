package e2e

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// modulePath is talaria's import path, so a package can be named to `go list`
// and matched in its output without repeating the prefix at each use.
const modulePath = "github.com/Teeeep/talaria"

// executorAndTwin are the two ends of the tool: `curl` is the outbound executor
// and `twin` the inbound server, so a package that reached either would be
// usable from only one side.
//
// internal/twin does not exist yet — the twin lands in Phase 6. It is named
// here anyway: the point of this test is that the rule holds when the package
// arrives, and a test that had to be extended on the day it mattered would not
// have held it.
var executorAndTwin = []string{
	"internal/curl",
	"internal/twin",
}

// boundary is one package and the packages it may not reach.
type boundary struct {
	pkg string
	// forbidden are packages pkg may not reach at all, transitively.
	forbidden []string
	// forbiddenDirect are packages pkg may not import itself. It is the
	// weaker rule, and deliberately so: the edge is tolerated through a
	// dependency, so this catches the import being written in this package
	// and claims nothing about the same package arriving indirectly.
	forbiddenDirect []string
	// why and whyDirect are the sentences printed when the respective edge
	// exists, so a failure says which rule broke rather than only that one
	// did.
	why       string
	whyDirect string
}

// boundaries are the import rules DESIGN.md §5 and CLAUDE.md state as prose.
//
// The shared packages are the ones §5 calls "consumed by **both** the curl
// executor and the twin server"; they may not reach either end, nor `corpus`,
// which backs both history and the recording proxy — an operation model that
// reached into a store of past calls would carry the store's shape into the
// twin. `corpus` itself is constrained in the other direction: it is written by
// both sides, so it takes a local observation struct rather than the executor's
// response type.
var boundaries = []boundary{
	{
		pkg:       "internal/operation",
		forbidden: append([]string{"internal/corpus"}, executorAndTwin...),
		why:       "it is shared with the twin server and may not reach the executor's side of the tool",
	},
	{
		pkg:       "internal/validate",
		forbidden: append([]string{"internal/corpus"}, executorAndTwin...),
		why:       "it is shared with the twin server and may not reach the executor's side of the tool",
	},
	{
		pkg:             "internal/corpus",
		forbidden:       executorAndTwin,
		why:             "the store is written by both ends of the tool and takes a local observation struct, not either end's response type",
		forbiddenDirect: []string{"internal/config"},
		whyDirect:       "the store must stay usable by the twin, which has no profiles: the profile's history setting arrives as the Enabled bool a caller passes to New",
	},
}

// TestPackageBoundariesHoldInTheRealImportGraph asserts DESIGN.md §5's boundary
// rules against the real import graph rather than against prose.
//
// Every other test in the suite passes whether or not these invariants hold, so
// nothing else can catch a breach. The dependencies are read transitively,
// because an indirect edge violates a rule just as completely as a direct
// import and is far easier to add by accident.
func TestPackageBoundariesHoldInTheRealImportGraph(t *testing.T) {
	t.Parallel()

	for _, b := range boundaries {
		t.Run(b.pkg, func(t *testing.T) {
			t.Parallel()

			deps := dependenciesOf(t, b.pkg)

			// A `go list` that printed nothing, or that resolved some other
			// package, would let every assertion below pass vacuously. Its own
			// import path is in its own transitive closure, so finding it there
			// proves the output is about the package under test.
			if !contains(deps, modulePath+"/"+b.pkg) {
				t.Fatalf("go list -deps %s does not include the package itself; "+
					"the %d dependencies read are not this package's", b.pkg, len(deps))
			}

			for _, banned := range b.forbidden {
				if contains(deps, modulePath+"/"+banned) {
					t.Errorf("%s depends on %s, which the boundary rules forbid: %s",
						b.pkg, banned, b.why)
				}
			}

			if len(b.forbiddenDirect) == 0 {
				return
			}

			imports := importsOf(t, b.pkg)

			for _, banned := range b.forbiddenDirect {
				if contains(imports, modulePath+"/"+banned) {
					t.Errorf("%s imports %s, which the boundary rules forbid: %s",
						b.pkg, banned, b.whyDirect)
				}
			}
		})
	}
}

// TestTheDirectImportRuleReadsDirectImportsOnly is what keeps forbiddenDirect
// from being either vacuous or a second spelling of forbidden. It asserts both
// halves against the real graph: internal/request imports internal/config in
// its own source, so a reader that saw nothing would be caught; internal/corpus
// reaches internal/config only through internal/request, so a reader that
// resolved transitively would be caught too — and that edge is the one the
// corpus entry deliberately tolerates until phase 2b splits the package.
func TestTheDirectImportRuleReadsDirectImportsOnly(t *testing.T) {
	t.Parallel()

	const config = modulePath + "/internal/config"

	if imports := importsOf(t, "internal/request"); !contains(imports, config) {
		t.Errorf("internal/request does not import %s directly; the direct-import "+
			"reader sees %d imports and every forbiddenDirect rule passes vacuously",
			config, len(imports))
	}

	if imports := importsOf(t, "internal/corpus"); contains(imports, config) {
		t.Errorf("internal/corpus imports %s directly; either the import was added, "+
			"or the reader is resolving transitively and the direct rule is claiming "+
			"to catch an indirect edge it cannot", config)
	}

	if deps := dependenciesOf(t, "internal/corpus"); !contains(deps, config) {
		t.Errorf("internal/corpus no longer reaches %s at all; the direct-only rule "+
			"was a deferral of that edge, so promote it to forbidden", config)
	}
}

// importsOf returns the packages named in pkg's own import blocks. Test files
// are not read: what the rule is about is whether the package compiles into the
// twin, and a package's tests do not travel with it.
func importsOf(t *testing.T, pkg string) []string {
	t.Helper()

	return goList(t, pkg, "-f", "{{range .Imports}}{{println .}}{{end}}")
}

// dependenciesOf returns pkg's transitive dependencies as import paths.
func dependenciesOf(t *testing.T, pkg string) []string {
	t.Helper()

	return goList(t, pkg, "-deps")
}

// goList runs `go list` over pkg from the module root and returns its output a
// line at a time.
func goList(t *testing.T, pkg string, args ...string) []string {
	t.Helper()

	cmd := exec.Command("go", append(append([]string{"list"}, args...), modulePath+"/"+pkg)...)
	cmd.Dir = filepath.Join("..", "..")

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("listing %s (go list %s): %v", pkg, strings.Join(args, " "), err)
	}

	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}

	return lines
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}
