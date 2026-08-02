package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
)

// writeConfig writes a config file in a temporary directory with the given
// permissions and returns its path.
func writeConfig(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// WriteFile applies the process umask, so state the mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	return path
}

const twoProfiles = `
profiles:
  staging:
    base-url: https://staging.example.com
    headers:
      X-Env: staging
    auth:
      bearerAuth: ${STAGING_TOKEN}
  prod:
    base-url: https://api.example.com
`

func TestProfileSuppliesBaseURLHeadersAndAuth(t *testing.T) {
	cfg, err := Load(writeConfig(t, 0o600, twoProfiles))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	prof, err := cfg.Profile("staging")
	if err != nil {
		t.Fatalf("Profile(staging): %v", err)
	}

	if prof.Name != "staging" {
		t.Errorf("Name = %q, want %q", prof.Name, "staging")
	}
	if want := "https://staging.example.com"; prof.BaseURL != want {
		t.Errorf("BaseURL = %q, want %q", prof.BaseURL, want)
	}
	if got := prof.Headers["X-Env"]; got != "staging" {
		t.Errorf("Headers[X-Env] = %q, want %q", got, "staging")
	}
	if got := prof.Auth["bearerAuth"]; got != "${STAGING_TOKEN}" {
		t.Errorf("Auth[bearerAuth] = %q, want the ${VAR} reference verbatim", got)
	}
}

func TestUnnamedProfileSelectsNothing(t *testing.T) {
	cfg, err := Load(writeConfig(t, 0o600, twoProfiles))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	prof, err := cfg.Profile("")
	if err != nil {
		t.Fatalf("Profile(\"\"): %v", err)
	}
	if prof != nil {
		t.Fatalf("Profile(\"\") = %+v, want nil — no --profile means no profile", prof)
	}
}

func TestMissingProfileListsTheOnesThatExist(t *testing.T) {
	cfg, err := Load(writeConfig(t, 0o600, twoProfiles))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, err = cfg.Profile("stagign")
	if err == nil {
		t.Fatal("Profile(stagign) succeeded; an unknown profile name is a bad invocation")
	}

	cerr := clierr.From(err)
	if cerr.Code != clierr.CodeUsage {
		t.Errorf("code = %d, want %d", cerr.Code, clierr.CodeUsage)
	}
	want := []string{"prod", "staging"}
	if len(cerr.Alternatives) != len(want) {
		t.Fatalf("alternatives = %v, want %v", cerr.Alternatives, want)
	}
	for i, name := range want {
		if cerr.Alternatives[i] != name {
			t.Fatalf("alternatives = %v, want %v (sorted)", cerr.Alternatives, want)
		}
	}
}

func TestLooseFilePermissionsAreRefused(t *testing.T) {
	path := writeConfig(t, 0o644, twoProfiles)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a world-readable config file; §5 requires mode 0600")
	}
	if !strings.Contains(err.Error(), "0600") {
		t.Errorf("error does not say what the mode must be: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file to fix: %v", err)
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Errorf("code = %d, want %d", code, clierr.CodeUsage)
	}
}

func TestStricterThanRequiredPermissionsAreAccepted(t *testing.T) {
	if _, err := Load(writeConfig(t, 0o400, twoProfiles)); err != nil {
		t.Fatalf("Load rejected a 0400 config file: %v", err)
	}
}

func TestMissingConfigFileIsNotAnError(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load of a missing config file failed: %v", err)
	}

	prof, err := cfg.Profile("")
	if err != nil || prof != nil {
		t.Fatalf("Profile(\"\") = %+v, %v; want nil, nil", prof, err)
	}
	if _, err := cfg.Profile("staging"); err == nil {
		t.Error("Profile(staging) succeeded against a config file that does not exist")
	}
}

func TestUnparseableConfigFileIsAUsageError(t *testing.T) {
	_, err := Load(writeConfig(t, 0o600, "profiles: [this is not a mapping\n"))
	if err == nil {
		t.Fatal("Load accepted an unparseable config file")
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Errorf("code = %d, want %d", code, clierr.CodeUsage)
	}
}

func TestDefaultPathFollowsXDGConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if want := filepath.Join(dir, "talaria", "config.yaml"); got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestEmptyPathUsesTheDefaultLocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	path := filepath.Join(dir, "talaria", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(twoProfiles), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if _, err := cfg.Profile("staging"); err != nil {
		t.Fatalf("Profile(staging) from the default location: %v", err)
	}
}
