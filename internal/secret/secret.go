// Package secret is the credential firewall from DESIGN.md §5a. Its invariant
// is that no consumer of stdout or stderr ever observes a credential value.
//
// The way that invariant is kept is structural rather than procedural: a
// SecretRef holds the *name* of a credential and no field that could hold its
// value, and it renders itself redacted under every printf verb and under
// encoding/json. Everything downstream — output, validation, history, corpus,
// errors — carries refs, so a leak requires a deliberate call to Resolve, which
// is greppable and reviewable. Forgetting to scrub a string is neither.
//
// Resolve belongs to internal/curl at exec time and to nothing else.
package secret

import (
	"encoding/json"
	"os"

	"github.com/Teeeep/talaria/internal/clierr"
)

// SourceEnv marks a ref that names an environment variable. It is the only
// source talaria resolves today; profile-file credentials arrive as env refs
// after the profile layer has exported them.
const SourceEnv = "env"

// SecretRef names a credential without being able to carry it. The absent
// value field is the design: a type that *cannot* print a secret is what makes
// every other package in talaria safe by construction.
type SecretRef struct {
	// Source says where the value will be read from at exec time, e.g. SourceEnv.
	Source string
	// Name identifies the credential within that source — for SourceEnv, the
	// environment variable's name. It is safe to print; that is the point.
	Name string
}

// Env returns a ref to the named environment variable.
func Env(name string) SecretRef { return SecretRef{Source: SourceEnv, Name: name} }

// String returns the display form, <redacted:env:NAME>. Defining it on the
// value receiver is deliberate: fmt then uses it for a SecretRef, a *SecretRef,
// and a SecretRef held as a field of some larger struct printed with %v.
func (r SecretRef) String() string { return "<redacted:" + r.Source + ":" + r.Name + ">" }

// GoString returns the same redacted form, so %#v — the verb reached for when
// debugging, i.e. exactly when a leak would be least expected — is safe too.
func (r SecretRef) GoString() string { return r.String() }

// MarshalJSON renders the ref as the redacted string. Any JSON talaria emits
// that carries a credential position carries this instead of a value.
func (r SecretRef) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// Symbolic returns the shell form for emitted and dry-run curl, $NAME, which is
// runnable wherever the variable is set and useless to exfiltrate (§5a).
//
// A ref from any other source has nothing to interpolate and falls back to the
// redacted form: the emitted command stops being copy-pasteable, which is the
// correct trade against printing the value.
func (r SecretRef) Symbolic() string {
	if r.Source == SourceEnv {
		return "$" + r.Name
	}

	return r.String()
}

// IsZero reports whether the ref names nothing, which is how "this security
// scheme has no credential configured" is represented.
func (r SecretRef) IsZero() bool { return r.Source == "" && r.Name == "" }

// Resolve reads the referenced value. It is the one crossing of the firewall,
// and internal/curl is the only package that should call it — long enough to
// write curl's config document, and no longer.
//
// An unset or empty variable is a CredentialMissing failure (exit 5) rather
// than a generic usage error, so an agent can respond by asking a human to set
// the named variable instead of guessing at the invocation (§4).
func (r SecretRef) Resolve() (string, error) {
	switch r.Source {
	case SourceEnv:
		value, ok := os.LookupEnv(r.Name)
		if !ok || value == "" {
			return "", clierr.CredentialMissing("no credential in $%s: set it and retry", r.Name)
		}

		return value, nil
	default:
		return "", clierr.Usage("cannot resolve secret %s: unknown source %q", r.Name, r.Source)
	}
}
