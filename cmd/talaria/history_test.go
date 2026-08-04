package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/replay"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
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
		"--base-url", srv.URL, allowHost(t, srv), "--output", "json")
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
		"--base-url", srv.URL, allowHost(t, srv), "--output", "json")
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

	code, stdout, stderr := runHistory(t, "replay", "testdata/call.yaml", "1",
		"--base-url", srv.URL, "--allow-host="+callHost(t, srv), "--output", "json")
	if code != 0 {
		t.Fatalf("history replay 1 = %d, want 0; stderr: %s", code, stderr)
	}

	// Replay renders the same envelope a call does, so an agent parses one shape.
	replayed := decodeCall(t, stdout)
	if replayed.Response == nil || replayed.Response.Status != http.StatusOK {
		t.Fatalf("replay produced no successful response:\n%s", stdout)
	}
	// Including the validation block: a replay is re-bound through the spec, so
	// it has the same contract to check against a call does. It used to be
	// omitted, because replay read a stored request and never saw a spec.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decoding the replay envelope: %v", err)
	}
	if _, ok := envelope["validation"]; !ok {
		t.Errorf("the replay envelope carries no validation block:\n%s", stdout)
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
		"--base-url", srv.URL, allowHost(t, srv), "--output", "json")
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

	code, _, stderr = runHistory(t, "replay", "testdata/call.yaml", public.ID,
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("replay of the getPublic id = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Path; got != "/public" {
		t.Fatalf("the first replay sent %q, want /public", got)
	}

	code, _, stderr = runHistory(t, "replay", "testdata/call.yaml", pet.ID,
		"--base-url", srv.URL, "--allow-host="+callHost(t, srv), "--output", "json")
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

	code, _, stderr = runHistory(t, "replay", "testdata/call.yaml", "1",
		"--base-url", srv.URL, "--output", "json")
	if code != 2 {
		t.Fatalf("history replay of a POST = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "--allow-mutations") {
		t.Errorf("error message %q does not name the flag that permits it", msg)
	}
	if entries := readHistory(t); len(entries) != 1 {
		t.Errorf("a gated replay wrote %d entries, want the original 1", len(entries))
	}

	code, _, stderr = runHistory(t, "replay", "testdata/call.yaml", "1", "--allow-mutations",
		"--base-url", srv.URL, "--output", "json")
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

// replayFor builds a replay against the call fixture, which is the spec every
// other test in this file records entries under.
func replayFor(t *testing.T, entry corpus.Entry) replay.Inputs {
	t.Helper()

	doc, err := spec.LoadFile(filepath.Join("testdata", "call.yaml"))
	if err != nil {
		t.Fatalf("LoadFile(call.yaml): %v", err)
	}

	return replay.Inputs{
		Entry:  entry,
		Doc:    doc,
		Index:  operation.NewIndexFor(doc),
		Stderr: io.Discard,
	}
}

// storedEntry is one recorded call of getPet, as the fixture's own server would
// have produced it. Tests corrupt one field at a time from here.
func storedEntry() corpus.Entry {
	return corpus.Entry{
		Source:      corpus.SourceCall,
		OperationID: "getPet",
		Method:      "GET",
		URL:         "https://api.invalid/v1/pets/42",
	}
}

// findPair returns the value of the first pair named name, case-insensitively.
func findPair(t *testing.T, pairs []request.Pair, name string) request.Value {
	t.Helper()

	for _, p := range pairs {
		if strings.EqualFold(p.Name, name) {
			return p.Value
		}
	}

	t.Fatalf("no pair named %q in %v", name, pairs)

	return request.Value{}
}

// replayErr asserts the replay failed with exit 2 — that entry, not the process
// — and returns the error so the caller can assert what it names.
func replayErr(t *testing.T, in replay.Inputs) error {
	t.Helper()

	req, err := replay.Build(in)
	if err == nil {
		t.Fatalf("replay succeeded, want a usage error; got %+v", req)
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Fatalf("replay error = %v (code %d), want usage (%d)", err, code, clierr.CodeUsage)
	}

	return err
}

// A stored entry is a file an agent (or anything else with write access to the
// history) can edit, so the scheme has to be checked again on the way back out
// rather than trusted because it was checked on the way in.
func TestReplayRejectsARecordedURLWhoseSchemeIsNotHTTP(t *testing.T) {
	for _, raw := range []string{"gopher://127.0.0.1:1234/x", "file:///etc/passwd"} {
		t.Run(raw, func(t *testing.T) {
			entry := storedEntry()
			entry.URL = raw

			err := replayErr(t, replayFor(t, entry))
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
	entry := storedEntry()
	entry.URL = "http://" + user + ":" + password + "@127.0.0.1:8898/pets/42"

	err := replayErr(t, replayFor(t, entry))
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
	entry := storedEntry()
	entry.OperationID, entry.Method = "createPet", "POST"
	entry.URL = "https://api.invalid/v1/pets"
	entry.Request.Body = &corpus.Body{
		ContentType: "application/octet-stream",
		Data:        base64.StdEncoding.EncodeToString(data),
		Encoding:    corpus.EncodingBase64,
	}

	req, err := replay.Build(replayFor(t, entry))
	if err != nil {
		t.Fatalf("replay: %v", err)
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
	entry := storedEntry()
	entry.OperationID, entry.Method = "createPet", "POST"
	entry.URL = "https://api.invalid/v1/pets"
	entry.Request.Body = &corpus.Body{Data: "AAAA", Encoding: "zstd+base64"}

	err := replayErr(t, replayFor(t, entry))
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

// The re-derivation, in one assertion: the entry says which operation and with
// which parameters, and the spec says where that goes. The stored URL's own
// host and prefix are read for neither.
func TestReplayRebuildsTheRequestFromTheSpec(t *testing.T) {
	entry := storedEntry()
	entry.URL = "https://api.invalid/some/other/prefix/pets/42?verbose=true"

	req, err := replay.Build(replayFor(t, entry))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if req.BaseURL != "https://api.invalid/v1" {
		t.Errorf("BaseURL = %q, want the spec's server https://api.invalid/v1", req.BaseURL)
	}
	if req.Path != "/pets/42" {
		t.Errorf("Path = %q, want /pets/42 rebound from the template", req.Path)
	}
	if got := findPair(t, req.Query, "verbose"); got.String() != "true" {
		t.Errorf("the recorded query parameter came back as %q, want true", got)
	}
	// The credential comes from config.Resolve against the current environment,
	// never from the entry — which holds no value to come from.
	if got := findPair(t, req.Headers, "Authorization"); !got.IsSecret() {
		t.Errorf("Authorization = %v, want a credential reference", got)
	}
}

// Nothing in a stored entry is resolved (DESIGN.md:408). A header edited to
// name a variable of the writer's choosing is dropped, not read: resolving it
// would make talaria a "read $ANY_VAR and send it" primitive driven by a file,
// and passing it through as a literal would put `<redacted:env:…>` on the wire.
func TestReplayDropsAStoredCredentialReferenceEntirely(t *testing.T) {
	const stolen = "AWS_SECRET_ACCESS_KEY"
	t.Setenv(stolen, "the-value-nothing-may-read")

	entry := storedEntry()
	entry.Request.Headers = map[string]string{
		"Authorization": "Bearer <redacted:env:" + stolen + ">",
		"X-Steal":       "<redacted:env:" + stolen + ">",
		"X-Trace":       "<redacted>",
		"X-Plain":       "kept",
	}

	req, err := replay.Build(replayFor(t, entry))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	for _, p := range req.Headers {
		if p.Value.IsSecret() && p.Value.Ref().Name == stolen {
			t.Fatalf("header %q resolves %s: a stored entry named a variable and was believed", p.Name, stolen)
		}
		if !p.Value.IsSecret() && strings.Contains(p.Value.Reveal(), "<redacted") {
			t.Errorf("header %q carries the redaction text %q on the wire", p.Name, p.Value.Reveal())
		}
	}
	if got := findPair(t, req.Headers, "X-Plain"); got.String() != "kept" {
		t.Errorf("X-Plain = %q, want the recorded literal", got)
	}
	for _, name := range []string{"X-Steal", "X-Trace"} {
		for _, p := range req.Headers {
			if strings.EqualFold(p.Name, name) {
				t.Errorf("header %q survived, want it dropped: %v", name, p.Value)
			}
		}
	}
	// Authorization is back, but as the reference config.Resolve produced from
	// the *current* environment — the spec's bearerAuth scheme, not the entry's
	// name.
	if got := findPair(t, req.Headers, "Authorization"); !got.IsSecret() || got.Ref().Name != config.EnvBearer {
		t.Errorf("Authorization = %v, want a reference to %s", got, config.EnvBearer)
	}
}

// deletedReplaySymbol is the allowlist that narrowed *which* variable a stored
// entry could name while leaving the fact that it could name one at all
// untouched. It is spelled at runtime so this file does not itself contain the
// token it is asserting the absence of.
var deletedReplaySymbol = "replayable" + "Env"

// The finding requires deletion, not narrowing: after this task nothing in a
// stored entry is resolved at all, so there is no allowlist left to hold.
func TestNothingInTheTreeResolvesAStoredVariableName(t *testing.T) {
	found, err := filepath.Glob(filepath.Join("..", "..", "*", "*", "*.go"))
	if err != nil {
		t.Fatalf("globbing the tree: %v", err)
	}
	more, err := filepath.Glob(filepath.Join("..", "..", "*", "*.go"))
	if err != nil {
		t.Fatalf("globbing the tree: %v", err)
	}

	for _, path := range append(found, more...) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if strings.Contains(string(data), deletedReplaySymbol) {
			t.Errorf("%s still names %s; the finding requires deletion, not narrowing",
				path, deletedReplaySymbol)
		}
	}
}

// A stored body holding a redaction is one talaria itself hollowed out. Sending
// it would put the literal text `<redacted>` where a client_secret stood — the
// review's reproduction, which arrived at the server verbatim with nothing on
// stderr.
func TestReplayRefusesABodyHoldingARedaction(t *testing.T) {
	entry := storedEntry()
	entry.OperationID, entry.Method = "createPet", "POST"
	entry.URL = "https://api.invalid/v1/pets"
	entry.Request.Body = &corpus.Body{
		ContentType: "application/json",
		Data:        `{"grant":"x","refresh_token":"` + secret.Placeholder + `"}`,
	}

	err := replayErr(t, replayFor(t, entry))
	if !strings.Contains(err.Error(), "redacted") {
		t.Errorf("error %v does not say the body holds a redacted value", err)
	}
}

// A hostile or merely stale entry fails *that entry* with exit 2 — never the
// process, never a panic. Each case is a field an editor of the file controls.
func TestReplayRefusesAnEntryThatIsNotAValidRequest(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*corpus.Entry)
		names string
	}{
		{"no operationId", func(e *corpus.Entry) { e.OperationID = "" }, "operationId"},
		{"an operation the spec no longer has",
			func(e *corpus.Entry) { e.OperationID = "getRetiredPet" }, "getRetiredPet"},
		{"a method the spec disagrees with", func(e *corpus.Entry) { e.Method = "DELETE" }, "DELETE"},
		{"too few path segments",
			func(e *corpus.Entry) { e.URL = "https://api.invalid/pets" }, "segments"},
		{"a path that is not this operation's",
			func(e *corpus.Entry) { e.URL = "https://api.invalid/v1/steal/42" }, "getPet"},
		// Refused at the parse, which is the earliest point that can see it: a
		// URL Go will not parse never reaches the template match below.
		{"a segment that will not percent-decode",
			func(e *corpus.Entry) { e.URL = "https://api.invalid/v1/pets/%zz" }, "cannot be parsed"},
		{"a URL that will not parse",
			func(e *corpus.Entry) { e.URL = "https://api.invalid/v1/pets/4 2\x7f%" }, "cannot be parsed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := storedEntry()
			tc.edit(&entry)

			err := replayErr(t, replayFor(t, entry))
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("error %v does not name %q", err, tc.names)
			}
		})
	}
}

// The stored host is compared with where this invocation would send, and a
// disagreement fails the entry rather than being silently redirected
// (DESIGN.md:407, the replay table's Target host row).
func TestReplayRefusesAStoredHostOutsideTheAllowedSet(t *testing.T) {
	entry := storedEntry()
	entry.URL = "http://127.0.0.1:19950/pets/42"

	err := replayErr(t, replayFor(t, entry))
	if !strings.Contains(err.Error(), "127.0.0.1:19950") {
		t.Errorf("error %v does not name the stored host", err)
	}
	if !strings.Contains(err.Error(), "--allow-host") {
		t.Errorf("error %v does not name the flag that would permit it", err)
	}

	// Allowed explicitly, it replays — and still goes to the spec's server.
	r := replayFor(t, entry)
	r.AllowHosts = []string{"127.0.0.1:19950"}
	req, err := replay.Build(r)
	if err != nil {
		t.Fatalf("replay with the host allowed: %v", err)
	}
	if req.BaseURL != "https://api.invalid/v1" {
		t.Errorf("BaseURL = %q, want the spec's server: an allowed host is not a target", req.BaseURL)
	}
}

// --base-url is accepted and acted on. It used to be inherited, ignored and
// silently discarded, which is worse than erroring: the caller reads the
// retarget as having happened.
func TestReplayHonoursBaseURL(t *testing.T) {
	r := replayFor(t, storedEntry())
	r.BaseURL = "https://elsewhere.example"

	req, err := replay.Build(r)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if req.BaseURL != "https://elsewhere.example" {
		t.Errorf("BaseURL = %q, want the flag's value", req.BaseURL)
	}
	// And an off-spec target withholds the credential, exactly as `call` does.
	if len(req.Withheld) != 1 {
		t.Errorf("credentials_withheld = %+v, want the bearer scheme withheld", req.Withheld)
	}
}

// End to end: the review's reproduction through the real command. The refusal
// has to happen before curl runs, so the server sees nothing at all.
func TestHistoryReplayNeverSendsAStoredVariablesValue(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	entry := seedEntry(corpus.SourceCall, time.Minute, "exfil", "GET", srv.URL+"/pets/42", 0)
	entry.OperationID = "getPet"
	entry.Request.Headers = map[string]string{"X-Steal": "<redacted:env:MY_UNRELATED_SECRET>"}
	seedHistory(t, entry)

	t.Setenv("MY_UNRELATED_SECRET", callCanary)

	code, _, stderr := runHistory(t, "replay", "testdata/call.yaml", "1",
		"--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("history replay = %d, want 0; stderr: %s", code, stderr)
	}

	rec := srv.received()
	if got := rec.Header.Get("X-Steal"); got != "" {
		t.Errorf("the server received X-Steal: %q", got)
	}
	for name, values := range rec.Header {
		for _, v := range values {
			if strings.Contains(v, callCanary) {
				t.Errorf("the stored variable's value reached the server in %s: %q", name, v)
			}
		}
	}
}

// callHost is a test server's authority, for --allow-host.
func callHost(t *testing.T, srv *callServer) string {
	t.Helper()

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing %q: %v", srv.URL, err)
	}

	return parsed.Host
}

// A stored content_type is the one field replay hands straight to the request
// without the binder ever seeing it (replay.body), and its original source is a
// key in the spec's content: map. Both surfaces it can reach are closed: the
// config document curl reads, and the command --dry-run prints.
func TestReplayCannotSmuggleAHeaderThroughAStoredContentType(t *testing.T) {
	// Set so the document gets as far as the body. Unset, BuildConfig refuses
	// with exit 5 for the missing credential and the content type is never
	// reached — a refusal that would pass this test without testing anything.
	t.Setenv("TALARIA_AUTH_BEARER", "token")
	t.Setenv("TALARIA_AUTH_PETKEY", "key")

	entry := storedEntry()
	entry.OperationID, entry.Method = "createPet", "POST"
	entry.URL = "https://api.invalid/v1/pets"
	entry.Request.Body = &corpus.Body{
		ContentType: "application/json\r\nX-Injected: pwned",
		Data:        `{"name":"Rex"}`,
	}

	req, err := replay.Build(replayFor(t, entry))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	config, _, cleanup, err := curl.BuildConfig(req, curl.Capture{})
	t.Cleanup(cleanup)
	if err == nil {
		t.Fatalf("BuildConfig succeeded, want a refusal; document:\n%s", config)
	}
	if code := clierr.From(err).Code; code != clierr.CodeUsage {
		t.Errorf("exit code = %d, want %d", code, clierr.CodeUsage)
	}
	if config != nil {
		t.Errorf("a document was built despite the refusal:\n%s", config)
	}

	// --dry-run never builds a document, so the refusal above would not be
	// reached at all on that path.
	if got := curl.Render(req); strings.Contains(got, "X-Injected") {
		t.Errorf("Render() = %q, want no smuggled header in the emitted command", got)
	}
}
