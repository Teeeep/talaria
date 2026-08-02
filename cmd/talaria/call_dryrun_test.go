package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/spec"
)

// callCanary is the credential value the tests export. It must not appear on
// stdout or stderr under any format (DESIGN.md §5a).
const callCanary = "canary-9b2d7a-do-not-leak"

// callJSON is the dry-run payload: the §4 request block inside the versioned
// envelope, minus the response block a dry run never has.
type callJSON struct {
	Schema  string `json:"schema"`
	DryRun  bool   `json:"dry_run"`
	Request struct {
		Curl    string            `json:"curl"`
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"request"`
	Response any `json:"response"`
}

// errJSON is the stderr shape from internal/clierr.
type errJSON struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// runCall runs `call` with a credential present and no ambient spec, and fails
// the test if either stream carries the credential.
func runCall(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv(spec.EnvSpec, "")
	t.Setenv("TALARIA_AUTH_BEARER", callCanary)
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", callCanary)

	var out, errOut strings.Builder
	code = run(append([]string{"call"}, args...), &out, &errOut)
	stdout, stderr = out.String(), errOut.String()

	if strings.Contains(stdout, callCanary) {
		t.Fatalf("call leaked the credential on stdout:\n%s", stdout)
	}
	if strings.Contains(stderr, callCanary) {
		t.Fatalf("call leaked the credential on stderr:\n%s", stderr)
	}

	return code, stdout, stderr
}

func decodeErr(t *testing.T, stderr string) errJSON {
	t.Helper()

	var got errJSON
	if err := json.Unmarshal([]byte(stderr), &got); err != nil {
		t.Fatalf("decoding stderr %q: %v", stderr, err)
	}

	return got
}

func TestCallDryRunPrintsSymbolicCurlAndExitsZero(t *testing.T) {
	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42", "--dry-run", "--output", "pretty")
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	for _, want := range []string{
		"curl -s ",
		`-H "Authorization: Bearer $TALARIA_AUTH_BEARER"`,
		"https://api.invalid/v1/pets/42",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestCallDryRunJSONCarriesTheRequestBlock(t *testing.T) {
	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42", "--query", "verbose=true",
		"--dry-run", "--output", "json")
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	var got callJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}

	if got.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", got.Schema)
	}
	if !got.DryRun {
		t.Error("dry_run = false, want true")
	}
	if got.Response != nil {
		t.Errorf("response = %v, want it absent from a dry run", got.Response)
	}
	if got.Request.Method != "GET" {
		t.Errorf("request.method = %q, want GET", got.Request.Method)
	}
	if want := "https://api.invalid/v1/pets/42?verbose=true"; got.Request.URL != want {
		t.Errorf("request.url = %q, want %q", got.Request.URL, want)
	}
	// §4's sketch: headers carry the redacted reference, not the value and not
	// the shell form — this field is read, not run. The scheme prefix stays,
	// because the header really does read `Bearer <token>` on the wire.
	if want := "Bearer <redacted:env:TALARIA_AUTH_BEARER>"; got.Request.Headers["Authorization"] != want {
		t.Errorf("request.headers[Authorization] = %q, want %q", got.Request.Headers["Authorization"], want)
	}
	if !strings.Contains(got.Request.Curl, "$TALARIA_AUTH_BEARER") {
		t.Errorf("request.curl does not reference the env var:\n%s", got.Request.Curl)
	}
}

func TestCallDryRunRedactsAQueryStringAPIKey(t *testing.T) {
	code, stdout, stderr := runCall(t, "testdata/call.yaml", "getKeyed", "--dry-run", "--output", "json")
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	var got callJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}

	if want := "api_key=<redacted:env:TALARIA_AUTH_APIKEY_PETKEY>"; !strings.Contains(got.Request.URL, want) {
		t.Errorf("request.url = %q, want it to contain %q", got.Request.URL, want)
	}
	if want := "api_key=$TALARIA_AUTH_APIKEY_PETKEY"; !strings.Contains(got.Request.Curl, want) {
		t.Errorf("request.curl = %q, want it to contain %q", got.Request.Curl, want)
	}
}

func TestCallRefusesAMutationWithoutAllowMutations(t *testing.T) {
	code, _, stderr := runCall(t, "testdata/call.yaml", "createPet", "--dry-run")
	if code != int(clierr.CodeUsage) {
		t.Fatalf("call POST --dry-run = %d, want %d; stderr: %s", code, clierr.CodeUsage, stderr)
	}

	msg := decodeErr(t, stderr).Error.Message
	for _, want := range []string{"--allow-mutations", "POST", "createPet"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not mention %q", msg, want)
		}
	}
}

func TestCallGatesMutationsBeforeBindingParameters(t *testing.T) {
	// deleteAllPets is a mutation and the invocation is also malformed. The
	// gate is evaluated first, so an agent learns the rule in one round trip
	// instead of fixing the flag and then discovering the gate.
	code, _, stderr := runCall(t, "testdata/call.yaml", "deleteAllPets", "--header", "malformed", "--dry-run")
	if code != int(clierr.CodeUsage) {
		t.Fatalf("call DELETE = %d, want %d; stderr: %s", code, clierr.CodeUsage, stderr)
	}

	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "--allow-mutations") {
		t.Errorf("error message %q does not mention --allow-mutations", msg)
	}
}

func TestCallAllowsAMutationWithTheFlag(t *testing.T) {
	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations", "--dry-run", "--output", "json")
	if code != 0 {
		t.Fatalf("call POST --allow-mutations --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "-X POST") {
		t.Errorf("dry-run curl does not name the method:\n%s", stdout)
	}
}

func TestCallNeedsNoFlagForSafeMethods(t *testing.T) {
	code, _, stderr := runCall(t, "testdata/call.yaml", "getPublic", "--dry-run")
	if code != 0 {
		t.Fatalf("call GET --dry-run = %d, want 0; stderr: %s", code, stderr)
	}
}

func TestCallReportsAMissingRequiredParam(t *testing.T) {
	code, _, stderr := runCall(t, "testdata/call.yaml", "getPet", "--dry-run")
	if code != int(clierr.CodeUsage) {
		t.Fatalf("call without a required param = %d, want %d", code, clierr.CodeUsage)
	}

	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "petId") {
		t.Errorf("error message %q does not name the missing parameter", msg)
	}
}

func TestCallWithoutDryRunReportsThatExecutionIsNotAvailableYet(t *testing.T) {
	// Phase 1 ends at the dry run: there is no execution path at all yet, so
	// the command says so rather than silently printing a dry run.
	code, _, stderr := runCall(t, "testdata/call.yaml", "getPublic")
	if code == 0 {
		t.Fatalf("call without --dry-run = 0, want a failure; stderr: %s", stderr)
	}

	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "--dry-run") {
		t.Errorf("error message %q does not point at --dry-run", msg)
	}
}

func TestCallReportsAnUnknownOperation(t *testing.T) {
	code, _, stderr := runCall(t, "testdata/call.yaml", "noSuchOperation", "--dry-run")
	if code != int(clierr.CodeUsage) {
		t.Fatalf("call with an unknown operation = %d, want %d; stderr: %s", code, clierr.CodeUsage, stderr)
	}
}
