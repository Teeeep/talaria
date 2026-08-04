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

// bodyCanary is what a file the caller pointed --body at holds: a token some
// CI job or human wrote, which the agent reading stdout never saw. §3 principle
// 0 names stdout first among the surfaces a credential must not reach, and
// history already redacts this field — printing it raw has the firewall
// backwards.
const bodyCanary = "file-refresh-CANARY-31d8ab"

func TestCallRedactsASecretInARequestBodyReadFromAFile(t *testing.T) {
	sent := `{"refresh_token":"` + bodyCanary + `"}`
	path := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(path, []byte(sent), 0o600); err != nil {
		t.Fatalf("writing the body file: %v", err)
	}

	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations", "--body", "@"+path,
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	// The server received the real bytes. Redaction answers what talaria prints,
	// never what it sends, and a call that quietly sent nothing would pass every
	// assertion below.
	if got := srv.received().Body; got != sent {
		t.Errorf("the server received %q, want the file's bytes %q", got, sent)
	}

	if strings.Contains(stdout, bodyCanary) {
		t.Fatalf("call printed a secret out of the request body file:\n%s", stdout)
	}

	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(decodeCall(t, stdout).Request.Body), &body); err != nil {
		t.Fatalf("request.body is not JSON: %v", err)
	}
	if body.RefreshToken != "<redacted>" {
		t.Errorf("request.body.refresh_token = %q, want <redacted>", body.RefreshToken)
	}
}

// typedBodyCanary is a credential the caller typed into --body. Being typed
// makes the *bytes* the caller's own — which is why the reproduction inlines
// them — but a credential-shaped field inside them is still a credential, and
// §5a promises the emitted curl is "useless to exfiltrate".
const typedBodyCanary = "typed-refresh-CANARY-9f22c1"

func TestCallRedactsASecretInABodyTheCallerTyped(t *testing.T) {
	sent := `{"refresh_token":"` + typedBodyCanary + `"}`

	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations", "--body", sent,
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	// The server received the real bytes. Redaction answers what talaria prints,
	// never what it sends.
	if got := srv.received().Body; got != sent {
		t.Errorf("the server received %q, want the typed bytes %q", got, sent)
	}

	if strings.Contains(stdout, typedBodyCanary) {
		t.Fatalf("call printed a secret out of the typed request body:\n%s", stdout)
	}

	got := decodeCall(t, stdout)
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(got.Request.Body), &body); err != nil {
		t.Fatalf("request.body is not JSON: %v", err)
	}
	if body.RefreshToken != "<redacted>" {
		t.Errorf("request.body.refresh_token = %q, want <redacted>", body.RefreshToken)
	}
	assertCurlInlinesTheShownBody(t, got)
}

// TestCallRedactsAConfiguredBodyPathInTheEmittedCurl is the configurable half:
// redact.body-paths is one list, so a path it names is hidden on every surface
// or the setting is a half-measure.
func TestCallRedactsAConfiguredBodyPathInTheEmittedCurl(t *testing.T) {
	writeRedactConfig(t, "redact:\n  body-paths:\n    - data.token\n")

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations",
		"--body", `{"data":{"token":"`+typedBodyCanary+`"}}`,
		"--dry-run", "--output", "json")
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	if strings.Contains(stdout, typedBodyCanary) {
		t.Fatalf("call printed a configured secret body path:\n%s", stdout)
	}

	got := decodeCall(t, stdout)
	assertCurlInlinesTheShownBody(t, got)

	var body struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(got.Request.Body), &body); err != nil {
		t.Fatalf("request.body is not JSON: %v", err)
	}
	if body.Data.Token != "<redacted>" {
		t.Errorf("request.body.data.token = %q, want <redacted>", body.Data.Token)
	}
}

// assertCurlInlinesTheShownBody is the agreement the drift was: request.curl
// must carry exactly the bytes request.body shows, quoted for a shell. Two
// fields redacted at two call sites is how one of them kept printing the live
// value.
func assertCurlInlinesTheShownBody(t *testing.T, got callExecJSON) {
	t.Helper()

	want := `--data-raw '` + strings.ReplaceAll(got.Request.Body, "'", `'\''`) + `'`
	if !strings.Contains(got.Request.Curl, want) {
		t.Errorf("request.curl = %s\nwant it to inline %s", got.Request.Curl, want)
	}
}

// TestCallRedactsANestedArrayElementInTheEmittedCurl: a path naming a field of
// every element of an array is the shape a token list takes, and the curl
// surface has to honour it element by element rather than only at the root.
func TestCallRedactsANestedArrayElementInTheEmittedCurl(t *testing.T) {
	writeRedactConfig(t, "redact:\n  body-paths:\n    - items.access_token\n")

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations",
		"--body", `{"items":[{"access_token":"`+typedBodyCanary+`"},{"access_token":"`+typedBodyCanary+`"}]}`,
		"--dry-run", "--output", "json")
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	if strings.Contains(stdout, typedBodyCanary) {
		t.Fatalf("call printed a secret nested in a body array:\n%s", stdout)
	}

	got := decodeCall(t, stdout)
	assertCurlInlinesTheShownBody(t, got)

	var body struct {
		Items []struct {
			AccessToken string `json:"access_token"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(got.Request.Body), &body); err != nil {
		t.Fatalf("request.body is not JSON: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("request.body.items has %d elements, want 2", len(body.Items))
	}
	for i, item := range body.Items {
		if item.AccessToken != "<redacted>" {
			t.Errorf("request.body.items[%d].access_token = %q, want <redacted>", i, item.AccessToken)
		}
	}
}

// TestCallInlinesABodyTheRedactorCannotRewriteUnchanged covers the inputs that
// have no JSON document to rewrite. The redactor returns those verbatim, so the
// emitted command must still carry them byte for byte, correctly quoted — a
// reproduction that mangles the body sends something else than the call did.
func TestCallInlinesABodyTheRedactorCannotRewriteUnchanged(t *testing.T) {
	// A configured path none of these bodies has: the redactor runs and finds
	// nothing to rewrite, which is the pass-through the cases are about.
	writeRedactConfig(t, "redact:\n  body-paths:\n    - data.token\n")

	cases := []struct {
		name string
		body string
		// want is the whole quoted --data-raw word, so the assertion covers the
		// shell escaping as well as the bytes.
		want string
	}{
		{"not JSON at all", "plain text, not JSON", `'plain text, not JSON'`},
		{"a bare null", "null", `'null'`},
		{"an empty body", "", `''`},
		{
			"a single quote, which is also what Render quotes with",
			`{"note":"it's here"}`,
			`'{"note":"it'\''s here"}'`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCall(t,
				"testdata/call.yaml", "createPet", "--allow-mutations",
				"--body", tc.body, "--dry-run", "--output", "json")
			if code != 0 {
				t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
			}

			got := decodeCall(t, stdout)
			if got.Request.Body != tc.body {
				t.Errorf("request.body = %q, want the body unchanged %q", got.Request.Body, tc.body)
			}
			if want := "--data-raw " + tc.want; !strings.Contains(got.Request.Curl, want) {
				t.Errorf("request.curl = %s\nwant it to inline %s", got.Request.Curl, want)
			}
		})
	}
}

// TestCallStillShowsABodyTheCallerTyped is the other half: --body given as a
// literal is already in the agent's hands, so the reproduction inlines it
// (DESIGN.md §3.4). Only the redaction of a credential-shaped field applies.
func TestCallStillShowsABodyTheCallerTyped(t *testing.T) {
	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations", "--body", `{"name":"Rex"}`,
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeCall(t, stdout)
	if got.Request.Body != `{"name":"Rex"}` {
		t.Errorf("request.body = %q, want the literal the caller typed", got.Request.Body)
	}
	if !strings.Contains(got.Request.Curl, `--data-raw '{"name":"Rex"}'`) {
		t.Errorf("request.curl = %s\nwant the typed body inlined", got.Request.Curl)
	}
}

// TestCallSendsABodyThatAlreadyReadsAsRedacted covers the body whose own
// content looks like the placeholder. Redaction rewrites the displayed copy and
// nothing else, so a second pass over an already-redacted value has to leave it
// alone and the wire has to carry the literal text the caller wrote.
func TestCallSendsABodyThatAlreadyReadsAsRedacted(t *testing.T) {
	sent := `{"refresh_token":"<redacted>"}`
	path := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(path, []byte(sent), 0o600); err != nil {
		t.Fatalf("writing the body file: %v", err)
	}

	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations", "--body", "@"+path,
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	if got := srv.received().Body; got != sent {
		t.Errorf("the server received %q, want the file's bytes %q", got, sent)
	}

	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(decodeCall(t, stdout).Request.Body), &body); err != nil {
		t.Fatalf("request.body is not JSON: %v", err)
	}
	if body.RefreshToken != "<redacted>" {
		t.Errorf("request.body.refresh_token = %q, want it left as it came", body.RefreshToken)
	}
}

// TestCallSendsABinaryBodyUnmangled is the wire half of the same rule: the
// displayed copy is redacted, the bytes are not. A body that is not UTF-8 has
// no JSON document to rewrite, and rewriting it anyway would corrupt every
// upload.
func TestCallSendsABinaryBodyUnmangled(t *testing.T) {
	sent := string([]byte{0x00, 0xff, 0xfe, '{', '"', 'a', '"', ':', '1', '}', 0x80})
	path := filepath.Join(t.TempDir(), "body.bin")
	if err := os.WriteFile(path, []byte(sent), 0o600); err != nil {
		t.Fatalf("writing the body file: %v", err)
	}

	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations", "--body", "@"+path,
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	if got := srv.received().Body; got != sent {
		t.Errorf("the server received %q, want the file's bytes byte for byte", got)
	}
}

func TestCallWarnsOnceThatAQueryStringKeyReachesServerLogs(t *testing.T) {
	// getKeyed authenticates with an apiKey in the query string. §5a: talaria
	// keeps it out of its own output, and cannot keep it out of the server's
	// access log — so it says so, on stderr, without printing the value.
	srv := newCallServer(t, jsonPet)

	code, stdout, stderr := runCall(t,
		"testdata/call.yaml", "getKeyed", "--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
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
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
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
