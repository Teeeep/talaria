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

// shared are the packages DESIGN.md §5 calls "consumed by **both** the curl
// executor and the twin server".
var shared = []string{
	"internal/operation",
	"internal/validate",
	"internal/gen",
}

// forbidden are the packages a shared one may not reach. `curl` is the outbound
// executor and `twin` the inbound server, so a shared package that depended on
// either would be usable from only one side of the tool; `corpus` is listed
// with them because it backs both history and the recording proxy, and an
// operation model that reached into a store of past calls would carry the
// store's shape into the twin.
//
// internal/twin does not exist yet — the twin lands in Phase 6. It is named
// here anyway: the point of this test is that the rule holds when the package
// arrives, and a test that had to be extended on the day it mattered would not
// have held it.
var forbidden = []string{
	"internal/curl",
	"internal/corpus",
	"internal/twin",
}

// TestSharedPackagesDoNotDependOnTheExecutorOrTheTwin asserts DESIGN.md §5's
// boundary rule against the real import graph rather than against prose.
//
// Every other test in the suite passes whether or not this invariant holds, so
// nothing else can catch its breach. The dependencies are read transitively,
// because an indirect edge violates the rule just as completely as a direct
// import and is far easier to add by accident.
func TestSharedPackagesDoNotDependOnTheExecutorOrTheTwin(t *testing.T) {
	t.Parallel()

	for _, pkg := range shared {
		t.Run(pkg, func(t *testing.T) {
			t.Parallel()

			deps := dependenciesOf(t, pkg)

			// A `go list` that printed nothing, or that resolved some other
			// package, would let every assertion below pass vacuously. Its own
			// import path is in its own transitive closure, so finding it there
			// proves the output is about the package under test.
			if !contains(deps, modulePath+"/"+pkg) {
				t.Fatalf("go list -deps %s does not include the package itself; "+
					"the %d dependencies read are not this package's", pkg, len(deps))
			}

			for _, banned := range forbidden {
				if contains(deps, modulePath+"/"+banned) {
					t.Errorf("%s depends on %s, which DESIGN.md §5 forbids: "+
						"%s is shared with the twin server and may not reach the executor's side of the tool",
						pkg, banned, pkg)
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
