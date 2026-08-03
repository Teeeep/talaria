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

// boundary is one package and the packages it may not reach, transitively.
type boundary struct {
	pkg       string
	forbidden []string
	// why is the sentence printed when the edge exists, so a failure says
	// which rule broke rather than only that one did.
	why string
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
		pkg:       "internal/corpus",
		forbidden: executorAndTwin,
		why:       "the store is written by both ends of the tool and takes a local observation struct, not either end's response type",
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
		})
	}
}

// dependenciesOf returns pkg's transitive dependencies as import paths.
func dependenciesOf(t *testing.T, pkg string) []string {
	t.Helper()

	cmd := exec.Command("go", "list", "-deps", modulePath+"/"+pkg)
	cmd.Dir = filepath.Join("..", "..")

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("listing the dependencies of %s: %v", pkg, err)
	}

	var deps []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps = append(deps, line)
		}
	}

	return deps
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}
