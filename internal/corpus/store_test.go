package corpus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
)

// canary is the credential every test in this file greps the store's bytes for.
// DESIGN.md §5a calls the recording "a permanent artifact — the highest-risk
// surface in the tool" and answers it with redaction at *write* time, so the
// assertion that matters is not "the entry looks redacted" but "the value is
// nowhere in the file".
const canary = "history-CANARY-4d81f0"

// canaryRequest is a request carrying the canary through every channel that
// could put it on disk: a resolved-at-exec-time secret header, a literal
// Authorization header the user typed themselves, a literal cookie, a secret in
// the query string, and a body field the built-in redaction paths name.
func canaryRequest(t *testing.T) *request.Request {
	t.Helper()

	t.Setenv("TALARIA_TOKEN", canary)

	return &request.Request{
		OperationID: "getPet",
		Method:      "GET",
		BaseURL:     "https://api.example.com",
		Path:        "/pets/42",
		Query: []request.Pair{
			{Name: "api_key", Value: request.Secret(secret.Env("TALARIA_TOKEN"), request.EncodeRaw)},
			{Name: "verbose", Value: request.Literal("true")},
		},
		Headers: []request.Pair{
			{Name: "Authorization", Value: request.Secret(secret.Env("TALARIA_TOKEN"), request.EncodeBearer)},
			{Name: "X-Api-Key", Value: request.Literal(canary)},
			{Name: "Accept", Value: request.Literal("application/json")},
		},
		Cookies: []request.Pair{
			{Name: "session", Value: request.Literal(canary)},
		},
		Body: &request.Body{
			ContentType: "application/json",
			Data:        []byte(`{"refresh_token":"` + canary + `","name":"fido"}`),
		},
	}
}

// canaryResponse is a response carrying the canary back off the wire, in the
// two places §5a's leak-channel table names: Set-Cookie and a token field.
func canaryResponse() *Observed {
	return &Observed{
		Status: 200,
		Headers: map[string][]string{
			"Set-Cookie":   {"session=" + canary},
			"Content-Type": {"application/json"},
		},
		Body:     []byte(`{"access_token":"` + canary + `","id":42}`),
		TimingMS: 137,
	}
}

// newStore returns a recording store writing under a temporary directory, with
// TALARIA_HISTORY cleared so a value in the developer's own environment cannot
// silently turn every test in this file into a no-op.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()

	t.Setenv(EnvHistory, "")
	dir := filepath.Join(t.TempDir(), "state")

	return New(dir, true), filepath.Join(dir, "history.jsonl")
}

func TestAppendStoresWhatWasSentAndWhatCameBack(t *testing.T) {
	store, _ := newStore(t)

	before := time.Now().Add(-time.Second)
	if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), canaryResponse(), Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("read %d entries, want 1", len(entries))
	}

	got := entries[0]
	if got.Source != SourceCall {
		t.Errorf("Source = %q, want %q", got.Source, SourceCall)
	}
	if got.OperationID != "getPet" {
		t.Errorf("OperationID = %q, want getPet", got.OperationID)
	}
	if got.Method != "GET" {
		t.Errorf("Method = %q, want GET", got.Method)
	}
	if !strings.HasPrefix(got.URL, "https://api.example.com/pets/42?") {
		t.Errorf("URL = %q, want the bound path and query", got.URL)
	}
	if got.Timestamp.Before(before) || got.Timestamp.After(time.Now().Add(time.Second)) {
		t.Errorf("Timestamp = %v, want roughly now", got.Timestamp)
	}
	if got.Request.Headers["Accept"] != "application/json" {
		t.Errorf("request Accept = %q, want it kept", got.Request.Headers["Accept"])
	}
	if got.Request.Body == nil || !strings.Contains(got.Request.Body.Data, `"name":"fido"`) {
		t.Errorf("request body = %+v, want the non-secret fields kept", got.Request.Body)
	}
	if got.Response == nil {
		t.Fatal("Response is nil, want the observed response")
	}
	if got.Response.Status != 200 {
		t.Errorf("Response.Status = %d, want 200", got.Response.Status)
	}
	if got.Response.TimingMS != 137 {
		t.Errorf("Response.TimingMS = %d, want 137", got.Response.TimingMS)
	}
	if got.Response.Body == nil || !strings.Contains(got.Response.Body.Data, `"id":42`) {
		t.Errorf("response body = %+v, want the non-secret fields kept", got.Response.Body)
	}
	if ct := got.Response.Headers["Content-Type"]; len(ct) != 1 || ct[0] != "application/json" {
		t.Errorf("response Content-Type = %v, want it kept", ct)
	}
}

func TestAppendWritesAnRFC3339Timestamp(t *testing.T) {
	store, path := newStore(t)

	if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), nil, Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var line map[string]any
	if err := json.Unmarshal(firstLine(t, path), &line); err != nil {
		t.Fatalf("the entry is not JSON: %v", err)
	}

	stamp, ok := line["timestamp"].(string)
	if !ok {
		t.Fatalf("timestamp = %v, want a string", line["timestamp"])
	}
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", stamp, err)
	}
}

func TestAppendGivesEveryEntryAStableID(t *testing.T) {
	store, _ := newStore(t)

	for i := 0; i < 3; i++ {
		if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), nil, Redactors{})); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("read %d entries, want 3", len(entries))
	}

	seen := map[string]bool{}
	for i, entry := range entries {
		if entry.ID == "" {
			t.Fatalf("entry %d has no id", i)
		}
		if seen[entry.ID] {
			t.Errorf("entry %d reuses id %q", i, entry.ID)
		}
		seen[entry.ID] = true
		if want := entry.Timestamp.UTC().Format(time.RFC3339Nano); entry.ID != want {
			t.Errorf("entry %d id = %q, want its timestamp %q", i, entry.ID, want)
		}
	}

	// The point of the id is that a second read returns the same handles: the
	// positional index does not, because every append shifts it.
	again, err := store.Read()
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	for i, entry := range again {
		if entry.ID != entries[i].ID {
			t.Errorf("entry %d id changed between reads: %q then %q", i, entries[i].ID, entry.ID)
		}
	}
}

func TestAppendDisambiguatesEntriesSharingATimestamp(t *testing.T) {
	store, _ := newStore(t)

	// One instant, three entries: the clock is the id, so this is the case that
	// would otherwise put the same handle on all three.
	stamp := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		entry := NewEntry(SourceCall, canaryRequest(t), nil, Redactors{})
		entry.Timestamp = stamp
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	seen := map[string]bool{}
	for i, entry := range entries {
		if entry.ID == "" {
			t.Fatalf("entry %d has no id", i)
		}
		if seen[entry.ID] {
			t.Fatalf("entry %d reuses id %q: a duplicate id resolves to the wrong entry", i, entry.ID)
		}
		seen[entry.ID] = true
	}
}

// Finding 23: the shipped binary returned "the call was not recorded" for an
// entry that was on disk with its response, because the retention pass ran after
// the write and failed on a store past the read bound. An operator who believes
// that re-runs a mutating call.
func TestAppendDoesNotReportFailureForALineItWrote(t *testing.T) {
	store, path := newStore(t)

	writeStore(t, path, maximalStore(t, 384))

	err := store.Append(context.Background(), Entry{Source: SourceCall, Method: "GET", URL: "https://api.example.com/pets/new"})

	stored, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("reading the store back: %v", readErr)
	}
	wrote := bytes.Contains(stored, []byte("https://api.example.com/pets/new"))

	if err != nil && wrote {
		t.Fatalf("Append reported %q for an entry it had already written; the caller is told the call was not recorded", err)
	}
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !wrote {
		t.Fatal("Append returned nil without writing the entry")
	}
}

// Finding 24: a store that exists and cannot be read is not an empty one.
// Reporting no ids there retires uniqueID's collision check silently, and two
// entries sharing an id means `history replay` sends the wrong request.
func TestStoredIDsDistinguishesAMissingStoreFromAnUnreadableOne(t *testing.T) {
	dir := t.TempDir()

	ids, err := storedIDs(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("storedIDs on a store that was never written: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("got %d ids from a store that does not exist", len(ids))
	}

	// A directory opens and refuses to be read, which is the shape of every
	// unreadable store: the bytes are there and this process cannot have them.
	unreadable := filepath.Join(dir, "not-a-file")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := storedIDs(unreadable); err == nil {
		t.Error("storedIDs accepted an unreadable store, want the read error")
	}
}

func TestReadKeepsEntriesWrittenBeforeIDsExisted(t *testing.T) {
	store, path := newStore(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := `{"source":"call","method":"GET","url":"https://api.example.com/pets/1"}` + "\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("writing the store: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("read %d entries, want the 1 an older talaria wrote", len(entries))
	}
	if entries[0].ID != "" {
		t.Errorf("entry id = %q, want it empty rather than invented", entries[0].ID)
	}

	// A new entry alongside it still gets an id, and the old line keeps none.
	if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), nil, Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries, err = store.Read()
	if err != nil {
		t.Fatalf("Read after Append: %v", err)
	}
	if len(entries) != 2 || entries[0].ID != "" || entries[1].ID == "" {
		t.Errorf("ids after appending to an old store = %q, %q", entries[0].ID, entries[1].ID)
	}
}

func TestAppendRedactsAtWriteTime(t *testing.T) {
	store, path := newStore(t)

	entry := NewEntry(SourceCall, canaryRequest(t), canaryResponse(), Redactors{})
	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	if strings.Contains(string(data), canary) {
		t.Errorf("the canary is in the history file:\n%s", data)
	}
}

func TestAppendRedactsCredentialsSuppliedAsLiterals(t *testing.T) {
	store, _ := newStore(t)

	if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), canaryResponse(), Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	got := entries[0]
	// Typed into --header rather than resolved from a ref, so only name-based
	// redaction catches it.
	if v := got.Request.Headers["X-Api-Key"]; v != secret.Placeholder {
		t.Errorf("X-Api-Key = %q, want %q", v, secret.Placeholder)
	}
	if v := got.Request.Cookies["session"]; v != secret.Placeholder {
		t.Errorf("session cookie = %q, want %q", v, secret.Placeholder)
	}
	if v := got.Request.Headers["Authorization"]; !strings.Contains(v, "<redacted:env:TALARIA_TOKEN>") {
		t.Errorf("Authorization = %q, want the ref's redacted form", v)
	}
	if v := got.Response.Headers["Set-Cookie"]; len(v) != 1 || v[0] != secret.Placeholder {
		t.Errorf("Set-Cookie = %v, want it redacted", v)
	}
}

func TestAppendCreatesA0600FileInA0700Directory(t *testing.T) {
	store, path := newStore(t)

	if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), nil, Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}

	file, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if mode := file.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %04o, want 0600", mode)
	}

	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dir.Mode().Perm(); mode != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", mode)
	}
}

func TestAppendCapsEachSourceSeparately(t *testing.T) {
	store, _ := newStore(t)

	// One interactive call first: a run over a large spec must not be able to
	// evict a session of `call` history, which a single global cap would allow.
	if err := store.Append(context.Background(), Entry{Source: SourceCall, Method: "GET", URL: "https://api.example.com/pets/1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	for i := 0; i < maxPerSource+1; i++ {
		entry := Entry{Source: SourceReplay, Method: "GET", URL: "https://api.example.com/pets/2"}
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatalf("Append run entry %d: %v", i, err)
		}
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	counts := map[Source]int{}
	for _, e := range entries {
		counts[e.Source]++
	}
	if counts[SourceReplay] != maxPerSource {
		t.Errorf("kept %d run entries, want %d", counts[SourceReplay], maxPerSource)
	}
	if counts[SourceCall] != 1 {
		t.Errorf("kept %d call entries, want the one interactive call untouched", counts[SourceCall])
	}
}

func TestAppendTrimsOldestFirst(t *testing.T) {
	store, _ := newStore(t)

	for i := 0; i < maxPerSource+2; i++ {
		entry := Entry{Source: SourceReplay, Method: "GET", URL: "https://api.example.com/pets/" + strconv.Itoa(i)}
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != maxPerSource {
		t.Fatalf("kept %d entries, want %d", len(entries), maxPerSource)
	}
	if want := "https://api.example.com/pets/2"; entries[0].URL != want {
		t.Errorf("oldest kept entry = %q, want %q", entries[0].URL, want)
	}
	if want := "https://api.example.com/pets/" + strconv.Itoa(maxPerSource+1); entries[len(entries)-1].URL != want {
		t.Errorf("newest entry = %q, want %q", entries[len(entries)-1].URL, want)
	}
}

func TestConcurrentAppendsKeepEveryEntryTheyAcknowledged(t *testing.T) {
	store, _ := newStore(t)

	// Seed to one below the cap so every concurrent append crosses it and takes
	// the rewrite path. Below the cap trim returns early, so a test that stayed
	// under it would exercise nothing.
	for i := 0; i < maxPerSource-1; i++ {
		entry := Entry{Source: SourceReplay, Method: "GET", URL: "https://api.example.com/seed/" + strconv.Itoa(i)}
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatalf("seeding entry %d: %v", i, err)
		}
	}

	const (
		writers = 8
		each    = 10
	)

	// Every Append opens its own descriptor, so these goroutines contend exactly
	// as two talaria processes sharing a history file do.
	var (
		mu       sync.Mutex
		recorded []string
		wg       sync.WaitGroup
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()

			for i := 0; i < each; i++ {
				url := "https://api.example.com/pets/" + strconv.Itoa(w) + "-" + strconv.Itoa(i)
				if err := store.Append(context.Background(), Entry{Source: SourceReplay, Method: "GET", URL: url}); err != nil {
					return
				}
				// Only entries Append reported as written are claimed: a returned
				// error is the caller's cue to warn, and this test is about the
				// entries it promised were recorded.
				mu.Lock()
				recorded = append(recorded, url)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if len(recorded) != writers*each {
		t.Fatalf("Append acknowledged %d entries, want %d", len(recorded), writers*each)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.URL] = true
	}
	missing := 0
	for _, url := range recorded {
		if !present[url] {
			missing++
		}
	}
	if missing != 0 {
		t.Errorf("Append returned nil for %d of %d entries that are not in the store", missing, len(recorded))
	}
}

// Two processes finding the store past the read bound both try to repair it.
// The lock is what makes that safe: whichever trims first, neither may be told
// its entry was lost, and neither entry may actually be.
func TestConcurrentAppendsRepairAnOverBoundStoreWithoutLosingEachOther(t *testing.T) {
	store, path := newStore(t)

	writeStore(t, path, maximalStore(t, 384))

	const writers = 3
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()

			url := "https://api.example.com/pets/concurrent-" + strconv.Itoa(w)
			if err := store.Append(context.Background(), Entry{Source: SourceCall, Method: "GET", URL: url}); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("Append on an over-bound store: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.URL] = true
	}
	for w := 0; w < writers; w++ {
		if url := "https://api.example.com/pets/concurrent-" + strconv.Itoa(w); !present[url] {
			t.Errorf("%s is not in the store, though Append reported it recorded", url)
		}
	}
}

func TestAppendTruncatesLargeBodies(t *testing.T) {
	store, path := newStore(t)

	big := strings.Repeat("x", MaxBody+512)
	req := &request.Request{
		Method:  "POST",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Body:    &request.Body{ContentType: "text/plain", Data: []byte(big)},
	}
	resp := &Observed{Status: 200, Body: []byte(big), TimingMS: 4}

	if err := store.Append(context.Background(), NewEntry(SourceCall, req, resp, Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	for name, body := range map[string]*Body{
		"request":  entries[0].Request.Body,
		"response": entries[0].Response.Body,
	} {
		if body == nil {
			t.Fatalf("%s body is nil", name)
		}
		if !body.Truncated {
			t.Errorf("%s body Truncated = false, want true", name)
		}
		if len(body.Data) != MaxBody {
			t.Errorf("%s body kept %d bytes, want %d", name, len(body.Data), MaxBody)
		}
	}

	// A truncated body must not be able to truncate the *line*: the store is
	// JSONL, and one unparseable line would take the rest of the file with it.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line %d is not valid JSON", i)
		}
	}
}

func TestAppendIsANoOpWhenTheEnvVarTurnsHistoryOff(t *testing.T) {
	store, path := newStore(t)
	t.Setenv(EnvHistory, "off")

	if store.Recording() {
		t.Error("Recording() = true, want false with TALARIA_HISTORY=off")
	}
	if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), canaryResponse(), Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}

	assertNoFile(t, path)
}

func TestAppendIsANoOpWhenTheStoreIsDisabled(t *testing.T) {
	t.Setenv(EnvHistory, "")
	dir := filepath.Join(t.TempDir(), "state")
	store := New(dir, false)

	if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), canaryResponse(), Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}

	assertNoFile(t, filepath.Join(dir, "history.jsonl"))
}

func TestTheEnvVarBeatsAnEnabledSetting(t *testing.T) {
	// The profile setting is the user's default; the variable is the operator's
	// override, so it wins.
	store, path := newStore(t)
	t.Setenv(EnvHistory, "off")

	if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), nil, Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}

	assertNoFile(t, path)
}

func TestReadSkipsACorruptLine(t *testing.T) {
	store, path := newStore(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lines := `{"source":"call","method":"GET","url":"https://api.example.com/pets/1"}
{"source":"call","method":"GET",
{"source":"call","method":"GET","url":"https://api.example.com/pets/3"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatalf("writing the store: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("read %d entries, want the 2 readable ones", len(entries))
	}
	if entries[1].URL != "https://api.example.com/pets/3" {
		t.Errorf("second entry = %q, want the line after the corrupt one", entries[1].URL)
	}
}

// writeStore puts raw bytes at the store's path, creating the state directory.
// The history file is user-writable by design and may have been copied from
// another machine, so every hostile-input test here starts from bytes talaria
// never wrote.
func writeStore(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing the store: %v", err)
	}
}

// oversizedLine is one syntactically valid entry whose body is far past what
// newBody would ever write. Reading it means allocating whatever a line in a
// user-writable file asks for.
func oversizedLine(t *testing.T) []byte {
	t.Helper()

	entry := Entry{
		Source: SourceCall,
		Method: "GET",
		URL:    "https://api.example.com/pets/2",
		Request: EntryRequest{
			Body: &Body{ContentType: "application/json", Data: strings.Repeat("A", 4*maxEntryBytes)},
		},
	}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshalling the oversized entry: %v", err)
	}

	return line
}

func TestReadSkipsAnOversizedLine(t *testing.T) {
	store, path := newStore(t)

	var file bytes.Buffer
	file.WriteString(`{"source":"call","method":"GET","url":"https://api.example.com/pets/1"}` + "\n")
	file.Write(oversizedLine(t))
	file.WriteString("\n")
	file.WriteString(`{"source":"call","method":"GET","url":"https://api.example.com/pets/3"}` + "\n")
	writeStore(t, path, file.Bytes())

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("read %d entries, want the 2 that fit the bound", len(entries))
	}
	// §5a: a hostile entry fails that entry, never the process — and never the
	// entries recorded after it.
	if entries[1].URL != "https://api.example.com/pets/3" {
		t.Errorf("second entry = %q, want the line after the oversized one", entries[1].URL)
	}
}

func TestAppendSurvivesAnOversizedLineAlreadyInTheStore(t *testing.T) {
	store, path := newStore(t)

	writeStore(t, path, append(oversizedLine(t), '\n'))

	if err := store.Append(context.Background(), NewEntry(SourceCall, canaryRequest(t), canaryResponse(), Redactors{})); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 || entries[0].OperationID != "getPet" {
		t.Fatalf("read %d entries, want only the one just appended", len(entries))
	}
	if entries[0].ID == "" {
		t.Error("the appended entry has no id; the id path did not survive the oversized line")
	}
}

func TestReadRefusesAStorePastTheWholeFileBound(t *testing.T) {
	store, path := newStore(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("creating the store: %v", err)
	}
	chunk := append(bytes.Repeat([]byte("a"), 1<<20-1), '\n')
	for written := 0; written <= maxStoreBytes; written += len(chunk) {
		if _, err := f.Write(chunk); err != nil {
			t.Fatalf("writing the store: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	if _, err := store.Read(); err == nil {
		t.Fatal("Read accepted a store past the whole-file bound, want a refusal")
	}
}

// A history file that is a character device or a symlink to one is not a store
// talaria wrote, and reading it must neither block forever nor allocate until
// the process dies.
func TestReadOfAnEndlessStoreTerminates(t *testing.T) {
	store, path := newStore(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("no /dev/zero on this platform")
	}
	if err := os.Symlink("/dev/zero", path); err != nil {
		t.Fatalf("symlinking the store: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.Read()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Read accepted an endless store, want a refusal")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Read did not return within 30s on an endless store")
	}
}

func TestReadOfAStoreOfEmptyLinesIsEmpty(t *testing.T) {
	store, path := newStore(t)

	writeStore(t, path, bytes.Repeat([]byte("\n"), 1<<20))

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("read %d entries, want none", len(entries))
	}
}

// A line short enough to hold can still be expensive to decode: JSON nesting
// costs stack, not bytes. The decoder's own depth limit must be what stops it,
// entry-level like every other unreadable line.
func TestReadSkipsADeeplyNestedLine(t *testing.T) {
	store, path := newStore(t)

	const depth = 20000
	var file bytes.Buffer
	file.WriteString(strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth) + "\n")
	file.WriteString(`{"source":"call","method":"GET","url":"https://api.example.com/pets/1"}` + "\n")
	writeStore(t, path, file.Bytes())

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 || entries[0].URL != "https://api.example.com/pets/1" {
		t.Fatalf("read %d entries, want only the one after the nested line", len(entries))
	}
}

func TestReadOfAStoreThatWasNeverWrittenIsEmpty(t *testing.T) {
	store, _ := newStore(t)

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("read %d entries, want none", len(entries))
	}
}

func TestPathFollowsXDGStateHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	got, err := New("", true).Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join(dir, "talaria", "history.jsonl"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestPathFallsBackToTheHomeStateDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)

	got, err := New("", true).Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join(home, ".local", "state", "talaria", "history.jsonl"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists, want recording to have created nothing", path)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("%s exists, want recording to have created nothing", filepath.Dir(path))
	}
}

func firstLine(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}

	line, _, _ := strings.Cut(string(data), "\n")

	return []byte(line)
}

// ---------------------------------------------------------------------------
// The line Append writes is one a reader will hold.
//
// lines() drops any stored line past maxEntryBytes, so an Append that writes a
// longer one and returns nil reports an entry recorded that Store.Read, trim,
// storedIDs, `history`, `history show` and `history replay` all skip forever —
// and the next retention pass then deletes it with no diagnostic. None of the
// routes below needs a hostile peer: ordinary headers, an ordinary body of
// control bytes, and ordinary --query input each reach the bound on their own,
// because the expansion happens in the JSON encoding and MaxBody bounds the
// bytes before it.

// storedOrRefused is that invariant, as a helper: Append returning nil means
// Read returns the entry, and never both nil. It reports the entry Read gave
// back, so a caller can assert on what survived the shrink.
func storedOrRefused(t *testing.T, store *Store, e Entry) (Entry, bool) {
	t.Helper()

	appendErr := store.Append(context.Background(), e)

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, got := range entries {
		if got.URL == e.URL {
			return got, true
		}
	}

	if appendErr == nil {
		t.Fatalf("Append returned nil for an entry Read does not return; %d entries in the store", len(entries))
	}

	return Entry{}, false
}

// assertEveryLineIsReadable checks the file itself through lines() — the same
// split Read, storedIDs and trim's rewrite all work from, so a line it leaves
// out is one no reader returns and the next trim silently drops.
func assertEveryLineIsReadable(t *testing.T, path string, want int) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	if got := len(lines(data)); got != want {
		t.Fatalf("lines() returns %d of the %d lines in the store; the rest are past the %d-byte bound", got, want, maxEntryBytes)
	}
}

func TestAppendRecordsABodyWhoseJSONEncodingExpandsPastTheLineBound(t *testing.T) {
	store, path := newStore(t)

	// MaxBody of 0x01: valid UTF-8, so newBody stores it as text, and
	// encoding/json expands every byte of it six-fold as  — a
	// 393,693-byte line from a body newBody considered in bounds.
	req := &request.Request{Method: "POST", BaseURL: "https://api.example.com", Path: "/pets/expanded"}
	obs := &Observed{Status: 200, Body: bytes.Repeat([]byte{1}, MaxBody), TimingMS: 7}

	got, ok := storedOrRefused(t, store, NewEntry(SourceCall, req, obs, Redactors{}))
	if !ok {
		t.Fatal("the entry was refused; cutting the body further is enough to fit it")
	}
	if got.Response == nil || got.Response.Body == nil {
		t.Fatal("the recorded entry has no response body")
	}
	if !got.Response.Body.Truncated {
		t.Error("the body was cut to fit the line bound but is not marked truncated")
	}
	if got.Response.Status != 200 || got.Response.TimingMS != 7 {
		t.Errorf("the response metadata did not survive: %+v", got.Response)
	}
	assertEveryLineIsReadable(t, path, 1)
}

// verboseHeaders is ~280 KB of ordinary response headers: past the line bound
// on its own, and inside curl's own 300 KB ceiling, so no hostile peer is
// needed to produce it.
func verboseHeaders() map[string][]string {
	headers := map[string][]string{"Content-Type": {"application/json"}}
	for i := 0; i < 280; i++ {
		headers[fmt.Sprintf("X-Trace-%03d", i)] = []string{strings.Repeat("v", 1000)}
	}

	return headers
}

func TestAppendRecordsAnEntryWhoseResponseHeadersAreVerbose(t *testing.T) {
	store, path := newStore(t)

	req := &request.Request{Method: "GET", BaseURL: "https://api.example.com", Path: "/pets/verbose"}
	obs := &Observed{Status: 200, Headers: verboseHeaders(), Body: []byte(`{"id":42}`), TimingMS: 11}

	got, ok := storedOrRefused(t, store, NewEntry(SourceCall, req, obs, Redactors{}))
	if !ok {
		t.Fatal("the entry was refused; a verbose-but-legitimate server still gets its metadata recorded")
	}
	if got.Response == nil || got.Response.Status != 200 {
		t.Fatalf("the response metadata did not survive: %+v", got.Response)
	}
	if !got.Response.HeadersTruncated {
		t.Error("headers were dropped to fit the budget but the entry does not say so")
	}
	if len(got.Response.Headers) == 0 {
		t.Error("every header was dropped; the cap keeps what fits")
	}
	if got.Response.Body == nil || got.Response.Body.Data != `{"id":42}` {
		t.Errorf("the body did not survive the header cap: %+v", got.Response.Body)
	}
	assertEveryLineIsReadable(t, path, 1)
}

func TestAppendRefusesALineNothingLeftCanShrink(t *testing.T) {
	store, path := newStore(t)

	// Three --query values of 100 KB each: ordinary user input, no server
	// involved. The URL is not something history can cut and still be about the
	// call that was made, so the entry is refused — loudly, since recordCall
	// turns the error into a warning on stderr.
	req := &request.Request{
		Method:  "GET",
		BaseURL: "https://api.example.com",
		Path:    "/pets",
		Query: []request.Pair{
			{Name: "a", Value: request.Literal(strings.Repeat("x", 100_000))},
			{Name: "b", Value: request.Literal(strings.Repeat("y", 100_000))},
			{Name: "c", Value: request.Literal(strings.Repeat("z", 100_000))},
		},
	}

	entry := NewEntry(SourceCall, req, &Observed{Status: 200, TimingMS: 3}, Redactors{})
	err := store.Append(context.Background(), entry)
	if err == nil {
		t.Fatal("Append reported an entry recorded that no reader can return")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxEntryBytes)) {
		t.Errorf("the refusal does not name the %d-byte bound: %v", maxEntryBytes, err)
	}

	entries, readErr := store.Read()
	if readErr != nil {
		t.Fatalf("Read: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("the refused entry left %d entries behind", len(entries))
	}
	if data, statErr := os.ReadFile(path); statErr == nil && len(data) != 0 {
		t.Errorf("the refused entry wrote %d bytes to the store", len(data))
	}
}

// lineOfLength is an entry whose encoded line is exactly n bytes. The id is set
// so Append keeps it rather than assigning one of its own length, and the
// timestamp is fixed for the same reason; the URL carries the padding, since a
// run of ASCII costs one byte per byte in JSON.
func lineOfLength(t *testing.T, n int) Entry {
	t.Helper()

	entry := Entry{
		ID:        "2026-08-04T07:34:10.123456789Z",
		Timestamp: time.Date(2026, 8, 4, 7, 34, 10, 123456789, time.UTC),
		Source:    SourceCall,
		Method:    "GET",
		URL:       "https://api.example.com/pets/",
	}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshalling the padded entry: %v", err)
	}
	if len(line) > n {
		t.Fatalf("the unpadded entry is already %d bytes, past the %d asked for", len(line), n)
	}
	entry.URL += strings.Repeat("x", n-len(line))

	if line, err = json.Marshal(entry); err != nil || len(line) != n {
		t.Fatalf("padded entry is %d bytes (err %v), want %d", len(line), err, n)
	}

	return entry
}

func TestAppendRecordsALineExactlyAtTheReadBound(t *testing.T) {
	store, path := newStore(t)

	if _, ok := storedOrRefused(t, store, lineOfLength(t, maxEntryBytes)); !ok {
		t.Fatalf("a line of exactly %d bytes was refused; lines() admits it", maxEntryBytes)
	}
	assertEveryLineIsReadable(t, path, 1)
}

func TestAppendRefusesALineOneByteOverTheReadBound(t *testing.T) {
	store, _ := newStore(t)

	if err := store.Append(context.Background(), lineOfLength(t, maxEntryBytes+1)); err == nil {
		t.Fatalf("Append accepted a %d-byte line; lines() drops it", maxEntryBytes+1)
	}
}

func TestAShrunkBase64BodyStillDecodes(t *testing.T) {
	store, path := newStore(t)

	// Two bodies at MaxBody that are not valid UTF-8, so both are base64'd:
	// ~175 KB of the 256 KB before a single header, and the headers here are
	// 200 KB. Cutting a base64 Data at any offset that is not a four-character
	// quantum leaves a Data that no longer decodes.
	binary := bytes.Repeat([]byte{0xff, 0xfe}, MaxBody/2)
	req := &request.Request{
		Method:  "POST",
		BaseURL: "https://api.example.com",
		Path:    "/pets/binary",
		Body:    &request.Body{ContentType: "application/octet-stream", Data: binary},
	}
	obs := &Observed{Status: 200, Headers: verboseHeaders(), Body: binary, TimingMS: 5}

	got, ok := storedOrRefused(t, store, NewEntry(SourceCall, req, obs, Redactors{}))
	if !ok {
		t.Fatal("the entry was refused; cutting the bodies is enough to fit it")
	}
	for name, body := range map[string]*Body{
		"request":  got.Request.Body,
		"response": got.Response.Body,
	} {
		if body == nil {
			t.Fatalf("%s body is nil", name)
		}
		if body.Encoding != EncodingBase64 {
			t.Fatalf("%s body encoding = %q, want %s", name, body.Encoding, EncodingBase64)
		}
		if _, err := body.Bytes(); err != nil {
			t.Errorf("%s body no longer decodes after the cut: %v", name, err)
		}
	}
	assertEveryLineIsReadable(t, path, 1)
}
