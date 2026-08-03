// Package operation is the core model of the tool: one flat, spec-shaped
// description of every callable endpoint, built once from a *spec.Document and
// then read by everything downstream — list, describe, search, the request
// builder, the curl executor and response validation.
//
// The model deliberately stops at the schema boundary. Parameters, request
// bodies and responses carry the libopenapi schema proxy rather than a
// flattened copy, because describe and the data generator need the real schema
// — including its resolved $refs — and re-deriving it later would mean every
// consumer reaching back into the parser.
package operation

import (
	"github.com/pb33f/libopenapi/datamodel/high/base"
)

// StatusDefault is the status key OpenAPI uses for the fallback response — the
// one that applies when no explicit code matches.
const StatusDefault = "default"

// Operation is one method/path pair from a spec.
type Operation struct {
	// ID is the spec's operationId. It can be empty: not every spec sets one,
	// and synthesising a stable ID is a separate concern.
	ID string
	// Method is the HTTP method, upper-cased.
	Method string
	// Path is the templated path as written in the spec, e.g. /pets/{petId}.
	Path        string
	Summary     string
	Description string
	Tags        []string
	Deprecated  bool
	// Params are the path-item and operation parameters merged into one list,
	// with operation-level entries winning on a (name, in) collision.
	Params []Param
	// RequestBody is nil for operations that take no body.
	RequestBody *RequestBody
	// Responses are the declared response contracts, ordered by status with
	// StatusDefault last.
	Responses []Response
	// Security is the effective requirement list: the operation's own block if
	// it has one, otherwise the document-level block. Empty means no auth,
	// which is also what an explicit `security: []` resolves to. Any one
	// requirement in the list is enough to authenticate the call.
	Security []SecurityRequirement
}

// Param is a single input to an operation.
type Param struct {
	Name string
	// In is the location: path, query, header or cookie.
	In          string
	Required    bool
	Description string
	// Schema is the parameter's schema, still as a proxy so callers can render
	// or generate from the real thing. It can be nil for a content-typed
	// parameter, which is rare and not yet supported.
	Schema *base.SchemaProxy
}

// RequestBody is the body contract of an operation.
type RequestBody struct {
	Required    bool
	Description string
	// Content lists the accepted media types in spec order.
	Content []MediaType
}

// Response is one declared outcome of an operation.
type Response struct {
	// Status is the status code as written in the spec — "200", "4XX", or
	// StatusDefault.
	Status      string
	Description string
	// Content lists the response media types in spec order. It is empty for a
	// response with no body, such as a 204.
	Content []MediaType
}

// MediaType pairs a content type with the schema that applies to it.
type MediaType struct {
	ContentType string
	Schema      *base.SchemaProxy
}

// SecurityRequirement is one alternative way to authenticate an operation.
// Every scheme it names has to be satisfied together; a requirement with no
// schemes means authentication is optional.
type SecurityRequirement struct {
	Schemes []SecurityScheme
}

// SecurityScheme names one scheme from components.securitySchemes, along with
// the scopes this operation needs from it.
type SecurityScheme struct {
	Name   string
	Scopes []string
}

// safeMethods are the HTTP methods DESIGN.md §4 treats as read-only. Anything
// outside this set needs --allow-mutations, so the check is a deny-list by
// design: an unfamiliar method is assumed to change state.
var safeMethods = map[string]bool{
	"GET":     true,
	"HEAD":    true,
	"OPTIONS": true,
}

// IsMutation reports whether calling this operation may change server state,
// and so whether it falls under the --allow-mutations gate.
func (o Operation) IsMutation() bool {
	return !safeMethods[o.Method]
}

// ResponseFor returns the declared response for a status key, or nil if the
// operation does not declare one. Pass StatusDefault for the fallback; the
// caller decides whether to fall back, because a status with no contract is
// meaningful to validation.
func (o Operation) ResponseFor(status string) *Response {
	for i := range o.Responses {
		if o.Responses[i].Status == status {
			return &o.Responses[i]
		}
	}

	return nil
}
