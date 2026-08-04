package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionCanary is what the *server* sends back: a session cookie value. §5a
// puts response headers on the leak-channel table, so it must not reach stdout
// under any format.
const sessionCanary = "server-session-CANARY-7c41f0"

// writeRedactConfig writes a 0600 profile file into an isolated XDG config home
// and points the process at it, so the test never reads — or is affected by —
// the developer's own configuration.
func writeRedactConfig(t *testing.T, yaml string) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	path := filepath.Join(dir, "talaria")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("creating the config directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing the config file: %v", err)
	}
}

func TestCallRedactsSetCookieFromTheResponse(t *testing.T) {
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "session="+sessionCanary)
		jsonPet(w, nil)
	})

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	if strings.Contains(stdout, sessionCanary) {
		t.Fatalf("call printed a Set-Cookie value from the response:\n%s", stdout)
	}

	got := decodeCall(t, stdout)
	if cookie := got.Response.Headers["Set-Cookie"]; len(cookie) != 1 || cookie[0] != "<redacted>" {
		t.Errorf("response.headers[Set-Cookie] = %v, want [<redacted>]", cookie)
	}
	// The rest of the response is untouched: redaction is targeted, not a blanket
	// refusal to report what came back.
	if ct := got.Response.Headers["Content-Type"]; len(ct) == 0 {
		t.Error("response.headers[Content-Type] is absent; redaction dropped an innocent header")
	}
}

func TestCallRedactsConfiguredResponseBodyPaths(t *testing.T) {
	writeRedactConfig(t, "redact:\n  body-paths:\n    - data.token\n")

	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"token":"`+sessionCanary+`","name":"Rex"},"ok":true}`)
	})

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	if strings.Contains(stdout, sessionCanary) {
		t.Fatalf("call printed a configured secret body path:\n%s", stdout)
	}

	var body struct {
		Data struct {
			Token string `json:"token"`
			Name  string `json:"name"`
		} `json:"data"`
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(decodeCall(t, stdout).Response.Body, &body); err != nil {
		t.Fatalf("the redacted body is not embedded JSON: %v", err)
	}
	if body.Data.Token != "<redacted>" {
		t.Errorf("response.body.data.token = %q, want <redacted>", body.Data.Token)
	}
	if body.Data.Name != "Rex" || !body.OK {
		t.Errorf("response.body lost surrounding fields: %+v", body)
	}
}

func TestCallLeavesAnUnmatchedBodyExactlyAsItCame(t *testing.T) {
	writeRedactConfig(t, "redact:\n  body-paths:\n    - data.token\n")

	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	var got map[string]any
	if err := json.Unmarshal(decodeCall(t, stdout).Response.Body, &got); err != nil {
		t.Fatalf("response.body is not embedded JSON: %v", err)
	}
	if got["name"] != "Rex" || got["id"] != "42" {
		t.Errorf("response.body = %v, want the server's document unchanged", got)
	}
}

func TestCallWarnsOnceThatAQueryStringKeyReachesServerLogs(t *testing.T) {
	// getKeyed authenticates with an apiKey in the query string. §5a: talaria
	// keeps it out of its own output, and cannot keep it out of the server's
	// access log — so it says so, on stderr, without printing the value.
	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getKeyed", "--base-url", srv.URL, allowHost(t, srv), "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	for _, want := range []string{"api_key", "TALARIA_AUTH_APIKEY_PETKEY", "log"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not warn about %q:\n%s", want, stderr)
		}
	}
	// The warning belongs on stderr; stdout stays a clean parseable envelope.
	if strings.Contains(stdout, "log") {
		t.Errorf("the warning reached stdout:\n%s", stdout)
	}
}

func TestCallDoesNotWarnWhenNoCredentialIsInTheQueryString(t *testing.T) {
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, allowHost(t, srv), "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	if strings.TrimSpace(stderr) != "" {
		t.Errorf("a bearer-token call warned about the query string:\n%s", stderr)
	}
}

func TestCallWarnsOnADryRunToo(t *testing.T) {
	// The emitted curl is a command the caller may well run. The exposure is a
	// property of the request's shape, not of talaria having sent it.
	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getKeyed", "--dry-run", "--output", "json")
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "api_key") {
		t.Errorf("a dry run did not warn about the query-string key:\n%s", stderr)
	}
}
