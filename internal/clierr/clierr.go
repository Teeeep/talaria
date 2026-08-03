// Package clierr carries talaria's exit-code contract. Every failure that
// reaches the command line is an *Error holding one of the codes from
// DESIGN.md §4, so the mapping lives in one place rather than being rederived
// at each call site. Agents branch on these codes, and code 5 in particular
// exists so an agent can tell a human "set $NAME" instead of guessing.
package clierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Teeeep/talaria/internal/output"
)

// Code is a process exit code. The values are the contract in DESIGN.md §4 and
// are part of talaria's public interface: changing one is a breaking change.
type Code int

const (
	// CodeOK reports success. For call, an HTTP 4xx/5xx is still a successful
	// observation and exits 0; --fail-on-error changes that.
	CodeOK Code = 0
	// CodeRequestFailed reports that the request could not be completed at all
	// (network failure, curl itself failing). It is also the fallback for any
	// error that was never classified.
	CodeRequestFailed Code = 1
	// CodeUsage reports a bad invocation: unknown operation, missing required
	// parameter, unparseable flag value.
	CodeUsage Code = 2
	// CodeSpecLoad reports that the OpenAPI/Swagger spec could not be read or
	// parsed.
	CodeSpecLoad Code = 3
	// CodeValidation reports that a response violates the spec. Only reachable
	// with --fail-on-error or run.
	CodeValidation Code = 4
	// CodeCredentialMissing reports that a required security scheme has no
	// credential. It is distinct from CodeUsage precisely so an agent can act on
	// it by asking a human to set the named variable.
	CodeCredentialMissing Code = 5
)

// Error is a failure carrying the exit code talaria will exit with, plus the
// alternatives that would have been valid. It implements error and Unwrap, so
// callers may add context with fmt.Errorf("%w") without losing the code.
type Error struct {
	Code    Code
	Message string
	// Alternatives are the values that would have been accepted, rendered as
	// error.valid_alternatives (§3.1: "what failed, why, valid alternatives").
	Alternatives []string

	err error
}

func (e *Error) Error() string { return e.Message }

// Unwrap exposes the error passed to the constructor via %w, keeping errors.Is
// working through the classification.
func (e *Error) Unwrap() error { return e.err }

// WithAlternatives attaches the valid values for the thing that failed and
// returns e, so it can be chained onto a constructor.
func (e *Error) WithAlternatives(alts ...string) *Error {
	e.Alternatives = alts
	return e
}

// Usage reports a bad invocation (exit 2).
func Usage(format string, a ...any) *Error { return newError(CodeUsage, format, a...) }

// SpecLoad reports a spec that could not be read or parsed (exit 3).
func SpecLoad(format string, a ...any) *Error { return newError(CodeSpecLoad, format, a...) }

// Validation reports a response that violates the spec (exit 4).
func Validation(format string, a ...any) *Error { return newError(CodeValidation, format, a...) }

// CredentialMissing reports a required security scheme with no credential
// (exit 5).
func CredentialMissing(format string, a ...any) *Error {
	return newError(CodeCredentialMissing, format, a...)
}

// RequestFailed reports a request that could not be completed (exit 1).
func RequestFailed(format string, a ...any) *Error { return newError(CodeRequestFailed, format, a...) }

// newError formats the message through fmt.Errorf so constructors accept %w and
// keep the wrapped error reachable by errors.Is.
func newError(code Code, format string, a ...any) *Error {
	formatted := fmt.Errorf(format, a...)
	return &Error{
		Code:    code,
		Message: formatted.Error(),
		err:     errors.Unwrap(formatted),
	}
}

// From converts any error into an *Error so every failure path renders the same
// shape. An error that is or wraps an *Error keeps its code; anything else was
// never classified and falls back to CodeRequestFailed. From(nil) is nil.
func From(err error) *Error {
	if err == nil {
		return nil
	}

	var target *Error
	if errors.As(err, &target) {
		return target
	}

	return &Error{Code: CodeRequestFailed, Message: err.Error(), err: err}
}

// renderable is the stderr JSON shape: the same versioned envelope as stdout,
// with the failure under "error".
type renderable struct {
	Schema string        `json:"schema"`
	Error  renderableErr `json:"error"`
}

type renderableErr struct {
	Code         Code     `json:"code"`
	Message      string   `json:"message"`
	Alternatives []string `json:"valid_alternatives,omitempty"`
}

// Render writes err to w as one line of structured JSON. A nil error writes
// nothing. The payload contains only strings and an int, so marshalling cannot
// fail; a failed write to stderr is not itself reportable and is dropped.
func Render(w io.Writer, err error) {
	cerr := From(err)
	if cerr == nil {
		return
	}

	body, marshalErr := json.Marshal(renderable{
		Schema: output.SchemaVersion,
		Error: renderableErr{
			Code:         cerr.Code,
			Message:      cerr.Message,
			Alternatives: cerr.Alternatives,
		},
	})
	if marshalErr != nil {
		return
	}

	fmt.Fprintf(w, "%s\n", body)
}
