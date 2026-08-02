package operation

import (
	"sort"
	"strings"

	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3high "github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/orderedmap"

	"github.com/Teeeep/talaria/internal/spec"
)

// Extract walks a loaded spec and returns every operation it describes, in
// document order: path items as the spec lists them, and the methods under
// each path item in the order they appear. A spec with no paths yields nil
// rather than an error — an empty API is a valid one.
func Extract(doc *spec.Document) []Operation {
	if doc == nil || doc.Model == nil || doc.Model.Paths == nil {
		return nil
	}

	// The document-level requirement is the default for every operation that
	// does not state its own, so it is resolved once.
	docSecurity := extractSecurity(doc.Model.Security)

	var ops []Operation
	for path, item := range doc.Model.Paths.PathItems.FromOldest() {
		if item == nil {
			continue
		}
		for method, op := range item.GetOperations().FromOldest() {
			if op == nil {
				continue
			}
			ops = append(ops, buildOperation(path, method, item, op, docSecurity))
		}
	}

	return ops
}

func buildOperation(path, method string, item *v3high.PathItem, op *v3high.Operation, docSecurity []SecurityRequirement) Operation {
	out := Operation{
		ID:          op.OperationId,
		Method:      strings.ToUpper(method),
		Path:        path,
		Summary:     op.Summary,
		Description: op.Description,
		Tags:        op.Tags,
		Deprecated:  op.Deprecated != nil && *op.Deprecated,
		Params:      mergeParams(item.Parameters, op.Parameters),
		RequestBody: extractRequestBody(op.RequestBody),
		Responses:   extractResponses(op.Responses),
		Security:    docSecurity,
	}

	// An operation-level security block replaces the document-level one rather
	// than adding to it. libopenapi gives a non-nil empty slice for an explicit
	// `security: []` and nil when the key is absent, which is exactly the
	// distinction between "this endpoint needs no auth" and "inherit".
	if op.Security != nil {
		out.Security = extractSecurity(op.Security)
	}

	return out
}

// mergeParams folds the path-item parameters into the operation's own. Entries
// keep their declared order — path-item first — and an operation-level
// parameter replaces the path-item one in place when they share a (name, in),
// which is what the spec means by overriding.
func mergeParams(pathLevel, opLevel []*v3high.Parameter) []Param {
	merged := make([]*v3high.Parameter, 0, len(pathLevel)+len(opLevel))
	at := make(map[[2]string]int, len(pathLevel))

	add := func(p *v3high.Parameter) {
		if p == nil {
			return
		}
		key := [2]string{p.Name, p.In}
		if i, ok := at[key]; ok {
			merged[i] = p

			return
		}
		at[key] = len(merged)
		merged = append(merged, p)
	}
	for _, p := range pathLevel {
		add(p)
	}
	for _, p := range opLevel {
		add(p)
	}

	if len(merged) == 0 {
		return nil
	}

	params := make([]Param, 0, len(merged))
	for _, p := range merged {
		params = append(params, Param{
			Name:        p.Name,
			In:          p.In,
			Required:    p.Required != nil && *p.Required,
			Description: p.Description,
			Schema:      p.Schema,
		})
	}

	return params
}

func extractRequestBody(rb *v3high.RequestBody) *RequestBody {
	if rb == nil {
		return nil
	}

	return &RequestBody{
		Required:    rb.Required != nil && *rb.Required,
		Description: rb.Description,
		Content:     extractContent(rb.Content),
	}
}

// extractResponses flattens the coded responses and the default one into a
// single list. It sorts by status so output is stable across runs: libopenapi
// builds the code map concurrently, and "200 before 404 before default" is the
// order a reader expects anyway.
func extractResponses(responses *v3high.Responses) []Response {
	if responses == nil {
		return nil
	}

	var out []Response
	for status, resp := range responses.Codes.FromOldest() {
		out = append(out, newResponse(status, resp))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status < out[j].Status })

	if responses.Default != nil {
		out = append(out, newResponse(StatusDefault, responses.Default))
	}

	return out
}

func newResponse(status string, resp *v3high.Response) Response {
	out := Response{Status: status}
	if resp != nil {
		out.Description = resp.Description
		out.Content = extractContent(resp.Content)
	}

	return out
}

func extractContent(content *orderedmap.Map[string, *v3high.MediaType]) []MediaType {
	if content == nil {
		return nil
	}

	var out []MediaType
	for contentType, media := range content.FromOldest() {
		entry := MediaType{ContentType: contentType}
		if media != nil {
			entry.Schema = media.Schema
		}
		out = append(out, entry)
	}

	return out
}

// extractSecurity turns libopenapi's requirement objects into scheme names and
// scopes. Each requirement is one alternative; the schemes inside it all apply.
func extractSecurity(reqs []*base.SecurityRequirement) []SecurityRequirement {
	var out []SecurityRequirement
	for _, req := range reqs {
		if req == nil {
			continue
		}

		var schemes []SecurityScheme
		if req.Requirements != nil {
			for name, scopes := range req.Requirements.FromOldest() {
				schemes = append(schemes, SecurityScheme{Name: name, Scopes: scopes})
			}
		}
		out = append(out, SecurityRequirement{Schemes: schemes})
	}

	return out
}
