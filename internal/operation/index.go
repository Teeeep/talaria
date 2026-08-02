package operation

import (
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/spec"
)

// maxAlternatives caps the suggestion list on a failed lookup. The point of
// valid_alternatives is to let an agent self-correct, and a list as long as the
// spec is the context dump this tool exists to avoid (DESIGN.md §3.1).
const maxAlternatives = 5

// Index is the addressable form of a spec's operations: every operation has an
// id, ids are unique, and lookup by id or tag is a map hit rather than a scan.
// Build it once per loaded document — synthesis is deterministic, so two
// indexes over the same spec agree on every id.
type Index struct {
	ops  []Operation
	byID map[string]int
	ids  []string
	// schemas are the document's component schemas in spec order. They are
	// empty for an index built from operations alone, which is all list and
	// describe need; search and uses go through NewIndexFor to get them.
	schemas []NamedSchema
	// elements and reachable are the search structures, built on first use.
	// See searchElements and reachability for why they are not built here.
	elements  []element
	reachable map[string]map[string]bool
}

// NewIndex assigns every operation an id and indexes them. Operations that
// already carry an operationId keep it — an author's id is part of the API's
// contract and is never rewritten, even when a synthesised id wants it. The
// input slice is left untouched.
//
// An index built this way knows nothing about component schemas, so Uses and
// `search --kind schema` have nothing to report; use NewIndexFor for those.
func NewIndex(ops []Operation) *Index {
	return newIndex(ops, nil)
}

// NewIndexFor indexes a whole document: its operations and the component
// schemas that search and uses read. It is what every spec-reading command
// builds, so a spec loaded once answers every question about it.
func NewIndexFor(doc *spec.Document) *Index {
	return newIndex(Extract(doc), componentSchemas(doc))
}

func newIndex(ops []Operation, schemas []NamedSchema) *Index {
	ix := &Index{
		schemas: schemas,
		ops:     make([]Operation, len(ops)),
		byID:    make(map[string]int, len(ops)),
		ids:     make([]string, 0, len(ops)),
	}
	copy(ix.ops, ops)

	// Authored ids are claimed first so synthesis can never take one, whatever
	// order the two operations appear in.
	for i := range ix.ops {
		if id := ix.ops[i].ID; id != "" {
			ix.claim(id, i)
		}
	}
	for i := range ix.ops {
		if ix.ops[i].ID == "" {
			ix.claim(SynthesiseID(ix.ops[i].Method, ix.ops[i].Path), i)
		}
	}

	// ids follows spec order rather than claim order, so callers see the
	// document as it was written.
	for i := range ix.ops {
		ix.ids = append(ix.ids, ix.ops[i].ID)
	}

	return ix
}

// claim binds an id to operation i, appending a counter until it is free. A
// duplicate is a defect in the spec or a collision between two generalised
// paths; either way the later operation has to stay reachable.
func (ix *Index) claim(id string, i int) {
	unique := id
	for n := 2; ; n++ {
		if _, taken := ix.byID[unique]; !taken {
			break
		}
		unique = id + strconv.Itoa(n)
	}

	ix.ops[i].ID = unique
	ix.byID[unique] = i
}

// Operations returns every operation in spec order, with ids filled in.
func (ix *Index) Operations() []Operation { return ix.ops }

// IDs returns every id in spec order.
func (ix *Index) IDs() []string { return ix.ids }

// Lookup finds an operation by exact id. Matching is case-sensitive because
// operationIds are, but a miss comes back as a usage error (exit 2) carrying
// the closest ids as valid_alternatives — including the one that differs only
// in casing, which is the mistake callers actually make.
func (ix *Index) Lookup(id string) (Operation, error) {
	if i, ok := ix.byID[id]; ok {
		return ix.ops[i], nil
	}

	return Operation{}, clierr.Usage("unknown operation %q", id).
		WithAlternatives(closest(id, ix.ids)...)
}

// ByTag returns the operations carrying tag, in spec order. Tags are compared
// exactly, as the spec writes them.
func (ix *Index) ByTag(tag string) []Operation {
	var out []Operation
	for _, op := range ix.ops {
		for _, t := range op.Tags {
			if t == tag {
				out = append(out, op)

				break
			}
		}
	}

	return out
}

// closest ranks candidates by edit distance to the query and returns the best
// few. Distance is measured on the lower-cased forms so a casing-only mistake
// scores zero and leads the list; ties break on the candidate itself to keep
// output stable.
func closest(query string, candidates []string) []string {
	type scored struct {
		id   string
		dist int
	}

	query = strings.ToLower(query)
	ranked := make([]scored, 0, len(candidates))
	for _, candidate := range candidates {
		ranked = append(ranked, scored{id: candidate, dist: distance(query, strings.ToLower(candidate))})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].dist != ranked[j].dist {
			return ranked[i].dist < ranked[j].dist
		}

		return ranked[i].id < ranked[j].id
	})

	if len(ranked) > maxAlternatives {
		ranked = ranked[:maxAlternatives]
	}

	out := make([]string, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.id)
	}

	return out
}

// SynthesiseID derives a stable id for an operation the spec did not name, by
// joining the method to the path's segments: GET /pets/{id} becomes
// getPetsById. Templated segments read as "By" plus the variable, which is how
// people describe those endpoints out loud. The result depends only on the
// method and path, so it is identical on every load of the same spec.
func SynthesiseID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))

	for _, segment := range strings.Split(path, "/") {
		if segment == "" {
			continue
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			b.WriteString("By")
			segment = segment[1 : len(segment)-1]
		}
		b.WriteString(camel(segment))
	}

	// The root path contributes nothing, leaving the bare method — "get" for
	// GET /. Unlovely, but unique and predictable.
	return b.String()
}

// camel upper-cases the first letter of each word in a path segment and drops
// the separators between them, leaving the rest of each word alone so an
// already-camelCased variable such as orderItemId survives as OrderItemId.
func camel(segment string) string {
	var b strings.Builder
	upper := true
	for _, r := range segment {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upper = true

			continue
		}
		if upper {
			b.WriteRune(unicode.ToUpper(r))
			upper = false

			continue
		}
		b.WriteRune(r)
	}

	return b.String()
}

// distance is Levenshtein edit distance over runes, kept to two rows because
// it runs once per operation on a failed lookup.
func distance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}

	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}

	return prev[len(br)]
}
