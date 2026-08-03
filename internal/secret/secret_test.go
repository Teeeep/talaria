package secret

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
)

// canary is a value that must never appear in anything this package produces.
// Every assertion below greps for it rather than only comparing against the
// expected redacted string: an exact-match assertion passes just as happily
// when the value is leaked *alongside* the redaction.
const canary = "sk-live-CANARY-9f3a17c4"

func TestMarshalJSONRendersTheRefAndNeverTheValue(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	ref := Env("TALARIA_AUTH_BEARER")

	body, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// encoding/json escapes < and > whatever MarshalJSON returns, so the
	// decoded string is the surface a consumer actually sees. The raw bytes are
	// still greppable for the value, which is the assertion that matters.
	var got string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if want := "<redacted:env:TALARIA_AUTH_BEARER>"; got != want {
		t.Errorf("Marshal decoded to %q, want %q", got, want)
	}
	assertNoCanary(t, string(body))
}

// A SecretRef almost always reaches JSON as a field of something larger — a
// request's headers, a history entry. Marshalling on the value receiver is what
// makes that case safe whether the field holds a SecretRef or a *SecretRef.
func TestMarshalJSONIsSafeAsAStructField(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	ref := Env("TALARIA_AUTH_BEARER")
	payload := struct {
		Value    SecretRef  `json:"value"`
		Pointer  *SecretRef `json:"pointer"`
		Unsealed string     `json:"unsealed"`
	}{Value: ref, Pointer: &ref, Unsealed: "not a secret"}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got struct {
		Value    string `json:"value"`
		Pointer  string `json:"pointer"`
		Unsealed string `json:"unsealed"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	const redacted = "<redacted:env:TALARIA_AUTH_BEARER>"
	if got.Value != redacted {
		t.Errorf("value = %q, want %q", got.Value, redacted)
	}
	if got.Pointer != redacted {
		t.Errorf("pointer = %q, want %q", got.Pointer, redacted)
	}
	if got.Unsealed != "not a secret" {
		t.Errorf("unsealed = %q, want the field to pass through untouched", got.Unsealed)
	}
	assertNoCanary(t, string(body))
}

// The point of the type is that printing it is safe by construction: a stray
// log line or an error built with %v is the leak channel DESIGN.md §5a calls
// out as "where redaction bugs live", so every verb is asserted, including the
// ones a careless caller reaches for.
func TestEveryPrintfVerbRendersTheRedactedForm(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	ref := Env("TALARIA_AUTH_BEARER")
	const redacted = "<redacted:env:TALARIA_AUTH_BEARER>"

	if got := ref.String(); got != redacted {
		t.Errorf("String() = %q, want %q", got, redacted)
	}

	for _, verb := range []string{"%v", "%s", "%q", "%#v", "%+v"} {
		got := fmt.Sprintf(verb, ref)
		if !strings.Contains(got, redacted) {
			t.Errorf("Sprintf(%q) = %s, want it to contain %q", verb, got, redacted)
		}
		assertNoCanary(t, got)
	}

	// fmt consults a field's Stringer when printing the struct that holds it,
	// so a request logged whole is safe too.
	nested := struct{ Auth SecretRef }{Auth: ref}
	if got := fmt.Sprintf("%v", nested); !strings.Contains(got, redacted) {
		t.Errorf("Sprintf(%%v, nested) = %s, want it to contain %q", got, redacted)
	}
}

func TestSymbolicIsShellInterpolationForEnvRefs(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	got := Env("TALARIA_AUTH_BEARER").Symbolic()
	if want := "$TALARIA_AUTH_BEARER"; got != want {
		t.Errorf("Symbolic() = %q, want %q", got, want)
	}
	assertNoCanary(t, got)
}

// A ref that does not name an env var has nothing to interpolate, so Symbolic
// falls back to the redacted form: the emitted curl stops being runnable, which
// is the right trade against emitting a value.
func TestSymbolicFallsBackToRedactedForNonEnvRefs(t *testing.T) {
	ref := SecretRef{Source: "profile", Name: "prod.token"}

	if got, want := ref.Symbolic(), "<redacted:profile:prod.token>"; got != want {
		t.Errorf("Symbolic() = %q, want %q", got, want)
	}
}

func TestResolveReadsTheEnvVar(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	got, err := Env("TALARIA_AUTH_BEARER").Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != canary {
		t.Errorf("Resolve() = %q, want the env var's value", got)
	}
}

// Exit code 5 exists so an agent can tell a human "set $NAME" rather than
// guessing (DESIGN.md §4), which only works if the resolver classifies the
// failure itself.
func TestResolveMissingEnvVarIsCredentialMissing(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", "")

	_, err := Env("TALARIA_AUTH_BEARER").Resolve()
	if err == nil {
		t.Fatal("Resolve() succeeded on an unset env var, want an error")
	}
	if got := clierr.From(err).Code; got != clierr.CodeCredentialMissing {
		t.Errorf("Resolve() error code = %d, want %d", got, clierr.CodeCredentialMissing)
	}
	if !strings.Contains(err.Error(), "TALARIA_AUTH_BEARER") {
		t.Errorf("Resolve() error = %q, want it to name the variable to set", err)
	}
}

func TestIsSensitiveMatchesTheBuiltInList(t *testing.T) {
	// The built-in list from DESIGN.md §5a. Matching is on the header name, so
	// casing and the surrounding vendor prefix must not matter.
	tests := []struct {
		name string
		want bool
	}{
		{"Authorization", true},
		{"authorization", true},
		{"AUTHORIZATION", true},
		{"Cookie", true},
		{"Set-Cookie", true},
		{"Proxy-Authorization", true},
		{"X-API-Key", true},
		{"x-api-key", true},
		{"apikey", true},
		{"X-Auth-Token", true},
		{"X-Client-Secret", true},

		{"Content-Type", false},
		{"Accept", false},
		{"User-Agent", false},
		{"X-Request-Id", false},
		{"If-None-Match", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewRedactor().IsSensitive(tt.name); got != tt.want {
				t.Errorf("IsSensitive(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// The zero *Redactor is what a struct field defaults to, and defaulting to "no
// redaction" would make the firewall opt-in. It defaults to the built-in list.
func TestNilRedactorUsesTheBuiltInList(t *testing.T) {
	var r *Redactor

	if !r.IsSensitive("Authorization") {
		t.Error("nil Redactor did not match Authorization, want the built-in list")
	}
	if got := r.Value("Authorization", canary); got != Placeholder {
		t.Errorf("Value() = %q, want %q", got, Placeholder)
	}
}

func TestExtraPatternsExtendTheListAndNeverShrinkIt(t *testing.T) {
	r := NewRedactor("X-Acme-Session", "*credential*")

	for _, name := range []string{"X-Acme-Session", "x-acme-session", "X-Vault-Credential-Id"} {
		if !r.IsSensitive(name) {
			t.Errorf("IsSensitive(%q) = false, want the extra patterns to match", name)
		}
	}

	// Same instance, built-ins intact.
	for _, name := range []string{"Authorization", "Cookie", "X-API-Key"} {
		if !r.IsSensitive(name) {
			t.Errorf("IsSensitive(%q) = false, want extras to extend the built-ins, not replace them", name)
		}
	}

	// An extra that would match everything if it were treated as a substring
	// still cannot un-redact anything: extras only ever add.
	if r.IsSensitive("Content-Type") {
		t.Error("IsSensitive(\"Content-Type\") = true, want extras not to widen matching to every header")
	}
}

func TestHeadersRedactsSensitiveValuesAndLeavesTheRestAlone(t *testing.T) {
	in := map[string]string{
		"Authorization": "Bearer " + canary,
		"X-API-Key":     canary,
		"Content-Type":  "application/json",
		"Accept":        "application/json",
	}

	got := NewRedactor().Headers(in)

	want := map[string]string{
		"Authorization": Placeholder,
		"X-API-Key":     Placeholder,
		"Content-Type":  "application/json",
		"Accept":        "application/json",
	}
	for name, wantValue := range want {
		if got[name] != wantValue {
			t.Errorf("Headers()[%q] = %q, want %q", name, got[name], wantValue)
		}
	}
	if len(got) != len(want) {
		t.Errorf("Headers() = %v, want %d entries", got, len(want))
	}
	assertNoCanary(t, fmt.Sprint(got))

	// The caller's map is the one the request still holds; redaction is for the
	// output copy and must not reach back into it.
	if in["Authorization"] != "Bearer "+canary {
		t.Error("Headers() mutated its argument, want a redacted copy")
	}
}

// §5a's guarantee is about the header *name*, not about where the value came
// from: a user who types a bearer token into --header must not see it echoed
// back either.
func TestLiteralUserSuppliedValuesAreRedacted(t *testing.T) {
	got := NewRedactor().Headers(map[string]string{
		"Authorization": "Bearer " + canary,
		"cookie":        "session=" + canary,
	})

	for name, value := range got {
		if value != Placeholder {
			t.Errorf("Headers()[%q] = %q, want %q", name, value, Placeholder)
		}
	}
	assertNoCanary(t, fmt.Sprint(got))
}

func TestHeadersOfNilMapIsNil(t *testing.T) {
	if got := NewRedactor().Headers(nil); got != nil {
		t.Errorf("Headers(nil) = %v, want nil", got)
	}
}

// assertNoCanary fails if the canary value survived into s, whatever else the
// caller asserted about it.
func assertNoCanary(t *testing.T, s string) {
	t.Helper()

	if strings.Contains(s, canary) {
		t.Errorf("output leaked the secret value: %s", s)
	}
}

// TestParseRefRoundTripsTheDisplayForm is the property `history replay` depends
// on: a ref that was printed into a stored entry has to come back as the same
// ref, because the value it names was never written down.
func TestParseRefRoundTripsTheDisplayForm(t *testing.T) {
	want := Env("TALARIA_AUTH_BEARER")

	got, ok := ParseRef(want.String())
	if !ok {
		t.Fatalf("ParseRef(%q) reported no ref", want.String())
	}
	if got != want {
		t.Errorf("ParseRef(%q) = %+v, want %+v", want.String(), got, want)
	}
}

func TestParseRefRejectsAnythingElse(t *testing.T) {
	// Placeholder is the important one: a name-redacted literal names no
	// variable, so replaying it is impossible and must not be guessed at.
	for _, s := range []string{
		Placeholder,
		"",
		"Bearer <redacted:env:NAME>",
		"<redacted:env:>",
		"<redacted:NAME>",
		"<redacted:env:NAME",
		"plain-value",
	} {
		if ref, ok := ParseRef(s); ok {
			t.Errorf("ParseRef(%q) = %+v, want no ref", s, ref)
		}
	}
}
