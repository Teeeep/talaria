package curl

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
)

// canary is a value that must never appear in rendered output. Every test that
// exports a credential exports this one, so a leak fails a test rather than
// only being noticed in review.
const canary = "canary-4f8c1e-do-not-leak"

// bearerReq is a GET carrying a bearer token, the shape §3.4 uses as its
// example of symbolic rendering.
func bearerReq() *request.Request {
	return &request.Request{
		OperationID: "getPet",
		Method:      "GET",
		BaseURL:     "https://api.example.com/v1",
		Path:        "/pets/42",
		Headers: []request.Pair{{
			Name:  "Authorization",
			Value: request.Secret(secret.Env("TALARIA_AUTH_BEARER"), request.EncodeBearer),
		}},
	}
}

// assertNoCanary is the §5a invariant in one line: whatever else the renderer
// did, it did not print the credential.
func assertNoCanary(t *testing.T, got string) {
	t.Helper()
	if strings.Contains(got, canary) {
		t.Fatalf("rendered curl contains the credential value:\n%s", got)
	}
}

func TestRenderReferencesBearerTokenByEnvVarName(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BEARER", canary)

	got := Render(bearerReq())

	const want = `-H "Authorization: Bearer $TALARIA_AUTH_BEARER"`
	if !strings.Contains(got, want) {
		t.Errorf("Render() = %s\nwant it to contain %s", got, want)
	}
	assertNoCanary(t, got)
}

func TestRenderReferencesQueryAPIKeyByEnvVarName(t *testing.T) {
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", canary)

	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query: []request.Pair{{
			Name:  "api_key",
			Value: request.Secret(secret.Env("TALARIA_AUTH_APIKEY_PETKEY"), request.EncodeRaw),
		}},
	}

	got := Render(req)

	// $NAME rather than %24NAME: percent-encoding the reference would make the
	// emitted command send the literal text instead of the credential.
	const want = `api_key=$TALARIA_AUTH_APIKEY_PETKEY`
	if !strings.Contains(got, want) {
		t.Errorf("Render() = %s\nwant it to contain %s", got, want)
	}
	assertNoCanary(t, got)
}

func TestRenderReferencesCookieCredentialByEnvVarName(t *testing.T) {
	t.Setenv("TALARIA_AUTH_APIKEY_COOKIEKEY", canary)

	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Cookies: []request.Pair{
			{Name: "flavour", Value: request.Literal("salty")},
			{
				Name:  "session_id",
				Value: request.Secret(secret.Env("TALARIA_AUTH_APIKEY_COOKIEKEY"), request.EncodeRaw),
			},
		},
	}

	got := Render(req)

	const want = `-b "flavour=salty; session_id=$TALARIA_AUTH_APIKEY_COOKIEKEY"`
	if !strings.Contains(got, want) {
		t.Errorf("Render() = %s\nwant it to contain %s", got, want)
	}
	assertNoCanary(t, got)
}

func TestRenderQuotesLiteralHeaderValues(t *testing.T) {
	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Headers: []request.Pair{
			{Name: "X-Note", Value: request.Literal(`it's a "test" $HOME`)},
		},
	}

	got := Render(req)

	// Single quotes need no escaping except for the quote itself, and they stop
	// the shell expanding $HOME — the value is text the user gave, not a
	// reference talaria made.
	const want = `-H 'X-Note: it'\''s a "test" $HOME'`
	if !strings.Contains(got, want) {
		t.Errorf("Render() = %s\nwant it to contain %s", got, want)
	}
}

func TestRenderEscapesLiteralsSharingAWordWithACredential(t *testing.T) {
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", canary)

	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query: []request.Pair{
			{Name: "q", Value: request.Literal(`$HOME "x"`)},
			{Name: "api_key", Value: request.Secret(secret.Env("TALARIA_AUTH_APIKEY_PETKEY"), request.EncodeRaw)},
		},
	}

	got := Render(req)

	// The credential forces the URL into double quotes, so every other shell
	// metacharacter in the same word has to be escaped or neutralised. The
	// literal is percent-encoded by then, which is what does the neutralising.
	if strings.Contains(got, `"$HOME`) || strings.Contains(got, `$HOME"`) {
		t.Errorf("Render() left an unescaped $HOME inside a double-quoted word:\n%s", got)
	}
	if !strings.Contains(got, "q=%24HOME+%22x%22") {
		t.Errorf("Render() = %s\nwant the literal query value percent-encoded", got)
	}
	assertNoCanary(t, got)
}

func TestRenderPercentEncodesLiteralQueryValues(t *testing.T) {
	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query:   []request.Pair{{Name: "q", Value: request.Literal("a b")}},
	}

	got := Render(req)

	if !strings.Contains(got, "q=a+b") && !strings.Contains(got, "q=a%20b") {
		t.Errorf("Render() = %s\nwant the space in the query value encoded", got)
	}
}

func TestRenderUsesDashUForBasicAuth(t *testing.T) {
	t.Setenv("TALARIA_AUTH_BASIC", canary)

	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Headers: []request.Pair{{
			Name:  "Authorization",
			Value: request.Secret(secret.Env("TALARIA_AUTH_BASIC"), request.EncodeBasic),
		}},
	}

	got := Render(req)

	// Basic auth's header text is base64(user:password), which cannot be built
	// without the value. curl's -u takes the raw user:password, so the emitted
	// command stays both symbolic and runnable.
	const want = `-u "$TALARIA_AUTH_BASIC"`
	if !strings.Contains(got, want) {
		t.Errorf("Render() = %s\nwant it to contain %s", got, want)
	}
	if strings.Contains(got, "Authorization") {
		t.Errorf("Render() emitted an Authorization header for basic auth:\n%s", got)
	}
	assertNoCanary(t, got)
}

func TestRenderNamesTheMethodAndBodyForAMutation(t *testing.T) {
	req := &request.Request{
		Method:  "POST",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Body:    &request.Body{ContentType: "application/json", Data: []byte(`{"name":"Rex"}`)},
	}

	got := Render(req)

	for _, want := range []string{
		"-X POST",
		`-H 'Content-Type: application/json'`,
		`--data-raw '{"name":"Rex"}'`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Render() = %s\nwant it to contain %s", got, want)
		}
	}
}

// TestRenderNamesGETWhenItCarriesABody pins the emitted command to the config
// document the executed call actually reads. --data-raw makes curl switch to
// POST unless the method is named, while config.go always writes
// `request = "GET"` — so a GET with a body is the one case where leaving -X out
// prints a command that does something different from the call it reproduces.
func TestRenderNamesGETWhenItCarriesABody(t *testing.T) {
	req := &request.Request{
		Method:  http.MethodGet,
		BaseURL: "https://api.example.com",
		Path:    "/pets/search",
		Body:    &request.Body{ContentType: "application/json", Data: []byte(`{"name":"Rex"}`)},
	}

	got := Render(req)

	if !strings.Contains(got, "-X GET") {
		t.Errorf("Render() = %s\nwant it to contain -X GET: --data-raw without it sends POST", got)
	}
}

func TestRenderLeavesGETUnnamedWithoutABody(t *testing.T) {
	req := &request.Request{
		Method:  http.MethodGet,
		BaseURL: "https://api.example.com",
		Path:    "/pets",
	}

	got := Render(req)

	// GET is curl's default here, so naming it would be noise.
	if strings.Contains(got, "-X") {
		t.Errorf("Render() = %s\nwant no -X: a bodyless GET is what curl sends anyway", got)
	}
}

func TestRenderUsesDashIForHEAD(t *testing.T) {
	req := &request.Request{
		Method:  http.MethodHead,
		BaseURL: "https://api.example.com",
		Path:    "/pets",
	}

	got := Render(req)

	// -I, not -X HEAD: the emitted command has to reproduce the call, and pasted
	// `-X HEAD` waits for a body the server never sends.
	if !strings.Contains(got, "curl -q -s -I ") {
		t.Errorf("Render() = %s\nwant it to contain `curl -q -s -I `", got)
	}
	if strings.Contains(got, "-X HEAD") {
		t.Errorf("Render() = %s\nwant no -X HEAD in it", got)
	}
}

// TestRenderSendsALeadingAtBodyAsText pins the emitted command to the config
// document: config.go uses data-raw because `data` and `data-binary` read a
// leading @ as a filename. Rendered as --data-binary, the reproduction command
// reads a local file and sends it to the API instead of the body talaria sent.
func TestRenderSendsALeadingAtBodyAsText(t *testing.T) {
	req := &request.Request{
		Method:  http.MethodPost,
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Body:    &request.Body{ContentType: "text/plain", Data: []byte("@/etc/hostname")},
	}

	got := Render(req)

	if !strings.Contains(got, `--data-raw '@/etc/hostname'`) {
		t.Errorf("Render() = %s\nwant it to contain --data-raw '@/etc/hostname'", got)
	}
	if strings.Contains(got, "--data-binary") {
		t.Errorf("Render() = %s\nwant no --data-binary: it would read the local file", got)
	}
}

// TestRenderReferencesABodyTheReaderWasNeverShown is DESIGN.md §3.4 for the one
// input that travels in the opposite direction from the rest of the tool: the
// body comes from a human or from CI and is read back by the agent. A file or a
// stdin body can hold a credential the agent only ever handed talaria a path to,
// so the emitted command references it the way curl does rather than inlining
// its bytes. An argv body is already in the agent's hands and stays inline —
// TestRenderSendsALeadingAtBodyAsText above is that clause's guard.
func TestRenderReferencesABodyTheReaderWasNeverShown(t *testing.T) {
	cases := []struct {
		name string
		body request.Body
		want string
	}{
		{
			"a file body",
			request.Body{Source: request.BodyFile, Path: "/tmp/secret.json", Data: []byte(`{"token":"` + canary + `"}`)},
			`--data '@/tmp/secret.json'`,
		},
		{
			"a stdin body",
			request.Body{Source: request.BodyStdin, Data: []byte(`{"token":"` + canary + `"}`)},
			`--data '@-'`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			got := Render(&request.Request{
				Method:  http.MethodPost,
				BaseURL: "https://api.example.com",
				Path:    "/pets",
				Body:    &body,
			})

			assertNoCanary(t, got)
			if !strings.Contains(got, tc.want) {
				t.Errorf("Render() = %s\nwant it to contain %s", got, tc.want)
			}
			if strings.Contains(got, "--data-raw") {
				t.Errorf("Render() = %s\nwant no --data-raw: it would inline the bytes", got)
			}
		})
	}
}

// TestRenderQuotesAHostileFileBodyPathAsOneShellWord is the injection case for
// the reference above. The path is chosen on the same command line as the body,
// but a rendered command is pasted into a shell — so a path holding a quote, a
// substitution or a newline must arrive at curl as one word, unexpanded, or the
// "portable reproduction" is an execution primitive on paste.
//
// The assertion runs the rendered arguments through a real shell rather than
// re-implementing its quoting, because only sh can say how sh splits them.
func TestRenderQuotesAHostileFileBodyPathAsOneShellWord(t *testing.T) {
	path := "/tmp/tal aria's $(touch pwned) `touch pwned` \n body.json"

	got := Render(&request.Request{
		Method:  http.MethodPost,
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Body:    &request.Body{Source: request.BodyFile, Path: path, Data: []byte("{}")},
	})

	// Every word but the leading `curl` is handed to the shell, which prints back
	// the ones that start with @ — exactly one, if the path survived as a word.
	script := "set -- " + strings.TrimPrefix(got, "curl ") +
		`; for a do case "$a" in @*) printf '%s' "$a";; esac; done`
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("the rendered command is not valid shell: %v\n%s", err, got)
	}
	if string(out) != "@"+path {
		t.Errorf("sh read the body argument as %q, want %q\nrendered: %s", out, "@"+path, got)
	}
}

// TestRenderMirrorsTheCurlrcDisable keeps the reproduction honest about the
// call it reproduces: the executed command is `curl -q -K -`, so an emitted one
// without -q would read the reader's ~/.curlrc and behave differently from the
// call it claims to reproduce — and would apply that file's directives to a
// request the reader has just pasted their credential into.
func TestRenderMirrorsTheCurlrcDisable(t *testing.T) {
	if got := Render(bearerReq()); !strings.HasPrefix(got, "curl -q ") {
		t.Errorf("Render() = %s\nwant -q first, where curl reads it", got)
	}
}

func TestRenderPutsTheURLLast(t *testing.T) {
	got := Render(bearerReq())

	if !strings.HasPrefix(got, "curl -q -s ") {
		t.Errorf("Render() = %s\nwant it to start with `curl -q -s `", got)
	}
	if !strings.HasSuffix(got, `'https://api.example.com/v1/pets/42'`) {
		t.Errorf("Render() = %s\nwant it to end with the quoted URL", got)
	}
}

func TestURLShowsCredentialsRedacted(t *testing.T) {
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", canary)

	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query: []request.Pair{
			{Name: "q", Value: request.Literal("a b")},
			{Name: "api_key", Value: request.Secret(secret.Env("TALARIA_AUTH_APIKEY_PETKEY"), request.EncodeRaw)},
		},
	}

	got := URL(req)

	// §5a: symbolic in emitted curl, <redacted> in the URL an agent reads.
	const want = "https://api.example.com/pets?q=a+b&api_key=<redacted:env:TALARIA_AUTH_APIKEY_PETKEY>"
	if got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
	assertNoCanary(t, got)
}

// hiddenQueryReq is a GET whose api_key is a literal the user typed, hidden by
// name rather than resolved from a ref — the shape `--query api_key=…` builds.
func hiddenQueryReq() *request.Request {
	return &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query: []request.Pair{
			{Name: "q", Value: request.Literal("a b")},
			{Name: "api_key", Value: request.Literal(canary).Sensitive()},
		},
	}
}

// TestRenderRedactsAHiddenLiteralQueryValue: a hidden literal has no symbolic
// form, so it renders as the placeholder — and as the placeholder itself, not
// percent-encoded into %3Credacted%3E. Both renderers here are display-only.
func TestRenderRedactsAHiddenLiteralQueryValue(t *testing.T) {
	got := Render(hiddenQueryReq())

	if !strings.Contains(got, "?q=a+b&api_key=<redacted>") {
		t.Errorf("Render() = %s\nwant it to contain ?q=a+b&api_key=<redacted>", got)
	}
	if strings.Contains(got, "%3Credacted%3E") {
		t.Errorf("Render() percent-encoded the placeholder:\n%s", got)
	}
	assertNoCanary(t, got)
}

func TestURLRedactsAHiddenLiteralQueryValue(t *testing.T) {
	got := URL(hiddenQueryReq())

	const want = "https://api.example.com/pets?q=a+b&api_key=<redacted>"
	if got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
	assertNoCanary(t, got)
}

func TestURLOmitsTheQuestionMarkWithNoQuery(t *testing.T) {
	if got, want := URL(bearerReq()), "https://api.example.com/v1/pets/42"; got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
}

func TestRenderFallsBackToTheRedactedFormForANonEnvReference(t *testing.T) {
	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Headers: []request.Pair{{
			Name:  "X-Key",
			Value: request.Secret(secret.SecretRef{Source: "vault", Name: "prod/key"}, request.EncodeRaw),
		}},
	}

	got := Render(req)

	// Nothing to interpolate, so the command stops being copy-pasteable. That
	// is the correct trade against printing the value.
	const want = `-H 'X-Key: <redacted:vault:prod/key>'`
	if !strings.Contains(got, want) {
		t.Errorf("Render() = %s\nwant it to contain %s", got, want)
	}
}

// TestRenderRefusesAContentTypeThatWouldSplitTheRequest covers the surface
// BuildConfig does not: `history replay --dry-run` renders a command without
// ever building a config document, from a media type the history file supplied.
// word.String single-quotes and escapes only the quote, so a CRLF would reach
// the emitted command as two raw bytes — and an emitted command is one a caller
// may well paste and run.
func TestRenderRefusesAContentTypeThatWouldSplitTheRequest(t *testing.T) {
	for _, injection := range []string{
		"application/json\r\nX-Injected: 1",
		"application/json\r\n\r\nGET /admin HTTP/1.1",
		"application/json\nX-Injected: 1",
		"application/json\rX-Injected: 1",
	} {
		t.Run(injection, func(t *testing.T) {
			req := &request.Request{
				Method:  "POST",
				BaseURL: "https://api.example.com",
				Path:    "/pets",
				Body:    &request.Body{ContentType: injection, Data: []byte("{}")},
			}

			got := Render(req)

			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("Render() = %q\nwant no raw CR or LF: the emitted command splits when pasted", got)
			}
			if strings.Contains(got, "X-Injected") || strings.Contains(got, "/admin") {
				t.Errorf("Render() = %q\nwant the smuggled header gone, not merely quoted", got)
			}
		})
	}
}
