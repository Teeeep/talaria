package secret

import (
	"encoding/json"
	"strings"
	"testing"
)

// responseCanary stands in for a credential the *server* handed back — a
// session cookie, or the token a login endpoint issues. §5a says these are a
// known uncovered threat, mitigated by Set-Cookie redaction and configured body
// paths; every assertion here greps for it rather than only comparing against
// the expected redacted form.
const responseCanary = "server-issued-CANARY-4e91b7"

func TestResponseHeadersRedactsSetCookieByDefault(t *testing.T) {
	red := NewResponseRedactor(nil, nil)

	got := red.Headers(map[string][]string{
		"Set-Cookie":   {"session=" + responseCanary, "csrf=" + responseCanary},
		"Content-Type": {"application/json"},
	})

	for i, value := range got["Set-Cookie"] {
		if value != Placeholder {
			t.Errorf("Set-Cookie[%d] = %q, want %q", i, value, Placeholder)
		}
	}
	if n := len(got["Set-Cookie"]); n != 2 {
		t.Errorf("Set-Cookie has %d values, want both repetitions kept", n)
	}
	if got := got["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Errorf("Content-Type = %v, want it untouched", got)
	}

	assertNoResponseCanary(t, got)
}

func TestResponseHeadersLeavesTheOriginalAlone(t *testing.T) {
	// The Response the rest of talaria validates against must stay as it came
	// off the wire; only the copy bound for an output surface is redacted.
	headers := map[string][]string{"Set-Cookie": {"session=" + responseCanary}}

	NewResponseRedactor(nil, nil).Headers(headers)

	if got := headers["Set-Cookie"][0]; got != "session="+responseCanary {
		t.Errorf("the input map was modified: Set-Cookie = %q", got)
	}
}

func TestResponseHeadersOfNilMapIsNil(t *testing.T) {
	if got := NewResponseRedactor(nil, nil).Headers(nil); got != nil {
		t.Errorf("Headers(nil) = %v, want nil", got)
	}
}

func TestResponseHeadersExtraPatternsExtendTheBuiltInList(t *testing.T) {
	red := NewResponseRedactor([]string{"x-session-*"}, nil)

	got := red.Headers(map[string][]string{
		"X-Session-Id": {responseCanary},
		"Set-Cookie":   {"session=" + responseCanary},
	})

	if got := got["X-Session-Id"]; len(got) != 1 || got[0] != Placeholder {
		t.Errorf("X-Session-Id = %v, want it redacted by the extra pattern", got)
	}
	if got := got["Set-Cookie"]; len(got) != 1 || got[0] != Placeholder {
		t.Errorf("Set-Cookie = %v, want the built-in list still in effect", got)
	}
}

func TestBodyRedactsConfiguredPathsAndLeavesTheRestIntact(t *testing.T) {
	red := NewResponseRedactor(nil, []string{"data.token"})

	body := red.Body([]byte(`{"access_token":"` + responseCanary +
		`","data":{"token":"` + responseCanary + `","name":"Rex"},"count":42}`))

	assertNoResponseCanary(t, string(body))

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the redacted body is not valid JSON (%s): %v", body, err)
	}
	if got["access_token"] != Placeholder {
		t.Errorf("access_token = %v, want %q from the built-in list", got["access_token"], Placeholder)
	}

	data, ok := got["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %v, want the surrounding object preserved", got["data"])
	}
	if data["token"] != Placeholder {
		t.Errorf("data.token = %v, want %q", data["token"], Placeholder)
	}
	if data["name"] != "Rex" {
		t.Errorf("data.name = %v, want it untouched", data["name"])
	}
	if got["count"] != float64(42) {
		t.Errorf("count = %v, want 42", got["count"])
	}
}

func TestBodyPreservesNumbersItDoesNotRedact(t *testing.T) {
	// Decoding to any and re-encoding turns an int64 into 1.234567890123457e+18
	// unless numbers are kept as text. A body talaria rewrote must still report
	// what the server actually sent.
	red := NewResponseRedactor(nil, nil)

	body := red.Body([]byte(`{"access_token":"` + responseCanary + `","id":1234567890123456789}`))

	if !strings.Contains(string(body), "1234567890123456789") {
		t.Errorf("redaction reformatted a number it was not asked to touch:\n%s", body)
	}
}

func TestBodyOfAMissingPathIsANoOp(t *testing.T) {
	red := NewResponseRedactor(nil, []string{"data.token", "nothing.here.at.all"})

	original := `{"name":"Rex","data":{"name":"Rex"}}`

	if got := string(red.Body([]byte(original))); got != original {
		t.Errorf("Body = %q, want the bytes returned verbatim when nothing matched", got)
	}
}

func TestBodySkipsNonJSONBodies(t *testing.T) {
	red := NewResponseRedactor(nil, []string{"access_token"})

	for _, body := range []string{
		"not json at all",
		"",
		`[{"access_token":"x"}]`,
		`{"access_token":"x"} trailing junk`,
	} {
		if got := string(red.Body([]byte(body))); got != body {
			t.Errorf("Body(%q) = %q, want it returned unchanged", body, got)
		}
	}
}

func TestBodyDoesNotDescendIntoNonObjects(t *testing.T) {
	// data is a string, so data.token names nothing. Walking into it must not
	// panic and must not rewrite the value that is there.
	red := NewResponseRedactor(nil, []string{"data.token"})

	original := `{"data":"a string"}`

	if got := string(red.Body([]byte(original))); got != original {
		t.Errorf("Body = %q, want %q", got, original)
	}
}

func TestNilResponseRedactorStillRedacts(t *testing.T) {
	// A nil *ResponseRedactor is what an unset struct field is, and redaction is
	// never accidentally opt-in.
	var red *ResponseRedactor

	got := red.Headers(map[string][]string{"Set-Cookie": {"session=" + responseCanary}})
	if got := got["Set-Cookie"]; len(got) != 1 || got[0] != Placeholder {
		t.Errorf("Set-Cookie = %v, want it redacted by a nil redactor", got)
	}

	body := red.Body([]byte(`{"access_token":"` + responseCanary + `"}`))
	assertNoResponseCanary(t, string(body))
}

func TestQueryKeyWarningNamesTheParameterAndNoValue(t *testing.T) {
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", responseCanary)

	var out strings.Builder
	NewQueryKeyWarner().Warn(&out, "api_key", Env("TALARIA_AUTH_APIKEY_PETKEY"))

	got := out.String()
	for _, want := range []string{"api_key", "TALARIA_AUTH_APIKEY_PETKEY", "log"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not mention %q:\n%s", want, got)
		}
	}
	assertNoResponseCanary(t, got)
}

func TestQueryKeyWarningFiresOncePerWarner(t *testing.T) {
	warner := NewQueryKeyWarner()

	var out strings.Builder
	for range 3 {
		warner.Warn(&out, "api_key", Env("TALARIA_AUTH_APIKEY_PETKEY"))
	}

	if n := strings.Count(out.String(), "api_key"); n != 1 {
		t.Errorf("the warning fired %d times, want once per process:\n%s", n, out.String())
	}
}

// assertNoResponseCanary fails if value, rendered as text, contains the
// server-issued canary anywhere.
func assertNoResponseCanary(t *testing.T, value any) {
	t.Helper()

	rendered, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("rendering %v: %v", value, err)
	}
	if strings.Contains(string(rendered), responseCanary) {
		t.Errorf("a response credential survived redaction:\n%s", rendered)
	}
}
