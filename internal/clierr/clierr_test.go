package clierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

func TestConstructorsMapToDesignedExitCodes(t *testing.T) {
	// The mapping in DESIGN.md §4. Agents branch on these numbers, so they are
	// asserted literally rather than against the constants they came from.
	tests := []struct {
		name string
		err  *Error
		want int
	}{
		{"usage", Usage("unknown operation %q", "getPetz"), 2},
		{"spec load", SpecLoad("parsing spec"), 3},
		{"validation", Validation("body does not match schema"), 4},
		{"credential missing", CredentialMissing("set $TALARIA_AUTH_BEARER"), 5},
		{"request failed", RequestFailed("curl exited 7"), 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := int(tt.err.Code); got != tt.want {
				t.Errorf("Code = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSuccessCodeIsZero(t *testing.T) {
	if CodeOK != 0 {
		t.Errorf("CodeOK = %d, want 0", CodeOK)
	}
}

// decodeRendered renders err and decodes the single JSON line it produced.
func decodeRendered(t *testing.T, err error) map[string]any {
	t.Helper()

	var buf strings.Builder
	Render(&buf, err)

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("rendered error does not end in a newline: %q", out)
	}
	if strings.Count(strings.TrimSuffix(out, "\n"), "\n") != 0 {
		t.Errorf("rendered error spans multiple lines, want one: %q", out)
	}

	var got map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &got); jsonErr != nil {
		t.Fatalf("rendered error is not valid JSON: %v\ngot: %s", jsonErr, out)
	}
	return got
}

func TestRenderWritesStructuredJSON(t *testing.T) {
	got := decodeRendered(t, SpecLoad("reading spec: no such file"))

	if got["schema"] != "talaria/v1" {
		t.Errorf("schema = %v, want talaria/v1", got["schema"])
	}

	body, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field is %T, want an object", got["error"])
	}
	if body["code"] != float64(3) {
		t.Errorf("error.code = %v, want 3", body["code"])
	}
	if body["message"] != "reading spec: no such file" {
		t.Errorf("error.message = %v, want the constructor message", body["message"])
	}
	if _, present := body["valid_alternatives"]; present {
		t.Error("error.valid_alternatives is present on an error that carries none")
	}
}

func TestRenderIncludesValidAlternatives(t *testing.T) {
	// §3.1: errors say what failed, why, and what the valid alternatives are.
	err := Usage("unknown operation %q", "getPetz").
		WithAlternatives("getPetById", "listPets", "createPet")

	body := decodeRendered(t, err)["error"].(map[string]any)

	raw, ok := body["valid_alternatives"].([]any)
	if !ok {
		t.Fatalf("error.valid_alternatives is %T, want an array", body["valid_alternatives"])
	}

	got := make([]string, len(raw))
	for i, v := range raw {
		got[i] = fmt.Sprint(v)
	}
	want := []string{"getPetById", "listPets", "createPet"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("error.valid_alternatives = %v, want %v", got, want)
	}
}

func TestWrappingPreservesTheExitCode(t *testing.T) {
	// A caller that adds context with fmt.Errorf must not silently downgrade a
	// classified failure to the generic code 1.
	wrapped := fmt.Errorf("loading %s: %w", "petstore.yaml", SpecLoad("unexpected EOF"))

	var target *Error
	if !errors.As(wrapped, &target) {
		t.Fatalf("errors.As did not find *Error in %v", wrapped)
	}
	if target.Code != CodeSpecLoad {
		t.Errorf("Code = %d, want %d", target.Code, CodeSpecLoad)
	}
	if got := From(wrapped).Code; got != CodeSpecLoad {
		t.Errorf("From(wrapped).Code = %d, want %d", got, CodeSpecLoad)
	}
}

func TestConstructorsPreserveTheirOwnWrappedError(t *testing.T) {
	// Constructors take a format string, so %w must keep errors.Is working for
	// sentinel errors like fs.ErrNotExist.
	err := SpecLoad("reading spec: %w", fs.ErrNotExist)

	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("errors.Is(err, fs.ErrNotExist) = false, want true")
	}
	if want := "reading spec: " + fs.ErrNotExist.Error(); err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestFromClassifiesAnUnrecognisedErrorAsRequestFailure(t *testing.T) {
	got := From(errors.New("connection reset by peer"))

	if got.Code != CodeRequestFailed {
		t.Errorf("Code = %d, want %d", got.Code, CodeRequestFailed)
	}
	if got.Message != "connection reset by peer" {
		t.Errorf("Message = %q, want the original error text", got.Message)
	}
}

func TestFromNilIsNil(t *testing.T) {
	if got := From(nil); got != nil {
		t.Errorf("From(nil) = %v, want nil", got)
	}
}

func TestRenderNilWritesNothing(t *testing.T) {
	var buf strings.Builder
	Render(&buf, nil)

	if buf.Len() != 0 {
		t.Errorf("Render(nil) wrote %q, want nothing", buf.String())
	}
}
