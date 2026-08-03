package config

import (
	"regexp"
	"sort"
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
	// Supported reports whether talaria can speak this scheme itself. It is
	// false for a scheme outside v1's set — oauth2, openIdConnect, mutualTLS, an
	// apiKey in a place a request does not have — which Ref then points at
	// EnvBearer for: the only thing that can satisfy it is a token the caller
	// brought (§5 Auth). Such a scheme is reported, never hidden, because an
	// agent that cannot see it has no way to learn why every call returns 401.
	Supported bool `json:"supported"`
}

// Present reports whether the credential this names is set, without reading it.
// `auth check` is built on this: the answer an agent needs is "is it there",
// and the value is never part of the answer (§5 Auth).
func (c Credential) Present() bool { return c.Ref.Present() }

// Coverage is how one security requirement stands against the credentials that
// actually exist.
type Coverage int

const (
	// Unsupported means the requirement cannot be met as things stand: it names
	// a scheme the document never declared, or one talaria cannot speak and no
	// brought token covers.
	Unsupported Coverage = iota
	// Incomplete means talaria can supply every scheme the requirement names,
	// but at least one credential is not set.
	Incomplete
	// Satisfied means every scheme is supported and every credential is set.
	Satisfied
	// Optional means the requirement is empty: the spec is saying that
	// authentication is optional here.
	Optional
)

// Covers classifies one security requirement against byName, which maps a
// scheme name to the credential that would satisfy it — Schemes' result for a
// document, or the map Resolve builds for an operation. Both sources hold every
// scheme the document declares, supported or not, so a name missing from byName
// is one the document never declared.
//
// This is the one rule behind both Resolve's choice of alternative and `auth
// check`'s verdict. The two used to derive it separately, in separate packages,
// and drifted: `auth check` exited 0 on a spec offering either of two API keys
// with one of them exported, and `call` then refused the same spec with exit 5
// asking for the other one.
func Covers(req operation.SecurityRequirement, byName map[string]Credential) Coverage {
	if len(req.Schemes) == 0 {
		return Optional
	}

	// A requirement's schemes apply together, so one scheme that cannot be met
	// makes the whole alternative unusable, and one credential that is not set
	// makes it incomplete.
	coverage := Satisfied
	for _, want := range req.Schemes {
		cred, ok := byName[want.Name]
		switch {
		case !ok:
			return Unsupported
		case !cred.Supported && !cred.Present():
			// Nothing talaria could put on the wire: the scheme is one it does
			// not speak and no token was brought for it. Reporting this as a
			// merely incomplete alternative would let Resolve pick it and print
			// a request that cannot be authenticated.
			return Unsupported
		case !cred.Present():
			coverage = Incomplete
		}
	}

	return coverage
}

// Resolve maps the security schemes op requires onto credentials.
//
// A spec may offer several alternative requirements. The first one talaria can
// supply *and* has every credential for wins, so a spec offering either of two
// API keys resolves to whichever one the caller has exported. If no alternative
// is fully covered, the first supported one is returned anyway: the caller is
// heading for exit 5, and the error is only actionable if it names a variable.
// Every scheme within the chosen requirement is returned, because a
// requirement's schemes apply together.
//
// An alternative naming a scheme talaria cannot speak is usable when the caller
// brought a token for it, and it is then preferred over a later one exactly as
// any other satisfied alternative is: the token goes out as the bearer
// credential the scheme's own flow would have produced.
//
// An operation with no security, or with an empty requirement (auth optional),
// resolves to no credentials.
func Resolve(op operation.Operation, doc *spec.Document, prof *Profile) ([]Credential, error) {
	schemes := securitySchemes(doc)

	byName, err := declaredCredentials(op, schemes, prof)
	if err != nil {
		return nil, err
	}

	var (
		undeclared []string
		unmet      []string
		fallback   []Credential
	)
	for _, req := range op.Security {
		switch Covers(req, byName) {
		case Optional:
			return nil, nil
		case Unsupported:
			reason, missingScheme := blockedReason(req, schemes, byName)
			if missingScheme {
				undeclared = append(undeclared, reason)
			} else {
				unmet = append(unmet, reason)
			}
		case Satisfied:
			return credentials(req, byName), nil
		case Incomplete:
			if fallback == nil {
				fallback = credentials(req, byName)
			}
		}
	}

	switch {
	case fallback != nil:
		return fallback, nil
	case len(unmet) > 0:
		// Exit 5, not 2: the invocation is right and a variable is what stands
		// in the way, which is the one failure a human fixes rather than the
		// agent (§5 Auth).
		return nil, clierr.CredentialMissing("no credential for %s: %s",
			operationName(op), strings.Join(unmet, "; "))
	case len(undeclared) > 0:
		// No token can fix a spec that names a scheme it never declared.
		return nil, clierr.Usage("no usable security scheme for %s: %s",
			operationName(op), strings.Join(undeclared, "; "))
	}

	return nil, nil
}

// Schemes lists every security scheme the document declares, as the credential
// that would satisfy it, sorted by scheme name.
//
// Resolve answers "which credentials does this call use". This answers "which
// credentials does this spec ask for at all", which is the question `auth
// check` reports on. A scheme talaria cannot speak — OAuth2, OpenID Connect, an
// API key in a place a request does not have — is in the list with
// Supported false rather than left out of it: leaving it out reports "nothing
// required" for a spec no call can authenticate (§5 Auth).
func Schemes(doc *spec.Document, prof *Profile) ([]Credential, error) {
	declared := securitySchemes(doc)

	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	// Sorted, because the map iteration behind it is not, and a report whose
	// line order changes between runs is one no diff can be taken of.
	sort.Strings(names)

	out := make([]Credential, 0, len(names))
	for _, name := range names {
		cred, err := credentialFor(name, declared[name], prof)
		if err != nil {
			return nil, err
		}
		out = append(out, cred)
	}

	return out, nil
}

// blockedReason returns why a requirement Covers rejected cannot be met, and
// whether the spec itself is at fault — a scheme it never declared — rather
// than the environment. The two answers carry different exit codes, so the
// caller has to be able to tell them apart without reading the prose.
//
// A requirement's schemes apply together, so the first unmet one is the reason.
func blockedReason(
	req operation.SecurityRequirement,
	schemes map[string]*v3high.SecurityScheme,
	byName map[string]Credential,
) (reason string, undeclared bool) {
	for _, want := range req.Schemes {
		cred, ok := byName[want.Name]
		if !ok {
			return "scheme " + want.Name + " is not declared in components.securitySchemes", true
		}

		if !cred.Supported && !cred.Present() {
			return schemeReason(want.Name, schemes[want.Name]) +
				"; set " + cred.Ref.Symbolic() + " to bring your own token for it", false
		}
	}

	return "", false
}

// schemeReason returns why talaria cannot use one declared scheme, or "" if it
// can. v1 covers bearer, basic and API keys; OAuth2 and OpenID Connect are out
// of scope, and the caller is expected to bring a token for them (§5 Auth).
func schemeReason(name string, scheme *v3high.SecurityScheme) string {
	switch {
	case isHTTP(scheme, "bearer"), isHTTP(scheme, "basic"):
		return ""
	case strings.EqualFold(scheme.Type, "apiKey"):
		switch scheme.In {
		case InHeader, InQuery, InCookie:
			return ""
		default:
			return "scheme " + name + " puts its API key in " + scheme.In
		}
	default:
		return "scheme " + name + " is of unsupported type " + describeType(scheme)
	}
}

// declaredCredentials builds the credential for every scheme op names that the
// document declares, keyed by scheme name. Only a name the document never
// declared is left out, which is what Covers reads as an alternative no
// credential can meet.
//
// It is built for the whole operation rather than per alternative because the
// choice between alternatives depends on which credentials are present, which
// cannot be known before they are built. The only failure is a malformed
// profile entry, which is fatal rather than a reason to try another
// alternative: the user meant to configure this scheme and got it wrong.
func declaredCredentials(
	op operation.Operation,
	schemes map[string]*v3high.SecurityScheme,
	prof *Profile,
) (map[string]Credential, error) {
	out := map[string]Credential{}
	for _, req := range op.Security {
		for _, want := range req.Schemes {
			scheme, ok := schemes[want.Name]
			if _, done := out[want.Name]; done || !ok {
				continue
			}

			cred, err := credentialFor(want.Name, scheme, prof)
			if err != nil {
				return nil, err
			}

			out[want.Name] = cred
		}
	}

	return out, nil
}

// credentials collects the credentials of a requirement already known to be
// supported, in the order the requirement names its schemes.
func credentials(req operation.SecurityRequirement, byName map[string]Credential) []Credential {
	out := make([]Credential, 0, len(req.Schemes))
	for _, want := range req.Schemes {
		out = append(out, byName[want.Name])
	}

	return out
}

// credentialFor builds the Credential for one declared scheme: where the value
// goes on the request, and the name of the value that goes there.
//
// A scheme talaria cannot speak gets the bring-your-own-token credential —
// $TALARIA_AUTH_BEARER in the Authorization header — and Supported false. It
// may not fall through to the apiKey branch: that branch reads scheme.In and
// scheme.Name, which an oauth2 scheme does not have, and would mint a
// credential bound for a place the spec never named.
func credentialFor(name string, scheme *v3high.SecurityScheme, prof *Profile) (Credential, error) {
	cred := Credential{Scheme: name, In: InHeader, Name: "Authorization", Supported: true}
	switch {
	case isHTTP(scheme, "bearer"):
		cred.Kind, cred.Ref = KindBearer, secret.Env(EnvBearer)
	case isHTTP(scheme, "basic"):
		cred.Kind, cred.Ref = KindBasic, secret.Env(EnvBasic)
	case schemeReason(name, scheme) == "":
		cred.Kind = KindAPIKey
		cred.In, cred.Name = scheme.In, scheme.Name
		cred.Ref = secret.Env(EnvAPIKeyPrefix + envSuffix(name))
	default:
		cred.Supported = false
		cred.Kind, cred.Ref = KindBearer, secret.Env(EnvBearer)
	}

	// The profile is the more specific source and wins over the convention: it
	// is how one machine talks to staging and production at once.
	ref, err := profileRef(prof, name)
	if err != nil {
		return Credential{}, err
	}
	if !ref.IsZero() {
		cred.Ref = ref
	}

	return cred, nil
}

// envRef matches a profile auth entry: ${VAR} or $VAR, and nothing else.
var envRef = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$`)

// ReferencesEnv reports whether this profile's auth map names the given
// environment variable.
//
// It exists for `history replay`, which reads a variable's *name* out of a file
// on disk and would otherwise resolve whatever that file asked for. The set of
// names a replay may resolve is the TALARIA_AUTH_* convention plus this: the
// variables the profile in force deliberately points at. A literal entry — the
// one profileRef refuses — names no variable and so admits none.
//
// The nil receiver is "no --profile was given", which names nothing rather than
// everything.
func (p *Profile) ReferencesEnv(name string) bool {
	if p == nil || name == "" {
		return false
	}

	for _, entry := range p.Auth {
		if match := envRef.FindStringSubmatch(entry); match != nil && match[1] == name {
			return true
		}
	}

	return false
}

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
