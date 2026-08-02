package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/spec"
)

// authCanary is the credential value these tests export. `auth check` answers
// "is it there", so the value must not appear on either stream under any
// format (DESIGN.md §5a).
const authCanary = "canary-auth-4f81c3-do-not-leak"

// authProfileVar is the variable a profile points at, so the profile case and
// the convention case cannot pass for each other's reason.
const authProfileVar = "STAGING_TOKEN"

// authJSON is the envelope `auth check` prints. The schemes stay raw so a test
// can assert the *exact* object §4 specifies, field order included, rather than
// only the fields it thought to look at.
type authJSON struct {
	Schema  string            `json:"schema"`
	Schemes []json.RawMessage `json:"schemes"`
}

// isolateAuthEnv clears everything auth resolution reads from the environment,
// so a developer's own exported token cannot make one of these tests pass.
func isolateAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv(spec.EnvSpec, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, name := range []string{config.EnvBearer, config.EnvBasic, "TALARIA_AUTH_APIKEY_PETKEY", authProfileVar} {
		t.Setenv(name, "")
	}
}

// runAuth runs `auth check` and fails the test if either stream carries the
// credential value.
func runAuth(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut strings.Builder
	code = run(append([]string{"auth", "check"}, args...), &out, &errOut)
	stdout, stderr = out.String(), errOut.String()

	if strings.Contains(stdout, authCanary) {
		t.Fatalf("auth check leaked the credential on stdout:\n%s", stdout)
	}
	if strings.Contains(stderr, authCanary) {
		t.Fatalf("auth check leaked the credential on stderr:\n%s", stderr)
	}

	return code, stdout, stderr
}

func decodeAuth(t *testing.T, stdout string) authJSON {
	t.Helper()

	var got authJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}

	return got
}

func TestAuthCheckReportsPresenceInTheDocumentedShape(t *testing.T) {
	isolateAuthEnv(t)
	t.Setenv(config.EnvBearer, authCanary)

	code, stdout, stderr := runAuth(t, "testdata/auth.yaml", "--output", "json")
	if code != 0 {
		t.Fatalf("auth check = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeAuth(t, stdout)
	if got.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", got.Schema)
	}
	if len(got.Schemes) != 1 {
		t.Fatalf("reported %d schemes, want 1:\n%s", len(got.Schemes), stdout)
	}

	// The §4 sketch, character for character: an agent prompt keys off these
	// three fields, and the source is the *name* an agent asks a human to set.
	want := `{"scheme":"bearerAuth","source":"env:TALARIA_AUTH_BEARER","present":true}`
	if got := string(got.Schemes[0]); got != want {
		t.Errorf("scheme entry =\n %s\nwant\n %s", got, want)
	}
}

func TestAuthCheckExitsFiveWhenTheCredentialIsMissing(t *testing.T) {
	isolateAuthEnv(t)

	code, stdout, stderr := runAuth(t, "testdata/auth.yaml", "--output", "json")
	// Not 2: a missing credential is a fixable environment problem, and §4 gives
	// it its own code so an agent asks the human to set the variable instead of
	// re-reading its own invocation.
	if code != int(clierr.CodeCredentialMissing) {
		t.Fatalf("auth check = %d, want 5; stderr: %s", code, stderr)
	}

	got := decodeAuth(t, stdout)
	if len(got.Schemes) != 1 {
		t.Fatalf("reported %d schemes, want 1:\n%s", len(got.Schemes), stdout)
	}

	want := `{"scheme":"bearerAuth","source":"env:TALARIA_AUTH_BEARER","present":false}`
	if got := string(got.Schemes[0]); got != want {
		t.Errorf("scheme entry =\n %s\nwant\n %s", got, want)
	}

	// The error is the actionable half: it names the variable to export.
	if err := decodeErr(t, stderr); err.Error.Code != int(clierr.CodeCredentialMissing) ||
		!strings.Contains(err.Error.Message, "$"+config.EnvBearer) {
		t.Errorf("error = %+v, want code 5 naming $%s", err.Error, config.EnvBearer)
	}
}

func TestAuthCheckReportsEverySchemeTheSpecDeclares(t *testing.T) {
	// call.yaml declares a bearer scheme and a query API key, and has an
	// operation that requires each.
	isolateAuthEnv(t)
	t.Setenv(config.EnvBearer, authCanary)

	code, stdout, stderr := runAuth(t, "testdata/call.yaml", "--output", "json")
	if code != int(clierr.CodeCredentialMissing) {
		t.Fatalf("auth check = %d, want 5 with petKey unset; stderr: %s", code, stderr)
	}

	entries := decodeAuthEntries(t, stdout)
	if len(entries) != 2 {
		t.Fatalf("reported %d schemes, want 2:\n%s", len(entries), stdout)
	}
	if !entries["bearerAuth"].Present {
		t.Error("bearerAuth reported absent, but its variable is set")
	}
	if entries["petKey"].Present {
		t.Error("petKey reported present, but its variable is unset")
	}
	if want := "env:TALARIA_AUTH_APIKEY_PETKEY"; entries["petKey"].Source != want {
		t.Errorf("petKey source = %q, want %q", entries["petKey"].Source, want)
	}

	if err := decodeErr(t, stderr); !strings.Contains(err.Error.Message, "petKey") {
		t.Errorf("error message = %q, want it to name the unsatisfied scheme", err.Error.Message)
	}
}

func TestAuthCheckSucceedsWhenEverySchemeIsSatisfied(t *testing.T) {
	isolateAuthEnv(t)
	t.Setenv(config.EnvBearer, authCanary)
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", authCanary)

	code, stdout, stderr := runAuth(t, "testdata/call.yaml", "--output", "json")
	if code != 0 {
		t.Fatalf("auth check = %d, want 0; stderr: %s", code, stderr)
	}

	for name, entry := range decodeAuthEntries(t, stdout) {
		if !entry.Present {
			t.Errorf("%s reported absent, but its variable is set", name)
		}
	}
}

func TestAuthCheckReportsTheProfilesVariableAsTheSource(t *testing.T) {
	isolateAuthEnv(t)
	writeRedactConfig(t, "profiles:\n  staging:\n    auth:\n      bearerAuth: ${"+authProfileVar+"}\n")
	// The convention's variable is deliberately left unset: the credential can
	// only be found through the profile.
	t.Setenv(authProfileVar, authCanary)

	code, stdout, stderr := runAuth(t, "testdata/auth.yaml", "--profile", "staging", "--output", "json")
	if code != 0 {
		t.Fatalf("auth check --profile = %d, want 0; stderr: %s", code, stderr)
	}

	entry := decodeAuthEntries(t, stdout)["bearerAuth"]
	if want := "env:" + authProfileVar; entry.Source != want {
		t.Errorf("bearerAuth source = %q, want %q", entry.Source, want)
	}
	if !entry.Present {
		t.Error("bearerAuth reported absent, but the profile's variable is set")
	}
}

func TestAuthCheckNeverPrintsTheValueInAnyFormat(t *testing.T) {
	for _, format := range []string{"json", "pretty", "tsv"} {
		t.Run(format, func(t *testing.T) {
			isolateAuthEnv(t)
			t.Setenv(config.EnvBearer, authCanary)

			// runAuth fails the test if the canary appears on either stream.
			code, stdout, stderr := runAuth(t, "testdata/auth.yaml", "--output", format)
			if code != 0 {
				t.Fatalf("auth check --output %s = %d, want 0; stderr: %s", format, code, stderr)
			}
			if !strings.Contains(stdout, "bearerAuth") {
				t.Errorf("--output %s reported no scheme:\n%s", format, stdout)
			}
		})
	}
}

func TestCallExitsFiveBeforeSendingWhenACredentialIsMissing(t *testing.T) {
	// The credential is checked against the environment before curl runs, so an
	// agent learns what to ask for without the server ever seeing a request.
	isolateAuthEnv(t)
	srv := newCallServer(t, jsonPet)

	var out, errOut strings.Builder
	code := run([]string{
		"call", "testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--output", "json",
	}, &out, &errOut)

	if code != int(clierr.CodeCredentialMissing) {
		t.Fatalf("call = %d, want 5; stderr: %s", code, errOut.String())
	}
	if got := srv.received(); got.Path != "" {
		t.Errorf("the server saw %s %s; the request must not be sent", got.Method, got.Path)
	}
	if err := decodeErr(t, errOut.String()); !strings.Contains(err.Error.Message, "$"+config.EnvBearer) {
		t.Errorf("error message = %q, want it to name $%s", err.Error.Message, config.EnvBearer)
	}
}

// authEntry is one decoded scheme report, for the tests that look at fields
// rather than at the exact document.
type authEntry struct {
	Scheme  string `json:"scheme"`
	Source  string `json:"source"`
	Present bool   `json:"present"`
}

func decodeAuthEntries(t *testing.T, stdout string) map[string]authEntry {
	t.Helper()

	out := map[string]authEntry{}
	for _, raw := range decodeAuth(t, stdout).Schemes {
		var entry authEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("decoding scheme entry %s: %v", raw, err)
		}
		out[entry.Scheme] = entry
	}

	return out
}
