package output

import (
	"slices"
	"strings"

	"github.com/pb33f/libopenapi/datamodel/high/base"
)

// DefaultSchemaDepth is how deep the renderer walks before it stops. Deep
// enough for the shapes people actually send, shallow enough that a schema with
// a long tail of nested objects cannot become the context bomb this tool exists
// to prevent (DESIGN.md §3.2).
const DefaultSchemaDepth = 6

// CircularMarker ends the line of a property whose schema refers back to one of
// its own ancestors. Real specs self-reference constantly, so this is a normal
// outcome, not an error.
const CircularMarker = "[circular]"

// DepthMarker ends the line of a property that has children the renderer chose
// not to walk into, so a reader can tell the difference between "no more
// fields" and "not shown".
const DepthMarker = "[max depth]"

// indent is one nesting level of the rendered tree. Two spaces keeps deep
// schemas inside a terminal width.
const indent = "  "

// The type names the renderer invents when the spec does not supply one.
const (
	typeAny    = "any"
	typeObject = "object"
	typeArray  = "array"
	typeNull   = "null"
)

// SchemaNode is one node of a schema rendered for human and agent consumption
// alike: the pretty renderer turns it into lines, and `--output json` marshals
// the tree as it stands. Building it once and rendering twice is what keeps the
// two views from drifting.
//
// The tree is deliberately lossy. It answers "what do I put in this field", not
// "what does this schema validate" — no patterns, no bounds, no formats.
type SchemaNode struct {
	// Name is the property name. It is empty on the root of a schema that was
	// not reached through a property; callers naming a root — a parameter, a
	// request body — set it themselves.
	Name string `json:"name,omitempty"`
	// Type is the rendered type: a JSON Schema type, "[]" plus an element type
	// for an array, "a|b" for a union, or a "oneOf: a|b" summary for a
	// composition.
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required,omitempty"`
	// Description is the spec's description, collapsed onto one line.
	Description string       `json:"description,omitempty"`
	Enum        []string     `json:"enum,omitempty"`
	Properties  []SchemaNode `json:"properties,omitempty"`
	// Circular reports that this node's schema is one of its own ancestors, so
	// its properties were not walked.
	Circular bool `json:"circular,omitempty"`
	// Truncated reports that this node has properties the depth limit stopped
	// the renderer from walking.
	Truncated bool `json:"truncated,omitempty"`
}

// SchemaTree walks a schema into a SchemaNode, stopping at maxDepth levels of
// nesting and at any schema that refers back to one of its own ancestors. A
// maxDepth of zero or less means DefaultSchemaDepth. A nil schema yields the
// zero node, which renders as nothing.
func SchemaTree(sp *base.SchemaProxy, maxDepth int) SchemaNode {
	if sp == nil {
		return SchemaNode{}
	}
	if maxDepth <= 0 {
		maxDepth = DefaultSchemaDepth
	}

	w := walker{maxDepth: maxDepth, onPath: make(map[string]bool)}

	return w.node("", false, sp, 0)
}

// Lines renders the node as one line per property, nested properties indented
// one level per depth. The root's own line is suppressed when it is an unnamed
// object: a caller rendering a request body wants its fields, not an "(object)"
// header above them.
func (n SchemaNode) Lines() []string {
	if n.Type == "" && len(n.Properties) == 0 {
		return nil
	}

	var out []string
	n.appendLines(&out, 0)

	return out
}

// Line renders just this node, without its properties: the `name*: (type)
// description` form from DESIGN.md §3.2.
func (n SchemaNode) Line() string {
	var b strings.Builder
	if n.Name != "" {
		b.WriteString(n.Name)
		if n.Required {
			b.WriteString("*")
		}
		b.WriteString(": ")
	}
	b.WriteString("(" + n.Type + ")")

	for _, part := range []string{n.Description, enumPhrase(n.Enum), n.marker()} {
		if part != "" {
			b.WriteString(" " + part)
		}
	}

	return b.String()
}

func (n SchemaNode) appendLines(out *[]string, depth int) {
	next := depth
	if !n.suppressed() {
		*out = append(*out, strings.Repeat(indent, depth)+n.Line())
		next++
	}

	for _, p := range n.Properties {
		p.appendLines(out, next)
	}
}

// suppressed reports whether this node's own line adds nothing — an unnamed
// object whose properties speak for it.
func (n SchemaNode) suppressed() bool {
	return n.Name == "" && n.Type == typeObject && len(n.Properties) > 0
}

func (n SchemaNode) marker() string {
	switch {
	case n.Circular:
		return CircularMarker
	case n.Truncated:
		return DepthMarker
	default:
		return ""
	}
}

func enumPhrase(values []string) string {
	if len(values) == 0 {
		return ""
	}

	return "one of: " + strings.Join(values, ", ")
}

// walker carries the state one SchemaTree call needs: the depth limit and the
// $refs on the current path, which is what makes the cycle guard a guard rather
// than a global "seen once" filter — the same schema may legitimately appear in
// two sibling branches.
type walker struct {
	maxDepth int
	onPath   map[string]bool
}

func (w *walker) node(name string, required bool, sp *base.SchemaProxy, depth int) SchemaNode {
	n := SchemaNode{Name: name, Required: required, Type: typeAny}
	if sp == nil {
		return n
	}

	// The cycle key is the $ref rather than the resolved schema pointer:
	// recursion in a real spec always goes through a reference, and a ref
	// string is stable where libopenapi's resolved pointers need not be.
	if key := refKey(sp); key != "" {
		if w.onPath[key] {
			n.Type = typeName(sp, 0)
			n.Circular = true

			return n
		}
		w.onPath[key] = true
		defer delete(w.onPath, key)
	}

	s := sp.Schema()
	if s == nil {
		return n
	}

	n.Description = collapse(s.Description)
	n.Enum = enumValues(s)

	// An array delegates to its element: the element decides the type name, the
	// properties and the cycle verdict, and the array only prefixes "[]". The
	// element is at the same depth — an array is a container, not a level of
	// nesting a reader has to hold in their head.
	if items := arrayItems(s); items != nil {
		elem := w.node(name, required, items, depth)
		elem.Type = "[]" + elem.Type
		if elem.Description == "" {
			elem.Description = n.Description
		}

		return elem
	}

	n.Type = typeName(sp, 0)

	properties := propertyPairs(s)
	if len(properties) == 0 {
		return n
	}
	if depth >= w.maxDepth {
		n.Truncated = true

		return n
	}

	req := requiredSet(s)
	for _, p := range properties {
		n.Properties = append(n.Properties, w.node(p.name, req[p.name], p.schema, depth+1))
	}

	return n
}

// property is one entry of an object's properties, kept as a pair so allOf
// branches can be spliced into the parent's list in order.
type property struct {
	name   string
	schema *base.SchemaProxy
}

// propertyPairs lists the properties of a schema in spec order. allOf branches
// contribute theirs too: allOf means every branch applies at once, so the
// fields a caller has to supply are the union. oneOf and anyOf are a choice
// rather than a union, so they contribute nothing here — typeName names their
// branches instead.
func propertyPairs(s *base.Schema) []property {
	var out []property
	if s.Properties != nil {
		for name, schema := range s.Properties.FromOldest() {
			out = append(out, property{name: name, schema: schema})
		}
	}

	for _, branch := range s.AllOf {
		if branch == nil {
			continue
		}
		if sub := branch.Schema(); sub != nil {
			out = append(out, propertyPairs(sub)...)
		}
	}

	return out
}

// requiredSet collects the required property names of a schema and of its allOf
// branches, matching how propertyPairs merges them.
func requiredSet(s *base.Schema) map[string]bool {
	req := make(map[string]bool, len(s.Required))
	for _, name := range s.Required {
		req[name] = true
	}

	for _, branch := range s.AllOf {
		if branch == nil {
			continue
		}
		if sub := branch.Schema(); sub != nil {
			for name := range requiredSet(sub) {
				req[name] = true
			}
		}
	}

	return req
}

// arrayItems returns the element schema of an array, or nil for anything else.
// A schema with items but no declared type is treated as an array too: 3.1
// specs routinely omit the type.
func arrayItems(s *base.Schema) *base.SchemaProxy {
	if s.Items == nil || !s.Items.IsA() {
		return nil
	}
	if len(s.Type) > 0 && !slices.Contains(s.Type, typeArray) {
		return nil
	}

	return s.Items.A
}

// typeName names a schema's type for display. budget bounds the one place it
// recurses — an array element — so a self-referential array cannot spin here;
// the cycle guard in walker covers the structural walk, but typeName is also
// called on a node whose walk has already been cut short.
func typeName(sp *base.SchemaProxy, budget int) string {
	if sp == nil || budget > 2 {
		return typeAny
	}

	s := sp.Schema()
	if s == nil {
		return typeAny
	}

	if declared := declaredTypes(s); len(declared) > 0 {
		if len(declared) == 1 && declared[0] == typeArray {
			return "[]" + typeName(itemsOf(s), budget+1)
		}

		return strings.Join(declared, "|")
	}

	// No declared type: infer from what the schema actually constrains.
	switch {
	case len(s.OneOf) > 0:
		return branchSummary("oneOf", s.OneOf, budget)
	case len(s.AnyOf) > 0:
		return branchSummary("anyOf", s.AnyOf, budget)
	case s.Properties != nil && s.Properties.Len() > 0, len(s.AllOf) > 0:
		return typeObject
	case itemsOf(s) != nil:
		return "[]" + typeName(itemsOf(s), budget+1)
	default:
		return typeAny
	}
}

// branchSummary names a composition by its branch types rather than expanding
// each one. An agent needs to know it has a choice and between what; the full
// expansion of every branch is what `describe` exists to avoid.
func branchSummary(keyword string, branches []*base.SchemaProxy, budget int) string {
	names := make([]string, 0, len(branches))
	seen := make(map[string]bool, len(branches))
	for _, branch := range branches {
		name := typeName(branch, budget+1)
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}

	return keyword + ": " + strings.Join(names, "|")
}

// declaredTypes returns the schema's declared types with "null" dropped, since
// nullability is not what a caller is choosing between. A schema declared only
// null keeps it.
func declaredTypes(s *base.Schema) []string {
	out := make([]string, 0, len(s.Type))
	for _, t := range s.Type {
		if t != typeNull {
			out = append(out, t)
		}
	}
	if len(out) == 0 && len(s.Type) > 0 {
		return []string{typeNull}
	}

	return out
}

func itemsOf(s *base.Schema) *base.SchemaProxy {
	if s.Items == nil || !s.Items.IsA() {
		return nil
	}

	return s.Items.A
}

// refKey identifies a schema by the reference it was reached through, or "" for
// an inline schema, which cannot be its own ancestor.
func refKey(sp *base.SchemaProxy) string {
	if !sp.IsReference() {
		return ""
	}

	return sp.GetReference()
}

// enumValues renders the enum as the strings a caller would type. Values are
// yaml scalars; a non-scalar enum entry has no compact rendering and is skipped
// rather than dumped.
func enumValues(s *base.Schema) []string {
	if len(s.Enum) == 0 {
		return nil
	}

	out := make([]string, 0, len(s.Enum))
	for _, node := range s.Enum {
		if node == nil || node.Value == "" {
			continue
		}
		out = append(out, node.Value)
	}
	if len(out) == 0 {
		return nil
	}

	return out
}

// collapse folds a description onto one line. Spec descriptions are frequently
// multi-paragraph markdown, and one property per line is the format.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
