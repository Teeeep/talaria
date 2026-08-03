package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
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
		if err := store.Append(entry); err != nil {
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
	// `run` (Task 30) writes to the same store; without --source a smoke run over
	// a large spec buries the interactive history underneath it.
	isolateHistory(t)
	seedHistory(t,
		seedEntry(corpus.SourceCall, 3*time.Minute, "interactive", "GET", "https://api.test/a", 200),
		seedEntry(corpus.SourceRun, 2*time.Minute, "smoke", "GET", "https://api.test/b", 200),
		seedEntry(corpus.SourceReplay, time.Minute, "again", "GET", "https://api.test/a", 200),
	)

	cases := []struct {
		source string
		want   []string
	}{
		{"call", []string{"interactive"}},
		{"run", []string{"smoke"}},
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
	if len(alternatives.Error.Alternatives) != 3 {
		t.Errorf("error alternatives = %v, want the three sources", alternatives.Error.Alternatives)
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
		"--base-url", srv.URL, "--output", "json")
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

func TestHistoryReplayReissuesTheCallAndRecordsANewEntry(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42", "--query", "verbose=true",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	code, stdout, stderr := runHistory(t, "replay", "1", "--output", "json")
	if code != 0 {
		t.Fatalf("history replay 1 = %d, want 0; stderr: %s", code, stderr)
	}

	// Replay renders the same envelope a call does, so an agent parses one shape.
	replayed := decodeCall(t, stdout)
	if replayed.Response == nil || replayed.Response.Status != http.StatusOK {
		t.Fatalf("replay produced no successful response:\n%s", stdout)
	}

	rec := srv.received()
	if rec.Path != "/pets/42" {
		t.Errorf("server saw path %q on replay, want /pets/42", rec.Path)
	}
	if rec.Query != "verbose=true" {
		t.Errorf("server saw query %q on replay, want verbose=true", rec.Query)
	}
	// The credential is re-resolved from the environment: it was never written to
	// history, so this is the only place it could have come from (§5a).
	if got, want := rec.Header.Get("Authorization"), "Bearer "+callCanary; got != want {
		t.Errorf("server saw Authorization %q on replay, want %q", got, want)
	}

	entries := readHistory(t)
	if len(entries) != 2 {
		t.Fatalf("history holds %d entries after a replay, want 2", len(entries))
	}
	if entries[0].Source != corpus.SourceCall {
		t.Errorf("the original entry became %q; replay must leave it intact", entries[0].Source)
	}
	if entries[1].Source != corpus.SourceReplay {
		t.Errorf("the new entry's source = %q, want %q", entries[1].Source, corpus.SourceReplay)
	}
	if entries[1].OperationID != "getPet" {
		t.Errorf("the new entry's operation_id = %q, want getPet", entries[1].OperationID)
	}
}

// Two replays in a row is ordinary agent behaviour, and it is where the
// positional index breaks: replay appends, so every index printed by the
// listing the agent is reading has already shifted by the time it issues the
// second command. The ids are the same two handles before and after.
func TestReplayingTwoIDsInARowReissuesTwoDifferentEntries(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call getPet = %d, want 0; stderr: %s", code, stderr)
	}
	code, _, stderr = runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call getPublic = %d, want 0; stderr: %s", code, stderr)
	}

	code, stdout, stderr := runHistory(t, "--output", "json")
	if code != 0 {
		t.Fatalf("history = %d, want 0; stderr: %s", code, stderr)
	}
	listed := decodeHistoryList(t, stdout)
	if len(listed.Entries) != 2 {
		t.Fatalf("history listed %d entries, want 2:\n%s", len(listed.Entries), stdout)
	}
	public, pet := listed.Entries[0], listed.Entries[1]
	if public.OperationID != "getPublic" || pet.OperationID != "getPet" {
		t.Fatalf("history listed %q then %q, want getPublic then getPet", public.OperationID, pet.OperationID)
	}

	code, _, stderr = runHistory(t, "replay", public.ID, "--output", "json")
	if code != 0 {
		t.Fatalf("replay of the getPublic id = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Path; got != "/public" {
		t.Fatalf("the first replay sent %q, want /public", got)
	}

	code, _, stderr = runHistory(t, "replay", pet.ID, "--output", "json")
	if code != 0 {
		t.Fatalf("replay of the getPet id = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Path; got != "/pets/42" {
		t.Errorf("the second replay sent %q, want /pets/42: the handle resolved to the wrong entry", got)
	}

	entries := readHistory(t)
	if len(entries) != 4 {
		t.Fatalf("history holds %d entries after two replays, want 4", len(entries))
	}
	if entries[0].ID != pet.ID || entries[1].ID != public.ID {
		t.Errorf("the original ids became %q and %q; replaying must not rewrite them",
			entries[0].ID, entries[1].ID)
	}
}

func TestHistoryReplayStillRequiresAllowMutations(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations", "--body", `{"name":"Rex"}`,
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call createPet = %d, want 0; stderr: %s", code, stderr)
	}

	code, _, stderr = runHistory(t, "replay", "1", "--output", "json")
	if code != 2 {
		t.Fatalf("history replay of a POST = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "--allow-mutations") {
		t.Errorf("error message %q does not name the flag that permits it", msg)
	}
	if entries := readHistory(t); len(entries) != 1 {
		t.Errorf("a gated replay wrote %d entries, want the original 1", len(entries))
	}

	code, _, stderr = runHistory(t, "replay", "1", "--allow-mutations", "--output", "json")
	if code != 0 {
		t.Fatalf("history replay --allow-mutations = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Body; got != `{"name":"Rex"}` {
		t.Errorf("server saw body %q on replay, want the recorded one", got)
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

// A stored entry is a file an agent (or anything else with write access to the
// history) can edit, so the scheme has to be checked again on the way back out
// rather than trusted because it was checked on the way in.
func TestReplayRejectsARecordedURLWhoseSchemeIsNotHTTP(t *testing.T) {
	for _, raw := range []string{"gopher://127.0.0.1:1234/x", "file:///etc/passwd"} {
		t.Run(raw, func(t *testing.T) {
			entry := corpus.Entry{Source: corpus.SourceCall, Method: "GET", URL: raw}

			_, err := replayRequest(io.Discard, entry, nil)
			if err == nil {
				t.Fatalf("replayRequest(%q) succeeded, want a usage error", raw)
			}
			if code := clierr.From(err).Code; code != clierr.CodeUsage {
				t.Fatalf("replayRequest(%q) error = %v (code %d), want usage (%d)",
					raw, err, code, clierr.CodeUsage)
			}
			scheme, _, _ := strings.Cut(raw, ":")
			if !strings.Contains(err.Error(), scheme) {
				t.Errorf("error %v does not name the offending scheme %q", err, scheme)
			}
		})
	}
}

// Userinfo is the credential position a URL carries in cleartext, so a stored
// entry someone edited to hold one must be refused on the way back out for the
// same reason its scheme is — and the refusal must not quote what it refused.
func TestReplayRejectsARecordedURLCarryingCredentials(t *testing.T) {
	const user, password = "admin", "s3cr3t"
	entry := corpus.Entry{
		Source: corpus.SourceCall,
		Method: "GET",
		URL:    "http://" + user + ":" + password + "@127.0.0.1:8898/pets",
	}

	_, err := replayRequest(io.Discard, entry, nil)
	if err == nil {
		t.Fatal("replayRequest accepted a recorded URL with userinfo, want a usage error")
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("replayRequest error = %v (code %d), want usage (%d)", err, code, clierr.CodeUsage)
	}
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), user) {
		t.Errorf("the refusal echoes the userinfo it refused: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:8898") {
		t.Errorf("error %v does not name the host it refused", err)
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

// A replay has to put the original bytes back on the wire. The entry stores a
// non-UTF-8 body base64'd (internal/corpus), so replay has to decode it —
// reading Data as text would send 2048 replacement-character bytes in place of
// the 1024 that were sent.
func TestReplaySendsTheOriginalBytesOfABinaryBody(t *testing.T) {
	data := binaryBody()
	entry := corpus.Entry{
		Source: corpus.SourceCall,
		Method: "POST",
		URL:    "https://api.example.com/pets/42/photo",
		Request: corpus.EntryRequest{
			Body: &corpus.Body{
				ContentType: "application/octet-stream",
				Data:        base64.StdEncoding.EncodeToString(data),
				Encoding:    corpus.EncodingBase64,
			},
		},
	}

	req, err := replayRequest(io.Discard, entry, nil)
	if err != nil {
		t.Fatalf("replayRequest: %v", err)
	}
	if req.Body == nil {
		t.Fatal("the replayed request has no body")
	}
	if !bytes.Equal(req.Body.Data, data) {
		t.Errorf("the replay carries %d bytes, want the recorded %d", len(req.Body.Data), len(data))
	}
}

// An entry written by a newer talaria may use an encoding this build cannot
// read. Replaying its Data as literal text would send something the original
// call did not, so it is refused the way a truncated body already is.
func TestReplayRefusesABodyEncodingItDoesNotKnow(t *testing.T) {
	entry := corpus.Entry{
		Source:  corpus.SourceCall,
		Method:  "POST",
		URL:     "https://api.example.com/pets",
		Request: corpus.EntryRequest{Body: &corpus.Body{Data: "AAAA", Encoding: "zstd+base64"}},
	}

	_, err := replayRequest(io.Discard, entry, nil)
	if err == nil {
		t.Fatal("replayRequest accepted an unknown body encoding, want a usage error")
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("error = %v (code %d), want usage (%d)", err, code, clierr.CodeUsage)
	}
	if !strings.Contains(err.Error(), "zstd+base64") {
		t.Errorf("error %v does not name the encoding it refused", err)
	}
}

// `history show` prints the entry for a human to read. A base64 body is not
// text, and printing its encoded form as if it were the body would be a lie
// about what was sent.
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

func TestReplayAcceptsARecordedHTTPURL(t *testing.T) {
	entry := corpus.Entry{Source: corpus.SourceCall, Method: "GET", URL: "https://api.example.com/pets/42"}

	req, err := replayRequest(io.Discard, entry, nil)
	if err != nil {
		t.Fatalf("replayRequest: %v", err)
	}
	if req.BaseURL != "https://api.example.com" {
		t.Errorf("BaseURL = %q, want https://api.example.com", req.BaseURL)
	}
}

// The store is a plain file, so `<redacted:env:NAME>` in it is an instruction
// an attacker can write. Resolving any name it likes would make replay a
// "read $ANY_VAR and send it to $ANY_URL" primitive, run from the one process
// the deployment trusts with credentials — so the name has to be inside the
// namespace the firewall is scoped to (§5a).
func TestReplayRefusesAnEnvVarOutsideTheAuthNamespace(t *testing.T) {
	const stolen = "AWS_SECRET_ACCESS_KEY"
	entry := corpus.Entry{
		Source:  corpus.SourceCall,
		Method:  "GET",
		URL:     "http://127.0.0.1:19950/exfil",
		Request: corpus.EntryRequest{Headers: map[string]string{"X-Steal": "<redacted:env:" + stolen + ">"}},
	}

	_, err := replayRequest(io.Discard, entry, nil)
	if err == nil {
		t.Fatal("replayRequest resolved an env var outside the auth namespace, want a usage error")
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("error = %v (code %d), want usage (%d)", err, code, clierr.CodeUsage)
	}
	if !strings.Contains(err.Error(), stolen) {
		t.Errorf("error %v does not name the variable it refused", err)
	}
	if !strings.Contains(err.Error(), "X-Steal") {
		t.Errorf("error %v does not name the header the variable stood in", err)
	}
}

// The query string is rebuilt by its own code path, and a credential in a query
// parameter is the position DESIGN.md already warns about — so the refusal has
// to hold there too.
func TestReplayRefusesAnEnvVarOutsideTheAuthNamespaceInTheQuery(t *testing.T) {
	const stolen = "GITHUB_TOKEN"
	entry := corpus.Entry{
		Source: corpus.SourceCall,
		Method: "GET",
		URL:    "http://127.0.0.1:19950/exfil?leak=%3Credacted%3Aenv%3A" + stolen + "%3E",
	}

	_, err := replayRequest(io.Discard, entry, nil)
	if err == nil {
		t.Fatal("replayRequest resolved an env var outside the auth namespace, want a usage error")
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("error = %v (code %d), want usage (%d)", err, code, clierr.CodeUsage)
	}
	if !strings.Contains(err.Error(), stolen) {
		t.Errorf("error %v does not name the variable it refused", err)
	}
}

// The three TALARIA_AUTH_* forms are what `call` records, so every one of them
// has to survive the round trip: a replay that refuses its own recording would
// be worse than the hole it closes.
func TestReplayResolvesTheAuthNamespace(t *testing.T) {
	for _, name := range []string{config.EnvBearer, config.EnvBasic, config.EnvAPIKeyPrefix + "PETKEY"} {
		t.Run(name, func(t *testing.T) {
			entry := corpus.Entry{
				Source:  corpus.SourceCall,
				Method:  "GET",
				URL:     "https://api.example.com/pets/42",
				Request: corpus.EntryRequest{Headers: map[string]string{"Authorization": "<redacted:env:" + name + ">"}},
			}

			req, err := replayRequest(io.Discard, entry, nil)
			if err != nil {
				t.Fatalf("replayRequest: %v", err)
			}
			if len(req.Headers) != 1 {
				t.Fatalf("the replay carries %d headers, want the recorded 1", len(req.Headers))
			}
			if got := req.Headers[0].Value.Ref().Name; got != name {
				t.Errorf("the replayed header resolves %q, want %q", got, name)
			}
		})
	}
}

// A profile's `auth:` map is the other half of the namespace: it is how one
// machine talks to staging and production at once, and a call made under it
// records the variable *it* names, not the convention's.
func TestReplayResolvesAVariableTheProfileNames(t *testing.T) {
	prof := &config.Profile{Name: "staging", Auth: map[string]string{"bearerAuth": "${STAGING_TOKEN}"}}
	entry := corpus.Entry{
		Source:  corpus.SourceCall,
		Method:  "GET",
		URL:     "https://api.example.com/pets/42",
		Request: corpus.EntryRequest{Headers: map[string]string{"Authorization": "Bearer <redacted:env:STAGING_TOKEN>"}},
	}

	req, err := replayRequest(io.Discard, entry, prof)
	if err != nil {
		t.Fatalf("replayRequest under profile staging: %v", err)
	}
	if len(req.Headers) != 1 {
		t.Fatalf("the replay carries %d headers, want the recorded 1", len(req.Headers))
	}
	if got := req.Headers[0].Value.Ref().Name; got != "STAGING_TOKEN" {
		t.Errorf("the replayed header resolves %q, want STAGING_TOKEN", got)
	}
	// Without the profile the same entry is outside the namespace: the profile is
	// what admits the name, so dropping it must not leave the door open.
	if _, err := replayRequest(io.Discard, entry, nil); err == nil {
		t.Error("replayRequest resolved STAGING_TOKEN with no profile selected, want a usage error")
	}
}

// End to end: the reproduction from the review, through the real command. The
// refusal has to happen before curl runs, so the server sees nothing at all.
func TestHistoryReplayDoesNotSendAnEnvVarOutsideTheAuthNamespace(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	entry := seedEntry(corpus.SourceCall, time.Minute, "exfil", "GET", srv.URL+"/exfil", 0)
	entry.Request.Headers = map[string]string{"X-Steal": "<redacted:env:MY_UNRELATED_SECRET>"}
	seedHistory(t, entry)

	t.Setenv("MY_UNRELATED_SECRET", callCanary)

	code, _, stderr := runHistory(t, "replay", "1", "--output", "json")
	if code != 2 {
		t.Fatalf("history replay of an out-of-namespace variable = %d, want 2; stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "MY_UNRELATED_SECRET") {
		t.Errorf("the refusal does not name the variable it refused: %s", stderr)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the refused replay still reached the server: %+v", rec)
	}
}
