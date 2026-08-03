package corpus

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Teeeep/talaria/internal/request"
)

// binaryData is every byte value four times over: the bytes a protobuf, a gzip
// stream or an image body is made of, and precisely the ones encoding/json
// rewrites to U+FFFD when they are carried in a Go string.
func binaryData() []byte {
	out := make([]byte, 0, 4*256)
	for i := 0; i < 4; i++ {
		for b := 0; b < 256; b++ {
			out = append(out, byte(b))
		}
	}

	return out
}

func binaryEntry(t *testing.T, data []byte) Entry {
	t.Helper()

	return NewEntry(SourceCall, &request.Request{
		OperationID: "uploadPet",
		Method:      "POST",
		BaseURL:     "https://api.example.com",
		Path:        "/pets/42/photo",
		Body:        &request.Body{ContentType: "application/octet-stream", Data: data},
	}, nil, Redactors{})
}

// The corpus is what the twin will replay from (DESIGN.md §5), so a recorded
// body has to be the bytes that went on the wire and not a lossy rendering of
// them. Storing binary in a JSON string loses every invalid byte to U+FFFD,
// which for this input turns 1024 bytes into 2048 different ones.
func TestBinaryRequestBodySurvivesTheRoundTripThroughJSON(t *testing.T) {
	data := binaryData()

	line, err := json.Marshal(binaryEntry(t, data))
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}

	var back Entry
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}

	body := back.Request.Body
	if body == nil {
		t.Fatal("the recorded entry has no request body")
	}
	if body.Encoding != EncodingBase64 {
		t.Errorf("encoding = %q, want %q", body.Encoding, EncodingBase64)
	}

	got, err := body.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("the round trip returned %d bytes, want the original %d", len(got), len(data))
	}
	if decoded, err := base64.StdEncoding.DecodeString(body.Data); err != nil || !bytes.Equal(decoded, data) {
		t.Errorf("Data is not the base64 of the original bytes (err %v)", err)
	}
}

// The readable case must stay readable: a body that is valid UTF-8 is stored
// exactly as it was before this encoding existed, so old stores keep parsing
// and `history show` keeps printing text.
func TestUTF8RequestBodyIsStoredVerbatim(t *testing.T) {
	const text = `{"name":"Fido — good dog"}`

	entry := NewEntry(SourceCall, &request.Request{
		Method:  "POST",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Body:    &request.Body{ContentType: "application/json", Data: []byte(text)},
	}, nil, Redactors{})

	body := entry.Request.Body
	if body == nil {
		t.Fatal("the recorded entry has no request body")
	}
	if body.Encoding != "" {
		t.Errorf("encoding = %q, want it absent for a UTF-8 body", body.Encoding)
	}
	if body.Data != text {
		t.Errorf("Data = %q, want the body verbatim %q", body.Data, text)
	}

	got, err := body.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(got) != text {
		t.Errorf("Bytes = %q, want %q", got, text)
	}
}

// A store written by a newer talaria may use an encoding this build has never
// heard of. Reading its Data as literal text would send bytes the original call
// did not, so it is refused instead.
func TestBodyBytesRefusesAnUnknownEncoding(t *testing.T) {
	body := Body{ContentType: "application/octet-stream", Data: "AAAA", Encoding: "zstd+base64"}

	got, err := body.Bytes()
	if err == nil {
		t.Fatalf("Bytes() returned %q for an unknown encoding, want an error", got)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("zstd+base64")) {
		t.Errorf("error %v does not name the encoding it refused", err)
	}
}

func TestBodyBytesRefusesUndecodableBase64(t *testing.T) {
	body := Body{Data: "not valid base64!!", Encoding: EncodingBase64}

	if _, err := body.Bytes(); err == nil {
		t.Fatal("Bytes() accepted a Data that is not base64, want an error")
	}
}

// The store is written by the curl executor today and by the twin later, so
// NewEntry takes an observation of its own shape rather than the executor's
// response type. What it records may not change with the signature: the four
// fields land where the *curl.Response form put them, and the response redactor
// still runs over the headers and the body on the way in.
func TestNewEntryRecordsTheObservationItWasGiven(t *testing.T) {
	entry := NewEntry(SourceCall, nil, &Observed{
		Status: 201,
		Headers: map[string][]string{
			"Set-Cookie":   {"session=" + canary},
			"Content-Type": {"application/json"},
		},
		Body:     []byte(`{"access_token":"` + canary + `","id":42}`),
		TimingMS: 137,
	}, Redactors{})

	resp := entry.Response
	if resp == nil {
		t.Fatal("the recorded entry has no response")
	}
	if resp.Status != 201 {
		t.Errorf("status = %d, want 201", resp.Status)
	}
	if resp.TimingMS != 137 {
		t.Errorf("timing_ms = %d, want 137", resp.TimingMS)
	}
	if got := resp.Headers["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Errorf("Content-Type = %q, want the header verbatim", got)
	}
	if resp.Body == nil {
		t.Fatal("the recorded entry has no response body")
	}
	if resp.Body.ContentType != "application/json" {
		t.Errorf("body content type = %q, want it read off the observed headers", resp.Body.ContentType)
	}

	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	if bytes.Contains(line, []byte(canary)) {
		t.Errorf("the credential survived into the entry: %s", line)
	}
}

// A dry run and a connection failure both produce a request that was never
// observed. entry.go documents the nil case as supported, so it has to stay
// supported through the signature change: the request is still worth recording
// as something that was tried.
func TestNewEntryWithoutAnObservationRecordsTheRequestAlone(t *testing.T) {
	entry := NewEntry(SourceCall, &request.Request{
		OperationID: "getPet",
		Method:      "GET",
		BaseURL:     "https://api.example.com",
		Path:        "/pets/42",
	}, nil, Redactors{})

	if entry.Response != nil {
		t.Errorf("Response = %+v, want nil for an unobserved call", entry.Response)
	}
	if entry.OperationID != "getPet" {
		t.Errorf("operation_id = %q, want getPet", entry.OperationID)
	}
	if entry.URL != "https://api.example.com/pets/42" {
		t.Errorf("url = %q, want the request's", entry.URL)
	}
}
