package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/secret"
	"github.com/Teeeep/talaria/internal/spec"
)

// canary is the value the firewall must never let out. It is set as the
// contents of every credential env var these tests touch, so any assertion that
// it is absent is an assertion that no value escaped.
const canary = "s3cr3t-canary-value"

// fixtureOp loads the auth fixture and returns the operation with this ID along
// with the document it came from.
func fixtureOp(t *testing.T, id string) (operation.Operation, *spec.Document) {
	t.Helper()

	doc, err := spec.LoadFile(filepath.Join("testdata", "auth.yaml"))
	if err != nil {
		t.Fatalf("LoadFile(auth.yaml): %v", err)
	}

	for _, op := range operation.Extract(doc) {
		if op.ID == id {
			return op, doc
		}
	}

	t.Fatalf("fixture has no operation %q", id)
	return operation.Operation{}, nil
}

func resolveFixture(t *testing.T, id string, prof *Profile) []Credential {
	t.Helper()

	op, doc := fixtureOp(t, id)
	creds, err := Resolve(op, doc, prof)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", id, err)
	}

	return creds
}

func TestResolveMapsSchemesToConventionalEnvVars(t *testing.T) {
	tests := []struct {
		id   string
		want Credential
	}{
		{
			id: "getBearer",
			want: Credential{
				Scheme: "bearerAuth", Kind: KindBearer, In: InHeader, Name: "Authorization",
				Ref: secret.Env("TALARIA_AUTH_BEARER"), Supported: true,
			},
		},
		{
			id: "getBasic",
			want: Credential{
				Scheme: "basicAuth", Kind: KindBasic, In: InHeader, Name: "Authorization",
				Ref: secret.Env("TALARIA_AUTH_BASIC"), Supported: true,
			},
		},
		{
			id: "getHeaderKey",
			want: Credential{
				Scheme: "petKey", Kind: KindAPIKey, In: InHeader, Name: "X-Pet-Key",
				Ref: secret.Env("TALARIA_AUTH_APIKEY_PETKEY"), Supported: true,
			},
		},
		{
			id: "getQueryKey",
			want: Credential{
				Scheme: "queryKey", Kind: KindAPIKey, In: InQuery, Name: "api_key",
				Ref: secret.Env("TALARIA_AUTH_APIKEY_QUERYKEY"), Supported: true,
			},
		},
		{
			id: "getCookieKey",
			want: Credential{
				Scheme: "cookieKey", Kind: KindAPIKey, In: InCookie, Name: "session",
				Ref: secret.Env("TALARIA_AUTH_APIKEY_COOKIEKEY"), Supported: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			creds := resolveFixture(t, tt.id, nil)

			if len(creds) != 1 {
				t.Fatalf("Resolve(%s) returned %d credentials, want 1: %v", tt.id, len(creds), creds)
			}
			if !reflect.DeepEqual(creds[0], tt.want) {
				t.Errorf("Resolve(%s) = %+v, want %+v", tt.id, creds[0], tt.want)
			}
		})
	}
}

func TestResolveReturnsEverySchemeOfARequirement(t *testing.T) {
	creds := resolveFixture(t, "getBoth", nil)

	var got []string
	for _, c := range creds {
		got = append(got, c.Scheme)
	}

	want := []string{"bearerAuth", "petKey"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schemes = %v, want %v — both schemes of one requirement are needed together", got, want)
	}
}

func TestResolveSkipsRequirementsItCannotSatisfy(t *testing.T) {
	// The operation offers oauth2 first and bearer second. With no token to
	// bring, talaria cannot run the OAuth flow, so the second alternative is the
	// one whose variable an agent can be told to export.
	t.Setenv(EnvBearer, "")

	creds := resolveFixture(t, "getOAuthOrBearer", nil)

	if len(creds) != 1 || creds[0].Scheme != "bearerAuth" {
		t.Fatalf("Resolve(getOAuthOrBearer) = %+v, want the bearerAuth alternative", creds)
	}
}

// envKeyA and envKeyB are the conventional variables behind getEitherKey's two
// alternatives, which talaria can supply either of.
const (
	envKeyA = EnvAPIKeyPrefix + "KEYA"
	envKeyB = EnvAPIKeyPrefix + "KEYB"
)

// isolateKeys unsets every variable the agreement cases read, so each test
// states exactly which credentials exist and a developer's own environment
// cannot decide the answer for it.
func isolateKeys(t *testing.T) {
	t.Helper()
	t.Setenv(envKeyA, "")
	t.Setenv(envKeyB, "")
	t.Setenv(EnvBearer, "")
}

func TestResolvePrefersTheAlternativeWhoseCredentialIsSet(t *testing.T) {
	// The spec offers keyA or keyB and only keyB is exported. Choosing keyA
	// because it is written first refuses a call the spec allows, and refuses it
	// by asking a human to export a variable the spec did not require.
	isolateKeys(t)
	t.Setenv(envKeyB, canary)

	creds := resolveFixture(t, "getEitherKey", nil)

	if len(creds) != 1 || creds[0].Scheme != "keyB" {
		t.Fatalf("Resolve(getEitherKey) = %+v, want the keyB alternative: it is the one with a credential", creds)
	}
	if !creds[0].Present() {
		t.Error("Resolve chose an alternative whose credential is not set")
	}
}

func TestResolveFallsBackToTheFirstSupportedAlternativeWhenNoneIsSet(t *testing.T) {
	// Nothing is exported, so there is nothing to prefer. The answer still has
	// to name a variable, or exit 5 leaves an agent with nothing to act on.
	isolateKeys(t)

	creds := resolveFixture(t, "getEitherKey", nil)

	if len(creds) != 1 || creds[0].Scheme != "keyA" {
		t.Fatalf("Resolve(getEitherKey) = %+v, want the first alternative, keyA", creds)
	}
	if got, want := creds[0].Ref.Symbolic(), "$"+envKeyA; got != want {
		t.Errorf("ref = %q, want %q — the missing-credential error is built from this", got, want)
	}
}

func TestResolveAgreesWithTheCoverageAuthCheckReports(t *testing.T) {
	// `auth check` reports on Covers and `call` acts on Resolve. Every
	// arrangement of the two alternatives has to give both the same verdict, or
	// the pre-flight blesses a call that then fails, or refuses one that works.
	tests := []struct {
		name string
		op   string
		set  []string
		want bool
	}{
		{name: "neither", op: "getEitherKey"},
		{name: "first", op: "getEitherKey", set: []string{envKeyA}, want: true},
		{name: "second", op: "getEitherKey", set: []string{envKeyB}, want: true},
		{name: "both", op: "getEitherKey", set: []string{envKeyA, envKeyB}, want: true},
		// The arrangement they used to disagree on outright: `auth check` reported
		// no schemes at all and exited 0, and `call` refused the same spec.
		{name: "unsupported alone", op: "getOAuthOnly"},
		{name: "unsupported with a token", op: "getOAuthOnly", set: []string{EnvBearer}, want: true},
		{name: "unsupported beside a supported one", op: "getOAuthOrBearer"},
		{name: "either satisfied by one token", op: "getOAuthOrBearer", set: []string{EnvBearer}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateKeys(t)
			for _, name := range tt.set {
				t.Setenv(name, canary)
			}

			op, doc := fixtureOp(t, tt.op)

			declared, err := Schemes(doc, nil)
			if err != nil {
				t.Fatalf("Schemes: %v", err)
			}
			byName := make(map[string]Credential, len(declared))
			for _, cred := range declared {
				byName[cred.Scheme] = cred
			}

			// `auth check`'s verdict: some alternative is covered outright.
			checked := false
			for _, req := range op.Security {
				if c := Covers(req, byName); c == Satisfied || c == Optional {
					checked = true
				}
			}

			// `call`'s verdict: Resolve succeeded and every credential it chose is
			// set. A refusal is as much a verdict as a credential is.
			creds, err := Resolve(op, doc, nil)
			resolved := err == nil
			for _, cred := range creds {
				if !cred.Present() {
					resolved = false
				}
			}

			if checked != resolved {
				t.Fatalf("auth check says satisfied=%v, but Resolve gave %+v, %v (satisfied=%v)",
					checked, creds, err, resolved)
			}
			if checked != tt.want {
				t.Fatalf("satisfied = %v with %v exported, want %v", checked, tt.set, tt.want)
			}
		})
	}
}

// A scheme talaria cannot run the flow for is unsupported, not invisible
// (DESIGN.md §5). With no token to substitute the operation is unsatisfiable,
// and the error has to say which variable would make it satisfiable — exit 2
// sends an agent looking for a mistake in its own invocation.
func TestResolveReportsAnUnsupportedSchemeAsAMissingCredential(t *testing.T) {
	t.Setenv(EnvBearer, "")

	op, doc := fixtureOp(t, "getOAuthOnly")

	_, err := Resolve(op, doc, nil)
	if err == nil {
		t.Fatal("Resolve(getOAuthOnly) succeeded; oauth2 has no credential and must be reported")
	}

	var cerr *clierr.Error
	if !errors.As(err, &cerr) || cerr.Code != clierr.CodeCredentialMissing {
		t.Fatalf("Resolve(getOAuthOnly) error = %v (code %v), want a credential-missing error (5)",
			err, clierr.From(err).Code)
	}
	for _, want := range []string{"oauth2", EnvBearer} {
		if !strings.Contains(cerr.Error(), want) {
			t.Errorf("error = %q, want it to name %q", cerr, want)
		}
	}
}

// The bring-your-own-token clause: the user obtained the token however the flow
// demands, and talaria carries it.
func TestResolveSatisfiesAnUnsupportedSchemeWithABearerToken(t *testing.T) {
	t.Setenv(EnvBearer, canary)

	creds := resolveFixture(t, "getOAuthOnly", nil)

	want := Credential{
		Scheme: "oauth2", Kind: KindBearer, In: InHeader, Name: "Authorization",
		Ref: secret.Env(EnvBearer), Supported: false,
	}
	if len(creds) != 1 || !reflect.DeepEqual(creds[0], want) {
		t.Fatalf("Resolve(getOAuthOnly) = %+v, want %+v", creds, want)
	}
	if !creds[0].Present() {
		t.Error("the token is exported but the credential reports absent")
	}
}

// Every kind of unsupported scheme behaves the same way, whether the type is
// out of scope or an apiKey names a place a request does not have.
func TestUnsupportedSchemeKindsAllBehaveAlike(t *testing.T) {
	for _, tc := range []struct{ id, scheme string }{
		{"getOAuthOnly", "oauth2"},
		{"getOIDCOnly", "openIdConnect"},
		{"getMutualTLSOnly", "mutualTLS"},
		{"getBodyKeyOnly", "bodyKey"},
	} {
		t.Run(tc.scheme, func(t *testing.T) {
			op, doc := fixtureOp(t, tc.id)

			t.Setenv(EnvBearer, "")
			if _, err := Resolve(op, doc, nil); clierr.From(err).Code != clierr.CodeCredentialMissing {
				t.Fatalf("Resolve(%s) with no token = %v, want a credential-missing error (5)", tc.id, err)
			}

			t.Setenv(EnvBearer, canary)
			creds, err := Resolve(op, doc, nil)
			if err != nil {
				t.Fatalf("Resolve(%s) with a token: %v", tc.id, err)
			}
			if len(creds) != 1 || creds[0].Scheme != tc.scheme || creds[0].Supported || !creds[0].Present() {
				t.Fatalf("Resolve(%s) = %+v, want the %s scheme carried by the token", tc.id, creds, tc.scheme)
			}
		})
	}
}

// Schemes answers "which credentials does this spec ask for at all", which is
// the question `auth check` reports on. A scheme it leaves out is one a caller
// cannot discover exists.
func TestSchemesReportsUnsupportedSchemesRatherThanHidingThem(t *testing.T) {
	_, doc := fixtureOp(t, "getBearer")

	declared, err := Schemes(doc, nil)
	if err != nil {
		t.Fatalf("Schemes: %v", err)
	}

	byName := map[string]Credential{}
	for _, cred := range declared {
		byName[cred.Scheme] = cred
	}

	for _, name := range []string{"oauth2", "openIdConnect", "mutualTLS", "bodyKey"} {
		cred, ok := byName[name]
		switch {
		case !ok:
			t.Errorf("Schemes left out %s; an unsupported scheme is reported, not hidden", name)
		case cred.Supported:
			t.Errorf("%s reported as supported", name)
		case cred.Ref != secret.Env(EnvBearer):
			t.Errorf("%s source = %v, want %v: it is the variable a caller can act on",
				name, cred.Ref, secret.Env(EnvBearer))
		}
	}

	// The guard that the supported path did not move with it.
	if cred := byName["bearerAuth"]; !cred.Supported || cred.Ref != secret.Env(EnvBearer) {
		t.Errorf("bearerAuth = %+v, want supported with its conventional variable", cred)
	}
}

// A requirement naming a scheme the document never declares is a broken spec,
// not a missing credential: no variable would fix it, so exit 5 would send an
// agent to export something that cannot help.
func TestResolveReportsARequirementNamingAnUndeclaredScheme(t *testing.T) {
	t.Setenv(EnvBearer, canary)

	op, doc := fixtureOp(t, "getUndeclared")

	_, err := Resolve(op, doc, nil)
	if err == nil {
		t.Fatal("Resolve(getUndeclared) succeeded; ghostKey is not declared anywhere")
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("error code = %d, want %d for a spec that names an undeclared scheme", code, clierr.CodeUsage)
	}
	if !strings.Contains(err.Error(), "ghostKey") {
		t.Errorf("error = %q, want it to name the undeclared scheme", err)
	}
}

// envSuffix flattens every rune outside [A-Z0-9] to _, so `key-a` and `key.a`
// both read TALARIA_AUTH_APIKEY_KEY_A. Silently letting one export answer for
// two different schemes sends a credential somewhere its owner never named.
func TestCollidingSchemeNamesAreReportedRatherThanSharingOneVariable(t *testing.T) {
	doc, err := spec.LoadFile(filepath.Join("testdata", "collide.yaml"))
	if err != nil {
		t.Fatalf("LoadFile(collide.yaml): %v", err)
	}

	var op operation.Operation
	for _, candidate := range operation.Extract(doc) {
		if candidate.ID == "getEither" {
			op = candidate
		}
	}

	for name, err := range map[string]error{
		"Schemes": func() error { _, err := Schemes(doc, nil); return err }(),
		"Resolve": func() error { _, err := Resolve(op, doc, nil); return err }(),
	} {
		if err == nil {
			t.Fatalf("%s accepted two schemes sharing one environment variable", name)
		}
		if code := clierr.From(err).Code; code != clierr.CodeUsage {
			t.Errorf("%s error code = %d, want %d", name, code, clierr.CodeUsage)
		}
		for _, want := range []string{"key-a", "key.a", EnvAPIKeyPrefix + "KEY_A"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s error = %q, want it to name %q", name, err, want)
			}
		}
	}
}

func TestResolveReturnsNothingForUnauthenticatedOperations(t *testing.T) {
	if creds := resolveFixture(t, "getOpen", nil); len(creds) != 0 {
		t.Fatalf("Resolve(getOpen) = %+v, want no credentials for `security: []`", creds)
	}
}

func TestResolveNeverCarriesACredentialValue(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	creds := resolveFixture(t, "getBearer", nil)

	body, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("Marshal(creds): %v", err)
	}

	// %s used to be here too. Credential now holds a bool, so vet and
	// staticcheck both reject %s on it: no code that lints can reach that
	// rendering, and %v covers the same fields through the same Ref.
	for name, rendering := range map[string]string{
		"json": string(body),
		"%v":   fmt.Sprintf("%v", creds),
		"%+v":  fmt.Sprintf("%+v", creds),
		"%#v":  fmt.Sprintf("%#v", creds),
	} {
		if strings.Contains(rendering, canary) {
			t.Errorf("%s rendering of a credential leaked the value: %s", name, rendering)
		}
	}

	// The env var name is what the agent needs, and it is safe to print.
	if !strings.Contains(string(body), "TALARIA_AUTH_BEARER") {
		t.Errorf("marshalled credential does not name the env var to set: %s", body)
	}
}

func TestCredentialPresenceIsReportedWithoutTheValue(t *testing.T) {
	creds := resolveFixture(t, "getBearer", nil)
	cred := creds[0]

	t.Setenv("TALARIA_AUTH_BEARER", "")
	if cred.Present() {
		t.Error("Present() is true for an empty env var")
	}

	t.Setenv("TALARIA_AUTH_BEARER", canary)
	if !cred.Present() {
		t.Error("Present() is false for a set env var")
	}
}

func TestProfileAuthOverridesTheConventionalEnvVar(t *testing.T) {
	prof := &Profile{
		Name: "staging",
		Auth: map[string]string{"bearerAuth": "${STAGING_TOKEN}"},
	}

	creds := resolveFixture(t, "getBearer", prof)

	if len(creds) != 1 {
		t.Fatalf("Resolve returned %d credentials, want 1", len(creds))
	}
	if got, want := creds[0].Ref, secret.Env("STAGING_TOKEN"); got != want {
		t.Fatalf("ref = %v, want %v — a profile ${VAR} entry names that variable", got, want)
	}
}

func TestProfileAuthKeepsSchemesItDoesNotMention(t *testing.T) {
	prof := &Profile{Name: "staging", Auth: map[string]string{"petKey": "${PET_KEY}"}}

	creds := resolveFixture(t, "getBoth", prof)

	if len(creds) != 2 {
		t.Fatalf("Resolve returned %d credentials, want 2", len(creds))
	}
	if got, want := creds[0].Ref, secret.Env("TALARIA_AUTH_BEARER"); got != want {
		t.Errorf("bearerAuth ref = %v, want the conventional %v", got, want)
	}
	if got, want := creds[1].Ref, secret.Env("PET_KEY"); got != want {
		t.Errorf("petKey ref = %v, want %v", got, want)
	}
}

func TestProfileAuthRefusesAnInlinedSecret(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", "")

	op, doc := fixtureOp(t, "getBearer")
	prof := &Profile{Name: "staging", Auth: map[string]string{"bearerAuth": canary}}

	_, err := Resolve(op, doc, prof)
	if err == nil {
		t.Fatal("Resolve accepted a literal credential in a profile; only ${VAR} references are allowed")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("the error for an inlined secret echoed it back: %v", err)
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("error code = %d, want %d", code, clierr.CodeUsage)
	}
}
