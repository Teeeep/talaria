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
	"github.com/Teeeep/talaria/internal/spec"
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

			prof, err := selectProfile(cmd, cfg)
			if err != nil {
				return err
			}

			baseURL, allowHosts, err := hostFlags(cmd)
			if err != nil {
				return err
			}

			req, err := replay{
				Entry:      entry,
				Doc:        doc,
				Index:      index,
				Profile:    prof,
				BaseURL:    baseURL,
				AllowHosts: allowHosts,
				Redactor:   newRedactors(cfg).Request,
				Stderr:     cmd.ErrOrStderr(),
			}.request()
			if err != nil {
				return err
			}

			warnWithheldCredentials(cmd.ErrOrStderr(), req)

			renderer := output.New(format, cmd.OutOrStdout())
			redactors := newRedactors(cfg)

			resp, execErr := curl.ExecuteWith(cmd.Context(), req, timeoutOptions(curl.DefaultMaxTime.Seconds()))
			recordCall(cmd.ErrOrStderr(), store, corpus.SourceReplay, req, resp, redactors)
			if execErr != nil {
				return execErr
			}

			// A replay is re-bound through the spec, so it has the same contract to
			// check against a call does and renders the same validation block.
			view := redactResponse(resp, redactors.Response)

			return renderer.Render(callPayload(req, view, validateResponse(cmd.ErrOrStderr(), doc, req, view)))
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

// replay is one `history replay`: a stored entry re-derived against the spec,
// the flags and the environment in force now.
//
// DESIGN.md §5a's rule is that a history entry is data, never instruction. The
// entry says which operation ran, with which parameters and which body. It does
// not say where the request goes — that comes from --base-url, the profile or
// the spec — and it does not say which credential to attach, because nothing in
// it is resolved: credentials come back through config.Resolve exactly as they
// do for a call.
type replay struct {
	Entry      corpus.Entry
	Doc        *spec.Document
	Index      *operation.Index
	Profile    *config.Profile
	BaseURL    string
	AllowHosts []string
	// Redactor is the config file's display patterns, passed through so a
	// replayed request hides the same fields the original did.
	Redactor *secret.Redactor
	// Stderr carries the warnings for fields the entry could not give back.
	Stderr io.Writer
}

// request rebuilds the runnable request, or fails *this entry* with exit 2.
//
// Every refusal below is per-entry rather than per-process, which is what makes
// a hostile store survivable: one edited line stops one replay, and `history`,
// `history show` and every other entry go on working.
func (r replay) request() (*request.Request, error) {
	op, err := r.operation()
	if err != nil {
		return nil, err
	}

	stored, err := r.storedURL()
	if err != nil {
		return nil, err
	}

	body, err := r.body()
	if err != nil {
		return nil, err
	}

	target, err := r.target(stored)
	if err != nil {
		return nil, err
	}

	params, err := replayParams(op, stored.EscapedPath())
	if err != nil {
		return nil, err
	}

	query, extra, err := r.query(op, stored.RawQuery)
	if err != nil {
		return nil, err
	}

	creds, err := config.Resolve(op, r.Doc, r.Profile)
	if err != nil {
		return nil, err
	}

	req, err := request.Build(request.Inputs{
		Op:         op,
		Doc:        r.Doc,
		Profile:    r.Profile,
		Creds:      creds,
		BaseURL:    target,
		AllowHosts: r.AllowHosts,
		Params:     append(params, query...),
		Query:      extra,
		Headers:    r.headers(op),
		Redactor:   r.Redactor,
	})
	if err != nil {
		return nil, err
	}

	// Set after Build rather than through Inputs.Body: that field carries
	// --body's three forms, so a stored body beginning `@` or equal to `-` would
	// be read as a file path or as this process's stdin. The bytes recorded are
	// the bytes to send, and nothing about them selects a source.
	req.Body = body

	return req, nil
}

// operation looks the entry's operationId up in the *current* spec. An entry
// that names none, or one the spec no longer has, cannot be re-derived — and
// re-deriving is the whole of what replay now does.
func (r replay) operation() (operation.Operation, error) {
	if r.Entry.OperationID == "" {
		return operation.Operation{}, clierr.Usage(
			"the entry records no operationId, so there is nothing in the spec to replay it against")
	}

	op, err := r.Index.Lookup(r.Entry.OperationID)
	if err != nil {
		return operation.Operation{}, err
	}

	// The method is part of what the operation is. An entry whose method has
	// drifted from the spec's is either a spec that changed under it or a line
	// somebody edited; either way, sending the stored one would issue a request
	// this spec does not describe.
	if !strings.EqualFold(r.Entry.Method, op.Method) {
		return operation.Operation{}, clierr.Usage(
			"the entry records a %s but %s is a %s in this spec",
			r.Entry.Method, op.ID, op.Method)
	}

	return op, nil
}

// storedURL parses the entry's URL, which is read for its path and its query
// and never for its host.
func (r replay) storedURL() (*url.URL, error) {
	// The store is a file on disk, so what it holds is checked on the way out as
	// well as on the way in. Both checks come before the parse error below,
	// which quotes the URL it could not read.
	if host, ok := request.Userinfo(r.Entry.URL); ok {
		return nil, clierr.Usage(
			"the recorded URL for %s carries a credential in its userinfo, so it will not be replayed; "+
				"re-run the call with %s=user:password set instead", host, config.EnvBasic)
	}

	parsed, err := url.Parse(r.Entry.URL)
	if err != nil {
		return nil, clierr.Usage("the recorded URL %q cannot be parsed: %v", r.Entry.URL, err)
	}
	if !request.IsHTTPScheme(parsed.Scheme) {
		return nil, clierr.Usage("the recorded URL %q has scheme %q; only http and https can be replayed",
			r.Entry.URL, parsed.Scheme)
	}

	return parsed, nil
}

// target is where the replay actually goes: --base-url, the profile, or the
// spec — never the stored URL (DESIGN.md:407).
//
// The stored host still has to agree with it. Silently retargeting a recorded
// call at a different host would make `replay` mean something other than "do
// that again", so a stored host that is neither the target nor in the allowed
// set fails the entry rather than being quietly redirected.
func (r replay) target(stored *url.URL) (string, error) {
	target, err := request.ResolveBaseURL(r.BaseURL, r.Profile, r.Doc)
	if err != nil {
		return "", err
	}

	storedHost := request.Host(r.Entry.URL)
	if storedHost == request.Host(target) {
		return target, nil
	}

	allowed, err := request.AllowedHosts(r.Doc, r.Profile, r.AllowHosts)
	if err != nil {
		return "", err
	}
	if allowed.Allows(r.Entry.URL) {
		return target, nil
	}

	return "", clierr.Usage(
		"the entry was recorded against %s, which is neither where this invocation would send (%s) "+
			"nor a host the spec declares; replay will not silently retarget it, so pass --base-url "+
			"or --allow-host %s if that is what you mean",
		storedHost, request.Host(target), storedHost)
}

// body returns the recorded request body as the bytes to send.
//
// A body holding a redaction placeholder is refused. The store is written
// redacted, so such a body is one talaria itself hollowed out — sending it would
// put the literal text `<redacted>` where a client_secret stood, which is not
// the request that was recorded and is not one anybody asked for.
func (r replay) body() (*request.Body, error) {
	stored := r.Entry.Request.Body
	if stored == nil {
		return nil, nil
	}
	if stored.Truncated {
		return nil, clierr.Usage(
			"the recorded request body was truncated at %d bytes, so replaying it would send something the original did not",
			corpus.MaxBody)
	}

	data, err := stored.Bytes()
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(data), secret.Placeholder) {
		return nil, clierr.Usage(
			"the recorded request body holds a redacted value, which history does not store, " +
				"so it cannot be replayed; re-run the call instead")
	}

	return &request.Body{ContentType: stored.ContentType, Data: data}, nil
}

// query splits the recorded query string into the parameters the operation
// declares and the ones it does not, so a declared one is bound and checked
// like any other rather than appended raw.
//
// A value that is a redaction is dropped with a warning: it was a credential
// position, and config.Resolve is what puts a credential back.
func (r replay) query(op operation.Operation, raw string) (declared, extra []string, err error) {
	locations := declaredParams(op)

	// Walked rather than url.ParseQuery'd because a map would lose the order,
	// and a replay that reorders the query string is not the same request.
	for _, field := range strings.Split(raw, "&") {
		if field == "" {
			continue
		}

		rawName, rawValue, _ := strings.Cut(field, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			return nil, nil, clierr.Usage("the recorded query parameter %q cannot be parsed: %v", rawName, err)
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return nil, nil, clierr.Usage("the recorded value of query parameter %q cannot be parsed: %v", name, err)
		}

		if isRedacted(value) {
			warnUnreplayable(r.Stderr, "query parameter", name)
			continue
		}

		if locations[name] == "query" {
			declared = append(declared, name+"="+value)
			continue
		}
		extra = append(extra, name+"="+value)
	}

	return declared, extra, nil
}

// headers returns the recorded headers as --header flags, dropping the ones a
// stored entry must not decide.
//
// Anything that is a redaction goes: a `<redacted:env:NAME>` is the store
// naming a variable, and after this task nothing in an entry is resolved — the
// credential comes back through config.Resolve or not at all. Passing it
// through as a literal would put that text on the wire, and resolving it would
// make talaria a "read $ANY_VAR and send it" primitive driven by a file.
func (r replay) headers(op operation.Operation) []string {
	locations := declaredParams(op)

	out := make([]string, 0, len(r.Entry.Request.Headers))
	for _, name := range sortedKeys(r.Entry.Request.Headers) {
		value := r.Entry.Request.Headers[name]
		if isRedacted(value) {
			warnUnreplayable(r.Stderr, "header", name)
			continue
		}

		out = append(out, name+"="+value)
	}

	// Cookies have no flag to come back through, so a recorded one is only
	// replayable when the operation declares it as a parameter — and that is
	// bound below by name, alongside the path and query parameters.
	for _, name := range sortedKeys(r.Entry.Request.Cookies) {
		if locations[name] != "cookie" || isRedacted(r.Entry.Request.Cookies[name]) {
			warnUnreplayable(r.Stderr, "cookie", name)
		}
	}

	return out
}

// replayParams recovers the operation's path parameters by matching the stored
// path against the operation's path template.
//
// The template is matched against the *tail* of the stored path: a recorded URL
// carries whatever prefix its server had (/v1, /api/v2), and the replay's own
// base URL supplies its own. Everything before the template's segments is
// therefore discarded rather than compared.
func replayParams(op operation.Operation, storedPath string) ([]string, error) {
	want := pathSegments(op.Path)
	got := pathSegments(storedPath)
	if len(got) < len(want) {
		return nil, clierr.Usage(
			"the recorded path %q has %d segments, too few for %s's %q",
			storedPath, len(got), op.ID, op.Path)
	}
	got = got[len(got)-len(want):]

	var params []string
	for i, segment := range want {
		name, ok := strings.CutPrefix(segment, "{")
		if !ok || !strings.HasSuffix(name, "}") {
			if segment != got[i] {
				return nil, clierr.Usage(
					"the recorded path %q does not match %s's %q", storedPath, op.ID, op.Path)
			}
			continue
		}

		value, err := url.PathUnescape(got[i])
		if err != nil {
			return nil, clierr.Usage(
				"the recorded path segment for %s will not percent-decode: %v",
				strings.TrimSuffix(name, "}"), err)
		}

		params = append(params, strings.TrimSuffix(name, "}")+"="+value)
	}

	return params, nil
}

// pathSegments splits a path into its non-empty segments, so a leading or
// trailing slash does not change the count either side of the comparison.
func pathSegments(path string) []string {
	var out []string
	for _, segment := range strings.Split(path, "/") {
		if segment != "" {
			out = append(out, segment)
		}
	}

	return out
}

// declaredParams maps each parameter the operation declares to where it goes,
// so a recorded value can be routed to the flag that binds it.
func declaredParams(op operation.Operation) map[string]string {
	out := make(map[string]string, len(op.Params))
	for _, p := range op.Params {
		out[p.Name] = p.In
	}

	return out
}

// isRedacted reports whether a stored value is a redaction rather than
// something that was really sent: either the bare placeholder a literal became,
// or the `<redacted:env:NAME>` a credential reference became.
func isRedacted(value string) bool {
	if value == secret.Placeholder {
		return true
	}

	// The prefix a scheme puts in front of a credential is part of the stored
	// text — `Bearer <redacted:env:…>` — so the ref is looked for after it.
	for _, enc := range []request.Encoding{request.EncodeBearer, request.EncodeBasic, request.EncodeRaw} {
		prefix := request.Secret(secret.SecretRef{}, enc).Prefix()
		if _, ok := secret.ParseRef(strings.TrimPrefix(value, prefix)); ok {
			return true
		}
	}

	return false
}

// warnUnreplayable reports a field the replay had to drop. It is a warning
// rather than a failure: the request is still worth making, the credential a
// redacted field held is re-resolved from the environment anyway, and a 401
// with an explanation on stderr is more useful than a refusal.
func warnUnreplayable(stderr io.Writer, kind, name string) {
	fmt.Fprintf(stderr,
		"warning: the recorded %s %q held a redacted value, which history does not store; replaying without it\n",
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
