package request

import (
	"fmt"
	"strings"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/operation"
)

// Refuses reports the defects that make Build exit 2 for this document whatever
// the caller's flags say: a `paths:` key that is not an absolute path, and a
// credential whose scheme sends it under a name that cannot go where the scheme
// puts it. Both are spec-controlled text this package refuses to put on the
// wire, so a document carrying one cannot be called at all.
//
// It exists because `auth check` and `call` may not disagree (DESIGN.md:329)
// and the verdict `auth check` returns — config.Unsatisfied — cannot see these:
// the charset rules are this package's, and internal/config may not import it,
// since request imports config. So the pre-flight asks request about the
// document's strings exactly as it already asks it about the hosts.
//
// It answers about the *document*, never about the invocation. A required
// --param nobody passed is also an exit 2 from Build, and is deliberately not
// here: that is a command line to correct, and reporting it would tell an agent
// its spec was broken when the spec is fine.
//
// Every operation and every declared scheme is asked, because `auth check`
// answers about the spec: one hostile `paths:` key is enough to make a spec one
// no agent should be told is callable.
func Refuses(ops []operation.Operation, creds []config.Credential) error {
	var problems []string

	for _, op := range ops {
		if !isPathTemplate(op.Path) {
			problems = append(problems, badPathTemplate(op))
		}
	}

	for _, cred := range creds {
		if problem := credentialNameProblem(cred); problem != "" {
			problems = append(problems, problem)
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return clierr.Usage("this spec cannot be called: %s", strings.Join(problems, "; "))
}

// badPathTemplate is the refusal isPathTemplate earns an operation, in one
// place because two surfaces report it: binder.path when a call is built, and
// Refuses when `auth check` asks the same question ahead of the call. A
// pre-flight whose wording differs from the failure it predicts reads as a
// second, unrelated problem.
func badPathTemplate(op operation.Operation) string {
	return fmt.Sprintf("the spec gives %s the path %q, which is not an absolute path: it must begin with / "+
		"and hold no space or control character, or it would move the request to another host",
		operationName(op), op.Path)
}

// credentialNameProblem returns why this credential's name cannot go where its
// scheme sends it, or "" if it can. It is the seam binder.credentialName and
// Refuses share, for badPathTemplate's reason.
//
// For an apiKey scheme the name is `components.securitySchemes.<x>.name` — a
// string the spec supplies and this process puts on the wire as a header name,
// a query name or a cookie name. That is the same class as a media type, a
// `paths:` key and a server variable, each of which is gated where it is read.
// A name carrying a colon renders the malformed header `X-Key: yes:
// <credential>`, and one carrying a CR or LF ends the line early and appends a
// header nobody wrote — with the credential attached, since it is the
// credential's own header.
//
// The rule is exactly binder.located's, because the two names end up in the
// same places: only a header name is an HTTP field name, while a query or
// cookie name is an ordinary string an API is free to spell `filter[key]` and
// only has to stay inside its own field.
func credentialNameProblem(cred config.Credential) string {
	switch {
	case cred.In == inHeader && !isFieldName(cred.Name):
		return fmt.Sprintf("scheme %q sends its credential in header %q, which is not a valid HTTP header name",
			cred.Scheme, cred.Name)
	case cred.In != inHeader && SplitsRequest(cred.Name, ""):
		return fmt.Sprintf("scheme %q sends its credential in %s %q, whose name carries a carriage return or "+
			"newline and would append a header the caller did not write", cred.Scheme, cred.In, cred.Name)
	}

	return ""
}
