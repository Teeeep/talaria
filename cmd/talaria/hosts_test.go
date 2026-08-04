package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
)

// writeStagingProfile writes README.md's headline profile: a base URL and a
// credential named together in one 0600 file, with no allow_hosts. DESIGN.md
// §5a source 4 is the whole of what makes it work — without it the credential
// is resolved from the same file that names the destination and then withheld
// from it, and the fix the warning suggests is to repeat the host.
func writeStagingProfile(t *testing.T, baseURL string) {
	t.Helper()

	writeRedactConfig(t, fmt.Sprintf(
		"profiles:\n  staging:\n    base-url: %q\n    auth:\n      bearerAuth: ${%s}\n",
		baseURL, authProfileVar))
}

func TestCallSendsTheProfilesCredentialToTheProfilesOwnBaseURL(t *testing.T) {
	srv := newCallServer(t, jsonPet)
	writeStagingProfile(t, srv.URL)
	t.Setenv(authProfileVar, callCanary)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--profile", "staging", "--output", "json")
	if code != 0 {
		t.Fatalf("call --profile staging = %d, want 0; stderr: %s", code, stderr)
	}

	if got, want := srv.received().Header.Get("Authorization"), "Bearer "+callCanary; got != want {
		t.Errorf("server saw Authorization %q, want %q: the profile names this host itself", got, want)
	}
	if got := decodeCall(t, stdout).CredentialsWithheld; len(got) != 0 {
		t.Errorf("credentials_withheld = %+v, want nothing withheld from the profile's own base-url", got)
	}
	if stderr != "" {
		t.Errorf("the profile's own base-url warned: %s", stderr)
	}
}

// Source 4 is the profile's base URL, not "wherever this invocation points".
// A --base-url flag is a per-invocation redirection and stays outside the set,
// with a profile active and without one — this is the test that keeps source 4
// from becoming a general escape hatch.
func TestAnOffSetBaseURLFlagStillWithholdsWithAProfileActive(t *testing.T) {
	srv := newCallServer(t, jsonPet)
	writeStagingProfile(t, "https://staging.example.com")
	t.Setenv(authProfileVar, callCanary)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--profile", "staging", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call --profile staging --base-url = %d, want 0; stderr: %s", code, stderr)
	}

	assertNoCanaryOnTheWire(t, srv.received())

	withheld := decodeCall(t, stdout).CredentialsWithheld
	if len(withheld) != 1 {
		t.Fatalf("credentials_withheld has %d entries, want 1:\n%s", len(withheld), stdout)
	}
	if host := strings.TrimPrefix(srv.URL, "http://"); withheld[0].Host != host {
		t.Errorf("credentials_withheld[0].host = %q, want %q", withheld[0].Host, host)
	}
}

// A profile only allows a host when a human selected it by name. One sitting
// unselected in the config file is a host nobody named for this invocation.
func TestAnUnselectedProfilesBaseURLIsNotInTheHostSet(t *testing.T) {
	srv := newCallServer(t, jsonPet)
	writeStagingProfile(t, srv.URL)
	t.Setenv(authProfileVar, callCanary)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	assertNoCanaryOnTheWire(t, srv.received())
	if got := decodeCall(t, stdout).CredentialsWithheld; len(got) != 1 {
		t.Errorf("credentials_withheld = %+v, want the credential withheld: no profile was selected", got)
	}
}

// A base-url a human wrote and talaria cannot read is exit 2, the same rule
// --allow-host and allow_hosts follow. Dropping it silently would leave the
// operator with a credential withheld from the host their own file names.
func TestAMalformedProfileBaseURLIsAUsageError(t *testing.T) {
	for _, baseURL := range []string{
		"not a url",
		"://staging.example.com",
		"ftp://staging.example.com",
		"file:///etc/passwd",
		"/v1/pets",
		"https://staging.example.com\n",
	} {
		t.Run(baseURL, func(t *testing.T) {
			writeStagingProfile(t, baseURL)
			t.Setenv(authProfileVar, callCanary)

			code, _, stderr := runCall(t,
				"testdata/call.yaml", "getPet", "--param", "petId=42",
				"--profile", "staging", "--dry-run", "--output", "json")
			if clierr.Code(code) != clierr.CodeUsage {
				t.Fatalf("call with base-url %q = %d, want %d; stderr: %s",
					baseURL, code, clierr.CodeUsage, stderr)
			}
			if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "base URL") {
				t.Errorf("error message = %q, want it to name the base URL", msg)
			}
		})
	}
}

// Userinfo in a base URL is refused from every source (README.md), and the
// message names the fix. Adding the host to the allowed set must not shortcut
// that: the profile contributes a host, never a credential.
func TestAProfileBaseURLCarryingUserinfoIsStillRefused(t *testing.T) {
	writeStagingProfile(t, "https://user:password@staging.example.com")
	t.Setenv(authProfileVar, callCanary)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--profile", "staging", "--dry-run", "--output", "json")
	if clierr.Code(code) != clierr.CodeUsage {
		t.Fatalf("call with a userinfo base-url = %d, want %d; stderr: %s", code, clierr.CodeUsage, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "userinfo") {
		t.Errorf("error message = %q, want it to name the userinfo", msg)
	}
}
