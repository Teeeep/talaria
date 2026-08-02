package main

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
)

// historyEntryView is one line of `history`: enough to recognise a call and its
// index, and nothing more. The whole entry is what `history show` is for.
type historyEntryView struct {
	// Index is the entry's position in the whole store, newest first and
	// 1-based. It is *not* its position in a filtered list: it is the number
	// `history show` and `history replay` take, so filtering must not renumber
	// it.
	Index       int       `json:"index"`
	Timestamp   time.Time `json:"timestamp"`
	Source      string    `json:"source"`
	OperationID string    `json:"operation_id,omitempty"`
	Method      string    `json:"method"`
	// Path is the URL's path, which is what identifies a call at a glance; URL
	// carries the rest for anyone who needs it.
	Path   string `json:"path"`
	URL    string `json:"url"`
	Status int    `json:"status,omitempty"`
	// TimingMS is omitted along with Status for an entry that observed no
	// response, rather than reported as a zero that reads like a fast call.
	TimingMS int64 `json:"timing_ms,omitempty"`
}

// historyListView is the `history` payload.
type historyListView struct {
	Entries []historyEntryView `json:"entries"`
}

// historyShowView is the `history show` payload: one entry exactly as stored,
// alongside the index it was asked for.
type historyShowView struct {
	Index int          `json:"index"`
	Entry corpus.Entry `json:"entry"`
}

// historyFilter is the set of `history` flags, parsed. A nil field is a filter
// the caller did not ask for.
type historyFilter struct {
	operation string
	source    corpus.Source
	since     *time.Time
	status    *statusFilter
}

// statusFilter is `--status`: either an exact code or a class such as 4xx.
type statusFilter struct {
	low, high int
}

// matches reports whether a status falls in the filter's range.
func (s statusFilter) matches(status int) bool {
	return status >= s.low && status <= s.high
}

func newHistoryCmd() *cobra.Command {
	var (
		operationID string
		source      string
		since       string
		status      string
	)

	cmd := &cobra.Command{
		Use:   "history",
		Short: "List what talaria has already called and what came back",
		Long: "List the recorded request/response pairs, newest first. Everything here\n" +
			"was written redacted: a credential was never stored, so none can be read\n" +
			"back. Recording is switched off per profile with `history.enabled: false`\n" +
			"or everywhere with TALARIA_HISTORY=off.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			filter, err := parseHistoryFilter(operationID, source, since, status)
			if err != nil {
				return err
			}

			entries, err := loadHistory(cmd)
			if err != nil {
				return err
			}

			return output.New(format, cmd.OutOrStdout()).Render(historyPayload(entries, filter))
		},
	}

	cmd.Flags().StringVar(&operationID, "operation", "",
		"only entries for this operationId")
	cmd.Flags().StringVar(&since, "since", "",
		"only entries newer than a duration ago, e.g. 1h or 30m")
	cmd.Flags().StringVar(&status, "status", "",
		"only entries with this response status: an exact code (404) or a class (4xx)")
	cmd.Flags().StringVar(&source, "source", "",
		"only entries from this command: "+strings.Join(historySources(), "|"))

	cmd.AddCommand(newHistoryShowCmd())
	cmd.AddCommand(newHistoryReplayCmd())

	return cmd
}

func newHistoryShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <n>",
		Short: "Print one recorded request and response in full",
		Long: "Print the whole of one entry: the request as it was sent and the response\n" +
			"as it came back, both redacted. The index is the one `history` prints.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			entries, err := loadHistory(cmd)
			if err != nil {
				return err
			}

			index, entry, err := selectEntry(entries, args[0])
			if err != nil {
				return err
			}

			return output.New(format, cmd.OutOrStdout()).Render(historyShowPayload(index, entry))
		},
	}
}

func newHistoryReplayCmd() *cobra.Command {
	var allowMutations bool

	cmd := &cobra.Command{
		Use:   "replay <n>",
		Short: "Re-issue a recorded call",
		Long: "Send a recorded request again, to the URL it went to, and record the\n" +
			"result as a new entry. Credentials are resolved from the environment as\n" +
			"they were the first time — history holds their names, never their values,\n" +
			"so there is nothing there to read back.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			cfg, err := config.Load("")
			if err != nil {
				return err
			}

			store, err := openHistory(cmd, cfg)
			if err != nil {
				return err
			}

			entries, err := store.Read()
			if err != nil {
				return err
			}

			_, entry, err := selectEntry(entries, args[0])
			if err != nil {
				return err
			}

			// The same gate `call` applies, for the same reason: the method is what
			// makes a request dangerous, and a recorded POST is still a POST.
			if (operation.Operation{Method: entry.Method}).IsMutation() && !allowMutations {
				return clierr.Usage(
					"entry %s is a %s request and may change server state: pass --allow-mutations to replay it",
					args[0], entry.Method)
			}

			req, err := replayRequest(cmd.ErrOrStderr(), entry)
			if err != nil {
				return err
			}

			renderer := output.New(format, cmd.OutOrStdout())
			redactors := newRedactors(cfg)

			resp, execErr := curl.Execute(req)
			recordCall(cmd.ErrOrStderr(), store, corpus.SourceReplay, req, resp, redactors)
			if execErr != nil {
				return execErr
			}

			// No validation block: replay reads a recorded request and needs no
			// spec to send one, so there is no contract here to check against.
			return renderer.Render(callPayload(req, redactResponse(resp, redactors.Response), nil))
		},
	}

	cmd.Flags().BoolVar(&allowMutations, "allow-mutations", false,
		"permit replaying a method other than GET, HEAD or OPTIONS")

	return cmd
}

// openHistory builds the store with the recording setting the selected profile
// asks for. Reading never consults it — history written before recording was
// switched off is still history — but replay writes, so it has to be right.
func openHistory(cmd *cobra.Command, cfg *config.Config) (*corpus.Store, error) {
	prof, err := selectProfile(cmd, cfg)
	if err != nil {
		return nil, err
	}

	// The empty dir is the default state directory; internal/corpus locates it.
	return corpus.New("", prof.HistoryEnabled()), nil
}

// loadHistory loads the store for a read-only command. The profile still
// decides the Enabled flag so the store is constructed the one way, but nothing
// on this path writes.
func loadHistory(cmd *cobra.Command) ([]corpus.Entry, error) {
	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}

	store, err := openHistory(cmd, cfg)
	if err != nil {
		return nil, err
	}

	return store.Read()
}

// historyPayload renders the store newest first, keeping the entries the filter
// admits and the index each had before filtering.
func historyPayload(entries []corpus.Entry, filter historyFilter) output.Payload {
	view := historyListView{Entries: []historyEntryView{}}
	rows := [][]string{}

	// The store is oldest first, so walking it backwards is both the display
	// order and the index order.
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if !filter.admits(entry) {
			continue
		}

		line := newHistoryEntryView(len(entries)-i, entry)
		view.Entries = append(view.Entries, line)
		rows = append(rows, []string{
			strconv.Itoa(line.Index),
			line.Timestamp.Format(time.RFC3339),
			string(line.Source),
			line.Method,
			line.Path,
			statusCell(line.Status),
			line.OperationID,
		})
	}

	return output.Payload{
		Data: view,
		Table: output.Table{
			Headers: []string{"#", "WHEN", "SOURCE", "METHOD", "PATH", "STATUS", "OPERATION"},
			Rows:    rows,
		},
	}
}

func newHistoryEntryView(index int, entry corpus.Entry) historyEntryView {
	view := historyEntryView{
		Index:       index,
		Timestamp:   entry.Timestamp,
		Source:      string(entry.Source),
		OperationID: entry.OperationID,
		Method:      entry.Method,
		Path:        urlPath(entry.URL),
		URL:         entry.URL,
	}
	if entry.Response != nil {
		view.Status = entry.Response.Status
		view.TimingMS = entry.Response.TimingMS
	}

	return view
}

func historyShowPayload(index int, entry corpus.Entry) output.Payload {
	rows := [][]string{
		{entry.Method + " " + entry.URL},
	}
	for _, name := range sortedKeys(entry.Request.Headers) {
		rows = append(rows, []string{name + ": " + entry.Request.Headers[name]})
	}
	if entry.Request.Body != nil {
		rows = append(rows, []string{bodyLine(entry.Request.Body)})
	}
	if entry.Response != nil {
		rows = append(rows, []string{fmt.Sprintf("%d in %dms", entry.Response.Status, entry.Response.TimingMS)})
		if entry.Response.Body != nil {
			rows = append(rows, []string{bodyLine(entry.Response.Body)})
		}
	}

	return output.Payload{
		Data:  historyShowView{Index: index, Entry: entry},
		Table: output.Table{Rows: rows},
	}
}

// bodyLine renders a recorded body for a human. A body with no encoding is the
// text that was sent and prints as itself; anything else is not text, and
// printing its stored form would claim bytes went on the wire that did not. The
// summary says how big it was and how to get at it — `--output json` carries the
// entry as recorded, encoding and all.
func bodyLine(body *corpus.Body) string {
	if body.Encoding == "" {
		return body.Data
	}

	data, err := body.Bytes()
	if err != nil {
		return fmt.Sprintf("<body with unreadable %s encoding: %v>", body.Encoding, err)
	}

	return fmt.Sprintf("<binary body, %d bytes, %s in --output json>", len(data), body.Encoding)
}

// statusCell renders the status column, which is empty for an entry that
// observed no response rather than a 0 nobody's server returned.
func statusCell(status int) string {
	if status == 0 {
		return "-"
	}

	return strconv.Itoa(status)
}

// urlPath is the path component of a recorded URL, for the list's path column.
// A URL that will not parse — nothing writes one, but the store is a file on
// disk — degrades to itself rather than to nothing.
func urlPath(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	return parsed.EscapedPath()
}

// selectEntry resolves a 1-based, newest-first index against the store.
//
// Both failures are usage errors carrying what the caller can do instead: the
// valid range, or the fact that there is no history yet. An agent that guessed
// an index corrects itself from the message without a second command.
func selectEntry(entries []corpus.Entry, arg string) (int, corpus.Entry, error) {
	index, err := strconv.Atoi(arg)
	if err != nil {
		return 0, corpus.Entry{}, clierr.Usage("history index %q is not a number", arg)
	}

	if len(entries) == 0 {
		return 0, corpus.Entry{}, clierr.Usage("no history entry %d: the history is empty", index)
	}
	if index < 1 || index > len(entries) {
		return 0, corpus.Entry{}, clierr.Usage(
			"no history entry %d: the valid range is 1-%d", index, len(entries))
	}

	return index, entries[len(entries)-index], nil
}

// admits reports whether an entry survives every filter that was asked for.
func (f historyFilter) admits(entry corpus.Entry) bool {
	if f.operation != "" && entry.OperationID != f.operation {
		return false
	}
	if f.source != "" && entry.Source != f.source {
		return false
	}
	if f.since != nil && entry.Timestamp.Before(*f.since) {
		return false
	}
	if f.status != nil {
		// An entry with no response has no status to match, so a status filter
		// excludes it rather than matching it as a zero.
		if entry.Response == nil || !f.status.matches(entry.Response.Status) {
			return false
		}
	}

	return true
}

// parseHistoryFilter turns the flags into a filter, failing with exit 2 on
// anything it cannot read. Parsing happens before the store is opened so a
// typo is reported without touching the disk.
func parseHistoryFilter(operationID, source, since, status string) (historyFilter, error) {
	filter := historyFilter{operation: operationID}

	if source != "" {
		parsed, err := parseSource(source)
		if err != nil {
			return historyFilter{}, err
		}
		filter.source = parsed
	}

	if since != "" {
		age, err := time.ParseDuration(since)
		if err != nil || age < 0 {
			return historyFilter{}, clierr.Usage(
				"cannot read --since %q: give a duration such as 30m, 1h or 7d as 168h", since)
		}
		cutoff := time.Now().Add(-age)
		filter.since = &cutoff
	}

	if status != "" {
		parsed, err := parseStatusFilter(status)
		if err != nil {
			return historyFilter{}, err
		}
		filter.status = parsed
	}

	return filter, nil
}

// parseSource reads --source, offering the valid values on a miss.
func parseSource(value string) (corpus.Source, error) {
	for _, source := range historySources() {
		if strings.EqualFold(value, source) {
			return corpus.Source(source), nil
		}
	}

	return "", clierr.Usage("no history source %q", value).WithAlternatives(historySources()...)
}

// historySources is the set of commands that write to the store, in the order
// they are worth reading about.
func historySources() []string {
	return []string{string(corpus.SourceCall), string(corpus.SourceRun), string(corpus.SourceReplay)}
}

// parseStatusFilter reads --status: an exact code, or a class with x in place
// of the digits that vary.
func parseStatusFilter(value string) (*statusFilter, error) {
	const bad = "cannot read --status %q: give an exact code such as 404 or a class such as 4xx"

	trimmed := strings.ToLower(strings.TrimSpace(value))
	if len(trimmed) != 3 {
		return nil, clierr.Usage(bad, value)
	}

	if strings.HasSuffix(trimmed, "xx") {
		class, err := strconv.Atoi(trimmed[:1])
		if err != nil || class < 1 || class > 9 {
			return nil, clierr.Usage(bad, value)
		}

		return &statusFilter{low: class * 100, high: class*100 + 99}, nil
	}

	code, err := strconv.Atoi(trimmed)
	if err != nil {
		return nil, clierr.Usage(bad, value)
	}

	return &statusFilter{low: code, high: code}, nil
}

// replayRequest rebuilds a runnable request from a stored entry.
//
// Every credential position in the entry holds `<redacted:env:NAME>`, which
// becomes a reference again here — the value is resolved from the environment
// at exec time, exactly as it was on the original call. Nothing in this function
// reads a credential, because there is none in the store to read (§5a).
func replayRequest(stderr io.Writer, entry corpus.Entry) (*request.Request, error) {
	parsed, err := url.Parse(entry.URL)
	if err != nil {
		return nil, clierr.Usage("the recorded URL %q cannot be parsed: %v", entry.URL, err)
	}
	// The store is a file on disk, so its scheme is checked on the way out as
	// well as on the way in: an edited entry must not be able to replay as a
	// file read or a raw TCP write.
	if !request.IsHTTPScheme(parsed.Scheme) {
		return nil, clierr.Usage("the recorded URL %q has scheme %q; only http and https can be replayed",
			entry.URL, parsed.Scheme)
	}

	req := &request.Request{
		OperationID: entry.OperationID,
		Method:      entry.Method,
		BaseURL:     parsed.Scheme + "://" + parsed.Host,
		Path:        parsed.EscapedPath(),
	}

	req.Query, err = replayQuery(stderr, parsed.RawQuery)
	if err != nil {
		return nil, err
	}
	req.Headers = replayPairs(stderr, "header", entry.Request.Headers)
	req.Cookies = replayPairs(stderr, "cookie", entry.Request.Cookies)

	if body := entry.Request.Body; body != nil {
		if body.Truncated {
			return nil, clierr.Usage(
				"the recorded request body was truncated at %d bytes, so replaying it would send something the original did not",
				corpus.MaxBody)
		}
		data, err := body.Bytes()
		if err != nil {
			return nil, err
		}
		req.Body = &request.Body{ContentType: body.ContentType, Data: data}
	}

	return req, nil
}

// replayQuery rebuilds the query parameters in the order they were recorded.
// The raw string is walked rather than url.ParseQuery'd because a map would lose
// that order, and a replay that reorders the query string is not the same
// request.
func replayQuery(stderr io.Writer, raw string) ([]request.Pair, error) {
	var pairs []request.Pair

	for _, field := range strings.Split(raw, "&") {
		if field == "" {
			continue
		}

		rawName, rawValue, _ := strings.Cut(field, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			return nil, clierr.Usage("the recorded query parameter %q cannot be parsed: %v", rawName, err)
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return nil, clierr.Usage("the recorded value of query parameter %q cannot be parsed: %v", name, err)
		}

		replayed, ok := replayValue(value)
		if !ok {
			warnUnreplayable(stderr, "query parameter", name)
			continue
		}
		pairs = append(pairs, request.Pair{Name: name, Value: replayed})
	}

	return pairs, nil
}

// replayPairs rebuilds headers or cookies. The stored form is a map, so the
// order is gone and is restored as sorted-by-name: emitted curl and the config
// document both have to be deterministic, and byte-for-byte header order is not
// something an HTTP request depends on.
func replayPairs(stderr io.Writer, kind string, stored map[string]string) []request.Pair {
	var pairs []request.Pair

	for _, name := range sortedKeys(stored) {
		value, ok := replayValue(stored[name])
		if !ok {
			warnUnreplayable(stderr, kind, name)
			continue
		}
		pairs = append(pairs, request.Pair{Name: name, Value: value})
	}

	return pairs
}

// replayValue turns one stored value back into a request Value, reporting
// false for one that cannot be reproduced.
//
// A `<redacted:env:NAME>` — with or without a scheme prefix — becomes a
// reference to the same variable. A bare `<redacted>` is a value the name-based
// matcher caught: a literal the user typed, which was deliberately not written
// down and so cannot come back. That is the firewall working, not a bug, and
// the caller says so on stderr rather than silently sending a request missing a
// header the original had.
func replayValue(stored string) (request.Value, bool) {
	if stored == secret.Placeholder {
		return request.Value{}, false
	}

	// Longest prefix first, so the raw encoding's empty prefix is the fallback
	// rather than the first match.
	for _, enc := range []request.Encoding{request.EncodeBearer, request.EncodeBasic, request.EncodeRaw} {
		prefix := encodingPrefix(enc)
		if prefix != "" && !strings.HasPrefix(stored, prefix) {
			continue
		}

		if ref, ok := secret.ParseRef(strings.TrimPrefix(stored, prefix)); ok {
			return request.Secret(ref, enc), true
		}
	}

	return request.Literal(stored), true
}

// encodingPrefix is the text a scheme puts in front of a credential — "Bearer "
// and the rest. It is asked of internal/request rather than repeated here, so
// the two cannot drift apart.
func encodingPrefix(enc request.Encoding) string {
	return request.Secret(secret.SecretRef{}, enc).Prefix()
}

// warnUnreplayable reports a field the replay had to drop. It is a warning
// rather than a failure: the request is still worth making, and a 401 with an
// explanation on stderr is more useful than a refusal.
func warnUnreplayable(stderr io.Writer, kind, name string) {
	fmt.Fprintf(stderr,
		"warning: the recorded %s %q held a redacted literal, which history does not store; replaying without it\n",
		kind, name)
}

// sortedKeys is the deterministic iteration order for the maps a stored entry
// holds.
func sortedKeys(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}
