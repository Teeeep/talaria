package output

import (
	"os"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	"github.com/pb33f/libopenapi/orderedmap"
	"github.com/pb33f/libopenapi/utils"
)

// schemaProxy wraps a hand-built schema so tests read like the spec fragment
// they stand for.
func schemaProxy(s *base.Schema) *base.SchemaProxy { return base.CreateSchemaProxy(s) }

// objectSchema builds an object schema from name/schema pairs in the given
// order, which is the order the renderer has to preserve.
func objectSchema(required []string, props ...any) *base.Schema {
	m := orderedmap.New[string, *base.SchemaProxy]()
	for i := 0; i < len(props); i += 2 {
		m.Set(props[i].(string), props[i+1].(*base.SchemaProxy))
	}

	return &base.Schema{Type: []string{"object"}, Required: required, Properties: m}
}

func scalarSchema(typ, description string) *base.SchemaProxy {
	return schemaProxy(&base.Schema{Type: []string{typ}, Description: description})
}

func arraySchema(items *base.SchemaProxy) *base.SchemaProxy {
	return schemaProxy(&base.Schema{
		Type:  []string{"array"},
		Items: &base.DynamicValue[*base.SchemaProxy, bool]{A: items},
	})
}

// enumSchema builds a scalar schema constrained to a fixed set of values.
func enumSchema(typ string, values ...string) *base.SchemaProxy {
	s := &base.Schema{Type: []string{typ}}
	for _, v := range values {
		s.Enum = append(s.Enum, utils.CreateStringNode(v))
	}

	return schemaProxy(s)
}

// treeLines is the whole path under test: walk the schema, then format the tree.
func treeLines(sp *base.SchemaProxy, maxDepth int) []string {
	return SchemaTree(sp, maxDepth).Lines()
}

func TestSchemaRequiredPropertyRendersWithAStar(t *testing.T) {
	got := treeLines(schemaProxy(objectSchema([]string{"name"},
		"name", scalarSchema("string", "the pet's name"),
	)), DefaultSchemaDepth)

	want := "name*: (string) the pet's name"
	if len(got) != 1 || got[0] != want {
		t.Errorf("rendered %q, want [%q]", got, want)
	}
}

func TestSchemaOptionalPropertyHasNoStar(t *testing.T) {
	got := treeLines(schemaProxy(objectSchema(nil,
		"age", scalarSchema("integer", "years old"),
	)), DefaultSchemaDepth)

	want := "age: (integer) years old"
	if len(got) != 1 || got[0] != want {
		t.Errorf("rendered %q, want [%q]", got, want)
	}
}

func TestSchemaNestsObjectsAndNamesArrayElementTypes(t *testing.T) {
	got := treeLines(schemaProxy(objectSchema([]string{"owner"},
		"tags", arraySchema(scalarSchema("string", "")),
		"owner", schemaProxy(objectSchema([]string{"name"},
			"name", scalarSchema("string", "who owns it"),
			"address", schemaProxy(objectSchema(nil,
				"city", scalarSchema("string", ""),
			)),
		)),
	)), DefaultSchemaDepth)

	want := []string{
		"tags: ([]string)",
		"owner*: (object)",
		"  name*: (string) who owns it",
		"  address: (object)",
		"    city: (string)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rendered:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSchemaRendersEnumsInline(t *testing.T) {
	got := treeLines(schemaProxy(objectSchema([]string{"status"},
		"status", enumSchema("string", "active", "archived"),
	)), DefaultSchemaDepth)

	want := "status*: (string) one of: active, archived"
	if len(got) != 1 || got[0] != want {
		t.Errorf("rendered %q, want [%q]", got, want)
	}
}

func TestSchemaTerminatesOnASelfReference(t *testing.T) {
	sp := fixtureResponseSchema(t, "testdata/recursive-schema.yaml", "getTree")

	// The guard is the point of the test: without it this call never returns,
	// so reaching the assertions at all is half the result.
	joined := strings.Join(treeLines(sp, DefaultSchemaDepth), "\n")

	if !strings.Contains(joined, CircularMarker) {
		t.Fatalf("a self-referencing schema rendered without %s:\n%s", CircularMarker, joined)
	}
	if !strings.Contains(joined, "id*: (string) The node identifier") {
		t.Errorf("the cycle guard swallowed the non-recursive properties:\n%s", joined)
	}
	// Both the direct ref and the one behind an array have to be caught.
	for _, want := range []string{
		"parent: (object) " + CircularMarker,
		"children: ([]object) " + CircularMarker,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("rendered output is missing %q:\n%s", want, joined)
		}
	}
}

func TestSchemaStopsAtMaxDepth(t *testing.T) {
	deepest := schemaProxy(objectSchema(nil, "d", scalarSchema("string", "")))
	for range 3 {
		deepest = schemaProxy(objectSchema(nil, "nested", deepest))
	}

	got := treeLines(deepest, 2)

	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, DepthMarker) {
		t.Fatalf("a schema deeper than the limit rendered without %s:\n%s", DepthMarker, joined)
	}
	// A max depth of 2 admits the root's properties and their children, no more.
	for _, line := range got {
		if depth := (len(line) - len(strings.TrimLeft(line, " "))) / 2; depth > 1 {
			t.Errorf("line is indented %d levels, want at most 1:\n%s", depth, joined)
		}
	}
	if strings.Contains(joined, "d: (string)") {
		t.Errorf("the deepest property survived a max depth of 2:\n%s", joined)
	}
}

func TestSchemaTreeCarriesTheStructureJSONNeeds(t *testing.T) {
	// --output json renders the tree, not the pretty string, so the tree has to
	// carry the same facts the lines do.
	tree := SchemaTree(schemaProxy(objectSchema([]string{"name"},
		"name", scalarSchema("string", "the pet's name"),
		"tags", arraySchema(scalarSchema("string", "")),
	)), DefaultSchemaDepth)

	if tree.Type != "object" {
		t.Errorf("root type = %q, want object", tree.Type)
	}
	if len(tree.Properties) != 2 {
		t.Fatalf("root has %d properties, want 2", len(tree.Properties))
	}

	name := tree.Properties[0]
	if name.Name != "name" || name.Type != "string" || !name.Required || name.Description != "the pet's name" {
		t.Errorf("first property = %+v, want a required string named name", name)
	}
	if tags := tree.Properties[1]; tags.Type != "[]string" || tags.Required {
		t.Errorf("second property = %+v, want an optional []string", tags)
	}
}

func TestSchemaHandlesAMissingSchema(t *testing.T) {
	// A parameter can carry no schema at all; describe still has to print a line
	// for it rather than crash.
	if got := treeLines(nil, DefaultSchemaDepth); len(got) != 0 {
		t.Errorf("a nil schema rendered %q, want no lines", got)
	}
}

// fixtureResponseSchema loads a spec fixture and returns the first response
// schema of one operation — the shortest path from a real spec, with real
// resolved $refs, to a schema proxy.
func fixtureResponseSchema(t *testing.T, path, operationID string) *base.SchemaProxy {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	doc, err := libopenapi.NewDocument(data)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	model, err := doc.BuildV3Model()
	if err != nil {
		t.Fatalf("building fixture model: %v", err)
	}

	for _, item := range model.Model.Paths.PathItems.FromOldest() {
		for _, op := range item.GetOperations().FromOldest() {
			if op.OperationId != operationID {
				continue
			}
			for _, resp := range op.Responses.Codes.FromOldest() {
				for _, media := range resp.Content.FromOldest() {
					return media.Schema
				}
			}
		}
	}

	t.Fatalf("fixture %s has no response schema for %s", path, operationID)

	return nil
}
