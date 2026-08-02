// Package validate checks an observed HTTP response against the contract the
// spec declares for it: was the status documented, was the content type
// declared, does the body match the schema.
//
// It takes plain data — method, URL, status, headers, body bytes — rather than
// an http.Response or anything from internal/curl. That is the §5 boundary
// rule: this package is shared with the twin server and, later, with replay
// from the corpus, so the caller converts its own representation into an Input
// and this package converts nothing back.
package validate

import (
	"bytes"
	"io"
	"net/http"
	"net/url"

	validator "github.com/pb33f/libopenapi-validator"
	liberrors "github.com/pb33f/libopenapi-validator/errors"
	"github.com/pb33f/libopenapi-validator/helpers"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/spec"
)

// Input is one observed HTTP exchange, reduced to the data validation needs.
// Headers may be nil; Body may be empty, which is how a 204 arrives.
type Input struct {
	// Method is the HTTP method of the request that produced the response.
	Method string
	// URL is the request URL as called. It may point somewhere other than the
	// spec's declared server — --base-url and the twin both do that — and
	// routing tolerates it (see Validator.Response).
	URL string
	// Status is the HTTP status code observed.
	Status int
	// Headers are the response headers.
	Headers http.Header
	// Body is the raw response body.
	Body []byte
}

// Result is the `validation` block of call's output (DESIGN.md §4). The three
// booleans answer the three questions independently, so an agent can tell "the
// server returned an undocumented 500" from "the documented 200 came back with
// the wrong shape" without parsing prose.
type Result struct {
	StatusDocumented      bool    `json:"status_documented"`
	ContentTypeDocumented bool    `json:"content_type_documented"`
	BodyValid             bool    `json:"body_valid"`
	Errors                []Error `json:"errors"`
}

// Error is one contract violation, flattened. A schema failure produces one
// Error per failing field rather than one per response, because the field is
// the actionable part.
type Error struct {
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
	// Field is the JSONPath to the offending value, e.g. "$[0].id". It is
	// empty for violations that are not about a field — an undocumented status
	// or content type.
	Field string `json:"field,omitempty"`
}

// Validator holds the compiled schemas for one document. Building it warms
// every schema in the spec, so `run` builds one and reuses it across the whole
// smoke test rather than paying that cost per response.
type Validator struct {
	v validator.Validator
}

// New builds a Validator for doc.
//
// No strictness options are passed. libopenapi-validator defaults to OpenAPI
// 3.1+ JSON Schema semantics, which DESIGN.md §8 flagged as a risk for 3.0
// documents; as of v0.14.0 the library reads the document's own version and
// rewrites the 3.0-only constructs (`nullable`, boolean exclusive bounds,
// singular `example`) before compiling, so the default is already correct.
// validate_test.go pins that behaviour against an adversarial 3.0 fixture.
func New(doc *spec.Document) *Validator {
	return &Validator{v: validator.NewValidatorFromV3Model(doc.Model)}
}

// Response validates in against the document.
//
// Routing is host- and prefix-tolerant by design: the validator matches on the
// path alone, so a call redirected by --base-url to a twin on localhost, with
// or without the server's path prefix, validates against the same operation as
// a call to the real host. The returned error is reserved for input this
// package cannot even attempt — an unparseable URL — not for a response that
// violates the contract, which is a Result with Errors and exit code 0 unless
// the caller asked for --fail-on-error.
func (val *Validator) Response(in Input) (*Result, error) {
	req, err := buildRequest(in)
	if err != nil {
		return nil, err
	}

	_, verrs := val.v.ValidateHttpResponse(req, buildResponse(in))

	res := &Result{
		StatusDocumented:      true,
		ContentTypeDocumented: true,
		BodyValid:             true,
		Errors:                []Error{},
	}
	for _, verr := range verrs {
		switch {
		case verr.ValidationType == helpers.PathValidation:
			// The spec has no such operation, so nothing about this response is
			// documented. Reporting the status as documented here would be a
			// claim about a contract that does not exist.
			res.StatusDocumented = false
			res.ContentTypeDocumented = false
			res.BodyValid = false
		case verr.ValidationSubType == helpers.ResponseBodyResponseCode:
			res.StatusDocumented = false
		case verr.ValidationSubType == helpers.RequestBodyContentType:
			res.ContentTypeDocumented = false
		default:
			res.BodyValid = false
		}
		res.Errors = append(res.Errors, flatten(verr)...)
	}

	return res, nil
}

// Response is the one-shot form: build a validator for doc and validate a
// single exchange. Callers validating many responses against one spec should
// use New instead.
func Response(doc *spec.Document, in Input) (*Result, error) {
	return New(doc).Response(in)
}

// buildRequest reconstructs the request the validator needs to locate the
// operation. Only the method and URL matter — the request body is never
// consulted when validating a response.
func buildRequest(in Input) (*http.Request, error) {
	parsed, err := url.Parse(in.URL)
	if err != nil {
		return nil, clierr.Usage("validating response: parsing request URL %q: %w", in.URL, err)
	}

	return &http.Request{
		Method: in.Method,
		URL:    parsed,
		Host:   parsed.Host,
		Header: http.Header{},
	}, nil
}

// buildResponse wraps the observed data in the http.Response the validator
// takes. The body is a fresh reader over the caller's bytes, so validating the
// same Input twice reads the same body twice.
func buildResponse(in Input) *http.Response {
	headers := in.Headers
	if headers == nil {
		headers = http.Header{}
	}

	return &http.Response{
		StatusCode:    in.Status,
		Header:        headers,
		Body:          io.NopCloser(bytes.NewReader(in.Body)),
		ContentLength: int64(len(in.Body)),
	}
}

// flatten turns one library error into the Errors it should surface as. A
// schema failure carries a list of per-field failures; anything else — an
// undocumented status, an undeclared content type — is a single Error.
func flatten(verr *liberrors.ValidationError) []Error {
	if len(verr.SchemaValidationErrors) == 0 {
		return []Error{{Message: verr.Message, Reason: verr.Reason}}
	}

	out := make([]Error, 0, len(verr.SchemaValidationErrors))
	for _, failure := range verr.SchemaValidationErrors {
		field := failure.FieldPath
		if field == "" {
			field = failure.FieldName
		}
		out = append(out, Error{Message: verr.Message, Reason: failure.Reason, Field: field})
	}

	return out
}
