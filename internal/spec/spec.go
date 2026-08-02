// Package spec loads OpenAPI descriptions into one in-memory representation.
// Everything downstream — the operation model, list, describe, call — reads a
// *Document and never touches the parser, so the differences between JSON and
// YAML, between 3.0 and 3.1, and (from Task 5) between Swagger 2.0 and OpenAPI
// 3.x stop at this package boundary.
package spec

import (
	v3high "github.com/pb33f/libopenapi/datamodel/high/v3"
)

// Document is a loaded, built OpenAPI 3.x description. Model is the high-level
// libopenapi model with references already resolved; callers read it directly
// rather than re-parsing the source bytes.
type Document struct {
	// Version is the exact spec version string, e.g. "3.0.3" or "3.1.0".
	Version string
	// Source describes where the bytes came from — a file path, a URL, or
	// "<bytes>" for an in-memory spec. It exists so errors raised well after
	// loading can still say which spec they came from.
	Source string
	// Model is the built OpenAPI 3.x model.
	Model *v3high.Document
}
