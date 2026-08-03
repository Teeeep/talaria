package main

import (
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

// replayFlags is what every replay now needs. The positional argument names the
// entry, so the spec can only come from --spec; the target host comes from the
// flags rather than the stored URL; and the test server's host is off-spec, so
// --allow-host is what lets a credential reach it.
func replayFlags(srv *callServer) []string {
	return []string{
		"--spec", "testdata/call.yaml", "--base-url", srv.URL,
		"--allow-host", "127.0.0.1", "--output", "json",
	}
}

// runReplay runs `history replay <handle>` with the flags above plus extra.
func runReplay(t *testing.T, srv *callServer, handle string, extra ...string) (int, string, string) {
	t.Helper()

	args := append([]string{"replay", handle}, replayFlags(srv)...)

	return runHistory(t, append(args, extra...)...)
}

func TestHistoryReplayReissuesTheCallAndRecordsANewEntry(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42", "--query", "verbose=true",
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	code, stdout, stderr := runReplay(t, srv, "1")
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

// A replay is re-derived from the spec, so there is a contract to check the
// response against — and an agent that reads `call`'s validation block must be
// able to read replay's too.
func TestHistoryReplayEmitsAValidationBlock(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	code, stdout, stderr := runReplay(t, srv, "1")
	if code != 0 {
		t.Fatalf("history replay = %d, want 0; stderr: %s", code, stderr)
	}
	if decodeCall(t, stdout).Validation == nil {
		t.Errorf("replay emitted no validation block:\n%s", stdout)
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
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
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

	code, _, stderr = runReplay(t, srv, public.ID)
	if code != 0 {
		t.Fatalf("replay of the getPublic id = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Path; got != "/public" {
		t.Fatalf("the first replay sent %q, want /public", got)
	}

	code, _, stderr = runReplay(t, srv, pet.ID)
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
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call createPet = %d, want 0; stderr: %s", code, stderr)
	}

	code, _, stderr = runReplay(t, srv, "1")
	if code != 2 {
		t.Fatalf("history replay of a POST = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "--allow-mutations") {
		t.Errorf("error message %q does not name the flag that permits it", msg)
	}
	if entries := readHistory(t); len(entries) != 1 {
		t.Errorf("a gated replay wrote %d entries, want the original 1", len(entries))
	}

	code, _, stderr = runReplay(t, srv, "1", "--allow-mutations")
	if code != 0 {
		t.Fatalf("history replay --allow-mutations = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Body; got != `{"name":"Rex"}` {
		t.Errorf("server saw body %q on replay, want the recorded one", got)
	}
}

// The mutation gate is decided from the spec, not from the entry. A one-word
// edit to a JSONL line turned a DELETE replay into an ungated one, which is the
// one place here where believing the file costs a write against a live API.
func TestHistoryReplayGatesOnTheSpecsMethodNotTheStoredOne(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// Stored as GET; deleteAllPets is a DELETE in the spec.
	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "deleteAllPets", "GET", srv.URL+"/pets", 0))

	code, _, stderr := runReplay(t, srv, "1")
	if code != 2 {
		t.Fatalf("replay of a spec DELETE stored as GET = %d, want 2; stderr: %s", code, stderr)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the gated replay still reached the server: %+v", rec)
	}

	// And the inverse: the hostile file must not get to make replay *harder*
	// either, or a stored "DELETE" turns every GET into a flag-gated call.
	isolateHistory(t)
	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPublic", "DELETE", srv.URL+"/public", 0))

	code, _, stderr = runReplay(t, srv, "1")
	if code != 0 {
		t.Fatalf("replay of a spec GET stored as DELETE = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Method; got != "GET" {
		t.Errorf("the replay sent %q, want the spec's GET", got)
	}
}

// Finding 2, at the wire. A hand-edited `url` must not decide where the request
// goes: the destination is the spec and the flags, and the planted listener
// must see nothing at all.
func TestHistoryReplayIgnoresAHandEditedStoredURL(t *testing.T) {
	isolateHistory(t)
	real := newCallServer(t, jsonPet)
	planted := newCallServer(t, jsonPet)

	// Both are on 127.0.0.1, so --allow-host admits the planted host too: the
	// point is that even an *allowed* stored URL does not choose the target.
	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", planted.URL+"/pets/42", 0))

	code, _, stderr := runReplay(t, real, "1")
	if code != 0 {
		t.Fatalf("history replay = %d, want 0; stderr: %s", code, stderr)
	}

	if got := real.received().Path; got != "/pets/42" {
		t.Errorf("the replay sent %q to the host the flags name, want /pets/42", got)
	}
	if rec := planted.received(); rec.Method != "" {
		t.Errorf("the replay reached the planted listener: %+v", rec)
	}
	assertNoCanaryOnTheWire(t, planted.received())
}

// §5a's replay table, row "Target host": a stored host outside the currently
// allowed set is refused rather than silently retargeted. This is the one place
// replay and `call` deliberately differ — `call` withholds and runs.
func TestHistoryReplayRefusesAStoredHostOutsideTheAllowedSet(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/pets/42", 0))

	code, _, stderr := runHistory(t, "replay", "1",
		"--spec", "testdata/call.yaml", "--base-url", srv.URL, "--output", "json")
	if code != 2 {
		t.Fatalf("replay of an off-set stored host = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "--allow-host") {
		t.Errorf("error message %q does not name the flag that permits it", msg)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the refused replay still reached the server: %+v", rec)
	}
}

// An entry whose operationId no longer names anything in the spec fails that
// entry, with the exit code an agent branches on — not the process, and not a
// request built from the stored fields.
func TestHistoryReplayFailsWhenTheOperationIsGoneFromTheSpec(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "retiredOperation", "GET", srv.URL+"/pets/42", 0))

	code, _, stderr := runReplay(t, srv, "1")
	if code != 2 {
		t.Fatalf("replay of a retired operation = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "retiredOperation") {
		t.Errorf("error message %q does not name the operation it could not find", msg)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the failed replay still reached the server: %+v", rec)
	}
}

// Replay re-derives through the spec, so it needs one. Failing with a usage
// error is a deliberate contract change: the alternative is a replay that
// silently sends the stored fields.
func TestHistoryReplayWithNoSpecIsAUsageError(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/pets/42", 0))

	code, _, stderr := runHistory(t, "replay", "1", "--output", "json")
	if code != 2 {
		t.Fatalf("replay with no spec = %d, want 2; stderr: %s", code, stderr)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the failed replay still reached the server: %+v", rec)
	}
}

// The entry id must never be mistaken for a spec reference: loadSpec gives a
// positional argument precedence over --spec and the environment, so threading
// the replay's argument through would make `replay 1` load a spec named 1.
func TestHistoryReplayTakesTheSpecFromTheFlagNotTheEntryHandle(t *testing.T) {
	srv := newCallServer(t, jsonPet)

	for _, form := range []string{"flag", "env"} {
		t.Run(form, func(t *testing.T) {
			isolateHistory(t)
			seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/pets/42", 0))

			args := []string{"replay", "1", "--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json"}
			if form == "flag" {
				args = append(args, "--spec", "testdata/call.yaml")
			} else {
				t.Setenv(spec.EnvSpec, "testdata/call.yaml")
			}

			code, _, stderr := runHistoryKeepingSpecEnv(t, form == "env", args...)
			if code != 0 {
				t.Fatalf("history replay 1 (%s) = %d, want 0; stderr: %s", form, code, stderr)
			}
			if got := srv.received().Path; got != "/pets/42" {
				t.Errorf("the replay sent %q, want /pets/42", got)
			}
		})
	}
}

// runHistoryKeepingSpecEnv is runHistory with the option of leaving TALARIA_SPEC
// alone, so the environment form of the spec reference can be tested at all.
func runHistoryKeepingSpecEnv(t *testing.T, keep bool, args ...string) (int, string, string) {
	t.Helper()

	if !keep {
		t.Setenv(spec.EnvSpec, "")
	}
	t.Setenv("TALARIA_AUTH_BEARER", callCanary)
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", callCanary)

	var out, errOut strings.Builder
	code := run(append([]string{"history"}, args...), &out, &errOut)

	if strings.Contains(out.String(), callCanary) {
		t.Fatalf("history leaked the credential on stdout:\n%s", out.String())
	}

	return code, out.String(), errOut.String()
}

// Finding 22, verified as `{"refresh_token":"<redacted>"}` arriving at the API
// with empty stderr. A stored body still carrying a redaction marker cannot be
// replayed: sending the marker is a login attempt whose password is the literal
// text `<redacted>`.
func TestHistoryReplayRefusesABodyStillCarryingARedactionMarker(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	entry := seedEntry(corpus.SourceCall, time.Minute, "createPet", "POST", srv.URL+"/pets", 0)
	entry.Request.Body = &corpus.Body{
		ContentType: "application/json",
		Data:        `{"refresh_token":"<redacted>"}`,
	}
	seedHistory(t, entry)

	code, _, stderr := runReplay(t, srv, "1", "--allow-mutations")
	if code != 2 {
		t.Fatalf("replay of a redacted body = %d, want 2; stderr: %s", code, stderr)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the refused replay still reached the server: %+v", rec)
	}
}

// Every credential position in a stored entry is dropped, whatever it names.
// Nothing in an entry is resolved any more, so an edited `<redacted:env:NAME>`
// is a value that goes nowhere rather than a variable talaria reads.
func TestHistoryReplayDropsAStoredCredentialPosition(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	entry := seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/pets/42", 0)
	entry.Request.Headers = map[string]string{
		"X-Steal":       "<redacted:env:MY_UNRELATED_SECRET>",
		"Authorization": "Bearer a-plausible-looking-literal",
	}
	seedHistory(t, entry)

	t.Setenv("MY_UNRELATED_SECRET", callCanary)

	code, _, stderr := runReplay(t, srv, "1")
	if code != 0 {
		t.Fatalf("history replay = %d, want 0; stderr: %s", code, stderr)
	}

	rec := srv.received()
	if got := rec.Header.Get("X-Steal"); got != "" {
		t.Errorf("the replay sent X-Steal %q; nothing in an entry is resolved", got)
	}
	// The credential comes from config.Resolve against the current environment,
	// never from the entry — so the stored literal is gone and the real one is
	// there in its place.
	if got, want := rec.Header.Get("Authorization"), "Bearer "+callCanary; got != want {
		t.Errorf("server saw Authorization %q, want the re-resolved %q", got, want)
	}
	if !strings.Contains(stderr, "X-Steal") {
		t.Errorf("stderr does not report the dropped header: %s", stderr)
	}
}

// A stored path that cannot be matched against the operation's template is exit
// 2, never a request to a half-substituted path.
func TestHistoryReplayFailsWhenTheStoredPathDoesNotMatchTheTemplate(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/dogs/42/extra", 0))

	code, _, stderr := runReplay(t, srv, "1")
	if code != 2 {
		t.Fatalf("replay of a mismatched path = %d, want 2; stderr: %s", code, stderr)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the failed replay still reached the server: %+v", rec)
	}
}

// The design doc requires replayableEnv be *deleted*, not made unreachable: an
// unreachable version passes every behaviour test and comes back next cycle.
func TestReplayableEnvIsGoneFromTheTree(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing the command package: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("the glob matched no Go files, so this test proves nothing")
	}

	checked := 0
	for _, path := range paths {
		// Test files are skipped because this one has to name the symbol to look
		// for it; a test file that referenced the real thing would not compile.
		if strings.HasSuffix(path, "_test.go") {
			continue
		}

		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		checked++

		if strings.Contains(string(source), "replayableEnv") {
			t.Errorf("%s still names replayableEnv; nothing in a stored entry is resolved any more", path)
		}
	}

	if checked == 0 {
		t.Fatal("no non-test Go file was read, so this test proves nothing")
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

// A replay has to put the original bytes back on the wire, so a stored body
// beginning with @ or - must not be read as a filename or as the console: the
// bytes are handed to the builder directly, never as an argv-style --body.
func TestHistoryReplaySendsAStoredBodyVerbatim(t *testing.T) {
	for _, body := range []string{`{"name":"Rex"}`, "@/etc/passwd", "-"} {
		t.Run(body, func(t *testing.T) {
			isolateHistory(t)
			srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
			})

			entry := seedEntry(corpus.SourceCall, time.Minute, "createPet", "POST", srv.URL+"/pets", 0)
			entry.Request.Body = &corpus.Body{ContentType: "application/json", Data: body}
			seedHistory(t, entry)

			code, _, stderr := runReplay(t, srv, "1", "--allow-mutations")
			if code != 0 {
				t.Fatalf("history replay = %d, want 0; stderr: %s", code, stderr)
			}
			if got := srv.received().Body; got != body {
				t.Errorf("server saw body %q, want the recorded %q", got, body)
			}
		})
	}
}

// `history show` prints the entry for a human to read. A base64 body is not
// text, and printing its stored form as if it were the body would be a lie
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
