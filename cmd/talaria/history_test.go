package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/spec"
)

// historyListJSON is the `history` payload: the store, newest first, one line
// per entry.
type historyListJSON struct {
	Schema  string `json:"schema"`
	Entries []struct {
		Index       int    `json:"index"`
		ID          string `json:"id"`
		Timestamp   string `json:"timestamp"`
		Source      string `json:"source"`
		OperationID string `json:"operation_id"`
		Method      string `json:"method"`
		Path        string `json:"path"`
		URL         string `json:"url"`
		Status      int    `json:"status"`
	} `json:"entries"`
}

// historyShowJSON is the `history show` payload: one whole entry, as recorded.
type historyShowJSON struct {
	Schema string `json:"schema"`
	Index  int    `json:"index"`
	Entry  struct {
		Source      string `json:"source"`
		OperationID string `json:"operation_id"`
		Method      string `json:"method"`
		URL         string `json:"url"`
		Request     struct {
			Headers map[string]string `json:"headers"`
			Cookies map[string]string `json:"cookies"`
			Body    *struct {
				Data string `json:"data"`
			} `json:"body"`
		} `json:"request"`
		Response *struct {
			Status  int                 `json:"status"`
			Headers map[string][]string `json:"headers"`
			Body    *struct {
				Data string `json:"data"`
			} `json:"body"`
		} `json:"response"`
	} `json:"entry"`
}

// isolateHistory points the state directory at a fresh temp dir and returns the
// path the store will write to, so a test can assert on the file's absence as
// well as on its contents.
func isolateHistory(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	return filepath.Join(dir, "talaria", "history.jsonl")
}

// runHistory runs `history` with the same credential present as runCall, and
// fails the test if either stream carries it. History is a permanent artifact
// (§5a), so the leak check matters more here than anywhere else.
func runHistory(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv(spec.EnvSpec, "")
	t.Setenv("TALARIA_AUTH_BEARER", callCanary)
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", callCanary)

	var out, errOut strings.Builder
	code = run(append([]string{"history"}, args...), &out, &errOut)
	stdout, stderr = out.String(), errOut.String()

	if strings.Contains(stdout, callCanary) {
		t.Fatalf("history leaked the credential on stdout:\n%s", stdout)
	}
	if strings.Contains(stderr, callCanary) {
		t.Fatalf("history leaked the credential on stderr:\n%s", stderr)
	}

	return code, stdout, stderr
}

func decodeHistoryList(t *testing.T, stdout string) historyListJSON {
	t.Helper()

	var got historyListJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}

	return got
}

func decodeHistoryShow(t *testing.T, stdout string) historyShowJSON {
	t.Helper()

	var got historyShowJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}

	return got
}

// seedEntry builds one stored entry without making a request, so a filter test
// can control age, source and status directly.
func seedEntry(source corpus.Source, age time.Duration, id, method, url string, status int) corpus.Entry {
	entry := corpus.Entry{
		Timestamp:   time.Now().UTC().Add(-age),
		Source:      source,
		OperationID: id,
		Method:      method,
		URL:         url,
	}
	if status != 0 {
		entry.Response = &corpus.EntryResponse{Status: status}
	}

	return entry
}

// seedHistory writes entries to the isolated store in the order given, which
// becomes their append order and so their age order.
func seedHistory(t *testing.T, entries ...corpus.Entry) {
	t.Helper()

	store := corpus.New("", true)
	for _, entry := range entries {
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatalf("seeding the history store: %v", err)
		}
	}
}

// readHistory returns the raw stored entries, oldest first.
func readHistory(t *testing.T) []corpus.Entry {
	t.Helper()

	entries, err := corpus.New("", true).Read()
	if err != nil {
		t.Fatalf("reading the history store: %v", err)
	}

	return entries
}

// indexes is the list's index column, for asserting that filtering does not
// renumber entries.
func indexes(list historyListJSON) []int {
	out := make([]int, 0, len(list.Entries))
	for _, entry := range list.Entries {
		out = append(out, entry.Index)
	}

	return out
}

// operations is the list's operation column, which is what most filter tests
// assert on.
func operations(list historyListJSON) []string {
	out := make([]string, 0, len(list.Entries))
	for _, entry := range list.Entries {
		out = append(out, entry.OperationID)
	}

	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestCallRecordsExactlyOneHistoryEntry(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	entries := readHistory(t)
	if len(entries) != 1 {
		t.Fatalf("history holds %d entries after one call, want 1", len(entries))
	}

	entry := entries[0]
	if entry.Source != corpus.SourceCall {
		t.Errorf("entry.source = %q, want %q", entry.Source, corpus.SourceCall)
	}
	if entry.OperationID != "getPet" {
		t.Errorf("entry.operation_id = %q, want getPet", entry.OperationID)
	}
	if entry.Response == nil || entry.Response.Status != http.StatusOK {
		t.Errorf("entry.response = %+v, want a 200", entry.Response)
	}
}

func TestCallDryRunRecordsNothing(t *testing.T) {
	// A dry run is a question about a request, not a request. Recording it would
	// make "what have I already tried" answer with things that never happened.
	path := isolateHistory(t)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42", "--dry-run", "--output", "json")
	if code != 0 {
		t.Fatalf("call --dry-run = %d, want 0; stderr: %s", code, stderr)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a dry run created the history file %s", path)
	}
}

func TestCallRecordsARequestThatNeverCompleted(t *testing.T) {
	// A refused connection is still something that was tried, and an agent that
	// asks "what have I already tried" should be told about it.
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)
	url := srv.URL
	srv.Close()

	if code, _, _ := runCall(t, "testdata/call.yaml", "getPublic", "--base-url", url); code != 1 {
		t.Fatalf("call against a closed port = %d, want 1", code)
	}

	entries := readHistory(t)
	if len(entries) != 1 {
		t.Fatalf("history holds %d entries after a failed call, want 1", len(entries))
	}
	if entries[0].Response != nil {
		t.Errorf("entry.response = %+v, want none for a request that got nothing back", entries[0].Response)
	}
}

func TestHistoryListsNewestFirstWithIndexTimestampMethodPathAndStatus(t *testing.T) {
	isolateHistory(t)
	seedHistory(t,
		seedEntry(corpus.SourceCall, 2*time.Minute, "getPublic", "GET", "https://api.test/public", 200),
		seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", "https://api.test/pets/42?verbose=true", 404),
	)

	code, stdout, stderr := runHistory(t, "--output", "json")
	if code != 0 {
		t.Fatalf("history = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeHistoryList(t, stdout)
	if got.Schema != "talaria/v1" {
		t.Errorf("schema = %q, want talaria/v1", got.Schema)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("history listed %d entries, want 2:\n%s", len(got.Entries), stdout)
	}

	newest := got.Entries[0]
	if newest.OperationID != "getPet" {
		t.Errorf("first entry is %q, want the newest (getPet)", newest.OperationID)
	}
	if newest.Index != 1 {
		t.Errorf("newest entry index = %d, want 1", newest.Index)
	}
	if newest.Method != "GET" {
		t.Errorf("entry.method = %q, want GET", newest.Method)
	}
	if newest.Path != "/pets/42" {
		t.Errorf("entry.path = %q, want /pets/42", newest.Path)
	}
	if newest.Status != http.StatusNotFound {
		t.Errorf("entry.status = %d, want 404", newest.Status)
	}
	if _, err := time.Parse(time.RFC3339, newest.Timestamp); err != nil {
		t.Errorf("entry.timestamp %q is not RFC3339: %v", newest.Timestamp, err)
	}
	if got.Entries[1].Index != 2 {
		t.Errorf("second entry index = %d, want 2", got.Entries[1].Index)
	}

	// The same five columns have to reach a human, not just an agent.
	code, pretty, stderr := runHistory(t, "--output", "pretty")
	if code != 0 {
		t.Fatalf("history --output pretty = %d, want 0; stderr: %s", code, stderr)
	}
	for _, want := range []string{"1", "GET", "/pets/42", "404"} {
		if !strings.Contains(pretty, want) {
			t.Errorf("pretty history does not contain %q:\n%s", want, pretty)
		}
	}
}

func TestHistoryListsAStableIDAlongsideTheIndex(t *testing.T) {
	isolateHistory(t)
	seedHistory(t,
		seedEntry(corpus.SourceCall, 2*time.Minute, "getPublic", "GET", "https://api.test/public", 200),
		seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", "https://api.test/pets/42", 404),
	)

	code, stdout, stderr := runHistory(t, "--output", "json")
	if code != 0 {
		t.Fatalf("history = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeHistoryList(t, stdout)
	if len(got.Entries) != 2 {
		t.Fatalf("history listed %d entries, want 2:\n%s", len(got.Entries), stdout)
	}
	for _, entry := range got.Entries {
		if entry.ID == "" {
			t.Fatalf("entry %d has no id in the list:\n%s", entry.Index, stdout)
		}
	}
	if got.Entries[0].ID == got.Entries[1].ID {
		t.Errorf("both entries list id %q", got.Entries[0].ID)
	}

	// The id has to reach a human too, or nobody can type it back in.
	newest := got.Entries[0].ID
	code, pretty, stderr := runHistory(t, "--output", "pretty")
	if code != 0 {
		t.Fatalf("history --output pretty = %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(pretty, newest) {
		t.Errorf("pretty history does not show the id %q:\n%s", newest, pretty)
	}

	// An append renumbers the index; the id is the handle that survives it.
	seedHistory(t, seedEntry(corpus.SourceCall, 0, "getKeyed", "GET", "https://api.test/keyed", 200))

	code, stdout, stderr = runHistory(t, "--output", "json")
	if code != 0 {
		t.Fatalf("history after an append = %d, want 0; stderr: %s", code, stderr)
	}
	after := decodeHistoryList(t, stdout)
	if len(after.Entries) != 3 {
		t.Fatalf("history listed %d entries, want 3", len(after.Entries))
	}
	if after.Entries[1].ID != newest {
		t.Errorf("the entry at index 2 has id %q, want the unchanged %q", after.Entries[1].ID, newest)
	}
	if after.Entries[1].Index == got.Entries[0].Index {
		t.Errorf("index %d did not shift after an append, so this test proves nothing", after.Entries[1].Index)
	}
}

func TestHistoryShowAcceptsAnIDAsWellAsAnIndex(t *testing.T) {
	isolateHistory(t)
	seedHistory(t,
		seedEntry(corpus.SourceCall, 2*time.Minute, "getPublic", "GET", "https://api.test/public", 200),
		seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", "https://api.test/pets/42", 404),
	)

	code, stdout, stderr := runHistory(t, "--output", "json")
	if code != 0 {
		t.Fatalf("history = %d, want 0; stderr: %s", code, stderr)
	}
	oldest := decodeHistoryList(t, stdout).Entries[1]

	code, stdout, stderr = runHistory(t, "show", oldest.ID, "--output", "json")
	if code != 0 {
		t.Fatalf("history show <id> = %d, want 0; stderr: %s", code, stderr)
	}
	if got := decodeHistoryShow(t, stdout); got.Entry.OperationID != "getPublic" {
		t.Errorf("history show %q showed %q, want getPublic", oldest.ID, got.Entry.OperationID)
	}

	// The documented positional form keeps working.
	code, stdout, stderr = runHistory(t, "show", "2", "--output", "json")
	if code != 0 {
		t.Fatalf("history show 2 = %d, want 0; stderr: %s", code, stderr)
	}
	if got := decodeHistoryShow(t, stdout); got.Entry.OperationID != "getPublic" {
		t.Errorf("history show 2 showed %q, want getPublic", got.Entry.OperationID)
	}

	code, _, stderr = runHistory(t, "show", "no-such-entry")
	if code != 2 {
		t.Fatalf("history show of an unknown handle = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "no-such-entry") {
		t.Errorf("error message %q does not name the handle asked for", msg)
	}
}

func TestHistoryOnAnEmptyStoreListsNothing(t *testing.T) {
	isolateHistory(t)

	code, stdout, stderr := runHistory(t, "--output", "json")
	if code != 0 {
		t.Fatalf("history on an empty store = %d, want 0; stderr: %s", code, stderr)
	}

	if got := decodeHistoryList(t, stdout); len(got.Entries) != 0 {
		t.Errorf("history listed %d entries from an empty store", len(got.Entries))
	}
}

func TestHistoryFiltersByOperation(t *testing.T) {
	isolateHistory(t)
	seedHistory(t,
		seedEntry(corpus.SourceCall, 3*time.Minute, "getPublic", "GET", "https://api.test/public", 200),
		seedEntry(corpus.SourceCall, 2*time.Minute, "getPet", "GET", "https://api.test/pets/42", 200),
		seedEntry(corpus.SourceCall, time.Minute, "getPublic", "GET", "https://api.test/public", 200),
	)

	code, stdout, stderr := runHistory(t, "--operation", "getPet", "--output", "json")
	if code != 0 {
		t.Fatalf("history --operation = %d, want 0; stderr: %s", code, stderr)
	}

	got := decodeHistoryList(t, stdout)
	if want := []string{"getPet"}; !equalStrings(operations(got), want) {
		t.Fatalf("history --operation getPet listed %v, want %v", operations(got), want)
	}
	// Filtering selects; it does not renumber. The index a filtered list shows is
	// the one `history show` takes, which is the whole point of showing it.
	if idx := indexes(got); len(idx) != 1 || idx[0] != 2 {
		t.Errorf("filtered index = %v, want [2] — the entry's position in the whole store", idx)
	}
}

func TestHistoryFiltersBySince(t *testing.T) {
	isolateHistory(t)
	seedHistory(t,
		seedEntry(corpus.SourceCall, 2*time.Hour, "old", "GET", "https://api.test/old", 200),
		seedEntry(corpus.SourceCall, time.Minute, "recent", "GET", "https://api.test/recent", 200),
	)

	code, stdout, stderr := runHistory(t, "--since", "1h", "--output", "json")
	if code != 0 {
		t.Fatalf("history --since = %d, want 0; stderr: %s", code, stderr)
	}

	if got := decodeHistoryList(t, stdout); !equalStrings(operations(got), []string{"recent"}) {
		t.Errorf("history --since 1h listed %v, want [recent]", operations(got))
	}
}

func TestHistoryRejectsAnUnparseableSince(t *testing.T) {
	isolateHistory(t)

	code, _, stderr := runHistory(t, "--since", "yesterday")
	if code != 2 {
		t.Fatalf("history --since yesterday = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "yesterday") {
		t.Errorf("error message %q does not name the bad value", msg)
	}
}

func TestHistoryFiltersByStatusClassAndExactCode(t *testing.T) {
	isolateHistory(t)
	seedHistory(t,
		seedEntry(corpus.SourceCall, 4*time.Minute, "ok", "GET", "https://api.test/a", 200),
		seedEntry(corpus.SourceCall, 3*time.Minute, "gone", "GET", "https://api.test/b", 404),
		seedEntry(corpus.SourceCall, 2*time.Minute, "teapot", "GET", "https://api.test/c", 418),
		seedEntry(corpus.SourceCall, time.Minute, "broken", "GET", "https://api.test/d", 500),
	)

	cases := []struct {
		status string
		want   []string
	}{
		{"4xx", []string{"teapot", "gone"}},
		{"404", []string{"gone"}},
		{"5xx", []string{"broken"}},
		{"2xx", []string{"ok"}},
	}

	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			code, stdout, stderr := runHistory(t, "--status", tc.status, "--output", "json")
			if code != 0 {
				t.Fatalf("history --status %s = %d, want 0; stderr: %s", tc.status, code, stderr)
			}

			if got := decodeHistoryList(t, stdout); !equalStrings(operations(got), tc.want) {
				t.Errorf("history --status %s listed %v, want %v", tc.status, operations(got), tc.want)
			}
		})
	}
}

func TestHistoryRejectsAnUnparseableStatus(t *testing.T) {
	isolateHistory(t)

	code, _, stderr := runHistory(t, "--status", "server-error")
	if code != 2 {
		t.Fatalf("history --status server-error = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "server-error") {
		t.Errorf("error message %q does not name the bad value", msg)
	}
}

func TestHistoryFiltersBySource(t *testing.T) {
	// `history replay` writes to the same store as `call`; --source is how a
	// caller separates what it issued from what it re-issued.
	isolateHistory(t)
	seedHistory(t,
		seedEntry(corpus.SourceCall, 3*time.Minute, "interactive", "GET", "https://api.test/a", 200),
		seedEntry(corpus.SourceReplay, time.Minute, "again", "GET", "https://api.test/a", 200),
	)

	cases := []struct {
		source string
		want   []string
	}{
		{"call", []string{"interactive"}},
		{"replay", []string{"again"}},
	}

	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			code, stdout, stderr := runHistory(t, "--source", tc.source, "--output", "json")
			if code != 0 {
				t.Fatalf("history --source %s = %d, want 0; stderr: %s", tc.source, code, stderr)
			}

			if got := decodeHistoryList(t, stdout); !equalStrings(operations(got), tc.want) {
				t.Errorf("history --source %s listed %v, want %v", tc.source, operations(got), tc.want)
			}
		})
	}
}

func TestHistoryRejectsAnUnknownSource(t *testing.T) {
	isolateHistory(t)

	code, _, stderr := runHistory(t, "--source", "twin")
	if code != 2 {
		t.Fatalf("history --source twin = %d, want 2; stderr: %s", code, stderr)
	}

	var alternatives struct {
		Error struct {
			Alternatives []string `json:"valid_alternatives"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &alternatives); err != nil {
		t.Fatalf("decoding stderr %q: %v", stderr, err)
	}
	if len(alternatives.Error.Alternatives) != 2 {
		t.Errorf("error alternatives = %v, want both sources", alternatives.Error.Alternatives)
	}
}

func TestHistoryShowPrintsTheWholeRedactedEntry(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session="+sessionCanary)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"42","name":"Rex"}`)
	})

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	code, stdout, stderr := runHistory(t, "show", "1", "--output", "json")
	if code != 0 {
		t.Fatalf("history show 1 = %d, want 0; stderr: %s", code, stderr)
	}
	// The canary that runHistory checks is the *request* credential; this is the
	// one the server sent back, and history is written redacted (§5a).
	if strings.Contains(stdout, sessionCanary) {
		t.Fatalf("history show printed a response credential:\n%s", stdout)
	}

	got := decodeHistoryShow(t, stdout)
	if got.Index != 1 {
		t.Errorf("index = %d, want 1", got.Index)
	}
	if got.Entry.Method != "GET" {
		t.Errorf("entry.method = %q, want GET", got.Entry.Method)
	}
	if want := "Bearer <redacted:env:TALARIA_AUTH_BEARER>"; got.Entry.Request.Headers["Authorization"] != want {
		t.Errorf("entry.request.headers[Authorization] = %q, want %q",
			got.Entry.Request.Headers["Authorization"], want)
	}
	if got.Entry.Response == nil {
		t.Fatalf("entry.response is absent:\n%s", stdout)
	}
	if got.Entry.Response.Status != http.StatusOK {
		t.Errorf("entry.response.status = %d, want 200", got.Entry.Response.Status)
	}
	if got.Entry.Response.Body == nil || !strings.Contains(got.Entry.Response.Body.Data, "Rex") {
		t.Errorf("entry.response.body does not carry what the server sent:\n%s", stdout)
	}
}

func TestHistoryShowRejectsAnOutOfRangeIndex(t *testing.T) {
	isolateHistory(t)
	seedHistory(t,
		seedEntry(corpus.SourceCall, 3*time.Minute, "a", "GET", "https://api.test/a", 200),
		seedEntry(corpus.SourceCall, 2*time.Minute, "b", "GET", "https://api.test/b", 200),
		seedEntry(corpus.SourceCall, time.Minute, "c", "GET", "https://api.test/c", 200),
	)

	code, _, stderr := runHistory(t, "show", "9")
	if code != 2 {
		t.Fatalf("history show 9 = %d, want 2; stderr: %s", code, stderr)
	}

	msg := decodeErr(t, stderr).Error.Message
	if !strings.Contains(msg, "9") {
		t.Errorf("error message %q does not name the index asked for", msg)
	}
	if !strings.Contains(msg, "1-3") {
		t.Errorf("error message %q does not state the valid range 1-3", msg)
	}
}

func TestHistoryShowOnAnEmptyStoreSaysSo(t *testing.T) {
	isolateHistory(t)

	code, _, stderr := runHistory(t, "show", "1")
	if code != 2 {
		t.Fatalf("history show on an empty store = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "empty") {
		t.Errorf("error message %q does not say the history is empty", msg)
	}
}
func TestHistoryDisabledInTheProfileRecordsNothing(t *testing.T) {
	path := isolateHistory(t)
	writeRedactConfig(t, "profiles:\n  quiet:\n    history:\n      enabled: false\n")
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42", "--profile", "quiet",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call --profile quiet = %d, want 0; stderr: %s", code, stderr)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("history.enabled: false still created %s", path)
	}
	if entries := readHistory(t); len(entries) != 0 {
		t.Errorf("history.enabled: false recorded %d entries", len(entries))
	}
}

// binaryBody is every byte value four times over: what a protobuf or an image
// upload is made of, and what a JSON string cannot hold.
func binaryBody() []byte {
	out := make([]byte, 0, 4*256)
	for i := 0; i < 4; i++ {
		for b := 0; b < 256; b++ {
			out = append(out, byte(b))
		}
	}

	return out
}

func TestHistoryShowDoesNotPrintABinaryBodyAsText(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(binaryBody())
	entry := corpus.Entry{
		Source: corpus.SourceCall,
		Method: "POST",
		URL:    "https://api.example.com/pets/42/photo",
		Request: corpus.EntryRequest{
			Body: &corpus.Body{
				ContentType: "application/octet-stream",
				Data:        encoded,
				Encoding:    corpus.EncodingBase64,
			},
		},
	}

	var printed string
	for _, row := range historyShowPayload(1, entry).Table.Rows {
		printed += strings.Join(row, " ") + "\n"
	}

	if strings.Contains(printed, encoded) {
		t.Errorf("history show printed the base64 payload as if it were the body:\n%s", printed)
	}
	if !strings.Contains(printed, "1024") || !strings.Contains(printed, corpus.EncodingBase64) {
		t.Errorf("history show does not say the body is %d base64-encoded bytes:\n%s", len(binaryBody()), printed)
	}
}

// The pretty view lists the request's headers as the whole of what was sent, so
// a set the store had to cap must say so there. `--output json` carries
// headers_truncated on the entry itself; this row is the human's half of it.
func TestHistoryShowSaysWhenTheStoredHeadersWereCapped(t *testing.T) {
	entry := corpus.Entry{
		Source: corpus.SourceCall,
		Method: "GET",
		URL:    "https://api.example.com/pets/42",
		Request: corpus.EntryRequest{
			Headers:          map[string]string{"Accept": "application/json"},
			HeadersTruncated: true,
		},
	}

	var printed string
	for _, row := range historyShowPayload(1, entry).Table.Rows {
		printed += strings.Join(row, " ") + "\n"
	}

	if !strings.Contains(printed, "truncated") {
		t.Errorf("history show reads as the whole header set though the entry says it is capped:\n%s", printed)
	}
}
