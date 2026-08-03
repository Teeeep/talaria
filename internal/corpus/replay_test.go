package corpus

import (
	"bytes"
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/operation"
)

// getPet is the operation these tests replay against: one path parameter, one
// declared query parameter, and a path template that is not the stored path.
func getPet() operation.Operation {
	return operation.Operation{
		ID:     "getPet",
		Method: "GET",
		Path:   "/pets/{petId}",
		Params: []operation.Param{
			{Name: "petId", In: "path", Required: true},
			{Name: "verbose", In: "query"},
		},
	}
}

// replayEntry is a stored entry for op with the given URL.
func replayEntry(url string) Entry {
	return Entry{Source: SourceCall, OperationID: "getPet", Method: "GET", URL: url}
}

func mustReplay(t *testing.T, entry Entry, op operation.Operation) Replayable {
	t.Helper()

	got, err := entry.Replay(op)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	return got
}

// wantUsage fails unless err is a usage error, which is the exit code an agent
// branches on: a hostile entry fails *that entry*, never the process.
func wantUsage(t *testing.T, err error, what string) {
	t.Helper()

	if err == nil {
		t.Fatalf("Replay accepted %s, want a usage error", what)
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("Replay of %s = %v (code %d), want usage (%d)", what, err, code, clierr.CodeUsage)
	}
}

// The base case, and the trap the plan names: the stored path carries the
// server's own prefix, so a match that required equal segment counts would fail
// every spec with a /v1 base.
func TestReplayRecoversPathParametersUnderAServerPrefix(t *testing.T) {
	for _, path := range []string{"/pets/42", "/v1/pets/42", "/api/v2/pets/42"} {
		t.Run(path, func(t *testing.T) {
			got := mustReplay(t, replayEntry("https://api.example.com"+path), getPet())

			if want := []string{"petId=42"}; !slices.Equal(got.Params, want) {
				t.Errorf("Params = %v, want %v", got.Params, want)
			}
		})
	}
}

// A path parameter re-enters through request.Build, which escapes it again — so
// what comes back here is the decoded value, not the escaped one.
func TestReplayDecodesAPathParameter(t *testing.T) {
	got := mustReplay(t, replayEntry("https://api.example.com/pets/a%2Fb%00c"), getPet())

	if want := []string{"petId=a/b\x00c"}; !slices.Equal(got.Params, want) {
		t.Errorf("Params = %q, want %q", got.Params, want)
	}
}

// A stored path that cannot be matched is exit 2, never a request to a
// half-substituted path.
func TestReplayRefusesAPathThatDoesNotMatchTheTemplate(t *testing.T) {
	for _, path := range []string{"/pets", "/dogs/42", "/", "/pets/42/photo/extra"} {
		t.Run(path, func(t *testing.T) {
			_, err := replayEntry("https://api.example.com" + path).Replay(getPet())
			wantUsage(t, err, "the path "+path)
		})
	}
}

// A declared query parameter goes back through Params so a required one is
// bound rather than reported missing; an undeclared one goes through --query,
// in the order it was recorded.
func TestReplayRoutesQueryParametersByWhatTheSpecDeclares(t *testing.T) {
	got := mustReplay(t,
		replayEntry("https://api.example.com/pets/42?verbose=true&page=2&page=3"), getPet())

	if !slices.Contains(got.Params, "verbose=true") {
		t.Errorf("Params = %v, want the declared verbose parameter bound there", got.Params)
	}
	if want := []string{"page=2", "page=3"}; !slices.Equal(got.Query, want) {
		t.Errorf("Query = %v, want %v in the recorded order", got.Query, want)
	}
}

// Both forms history writes where a credential stood: the bare placeholder and
// the ref form. Replaying either would send the note itself.
func TestReplayDropsEveryRedactionMarker(t *testing.T) {
	entry := replayEntry("https://api.example.com/pets/42?token=%3Credacted%3E")
	entry.Request.Headers = map[string]string{
		"X-Trace": "<redacted:env:SOME_VAR>",
		"Accept":  "application/json",
	}
	entry.Request.Cookies = map[string]string{"session": "<redacted>"}

	got := mustReplay(t, entry, getPet())

	joined := strings.Join(append(append(slices.Clone(got.Params), got.Query...), got.Headers...), " ")
	if strings.Contains(joined, redactionMark) {
		t.Errorf("a redaction marker survived into the inputs: %s", joined)
	}
	if !slices.Contains(got.Headers, "Accept=application/json") {
		t.Errorf("Headers = %v, want the ordinary header kept", got.Headers)
	}
	for _, want := range []string{`query parameter "token"`, `header "X-Trace"`, `cookie "session"`} {
		if !slices.Contains(got.Dropped, want) {
			t.Errorf("Dropped = %v, want it to name %s", got.Dropped, want)
		}
	}
}

// Adversarial: an entry with a plausible-looking literal under a credential
// name. Credentials come only from config.Resolve, so the position is dropped
// whatever it holds.
func TestReplayDropsACredentialShapedHeaderHoldingALiteral(t *testing.T) {
	entry := replayEntry("https://api.example.com/pets/42?api_key=stolen")
	entry.Request.Headers = map[string]string{
		"Authorization": "Bearer a-plausible-looking-literal",
		"X-Api-Token":   "also-plausible",
	}

	got := mustReplay(t, entry, getPet())

	joined := strings.Join(append(slices.Clone(got.Query), got.Headers...), " ")
	for _, leaked := range []string{"a-plausible-looking-literal", "also-plausible", "stolen"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("a credential-shaped position survived into the inputs: %s", joined)
		}
	}
}

// The store is a file on disk, so its URL is checked on the way out as well as
// on the way in: an edited entry must not replay as a file read or a raw TCP
// write.
func TestReplayRefusesAURLWhoseSchemeIsNotHTTP(t *testing.T) {
	for _, raw := range []string{"gopher://127.0.0.1:1234/x", "file:///etc/passwd"} {
		t.Run(raw, func(t *testing.T) {
			_, err := replayEntry(raw).Replay(getPet())
			wantUsage(t, err, raw)

			scheme, _, _ := strings.Cut(raw, ":")
			if !strings.Contains(err.Error(), scheme) {
				t.Errorf("error %v does not name the offending scheme %q", err, scheme)
			}
		})
	}
}

// Userinfo is the credential position a URL carries in cleartext, so an entry
// edited to hold one is refused — and the refusal must not quote what it
// refused.
func TestReplayRefusesAURLCarryingCredentials(t *testing.T) {
	const user, password = "admin", "s3cr3t"

	_, err := replayEntry("http://" + user + ":" + password + "@127.0.0.1:8898/pets/42").Replay(getPet())
	wantUsage(t, err, "a URL with userinfo")

	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), user) {
		t.Errorf("the refusal echoes the userinfo it refused: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:8898") {
		t.Errorf("error %v does not name the host it refused", err)
	}
}

// A replay has to put the original bytes back on the wire. A non-UTF-8 body is
// stored base64'd, so reading Data as text would send replacement characters in
// place of what was sent.
func TestReplayDecodesABinaryBody(t *testing.T) {
	data := make([]byte, 0, 4*256)
	for i := 0; i < 4; i++ {
		for b := 0; b < 256; b++ {
			data = append(data, byte(b))
		}
	}

	entry := replayEntry("https://api.example.com/pets/42")
	entry.Request.Body = &Body{
		ContentType: "application/octet-stream",
		Data:        base64.StdEncoding.EncodeToString(data),
		Encoding:    EncodingBase64,
	}

	got := mustReplay(t, entry, getPet())

	if !got.HasBody {
		t.Fatal("the replay carries no body")
	}
	if !bytes.Equal(got.Body, data) {
		t.Errorf("the replay carries %d bytes, want the recorded %d", len(got.Body), len(data))
	}
}

// An entry written by a newer talaria may use an encoding this build cannot
// read. Replaying its Data as literal text would send something the original
// call did not.
func TestReplayRefusesABodyEncodingItDoesNotKnow(t *testing.T) {
	entry := replayEntry("https://api.example.com/pets/42")
	entry.Request.Body = &Body{Data: "AAAA", Encoding: "zstd+base64"}

	_, err := entry.Replay(getPet())
	wantUsage(t, err, "an unknown body encoding")

	if !strings.Contains(err.Error(), "zstd+base64") {
		t.Errorf("error %v does not name the encoding it refused", err)
	}
}

// The truncation refusal lived in the old replay path and exists nowhere else,
// so it has to be re-asserted here or it disappears silently.
func TestReplayRefusesATruncatedBody(t *testing.T) {
	entry := replayEntry("https://api.example.com/pets/42")
	entry.Request.Body = &Body{Data: "half a body", Truncated: true}

	_, err := entry.Replay(getPet())
	wantUsage(t, err, "a truncated body")
}

// Finding 22: a stored body still carrying a redaction marker is refused rather
// than sent. The observed failure was `{"refresh_token":"<redacted>"}` arriving
// at the API with nothing on stderr.
func TestReplayRefusesABodyStillCarryingARedactionMarker(t *testing.T) {
	for _, body := range []string{
		`{"refresh_token":"<redacted>"}`,
		`{"token":"<redacted:env:TALARIA_AUTH_BEARER>"}`,
	} {
		t.Run(body, func(t *testing.T) {
			entry := replayEntry("https://api.example.com/pets/42")
			entry.Request.Body = &Body{ContentType: "application/json", Data: body}

			_, err := entry.Replay(getPet())
			wantUsage(t, err, "a body carrying a redaction marker")
		})
	}
}

// An empty recorded body is a body, and a missing one is not. Collapsing the
// two would drop a POST's empty payload or invent one for a GET.
func TestReplayDistinguishesAnEmptyBodyFromNone(t *testing.T) {
	none := mustReplay(t, replayEntry("https://api.example.com/pets/42"), getPet())
	if none.HasBody {
		t.Error("an entry with no body produced one")
	}

	entry := replayEntry("https://api.example.com/pets/42")
	entry.Request.Body = &Body{ContentType: "application/json", Data: ""}

	if empty := mustReplay(t, entry, getPet()); !empty.HasBody || len(empty.Body) != 0 {
		t.Errorf("an empty recorded body came back as HasBody=%v, %d bytes",
			empty.HasBody, len(empty.Body))
	}
}

// A template segment with more than one placeholder cannot be taken apart
// unambiguously, so it is refused rather than guessed at.
func TestReplayRefusesAMultiPlaceholderSegment(t *testing.T) {
	op := getPet()
	op.Path = "/pets/{a}-{b}"

	_, err := replayEntry("https://api.example.com/pets/x-y").Replay(op)
	wantUsage(t, err, "a segment with two placeholders")
}

// A single placeholder with literal text around it is the form OpenAPI allows
// and request.Build already substitutes, so it has to come back apart.
func TestReplayRecoversAPartiallyTemplatedSegment(t *testing.T) {
	op := getPet()
	op.Path = "/pets/pet-{petId}.json"

	got := mustReplay(t, replayEntry("https://api.example.com/pets/pet-42.json"), op)

	if want := []string{"petId=42"}; !slices.Equal(got.Params, want) {
		t.Errorf("Params = %v, want %v", got.Params, want)
	}
}
