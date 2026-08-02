// Package gen turns a schema into request data for `run` mode.
//
// Two rules shape everything here. The first is examples-first (DESIGN.md §5a):
// a value the spec author wrote is realistic in a way a generated one never is,
// so an `example` is used verbatim and nothing else is consulted. The second is
// determinism: every draw comes from a seeded source held on the Generator, so
// the same seed and the same spec produce byte-identical data. A smoke test
// whose body changes between runs fails once, passes next time and teaches
// nobody anything.
//
// The package deliberately depends only on the schema model. It is shared with
// the twin server (DESIGN.md §5), so it must not reach into internal/curl or
// internal/twin — the boundary is asserted by a test.
package gen

import (
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/pb33f/libopenapi/datamodel/high/base"
	yaml "go.yaml.in/yaml/v4"
)

// DefaultMaxDepth is how many levels of nesting the generator will produce
// before it stops descending. Deep enough for the bodies real APIs accept,
// shallow enough that a schema with a long tail of nested objects cannot turn a
// smoke test into a megabyte of noise.
const DefaultMaxDepth = 4

// maxRefRepeats is how many times one $ref may appear on a single branch: the
// value itself, plus one level of self-nesting. Unlike a renderer, a generator
// producing `parent: {}` for a recursive field emits data the server will
// reject, so one real level is worth having — and two is already more than a
// smoke test learns anything from.
const maxRefRepeats = 2

// epoch is the instant every generated timestamp is offset from. A fixed epoch
// rather than time.Now() is what makes date-time data reproducible: two runs at
// the same seed have to produce the same bytes, and a clock reading would break
// that on its own.
var epoch = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

// words is the vocabulary generated strings are built from. A short fixed list
// keeps values recognisable as test data at a glance, which matters when they
// turn up in someone's staging database.
var words = []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}

// Generator produces data from schemas. Create one per run: the seeded source
// lives on it, so two Generators made with the same seed are interchangeable
// and neither touches the global rand.
type Generator struct {
	rng *rand.Rand
	// MaxDepth bounds how far generation nests. Zero means DefaultMaxDepth.
	MaxDepth int
	// Fixtures are the user's own test data, consulted by DataFor after spec
	// examples and before generation. Nil is an empty set.
	Fixtures *Fixtures
}

// New returns a Generator drawing from seed. The same seed always produces the
// same data for the same schema.
func New(seed int64) *Generator {
	// PCG needs two words of state; deriving the second from the first keeps
	// the caller's single seed the whole story.
	return &Generator{
		rng:      rand.New(rand.NewPCG(uint64(seed), uint64(seed)+0x9e3779b9)),
		MaxDepth: DefaultMaxDepth,
	}
}

// Value generates one value for a schema, shaped for encoding/json: strings,
// float64/int64, bool, []any and map[string]any. A nil schema, or one the
// generator declined to descend into, yields nil.
func (g *Generator) Value(sp *base.SchemaProxy) any {
	v, _ := g.value(sp, 0, make(map[string]int))

	return v
}

// value generates a value, reporting whether it produced one at all. The bool
// is how the cycle guard and the depth limit express "there is nothing sensible
// to put here": the caller then omits the property or the array element rather
// than writing a null, which would be a claim about the data rather than an
// absence of it.
//
// onPath counts the $refs on the current branch, not every ref ever seen — the
// same schema may legitimately appear in two sibling properties, and only a
// reference back into one's own ancestry is a cycle.
func (g *Generator) value(sp *base.SchemaProxy, depth int, onPath map[string]int) (any, bool) {
	if sp == nil {
		return nil, false
	}

	// The cycle key is the $ref, matching internal/output's walker: recursion
	// in a real spec always goes through a reference.
	if key := refKey(sp); key != "" {
		if onPath[key] >= maxRefRepeats {
			return nil, false
		}
		onPath[key]++
		defer func() { onPath[key]-- }()
	}

	s := sp.Schema()
	if s == nil {
		return nil, false
	}

	// Examples win outright (DESIGN.md §5a), before type, format or enum.
	if v, ok := exampleValue(s); ok {
		return v, true
	}
	if len(s.Enum) > 0 {
		return decodeNode(s.Enum[g.rng.IntN(len(s.Enum))])
	}

	// oneOf and anyOf are a choice; take the first branch so the same spec
	// keeps producing the same shape as the schema grows siblings. allOf is not
	// a choice — every branch applies at once — and is merged into the object
	// case instead.
	if branch := firstBranch(s); branch != nil {
		return g.value(branch, depth, onPath)
	}

	switch schemaType(s) {
	case "object":
		return g.object(s, depth, onPath)
	case "array":
		return g.array(s, depth, onPath)
	case "boolean":
		return g.rng.IntN(2) == 0, true
	case "integer":
		return g.integer(s), true
	case "number":
		return g.number(s), true
	case "null":
		return nil, true
	default:
		return g.text(s), true
	}
}

// object generates a value for an object schema. Required properties are always
// present — omitting one produces a request the server rejects before it ever
// reaches the behaviour under test — while optional ones are included on a coin
// flip, so a run exercises more than the minimal body.
func (g *Generator) object(s *base.Schema, depth int, onPath map[string]int) (any, bool) {
	if depth >= g.maxDepth() {
		return nil, false
	}

	required := requiredSet(s)
	out := make(map[string]any)
	for _, p := range propertyPairs(s) {
		if !required[p.name] && g.rng.IntN(2) == 0 {
			continue
		}
		if v, ok := g.value(p.schema, depth+1, onPath); ok {
			out[p.name] = v
		}
	}

	return out, true
}

// array generates a value for an array schema. Length comes from the schema's
// own bounds rather than the seeded source: one element proves the shape, and a
// length that varies with the seed makes two runs harder to diff for no gain.
func (g *Generator) array(s *base.Schema, depth int, onPath map[string]int) (any, bool) {
	if depth >= g.maxDepth() {
		return nil, false
	}

	count := 1
	if s.MinItems != nil && int(*s.MinItems) > count {
		count = int(*s.MinItems)
	}
	if s.MaxItems != nil && int(*s.MaxItems) < count {
		count = int(*s.MaxItems)
	}

	// An element the generator declined to produce takes the whole array with
	// it: an empty array where the schema says minItems is data the server
	// rejects, and a shorter array is not what was asked for either.
	items := arrayItems(s)
	out := make([]any, 0, count)
	for range count {
		v, ok := g.value(items, depth+1, onPath)
		if !ok {
			return nil, false
		}
		out = append(out, v)
	}

	return out, true
}

// text generates a string, honouring the format hint and any length bounds.
// Formats matter more than they look: a plain word in a `uri` or `date-time`
// field fails validation at the server, and a smoke test that only ever proves
// "the API rejects nonsense" is not a smoke test.
func (g *Generator) text(s *base.Schema) string {
	var out string

	switch s.Format {
	case "date-time":
		out = g.timestamp().Format(time.RFC3339)
	case "date":
		out = g.timestamp().Format(time.DateOnly)
	case "time":
		out = g.timestamp().Format("15:04:05Z07:00")
	case "uuid":
		out = g.uuid()
	case "email":
		out = g.word() + "@example.com"
	case "hostname":
		out = g.word() + ".example.com"
	case "ipv4":
		out = fmt.Sprintf("192.0.2.%d", g.rng.IntN(254)+1)
	case "uri", "url", "uri-reference":
		out = "https://example.com/" + g.word()
	case "byte":
		out = base64.StdEncoding.EncodeToString([]byte(g.word()))
	case "password":
		out = g.word() + "-" + g.word()
	default:
		out = g.word()
	}

	return clampLength(out, s)
}

// integer generates an integer inside the schema's bounds, as int64 so it
// marshals without a decimal point.
func (g *Generator) integer(s *base.Schema) int64 {
	low, high := int64(1), int64(1000)
	if s.Minimum != nil {
		low = int64(*s.Minimum)
	}
	if s.Maximum != nil {
		high = int64(*s.Maximum)
	}
	if high <= low {
		return low
	}

	return low + g.rng.Int64N(high-low+1)
}

// number generates a fractional value inside the schema's bounds. The fraction
// is deliberate: a whole float is indistinguishable from an integer once it is
// JSON, and a `number` field is worth exercising as one.
func (g *Generator) number(s *base.Schema) float64 {
	if s.Minimum != nil || s.Maximum != nil {
		low, high := 0.0, 1000.0
		if s.Minimum != nil {
			low = *s.Minimum
		}
		if s.Maximum != nil {
			high = *s.Maximum
		}
		if high <= low {
			return low
		}

		return low + g.rng.Float64()*(high-low)
	}

	return float64(g.rng.IntN(1000)) + float64(g.rng.IntN(99)+1)/100
}

// timestamp returns an instant offset from the fixed epoch, so timestamps vary
// with the seed but not with the wall clock.
func (g *Generator) timestamp() time.Time {
	return epoch.Add(time.Duration(g.rng.IntN(365*24)) * time.Hour).UTC()
}

// uuid builds a version 4 UUID from the seeded source rather than crypto/rand,
// which is the whole point: these are test identifiers, and they have to repeat.
func (g *Generator) uuid() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(g.rng.IntN(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (g *Generator) word() string {
	return fmt.Sprintf("%s-%d", words[g.rng.IntN(len(words))], g.rng.IntN(1000))
}

func (g *Generator) maxDepth() int {
	if g.MaxDepth <= 0 {
		return DefaultMaxDepth
	}

	return g.MaxDepth
}

// clampLength brings a generated string inside the schema's length bounds.
// Padding repeats a filler character rather than generating more words: the
// point is to satisfy minLength, not to look pretty.
func clampLength(s string, schema *base.Schema) string {
	if schema.MaxLength != nil && int64(len(s)) > *schema.MaxLength {
		s = s[:*schema.MaxLength]
	}
	if schema.MinLength != nil && int64(len(s)) < *schema.MinLength {
		s += strings.Repeat("x", int(*schema.MinLength)-len(s))
	}

	return s
}

// exampleValue returns the schema's example, preferring the 3.0 singular
// `example` over the 3.1 `examples` list. A caller that wrote both meant both
// to be the same thing.
func exampleValue(s *base.Schema) (any, bool) {
	if s.Example != nil {
		return decodeNode(s.Example)
	}
	for _, node := range s.Examples {
		if node != nil {
			return decodeNode(node)
		}
	}

	return nil, false
}

// decodeNode turns a yaml node from the spec into the plain Go value the rest
// of the pipeline marshals.
func decodeNode(node *yaml.Node) (any, bool) {
	if node == nil {
		return nil, false
	}

	var out any
	if err := node.Decode(&out); err != nil {
		return nil, false
	}

	return out, true
}

// schemaType names the type to generate. A schema with no declared type is
// common in the wild, so the shape it does declare decides: properties mean an
// object, items mean an array, and anything else falls through to a string,
// which is the type most likely to be accepted.
func schemaType(s *base.Schema) string {
	for _, t := range s.Type {
		if t != "null" {
			return t
		}
	}

	if (s.Properties != nil && s.Properties.Len() > 0) || len(s.AllOf) > 0 {
		return "object"
	}
	if s.Items != nil && s.Items.IsA() {
		return "array"
	}
	if len(s.Type) > 0 {
		return "null" // the type was declared, and it was only "null"
	}

	return "string"
}

// firstBranch returns the branch to generate from for a oneOf/anyOf schema, or
// nil if the schema is not a choice.
func firstBranch(s *base.Schema) *base.SchemaProxy {
	for _, branches := range [][]*base.SchemaProxy{s.OneOf, s.AnyOf} {
		for _, branch := range branches {
			if branch != nil {
				return branch
			}
		}
	}

	return nil
}

// arrayItems returns the element schema of an array, or nil for a schema whose
// items are the 3.1 boolean form.
func arrayItems(s *base.Schema) *base.SchemaProxy {
	if s.Items == nil || !s.Items.IsA() {
		return nil
	}

	return s.Items.A
}

// property is one entry of an object's properties, kept as a pair so allOf
// branches can be spliced into the parent's list in order.
type property struct {
	name   string
	schema *base.SchemaProxy
}

// propertyPairs lists the properties of a schema in spec order, with allOf
// branches contributing theirs too: allOf means every branch applies at once,
// so the fields to generate are the union.
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

// refKey is the cycle-guard key for a schema: its $ref, or empty for an inline
// schema, which cannot be its own ancestor.
func refKey(sp *base.SchemaProxy) string {
	if !sp.IsReference() {
		return ""
	}

	return sp.GetReference()
}
