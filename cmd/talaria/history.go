package main

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/replay"
)

// historyEntryView is one line of `history`: enough to recognise a call and its
// index, and nothing more. The whole entry is what `history show` is for.
type historyEntryView struct {
	// Index is the entry's position in the whole store, newest first and
	// 1-based. It is *not* its position in a filtered list: it is the number
	// `history show` and `history replay` take, so filtering must not renumber
	// it.
	Index int `json:"index"`
	// ID is the entry's stable handle, which `show` and `replay` also take. It
	// is what an agent should keep hold of: unlike Index it does not shift when
	// something appends, and every one of those three commands appends. An entry
	// recorded before ids existed has none.
	ID          string    `json:"id"`
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
		Use:   "show <id|n>",
		Short: "Print one recorded request and response in full",
		Long: "Print the whole of one entry: the request as it was sent and the response\n" +
			"as it came back, both redacted. Name the entry by the id `history` prints,\n" +
			"which is stable, or by its index, which shifts every time anything is\n" +
			"recorded.",
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
		Use:   "replay [spec] <id|n>",
		Short: "Re-issue a recorded call, re-derived from the current spec",
		Long: "Re-send a recorded call and record the result as a new entry. The entry\n" +
			"supplies the operation, its parameters and its body; the spec, the flags\n" +
			"and the environment supply everything else — where the request goes and\n" +
			"which credentials it carries are never read back out of the file. Name the\n" +
			"entry by its id: a replay is itself recorded, so every index shifts as soon\n" +
			"as one runs.",
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			inv, err := newInvocation(cmd)
			if err != nil {
				return err
			}

			store := inv.history()

			entries, err := store.Read()
			if err != nil {
				return err
			}

			// The entry's handle is always last, so `replay spec.yaml 3` reads the
			// same way `call spec.yaml getPet` does.
			handle := args[len(args)-1]
			_, entry, err := selectEntry(entries, handle)
			if err != nil {
				return err
			}

			// The same gate `call` applies, for the same reason: the method is what
			// makes a request dangerous, and a recorded POST is still a POST.
			if (operation.Operation{Method: entry.Method}).IsMutation() && !allowMutations {
				return clierr.Usage(
					"entry %s is a %s request and may change server state: pass --allow-mutations to replay it",
					handle, entry.Method)
			}

			doc, index, err := loadSpec(cmd, args[:len(args)-1])
			if err != nil {
				return err
			}

			req, err := replay.Build(replay.Inputs{
				Entry:      entry,
				Doc:        doc,
				Index:      index,
				Profile:    inv.Profile,
				BaseURL:    inv.BaseURL,
				AllowHosts: inv.AllowHosts,
				Redactor:   inv.Redactors.Request,
				Stderr:     cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			warnWithheldCredentials(cmd.ErrOrStderr(), req)

			renderer := output.New(format, cmd.OutOrStdout())

			resp, execErr := curl.ExecuteWith(cmd.Context(), req, timeoutOptions(curl.DefaultMaxTime.Seconds()))
			recordCall(cmd.ErrOrStderr(), store, corpus.SourceReplay, req, resp, inv.Redactors)
			if execErr != nil {
				return execErr
			}

			// A replay is re-bound through the spec, so it has the same contract to
			// check against a call does and renders the same validation block.
			view := redactResponse(resp, inv.Redactors.Response)

			return renderer.Render(callPayload(
				req, view, validateResponse(cmd.ErrOrStderr(), doc, req, view), inv.Redactors.Response))
		},
	}

	cmd.Flags().BoolVar(&allowMutations, "allow-mutations", false,
		"permit replaying a method other than GET, HEAD or OPTIONS")

	return cmd
}

// loadHistory loads the store for a read-only command. The profile still
// decides the Enabled flag so the store is constructed the one way, but nothing
// on this path writes.
func loadHistory(cmd *cobra.Command) ([]corpus.Entry, error) {
	inv, err := newInvocation(cmd)
	if err != nil {
		return nil, err
	}

	return inv.history().Read()
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
			idCell(line.ID),
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
			Headers: []string{"#", "ID", "WHEN", "SOURCE", "METHOD", "PATH", "STATUS", "OPERATION"},
			Rows:    rows,
		},
	}
}

func newHistoryEntryView(index int, entry corpus.Entry) historyEntryView {
	view := historyEntryView{
		Index:       index,
		ID:          entry.ID,
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
	for _, name := range slices.Sorted(maps.Keys(entry.Request.Headers)) {
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

// idCell renders the id column. An entry recorded before ids existed has none,
// and prints as the same dash an absent status does rather than as a blank cell
// a reader could mistake for a column that failed to render.
func idCell(id string) string {
	if id == "" {
		return "-"
	}

	return id
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

// selectEntry resolves what `show` and `replay` are given: an entry's stable id,
// or a 1-based, newest-first positional index.
//
// The id is tried first and wins outright. An index is recomputed on every read
// and shifts as soon as anything appends — and `call`, `run` and `replay` all
// append, replay included — so `replay 2` followed by `replay 1` re-issues the
// same entry twice. The id is what makes two commands in a row mean two entries.
// The index stays supported because it is documented and is what a human reads
// off the list.
//
// Every failure is a usage error carrying what the caller can do instead: the
// valid range, or the fact that there is no history yet. An agent that guessed
// corrects itself from the message without a second command.
func selectEntry(entries []corpus.Entry, arg string) (int, corpus.Entry, error) {
	// Newest first, so an id that a hand-edited store repeats resolves to the
	// same entry the index would.
	if arg != "" {
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].ID == arg {
				return len(entries) - i, entries[i], nil
			}
		}
	}

	index, err := strconv.Atoi(arg)
	if err != nil {
		return 0, corpus.Entry{}, clierr.Usage(
			"no history entry %q: give an id from the ID column, or an entry's 1-based index", arg)
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
	return []string{string(corpus.SourceCall), string(corpus.SourceReplay)}
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
