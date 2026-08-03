package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// callValidationJSON is the §4 envelope reduced to what validation asserts on.
// It is spelled out here rather than reusing validate.Result so the tests pin
// the wire shape an agent parses, not the Go type behind it.
type callValidationJSON struct {
	DryRun   bool `json:"dry_run"`
	Response *struct {
		Status int `json:"status"`
	} `json:"response"`
	Validation *struct {
		StatusDocumented      bool `json:"status_documented"`
		ContentTypeDocumented bool `json:"content_type_documented"`
		BodyValid             bool `json:"body_valid"`
		Errors                []struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
			Field   string `json:"field"`
		} `json:"errors"`
	} `json:"validation"`
}

func decodeCallValidation(t *testing.T, stdout string) callValidationJSON {
	t.Helper()

	var got callValidationJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}
	if got.Validation == nil {
		t.Fatalf("the validation block is absent from an executed call:\n%s", stdout)
	}

	return got
}

// conformingPet answers with a body that satisfies the Pet schema.
func conformingPet(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"id":"42","name":"Rex"}`)
}

func TestCallReportsAConformingResponseAsValid(t *testing.T) {
	srv := newCallServer(t, conformingPet)

	code, stdout, stderr := runCall(t,
		"testdata/call_validate.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeCallValidation(t, stdout).Validation
	if !got.StatusDocumented || !got.ContentTypeDocumented || !got.BodyValid {
		t.Errorf("validation = %+v, want every check to pass", got)
	}
	if len(got.Errors) != 0 {
		t.Errorf("validation.errors = %+v, want none", got.Errors)
	}
}

func TestCallReportsAViolationAsAnObservationNotAFailure(t *testing.T) {
	// §4: a response that violates the spec is something talaria observed. The
	// exit code stays 0 unless the caller asked for --fail-on-error.
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42}`)
	})

	code, stdout, stderr := runCall(t,
		"testdata/call_validate.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call against a non-conforming response = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeCallValidation(t, stdout).Validation
	if got.BodyValid {
		t.Errorf("validation.body_valid = true for a body missing a required field: %+v", got)
	}
	if len(got.Errors) == 0 {
		t.Error("validation.errors is empty for a non-conforming body")
	}
	// The status was documented; only the body was wrong. The booleans answer
	// independently, which is what makes them worth reading separately.
	if !got.StatusDocumented {
		t.Errorf("validation.status_documented = false for a documented 200: %+v", got)
	}
}

func TestCallReportsAnUndocumentedStatus(t *testing.T) {
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	code, stdout, stderr := runCall(t,
		"testdata/call_validate.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeCallValidation(t, stdout).Validation
	if got.StatusDocumented {
		t.Errorf("validation.status_documented = true for an undocumented 500: %+v", got)
	}
	if len(got.Errors) == 0 {
		t.Error("validation.errors is empty for an undocumented status")
	}
}

func TestCallFailOnErrorExitsFourOnAValidationFailure(t *testing.T) {
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42}`)
	})

	code, stdout, stderr := runCall(t,
		"testdata/call_validate.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--fail-on-error", "--output", "json")
	if code != 4 {
		t.Fatalf("call --fail-on-error against a violation = %d, want 4; stderr: %s", code, stderr)
	}

	// The observation is still reported in full: --fail-on-error changes the
	// exit code, not what an agent gets to read.
	if got := decodeCallValidation(t, stdout).Validation; len(got.Errors) == 0 {
		t.Error("validation.errors is empty on the failing call's stdout")
	}
	if decodeErr(t, stderr).Error.Message == "" {
		t.Errorf("stderr carries no structured error:\n%s", stderr)
	}
}

func TestCallFailOnErrorExitsFourOnAnHTTPError(t *testing.T) {
	// The 404 is documented, so nothing here violates the spec: the exit code
	// comes from --fail-on-error's "nonzero exit if any HTTP >= 400" (§4).
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	code, stdout, stderr := runCall(t,
		"testdata/call_validate.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--fail-on-error", "--output", "json")
	if code != 4 {
		t.Fatalf("call --fail-on-error against a 404 = %d, want 4; stderr: %s", code, stderr)
	}

	got := decodeCallValidation(t, stdout)
	if got.Response == nil || got.Response.Status != http.StatusNotFound {
		t.Fatalf("the response block does not report the 404:\n%s", stdout)
	}
	if len(got.Validation.Errors) != 0 {
		t.Errorf("validation.errors = %+v, want none for a documented 404", got.Validation.Errors)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "404") {
		t.Errorf("error message %q does not name the status that failed the call", msg)
	}
}

func TestCallWithoutFailOnErrorStaysZeroOnAnHTTPError(t *testing.T) {
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	code, _, stderr := runCall(t,
		"testdata/call_validate.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call against a 404 = %d, want 0; stderr: %s", code, stderr)
	}
}

func TestCallValidatesTheRedactedResponseAndLeaksNothing(t *testing.T) {
	// §5a names "validation errors quoting the request" as a leak channel, and
	// this is the shape of the problem: a credential-issuing endpoint returns a
	// token that the schema rejects, so the validator quotes it back. Validating
	// the redacted representation is what keeps the canary out of the report.
	writeRedactConfig(t, "redact:\n  body-paths:\n    - token\n")

	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Set-Cookie", "session="+sessionCanary)
		_, _ = io.WriteString(w, `{"token":"`+sessionCanary+`"}`)
	})

	code, stdout, stderr := runCall(t,
		"testdata/call_validate.yaml", "getSession", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	// runCall already fails on the request-side canary; this covers the value
	// the server sent, which is what validation quotes.
	if strings.Contains(stdout, sessionCanary) {
		t.Fatalf("the response secret reached stdout:\n%s", stdout)
	}
	if strings.Contains(stderr, sessionCanary) {
		t.Fatalf("the response secret reached stderr:\n%s", stderr)
	}

	// The violation is still reported — a redacted string is not an integer —
	// so the leak was closed without blinding the check.
	got := decodeCallValidation(t, stdout).Validation
	if got.BodyValid {
		t.Errorf("validation.body_valid = true for a string where the schema wants an integer: %+v", got)
	}
}

func TestCallDryRunHasNoValidationBlock(t *testing.T) {
	// Nothing came back, so there is nothing to validate. An empty validation
	// block would read as "checked, and fine".
	code, stdout, stderr := runCall(t,
		"testdata/call_validate.yaml", "getPet", "--param", "petId=42",
		"--dry-run", "--output", "json")
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	var got callValidationJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}
	if got.Validation != nil {
		t.Errorf("a dry run carried a validation block: %+v", got.Validation)
	}
}
