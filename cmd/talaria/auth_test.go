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
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json",
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

// `auth check` is the pre-flight an agent trusts before it calls. §5a says it
// reports against the *resolved* host set, so "present" never means "will
// actually be sent": a --base-url the spec does not declare has to show up here
// rather than as an unexplained 401 one command later.
func TestAuthCheckReportsACredentialWithheldByTheHostSet(t *testing.T) {
	isolateAuthEnv(t)
	t.Setenv(config.EnvBearer, authCanary)

	code, stdout, stderr := runAuth(t,
		"testdata/auth.yaml", "--base-url", "http://localhost:9000", "--output", "json")
	if code != 0 {
		t.Fatalf("auth check --base-url = %d, want 0; stderr: %s", code, stderr)
	}

	entry := decodeAuthEntries(t, stdout)["bearerAuth"]
	if !entry.Present {
		t.Error("bearerAuth reported absent, but its variable is set")
	}
	if !entry.Withheld {
		t.Errorf("bearerAuth reported as sendable to a host the spec does not declare:\n%s", stdout)
	}

	// The override agrees with `call`'s: naming the host makes it sendable.
	code, stdout, stderr = runAuth(t, "testdata/auth.yaml",
		"--base-url", "http://localhost:9000", "--allow-host", "localhost", "--output", "json")
	if code != 0 {
		t.Fatalf("auth check --allow-host = %d, want 0; stderr: %s", code, stderr)
	}
	if decodeAuthEntries(t, stdout)["bearerAuth"].Withheld {
		t.Errorf("bearerAuth still reported withheld after --allow-host:\n%s", stdout)
	}
}

// With no --base-url the call goes to the spec's own server, which is in the
// set by definition — so the field must stay absent rather than appear on every
// ordinary invocation.
func TestAuthCheckReportsNothingWithheldForTheSpecsOwnServer(t *testing.T) {
	isolateAuthEnv(t)
	t.Setenv(config.EnvBearer, authCanary)

	code, stdout, stderr := runAuth(t, "testdata/auth.yaml", "--output", "json")
	if code != 0 {
		t.Fatalf("auth check = %d, want 0; stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, "withheld") {
		t.Errorf("auth check against the spec's own server reports a withheld credential:\n%s", stdout)
	}
}

// The pre-flight has to see what the call sees. Where a call goes is the base
// URL *joined to the operation's path*, and a `paths:` key beginning `@` turns
// the spec's own server into userinfo — so a spec whose servers[] block looks
// entirely ordinary still reaches a host nobody declared. An `auth check` that
// only ever looked at the base URL reports the credential as sendable and is
// wrong in the direction that matters.
func TestAuthCheckReportsACredentialWithheldByAHostilePathKey(t *testing.T) {
	isolateAuthEnv(t)
	t.Setenv(config.EnvBearer, authCanary)

	code, stdout, stderr := runAuth(t, "testdata/hostile_path.yaml", "--output", "json")
	if code != 0 {
		t.Fatalf("auth check = %d, want 0; stderr: %s", code, stderr)
	}

	entry := decodeAuthEntries(t, stdout)["bearerAuth"]
	if !entry.Present {
		t.Error("bearerAuth reported absent, but its variable is set")
	}
	if !entry.Withheld {
		t.Errorf("bearerAuth reported as sendable, but the operation's path moves the request off the spec's host:\n%s", stdout)
	}
}

// altKeyA and altKeyB are the variables behind alternatives.yaml's two
// interchangeable API keys.
const (
	altKeyA = "TALARIA_AUTH_APIKEY_KEYA"
	altKeyB = "TALARIA_AUTH_APIKEY_KEYB"
)

func TestAuthCheckAndCallPickTheSameAlternative(t *testing.T) {
	// The pre-flight and the call have to agree: `auth check` is documented as
	// following the resolution `call` uses, and an agent that trusts a 0 here
	// and then gets a 5 from `call` has no way to discover why.
	isolateAuthEnv(t)
	t.Setenv(altKeyA, "")
	t.Setenv(altKeyB, authCanary)

	code, _, stderr := runAuth(t, "testdata/alternatives.yaml", "--output", "json")
	if code != 0 {
		t.Fatalf("auth check = %d, want 0 with keyB exported; stderr: %s", code, stderr)
	}

	var out, errOut strings.Builder
	code = run([]string{
		"call", "testdata/alternatives.yaml", "getEither", "--dry-run", "--output", "json",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0 with keyB exported; stderr: %s", code, errOut.String())
	}

	curl := decodeCall(t, out.String()).Request.Curl
	if !strings.Contains(curl, "X-B") {
		t.Errorf("curl = %s, want the keyB header X-B: keyB is the alternative with a credential", curl)
	}
	if strings.Contains(curl, "X-A") {
		t.Errorf("curl = %s, want no X-A header: $%s is not set and the spec does not require it", curl, altKeyA)
	}
}

// unsupportedSchemes maps each operation of the unsupported fixture to the
// scheme it requires — one per shape talaria cannot speak.
var unsupportedSchemes = map[string]string{
	"getOAuth":     "oauth2",
	"getOIDC":      "oidc",
	"getMutualTLS": "mtls",
	"getPathKey":   "pathKey",
}

// A scheme talaria cannot speak is reported, not hidden (DESIGN.md:322). The
// old behaviour — an empty `schemes` array and exit 0 — told an agent there was
// nothing to do about a spec no call could authenticate.
func TestAuthCheckReportsASchemeItCannotSpeak(t *testing.T) {
	isolateAuthEnv(t)

	code, stdout, stderr := runAuth(t, "testdata/unsupported.yaml", "--output", "json")
	if code != int(clierr.CodeCredentialMissing) {
		t.Fatalf("auth check = %d, want 5; stdout: %s stderr: %s", code, stdout, stderr)
	}

	got := decodeAuth(t, stdout)
	if len(got.Schemes) != 4 {
		t.Fatalf("reported %d schemes, want 4:\n%s", len(got.Schemes), stdout)
	}

	// DESIGN.md:327's object, character for character: no source, because
	// there is no scheme-specific variable to name.
	want := `{"scheme":"mtls","supported":false,"present":false}`
	found := false
	for _, raw := range got.Schemes {
		if strings.Contains(string(raw), `"mtls"`) {
			found = true
			if string(raw) != want {
				t.Errorf("scheme entry =\n %s\nwant\n %s", raw, want)
			}
		}
	}
	if !found {
		t.Errorf("mtls is missing from the report:\n%s", stdout)
	}

	// The error is the actionable half: which scheme, and what to export.
	err := decodeErr(t, stderr)
	if err.Error.Code != int(clierr.CodeCredentialMissing) {
		t.Errorf("error code = %d, want 5", err.Error.Code)
	}
	for _, scheme := range unsupportedSchemes {
		if !strings.Contains(err.Error.Message, scheme) {
			t.Errorf("error %q does not name %s", err.Error.Message, scheme)
		}
	}
	if !strings.Contains(err.Error.Message, "$"+config.EnvBearer) {
		t.Errorf("error %q does not name $%s", err.Error.Message, config.EnvBearer)
	}
}

// Bring your own token: the scheme stays unsupported, and the token the caller
// obtained however its flow demands satisfies it (DESIGN.md:324).
func TestAuthCheckReportsAnUnsupportedSchemeSatisfiedByABroughtToken(t *testing.T) {
	isolateAuthEnv(t)
	t.Setenv(config.EnvBearer, authCanary)

	code, stdout, stderr := runAuth(t, "testdata/unsupported.yaml", "--output", "json")
	if code != 0 {
		t.Fatalf("auth check = %d, want 0 with $%s exported; stderr: %s", code, config.EnvBearer, stderr)
	}

	for name, entry := range decodeAuthEntries(t, stdout) {
		if !entry.Present {
			t.Errorf("%s reported absent, but a token is exported", name)
		}
		if entry.Supported == nil || *entry.Supported {
			t.Errorf("%s reported supported; the token satisfies it, the scheme is still one talaria cannot speak", name)
		}
	}
}

// The agreement clause (DESIGN.md:329): `auth check` never reports a scheme
// satisfied when the call would refuse it. This is the matrix — every
// unsupported shape, with and without a token — and the two verdicts have to
// match in every cell.
func TestAuthCheckAndCallAgreeOnUnsupportedSchemes(t *testing.T) {
	for _, token := range []string{"", authCanary} {
		state := "no-token"
		if token != "" {
			state = "token"
		}

		t.Run(state, func(t *testing.T) {
			isolateAuthEnv(t)
			t.Setenv(config.EnvBearer, token)

			checkCode, _, _ := runAuth(t, "testdata/unsupported.yaml", "--output", "json")

			for op, scheme := range unsupportedSchemes {
				var out, errOut strings.Builder
				callCode := run([]string{
					"call", "testdata/unsupported.yaml", op, "--dry-run", "--output", "json",
				}, &out, &errOut)

				if (checkCode == 0) != (callCode == 0) {
					t.Fatalf("auth check = %d but call %s = %d; the pre-flight and the call disagree.\nstderr: %s",
						checkCode, op, callCode, errOut.String())
				}
				if token == "" {
					if callCode != int(clierr.CodeCredentialMissing) {
						t.Errorf("call %s = %d, want 5 with no token; stderr: %s", op, callCode, errOut.String())
					}
					// Exit 5 is only actionable if it says which scheme and
					// which variable.
					if err := decodeErr(t, errOut.String()); !strings.Contains(err.Error.Message, scheme) ||
						!strings.Contains(err.Error.Message, "$"+config.EnvBearer) {
						t.Errorf("call %s error = %q, want it to name %s and $%s",
							op, err.Error.Message, scheme, config.EnvBearer)
					}
					continue
				}

				if callCode != 0 {
					t.Errorf("call %s = %d, want 0 with a token; stderr: %s", op, callCode, errOut.String())
					continue
				}
				// The token goes out as the bearer credential the scheme's own
				// flow would have produced.
				if curl := decodeCall(t, out.String()).Request.Curl; !strings.Contains(curl, "Authorization") {
					t.Errorf("call %s curl = %s, want the brought token in an Authorization header", op, curl)
				}
			}
		})
	}
}

// A scheme the document never declares is a broken spec, not an unset variable:
// no token can fix it, so both commands stay on exit 2.
func TestAuthCheckAndCallRefuseASchemeTheSpecNeverDeclares(t *testing.T) {
	isolateAuthEnv(t)
	t.Setenv(config.EnvBearer, authCanary)

	code, stdout, stderr := runAuth(t, "testdata/undeclared.yaml", "--output", "json")
	if code != int(clierr.CodeUsage) {
		t.Fatalf("auth check = %d, want 2; stdout: %s stderr: %s", code, stdout, stderr)
	}
	if err := decodeErr(t, stderr); !strings.Contains(err.Error.Message, "ghostScheme") {
		t.Errorf("error = %q, want it to name the undeclared scheme", err.Error.Message)
	}

	var out, errOut strings.Builder
	callCode := run([]string{
		"call", "testdata/undeclared.yaml", "getGhost", "--dry-run", "--output", "json",
	}, &out, &errOut)
	if callCode != int(clierr.CodeUsage) {
		t.Fatalf("call getGhost = %d, want 2; stderr: %s", callCode, errOut.String())
	}
}

// A scheme *name* is spec-controlled and reaches both streams. It may not forge
// a second line of envelope JSON on either of them.
func TestAHostileSchemeNameCannotForgeAnEnvelope(t *testing.T) {
	isolateAuthEnv(t)

	code, stdout, stderr := runAuth(t, "testdata/hostile_scheme.yaml", "--output", "json")
	if code == int(clierr.CodeSpecLoad) {
		// Refusing the document outright is a valid answer to a malformed one.
		return
	}
	if code != int(clierr.CodeCredentialMissing) {
		t.Fatalf("auth check = %d, want 5 or 3; stdout: %s stderr: %s", code, stdout, stderr)
	}

	for name, stream := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if strings.Count(strings.TrimSpace(stream), "\n") != 0 {
			t.Errorf("%s is more than one line; the scheme name broke out of its string:\n%s", name, stream)
		}
		if strings.ContainsAny(stream, "\r") {
			t.Errorf("%s carries a raw carriage return from the scheme name:\n%s", name, stream)
		}
	}

	// Both streams still decode as one envelope each, forged content and all.
	if got := decodeAuth(t, stdout); len(got.Schemes) != 1 {
		t.Errorf("reported %d schemes, want 1:\n%s", len(got.Schemes), stdout)
	}
	if err := decodeErr(t, stderr); err.Error.Code != int(clierr.CodeCredentialMissing) {
		t.Errorf("error code = %d, want 5", err.Error.Code)
	}
}

// authEntry is one decoded scheme report, for the tests that look at fields
// rather than at the exact document.
type authEntry struct {
	Scheme string `json:"scheme"`
	Source string `json:"source"`
	// Supported is a pointer because its absence is the ordinary case and means
	// supported; only a scheme talaria cannot speak carries it, as false.
	Supported *bool `json:"supported"`
	Present   bool  `json:"present"`
	Withheld  bool  `json:"withheld"`
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
