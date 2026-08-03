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
				Ref: secret.Env("TALARIA_AUTH_BEARER"),
			},
		},
		{
			id: "getBasic",
			want: Credential{
				Scheme: "basicAuth", Kind: KindBasic, In: InHeader, Name: "Authorization",
				Ref: secret.Env("TALARIA_AUTH_BASIC"),
			},
		},
		{
			id: "getHeaderKey",
			want: Credential{
				Scheme: "petKey", Kind: KindAPIKey, In: InHeader, Name: "X-Pet-Key",
				Ref: secret.Env("TALARIA_AUTH_APIKEY_PETKEY"),
			},
		},
		{
			id: "getQueryKey",
			want: Credential{
				Scheme: "queryKey", Kind: KindAPIKey, In: InQuery, Name: "api_key",
				Ref: secret.Env("TALARIA_AUTH_APIKEY_QUERYKEY"),
			},
		},
		{
			id: "getCookieKey",
			want: Credential{
				Scheme: "cookieKey", Kind: KindAPIKey, In: InCookie, Name: "session",
				Ref: secret.Env("TALARIA_AUTH_APIKEY_COOKIEKEY"),
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
	// The operation offers oauth2 first and bearer second. OAuth flows are out
	// of v1 scope, so the second alternative is the one to use.
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

// isolateKeys unsets both alternatives' variables, so each test states exactly
// which credentials exist and a developer's own environment cannot decide the
// answer for it.
func isolateKeys(t *testing.T) {
	t.Helper()
	t.Setenv(envKeyA, "")
	t.Setenv(envKeyB, "")
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
		set  []string
	}{
		{name: "neither"},
		{name: "first", set: []string{envKeyA}},
		{name: "second", set: []string{envKeyB}},
		{name: "both", set: []string{envKeyA, envKeyB}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateKeys(t)
			for _, name := range tt.set {
				t.Setenv(name, canary)
			}

			op, doc := fixtureOp(t, "getEitherKey")

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

			// `call`'s verdict: every credential Resolve chose is set.
			creds := resolveFixture(t, "getEitherKey", nil)
			resolved := true
			for _, cred := range creds {
				if !cred.Present() {
					resolved = false
				}
			}

			if checked != resolved {
				t.Fatalf("auth check says satisfied=%v, but Resolve chose %+v (satisfied=%v)",
					checked, creds, resolved)
			}
			if want := len(tt.set) > 0; checked != want {
				t.Fatalf("satisfied = %v with %v exported, want %v", checked, tt.set, want)
			}
		})
	}
}

func TestResolveReportsWhenNoRequirementIsSupported(t *testing.T) {
	op, doc := fixtureOp(t, "getOAuthOnly")

	_, err := Resolve(op, doc, nil)
	if err == nil {
		t.Fatal("Resolve(getOAuthOnly) succeeded; oauth2 is out of v1 scope and must be reported")
	}

	var cerr *clierr.Error
	if !errors.As(err, &cerr) || cerr.Code != clierr.CodeUsage {
		t.Fatalf("Resolve(getOAuthOnly) error = %v (code %v), want a usage error (2)", err, clierr.From(err).Code)
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

	for name, rendering := range map[string]string{
		"json": string(body),
		"%v":   fmt.Sprintf("%v", creds),
		"%+v":  fmt.Sprintf("%+v", creds),
		"%#v":  fmt.Sprintf("%#v", creds),
		"%s":   fmt.Sprintf("%s", creds),
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

// ReferencesEnv is the profile's half of `history replay`'s allowlist: the
// store names a variable, and this is what says whether the profile in force
// asked for it. A profile that names no variables must admit none.
func TestReferencesEnvAnswersForTheProfilesAuthMap(t *testing.T) {
	prof := &Profile{
		Name: "staging",
		Auth: map[string]string{
			"bearerAuth": "${STAGING_TOKEN}",
			"petKey":     "$PET_KEY",
			"broken":     canary,
		},
	}

	cases := []struct {
		name string
		want bool
	}{
		{"STAGING_TOKEN", true},
		{"PET_KEY", true},
		{"AWS_SECRET_ACCESS_KEY", false},
		// A literal entry is refused at Resolve time; it names no variable, so it
		// must not admit itself as one either.
		{canary, false},
		{"", false},
	}

	for _, tc := range cases {
		if got := prof.ReferencesEnv(tc.name); got != tc.want {
			t.Errorf("ReferencesEnv(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// No --profile is the common case, and it must not be the permissive one.
func TestReferencesEnvOfNoProfileIsFalse(t *testing.T) {
	var prof *Profile

	if prof.ReferencesEnv("STAGING_TOKEN") {
		t.Error("a nil profile admitted STAGING_TOKEN; with no profile, nothing is named")
	}
}
