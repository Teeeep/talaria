package config

import (
	"regexp"
	"strings"

	v3high "github.com/pb33f/libopenapi/datamodel/high/v3"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/secret"
	"github.com/Teeeep/talaria/internal/spec"
)

// The env-var convention from DESIGN.md §5. It is a convention rather than a
// setting on purpose: an agent can tell a human "export $TALARIA_AUTH_BEARER"
// having read nothing but the scheme's type.
const (
	// EnvBearer holds the token for an `http`/`bearer` scheme.
	EnvBearer = "TALARIA_AUTH_BEARER"
	// EnvBasic holds the user:password for an `http`/`basic` scheme.
	EnvBasic = "TALARIA_AUTH_BASIC"
	// EnvAPIKeyPrefix is followed by the upper-cased scheme name, so a scheme
	// called petKey reads TALARIA_AUTH_APIKEY_PETKEY.
	EnvAPIKeyPrefix = "TALARIA_AUTH_APIKEY_"
)

// Kind is the shape of a credential, which decides how internal/curl sends it.
type Kind string

const (
	// KindBearer is an RFC 6750 bearer token: Authorization: Bearer <value>.
	KindBearer Kind = "bearer"
	// KindBasic is HTTP basic auth, whose value is user:password.
	KindBasic Kind = "basic"
	// KindAPIKey is an opaque key sent verbatim in a header, query parameter or
	// cookie.
	KindAPIKey Kind = "apiKey"
)

// Credential locations, matching the OpenAPI `in` values.
const (
	InHeader = "header"
	InQuery  = "query"
	InCookie = "cookie"
)

// Credential is one security scheme resolved to a place on the request and the
// *name* of the value that goes there. It deliberately has no field that could
// hold the value: everything downstream can carry a Credential safely, and
// reading the secret is internal/curl calling Ref.Resolve at exec time.
type Credential struct {
	// Scheme is the key from components.securitySchemes.
	Scheme string `json:"scheme"`
	Kind   Kind   `json:"kind"`
	// In is where the credential goes: InHeader, InQuery or InCookie.
	In string `json:"in"`
	// Name is the header name, query parameter or cookie name. For bearer and
	// basic it is always Authorization.
	Name string `json:"name"`
	// Ref names the credential. It marshals redacted, like everywhere else.
	Ref secret.SecretRef `json:"source"`
}

// Present reports whether the credential this names is set, without reading it.
// `auth check` is built on this: the answer an agent needs is "is it there",
// and the value is never part of the answer (§5 Auth).
func (c Credential) Present() bool { return c.Ref.Present() }

// Resolve maps the security schemes op requires onto credentials.
//
// A spec may offer several alternative requirements; the first one talaria can
// satisfy wins, so a spec offering OAuth2 or a bearer token resolves to the
// bearer token rather than failing. Every scheme within the chosen requirement
// is returned, because a requirement's schemes apply together.
//
// An operation with no security, or with an empty requirement (auth optional),
// resolves to no credentials.
func Resolve(op operation.Operation, doc *spec.Document, prof *Profile) ([]Credential, error) {
	schemes := securitySchemes(doc)

	var unsupported []string
	for _, req := range op.Security {
		if len(req.Schemes) == 0 {
			return nil, nil
		}

		if reason := unsupportedReason(req, schemes); reason != "" {
			unsupported = append(unsupported, reason)
			continue
		}

		return credentials(req, schemes, prof)
	}

	if len(unsupported) == 0 {
		return nil, nil
	}

	return nil, clierr.Usage("no usable security scheme for %s: %s",
		operationName(op), strings.Join(unsupported, "; "))
}

// unsupportedReason returns why this requirement cannot be satisfied, or "" if
// it can. v1 covers bearer, basic and API keys; OAuth2 and OpenID Connect are
// out of scope, and the caller is expected to bring a token for them (§5 Auth).
func unsupportedReason(req operation.SecurityRequirement, schemes map[string]*v3high.SecurityScheme) string {
	for _, want := range req.Schemes {
		scheme, ok := schemes[want.Name]
		if !ok {
			return "scheme " + want.Name + " is not declared in components.securitySchemes"
		}

		switch {
		case isHTTP(scheme, "bearer"), isHTTP(scheme, "basic"):
		case strings.EqualFold(scheme.Type, "apiKey"):
			switch scheme.In {
			case InHeader, InQuery, InCookie:
			default:
				return "scheme " + want.Name + " puts its API key in " + scheme.In
			}
		default:
			return "scheme " + want.Name + " is of unsupported type " + describeType(scheme)
		}
	}

	return ""
}

// credentials builds one Credential per scheme of a requirement already known
// to be supported. The only failure left is a malformed profile entry, which is
// fatal rather than a reason to try another alternative: the user meant to
// configure this scheme and got it wrong.
func credentials(
	req operation.SecurityRequirement,
	schemes map[string]*v3high.SecurityScheme,
	prof *Profile,
) ([]Credential, error) {
	out := make([]Credential, 0, len(req.Schemes))
	for _, want := range req.Schemes {
		scheme := schemes[want.Name]

		cred := Credential{Scheme: want.Name, In: InHeader, Name: "Authorization"}
		switch {
		case isHTTP(scheme, "bearer"):
			cred.Kind, cred.Ref = KindBearer, secret.Env(EnvBearer)
		case isHTTP(scheme, "basic"):
			cred.Kind, cred.Ref = KindBasic, secret.Env(EnvBasic)
		default:
			cred.Kind = KindAPIKey
			cred.In, cred.Name = scheme.In, scheme.Name
			cred.Ref = secret.Env(EnvAPIKeyPrefix + envSuffix(want.Name))
		}

		// The profile is the more specific source and wins over the convention:
		// it is how one machine talks to staging and production at once.
		ref, err := profileRef(prof, want.Name)
		if err != nil {
			return nil, err
		}
		if !ref.IsZero() {
			cred.Ref = ref
		}

		out = append(out, cred)
	}

	return out, nil
}

// envRef matches a profile auth entry: ${VAR} or $VAR, and nothing else.
var envRef = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$`)

// profileRef reads the profile's entry for a scheme. A profile may only
// *reference* an environment variable; a literal value is refused, because a
// credential in a config file is a credential this process would have to carry,
// and the firewall's whole claim is that it never does.
//
// The error names neither the value nor any part of it.
func profileRef(prof *Profile, scheme string) (secret.SecretRef, error) {
	if prof == nil {
		return secret.SecretRef{}, nil
	}

	entry, ok := prof.Auth[scheme]
	if !ok || entry == "" {
		return secret.SecretRef{}, nil
	}

	match := envRef.FindStringSubmatch(entry)
	if match == nil {
		return secret.SecretRef{}, clierr.Usage(
			"profile %q sets auth for scheme %q to a literal value; "+
				"it must reference an environment variable, e.g. ${MY_TOKEN}",
			prof.Name, scheme)
	}

	return secret.Env(match[1]), nil
}

// securitySchemes flattens components.securitySchemes into a lookup. A document
// without any is not an error: an operation that names a scheme the document
// does not declare is reported per-requirement by unsupportedReason.
func securitySchemes(doc *spec.Document) map[string]*v3high.SecurityScheme {
	out := map[string]*v3high.SecurityScheme{}
	if doc == nil || doc.Model == nil || doc.Model.Components == nil {
		return out
	}

	for pair := doc.Model.Components.SecuritySchemes.First(); pair != nil; pair = pair.Next() {
		if scheme := pair.Value(); scheme != nil {
			out[pair.Key()] = scheme
		}
	}

	return out
}

func isHTTP(scheme *v3high.SecurityScheme, kind string) bool {
	return strings.EqualFold(scheme.Type, "http") && strings.EqualFold(scheme.Scheme, kind)
}

// describeType renders a scheme's type for an error message, including the
// http scheme when there is one, so "http (digest)" is distinguishable from
// the bearer and basic cases talaria does support.
func describeType(scheme *v3high.SecurityScheme) string {
	if scheme.Scheme != "" {
		return scheme.Type + " (" + scheme.Scheme + ")"
	}

	return scheme.Type
}

// envSuffix turns a scheme name into the tail of its env var: upper-cased, with
// everything outside [A-Z0-9] flattened to _ so a scheme called `pet-key` maps
// to a name a shell can actually export.
func envSuffix(scheme string) string {
	upper := strings.ToUpper(scheme)

	var b strings.Builder
	b.Grow(len(upper))
	for _, r := range upper {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	return b.String()
}

// operationName is the operation's ID, falling back to method and path for a
// spec that sets no operationId — an error naming neither is unactionable.
func operationName(op operation.Operation) string {
	if op.ID != "" {
		return op.ID
	}

	return op.Method + " " + op.Path
}
