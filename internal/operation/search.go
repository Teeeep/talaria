package operation

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pb33f/libopenapi/datamodel/high/base"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/spec"
)

// schemaRefPrefix is where OpenAPI 3.x keeps named schemas. It is both the
// prefix stripped off a $ref to get a schema's name and the pointer reported
// back as a schema result's location.
const schemaRefPrefix = "#/components/schemas/"

// Kind is the sort of spec element a search result names.
type Kind string

const (
	// KindAny is the zero Kind: search every kind rather than restrict to one.
	KindAny       Kind = ""
	KindOperation Kind = "operation"
	KindSchema    Kind = "schema"
	KindParam     Kind = "param"
)

// kinds lists every searchable Kind in the order shown to users.
var kinds = []Kind{KindOperation, KindSchema, KindParam}

// Kinds returns every valid --kind value. Callers rendering a structured usage
// error need the list as data, not as prose inside ParseKind's message.
func Kinds() []string {
	valid := make([]string, len(kinds))
	for i, k := range kinds {
		valid[i] = string(k)
	}

	return valid
}

// ParseKind converts a --kind value into a Kind. The empty string is KindAny,
// which searches everything.
func ParseKind(s string) (Kind, error) {
	if s == "" {
		return KindAny, nil
	}
	for _, k := range kinds {
		if Kind(s) == k {
			return k, nil
		}
	}

	return "", fmt.Errorf("unknown kind %q: valid values are %s", s, strings.Join(Kinds(), ", "))
}

// NamedSchema is one entry of components.schemas, kept as a proxy so callers
// read the real schema rather than a flattened copy.
type NamedSchema struct {
	Name   string
	Schema *base.SchemaProxy
}

// Result is one element search matched, labelled with the kind of thing it is
// so a caller can tell an endpoint from a schema from a parameter.
type Result struct {
	Kind Kind   `json:"kind"`
	Name string `json:"name"`
	// Where locates the element: "GET /pets" for an operation, the location
	// ("query", "path") for a parameter, and the component pointer for a
	// schema. It is what the caller feeds to the next command.
	Where   string `json:"where"`
	Summary string `json:"summary,omitempty"`
}

// Use is one operation that reaches a schema, and where in the operation it
// does so.
type Use struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Where lists the sites the schema is reachable from: "param:petId",
	// "request_body", "response:200".
	Where []string `json:"where"`
	// Direct reports that the schema is named at one of those sites itself,
	// rather than only reached through another schema.
	Direct bool `json:"direct"`
}

// The ranks a match scores, by the field it was found in. Lower leads the
// results: a caller who typed a concept wants the thing named after it before
// the things that merely mention it.
const (
	rankName    = iota // an operationId, schema name or parameter name
	rankAddress        // the path an operation lives at
	rankSummary        // a one-line summary
	rankProse          // a full description
)

// element is one searchable thing plus the lower-cased texts it can be found
// by. Matching against a prepared haystack is what keeps search from
// re-lowercasing the whole spec per query.
type element struct {
	result Result
	fields []field
}

// field is one searchable text of an element and the rank a match in it scores.
type field struct {
	text string
	rank int
}

// match returns the best rank at which query occurs in the element, or -1 when
// it does not occur at all.
func (e element) match(query string) int {
	best := -1
	for _, f := range e.fields {
		if f.text == "" || !strings.Contains(f.text, query) {
			continue
		}
		if best < 0 || f.rank < best {
			best = f.rank
		}
	}

	return best
}

// Search returns every element whose searchable text contains term, ranked by
// where the match landed and then by how close the element's name is to the
// term. Matching is case-insensitive and substring-based: an agent probing for
// a concept types the concept, not the exact identifier.
//
// A term that matches nothing yields an empty slice, not an error. "This API
// has no such thing" is a useful answer, not a usage mistake.
func (ix *Index) Search(term string, kind Kind) []Result {
	query := strings.ToLower(term)

	type scored struct {
		result Result
		rank   int
		dist   int
	}

	var matches []scored
	for _, el := range ix.searchElements() {
		if kind != KindAny && el.result.Kind != kind {
			continue
		}

		rank := el.match(query)
		if rank < 0 {
			continue
		}
		matches = append(matches, scored{
			result: el.result,
			rank:   rank,
			// Distance to the name, whichever field actually matched: among
			// equally-ranked hits the one whose name is closest to the term is
			// the one that was meant.
			dist: distance(query, el.fields[0].text),
		})
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		if matches[i].dist != matches[j].dist {
			return matches[i].dist < matches[j].dist
		}

		return matches[i].result.Name < matches[j].result.Name
	})

	out := make([]Result, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.result)
	}

	return out
}

// SchemaNames returns the document's component schema names in spec order.
func (ix *Index) SchemaNames() []string {
	out := make([]string, 0, len(ix.schemas))
	for _, s := range ix.schemas {
		out = append(out, s.Name)
	}

	return out
}

// Uses returns every operation that reaches the named component schema, whether
// the operation names it directly or arrives at it through another schema —
// an operation returning PetList is reported for Pet, because a caller changing
// Pet has to know about it.
//
// An unknown name is a usage error (exit 2) carrying the closest schema names,
// but a schema nothing references is not: the empty answer is the true one.
func (ix *Index) Uses(name string) ([]Use, error) {
	if !ix.hasSchema(name) {
		return nil, clierr.Usage("unknown schema %q", name).
			WithAlternatives(closest(name, ix.SchemaNames())...)
	}

	reach := ix.reachability()

	out := make([]Use, 0, len(ix.ops))
	for _, op := range ix.ops {
		var where []string
		seen := make(map[string]bool)
		direct := false

		for _, s := range sites(op) {
			refs := schemaRefs(s.schema)
			if !reaches(refs, name, reach) {
				continue
			}
			if refs[name] {
				direct = true
			}
			// Two media types on one response are one site as far as the
			// caller is concerned.
			if seen[s.label] {
				continue
			}
			seen[s.label] = true
			where = append(where, s.label)
		}

		if len(where) > 0 {
			out = append(out, Use{ID: op.ID, Method: op.Method, Path: op.Path, Where: where, Direct: direct})
		}
	}

	return out, nil
}

func (ix *Index) hasSchema(name string) bool {
	for _, s := range ix.schemas {
		if s.Name == name {
			return true
		}
	}

	return false
}

// reaches reports whether name is in refs or reachable from anything in it.
func reaches(refs map[string]bool, name string, reach map[string]map[string]bool) bool {
	if refs[name] {
		return true
	}
	for r := range refs {
		if reach[r][name] {
			return true
		}
	}

	return false
}

// site is one place inside an operation a schema can appear, with the label
// `uses` reports it under.
type site struct {
	label  string
	schema *base.SchemaProxy
}

// sites lists every schema-bearing place in an operation: its parameters, each
// media type of its request body, and each media type of each response.
func sites(op Operation) []site {
	var out []site

	for _, p := range op.Params {
		out = append(out, site{label: "param:" + p.Name, schema: p.Schema})
	}
	if op.RequestBody != nil {
		for _, m := range op.RequestBody.Content {
			out = append(out, site{label: "request_body", schema: m.Schema})
		}
	}
	for _, r := range op.Responses {
		for _, m := range r.Content {
			out = append(out, site{label: "response:" + r.Status, schema: m.Schema})
		}
	}

	return out
}

// searchElements returns the search haystack, building it on first use. It is
// built lazily rather than in the constructor because it resolves every
// component schema in the document, and list and describe — which build an
// index on every invocation — never search.
func (ix *Index) searchElements() []element {
	if ix.elements == nil {
		ix.elements = ix.buildElements()
	}

	return ix.elements
}

func (ix *Index) buildElements() []element {
	var out []element

	for _, op := range ix.ops {
		out = append(out, element{
			result: Result{
				Kind:    KindOperation,
				Name:    op.ID,
				Where:   op.Method + " " + op.Path,
				Summary: collapse(op.Summary),
			},
			fields: []field{
				{text: strings.ToLower(op.ID), rank: rankName},
				{text: strings.ToLower(op.Path), rank: rankAddress},
				{text: strings.ToLower(op.Summary), rank: rankSummary},
				{text: strings.ToLower(op.Description), rank: rankProse},
			},
		})
	}

	for _, s := range ix.schemas {
		description := collapse(schemaDescription(s.Schema))
		out = append(out, element{
			result: Result{
				Kind:    KindSchema,
				Name:    s.Name,
				Where:   schemaRefPrefix + s.Name,
				Summary: description,
			},
			fields: []field{
				{text: strings.ToLower(s.Name), rank: rankName},
				{text: strings.ToLower(description), rank: rankProse},
			},
		})
	}

	// A parameter is a property of the API, not of one endpoint: the same
	// petId on five operations is one thing to find, so it is reported once.
	seen := make(map[[2]string]bool)
	for _, op := range ix.ops {
		for _, p := range op.Params {
			key := [2]string{p.Name, p.In}
			if seen[key] {
				continue
			}
			seen[key] = true

			description := collapse(p.Description)
			out = append(out, element{
				result: Result{
					Kind:    KindParam,
					Name:    p.Name,
					Where:   p.In,
					Summary: description,
				},
				fields: []field{
					{text: strings.ToLower(p.Name), rank: rankName},
					{text: strings.ToLower(description), rank: rankProse},
				},
			})
		}
	}

	return out
}

// reachability returns, for every component schema, the set of schema names
// reachable from it. It is built on first use for the same reason as the search
// haystack: it walks every schema in the document, and most commands never ask.
func (ix *Index) reachability() map[string]map[string]bool {
	if ix.reachable == nil {
		ix.reachable = buildReachability(ix.schemas)
	}

	return ix.reachable
}

// buildReachability computes the transitive closure of the component schema
// reference graph. It iterates to a fixpoint rather than recursing, which is
// what makes a reference cycle — Pet referring to Owner referring back to Pet —
// terminate instead of blowing the stack.
func buildReachability(schemas []NamedSchema) map[string]map[string]bool {
	reach := make(map[string]map[string]bool, len(schemas))
	for _, s := range schemas {
		reach[s.Name] = schemaRefs(s.Schema)
	}

	for {
		changed := false
		for name := range reach {
			// Collected first, applied after: adding to a map mid-range is not
			// guaranteed to be seen by that range.
			var add []string
			for r := range reach[name] {
				for t := range reach[r] {
					if !reach[name][t] {
						add = append(add, t)
					}
				}
			}
			for _, t := range add {
				reach[name][t] = true
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	return reach
}

// schemaRefs returns the component schemas named directly by a schema: it walks
// the inline structure but stops at every $ref rather than following it, so the
// walk is over a finite tree and the following is left to the closure.
func schemaRefs(sp *base.SchemaProxy) map[string]bool {
	refs := make(map[string]bool)
	collectRefs(sp, refs)

	return refs
}

func collectRefs(sp *base.SchemaProxy, into map[string]bool) {
	if sp == nil {
		return
	}
	if name, ok := refName(sp); ok {
		into[name] = true

		return
	}

	s := sp.Schema()
	if s == nil {
		return
	}

	if s.Properties != nil {
		for _, p := range s.Properties.FromOldest() {
			collectRefs(p, into)
		}
	}
	if s.Items != nil && s.Items.IsA() {
		collectRefs(s.Items.A, into)
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties.IsA() {
		collectRefs(s.AdditionalProperties.A, into)
	}
	for _, branch := range [][]*base.SchemaProxy{s.AllOf, s.OneOf, s.AnyOf} {
		for _, b := range branch {
			collectRefs(b, into)
		}
	}
	collectRefs(s.Not, into)
}

// refName returns the schema name a proxy references, or false for an inline
// schema. A reference into components.schemas gives up its name directly;
// anything else — a converted Swagger definitions pointer, an external file —
// falls back to the last segment, which is the name everywhere it matters.
func refName(sp *base.SchemaProxy) (string, bool) {
	if sp == nil || !sp.IsReference() {
		return "", false
	}

	ref := sp.GetReference()
	if name := strings.TrimPrefix(ref, schemaRefPrefix); name != ref {
		return name, true
	}
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:], true
	}

	return ref, true
}

// schemaDescription resolves a component schema far enough to read its
// description, which is the only prose a schema is searchable by.
func schemaDescription(sp *base.SchemaProxy) string {
	if sp == nil {
		return ""
	}
	if s := sp.Schema(); s != nil {
		return s.Description
	}

	return ""
}

// componentSchemas lists a document's named schemas in spec order.
func componentSchemas(doc *spec.Document) []NamedSchema {
	if doc == nil || doc.Model == nil || doc.Model.Components == nil || doc.Model.Components.Schemas == nil {
		return nil
	}

	schemas := doc.Model.Components.Schemas
	out := make([]NamedSchema, 0, schemas.Len())
	for name, sp := range schemas.FromOldest() {
		out = append(out, NamedSchema{Name: name, Schema: sp})
	}

	return out
}

// collapse folds prose onto one line: spec descriptions are frequently
// multi-paragraph markdown, and search prints one line per result.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
