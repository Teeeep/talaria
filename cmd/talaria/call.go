package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	"github.com/Teeeep/talaria/internal/validate"
)

// callView is the payload from DESIGN.md §4's sketch. A dry run fills in
// everything except the response and validation blocks, so an agent parses one
// structure whether or not the request was sent.
type callView struct {
	DryRun  bool        `json:"dry_run"`
	Request requestView `json:"request"`
	// CredentialsWithheld is §5a's machine-readable half of the host-binding
	// rule: the schemes this request does not carry because its host is not one
	// the spec declares or a human allowed. Absent when nothing was withheld,
	// so its presence alone is the signal an agent branches on.
	CredentialsWithheld []request.Withheld `json:"credentials_withheld,omitempty"`
	Response            *responseView      `json:"response,omitempty"`
	Validation          *validate.Result   `json:"validation,omitempty"`
}

// requestView is what was, or would have been, sent. Curl is the symbolic
// reproduction; every other field is the redacted display form.
type requestView struct {
	Curl    string            `json:"curl"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Cookies map[string]string `json:"cookies,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// responseView is what came back. Headers keep their repetitions — Set-Cookie
// legitimately appears more than once, and HTTP forbids folding it into one
// comma-joined value the way requestView's map does.
type responseView struct {
	Status   int                 `json:"status"`
	Headers  map[string][]string `json:"headers"`
	Body     responseBody        `json:"body"`
	TimingMS int64               `json:"timing_ms"`
}

// responseBody is the raw bytes of a response, rendered per §4's sketch: a JSON
// body is embedded as JSON so an agent can reach into it with one parse instead
// of two, and anything else becomes a string.
type responseBody []byte

// MarshalJSON embeds the body when it is a JSON object or array, and quotes it
// otherwise.
//
// The object/array test is deliberately narrower than "is valid JSON": a
// plain-text body of `42` or `true` parses as JSON, and embedding it would
// report a document the server never sent. Composite syntax is unambiguous, so
// that is where the line goes.
func (b responseBody) MarshalJSON() ([]byte, error) {
	if len(b) == 0 {
		return []byte("null"), nil
	}

	trimmed := bytes.TrimLeft(b, " \t\r\n")
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed) {
		// encoding/json compacts and re-validates whatever a MarshalJSON returns,
		// so the body's own formatting does not leak into the envelope.
		return trimmed, nil
	}

	return json.Marshal(string(b))
}

func newCallCmd() *cobra.Command {
	var (
		params         []string
		queries        []string
		headers        []string
		body           []string
		dryRun         bool
		allowMutations bool
		failOnError    bool
		timeout        float64
	)

	// Built here rather than per-run so the warning fires once for the whole
	// command tree, which — since main calls run exactly once — is once per
	// process (§5a). Tests get a fresh tree, and so a fresh warner, per case.
	warner := secret.NewQueryKeyWarner()

	cmd := &cobra.Command{
		Use:   "call [spec] <operationId>",
		Short: "Build a request for one operation and show the curl that runs it",
		Long: "Bind parameters, headers and credentials to an operation and render the\n" +
			"request as curl. Credentials are referenced by environment-variable name,\n" +
			"never by value, so the emitted command is safe to paste into a bug report\n" +
			"and runnable wherever the variable is set.",
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			// The operationId is always last: with one argument the spec comes
			// from --spec or the environment, with two it is the first.
			id := args[len(args)-1]

			doc, index, err := loadSpec(cmd, args[:len(args)-1])
			if err != nil {
				return err
			}

			op, err := index.Lookup(id)
			if err != nil {
				return err
			}

			// Gated before the request is built, and before --dry-run is even
			// consulted: an agent that tried to POST learns the rule in one
			// round trip rather than after fixing an unrelated flag, and
			// without a network call (DESIGN.md §4).
			if op.IsMutation() && !allowMutations {
				return clierr.Usage(
					"%s is a %s request and may change server state: pass --allow-mutations to call it",
					operationName(op), op.Method)
			}

			// Read whether or not --profile was given: the redaction lists are a
			// security setting, and one that only takes effect when you happen to
			// be using a profile is one that silently does not.
			cfg, err := config.Load("")
			if err != nil {
				return err
			}

			req, err := buildRequest(cmd, cfg, op, doc, params, queries, headers, body)
			if err != nil {
				return err
			}

			// Before the dry-run branch: the exposure is a property of the
			// request's shape, and an emitted curl is a command the caller may
			// well run.
			warnQueryCredentials(cmd.ErrOrStderr(), warner, req)
			warnWithheld(cmd.ErrOrStderr(), req)

			store, err := openHistory(cmd, cfg)
			if err != nil {
				return err
			}

			renderer := output.New(format, cmd.OutOrStdout())
			redactors := newRedactors(cfg)
			if dryRun {
				// Nothing is recorded: a dry run is a question about a request,
				// not a request, and history answers "what have I already tried".
				// Nothing is validated either — an empty validation block would
				// read as "checked, and fine".
				return renderer.Render(callPayload(req, nil, nil))
			}

			resp, execErr := curl.ExecuteWith(cmd.Context(), req, timeoutOptions(timeout))
			// Recorded either way. A request that never completed is still
			// something that was tried, and the entry says so by having no
			// response block at all.
			recordCall(cmd.ErrOrStderr(), store, corpus.SourceCall, req, resp, redactors)
			if execErr != nil {
				return execErr
			}

			view := redactResponse(resp, redactors.Response)
			result := validateResponse(cmd.ErrOrStderr(), doc, req, view)

			// Rendered before the exit code is decided: --fail-on-error changes
			// what the process exits with, not what the caller gets to read. An
			// agent that asked for the flag still gets the full observation on
			// stdout to act on.
			if err := renderer.Render(callPayload(req, view, result)); err != nil {
				return err
			}
			if !failOnError {
				return nil
			}

			return callFailure(view, result)
		},
	}

	// Local to call: these are how one request is parameterised, and no other
	// command binds parameters. --profile and --base-url are persistent on the
	// root, because run and auth check take them too.
	cmd.Flags().StringArrayVar(&params, "param", nil,
		"bind a declared parameter, name=value (repeatable)")
	cmd.Flags().StringArrayVar(&queries, "query", nil,
		"add a query parameter, name=value (repeatable)")
	cmd.Flags().StringArrayVar(&headers, "header", nil,
		"add a request header, name=value (repeatable)")
	cmd.Flags().StringArrayVar(&body, "body", nil,
		"request body: a literal, @file to read one, or - to read this process's stdin")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"print the curl command and send nothing")
	cmd.Flags().BoolVar(&allowMutations, "allow-mutations", false,
		"permit a method other than GET, HEAD or OPTIONS")
	cmd.Flags().BoolVar(&failOnError, "fail-on-error", false,
		"exit 4 if the response is an HTTP error or violates the spec")
	cmd.Flags().Float64Var(&timeout, "timeout", curl.DefaultMaxTime.Seconds(),
		"give up on the request after this many seconds")

	return cmd
}

// timeoutOptions turns the --timeout flag into the executor's options, shared by
// call and run because both bound one request the same way.
//
// A non-positive value falls back to the default rather than meaning "no
// limit": a call that can hang forever is what the flag exists to prevent, and
// `--timeout 0` is far more likely to be a mistake than a request for one.
func timeoutOptions(seconds float64) curl.Options {
	return curl.Options{MaxTime: time.Duration(seconds * float64(time.Second))}
}

// newRedactors builds the pair of firewalls an entry passes through on its way
// to disk, extended with whatever the config file added. Request and response
// share the header list: a name worth hiding on the way back is worth hiding on
// the way out.
func newRedactors(cfg *config.Config) corpus.Redactors {
	return corpus.Redactors{
		Request:  secret.NewRedactor(cfg.Redact.Headers...),
		Response: secret.NewResponseRedactor(cfg.Redact.Headers, cfg.Redact.BodyPaths),
	}
}

// recordCall writes one entry, reporting a failure to write as a warning and
// nothing more.
//
// A call that reached the server and came back succeeded; whether talaria then
// managed to write the fact down is not a reason to change the exit code an
// agent branches on. The warning still goes to stderr, because history silently
// not recording is how a user discovers weeks later that it never was.
func recordCall(
	stderr io.Writer,
	store *corpus.Store,
	source corpus.Source,
	req *request.Request,
	resp *curl.Response,
	red corpus.Redactors,
) {
	if !store.Recording() {
		return
	}

	if err := store.Append(corpus.NewEntry(source, req, resp, red)); err != nil {
		fmt.Fprintf(stderr, "warning: the call was not recorded in history: %v\n", err)
	}
}

// buildRequest resolves the profile and the operation's credentials, then binds
// the flags to the operation.
func buildRequest(
	cmd *cobra.Command,
	cfg *config.Config,
	op operation.Operation,
	doc *spec.Document,
	params, queries, headers, body []string,
) (*request.Request, error) {
	prof, err := selectProfile(cmd, cfg)
	if err != nil {
		return nil, err
	}

	creds, err := config.Resolve(op, doc, prof)
	if err != nil {
		return nil, err
	}

	baseURL, err := cmd.Flags().GetString("base-url")
	if err != nil {
		return nil, clierr.Usage("%w", err)
	}

	hosts, err := allowedHosts(cmd, doc, prof)
	if err != nil {
		return nil, err
	}

	return request.Build(request.Inputs{
		Op:      op,
		Doc:     doc,
		Profile: prof,
		Creds:   creds,
		BaseURL: baseURL,
		Hosts:   hosts,
		Params:  params,
		Query:   queries,
		Headers: headers,
		Body:    body,
		// The same list history is redacted with. A pattern that hides a value
		// in the permanent artifact but not on the stdout an agent reads has
		// the firewall backwards.
		Redactor: newRedactors(cfg).Request,
		// The Go process owns the real stdin, and `--body -` is the only thing
		// that reads it. curl's stdin carries the config document and nothing
		// else (DESIGN.md §5a).
		Stdin: cmd.InOrStdin(),
	})
}

// selectProfile picks the profile named by --profile out of an already-loaded
// config, or returns nil when no name was given.
func selectProfile(cmd *cobra.Command, cfg *config.Config) (*config.Profile, error) {
	name, err := cmd.Flags().GetString("profile")
	if err != nil {
		return nil, clierr.Usage("%w", err)
	}
	if name == "" {
		return nil, nil
	}

	return cfg.Profile(name)
}

// warnQueryCredentials fires the one-time query-string warning if this request
// carries a credential in its URL. One warning covers the request: a scheme
// that puts two keys in the query string is a curiosity, and the point is made
// by the first.
func warnQueryCredentials(stderr io.Writer, warner *secret.QueryKeyWarner, req *request.Request) {
	for _, p := range req.Query {
		if p.Value.IsSecret() {
			warner.Warn(stderr, p.Name, p.Value.Ref())
			return
		}
	}
}

// redactResponse turns what came off the wire into the display form everything
// downstream reads. It is the only conversion from curl.Response to
// responseView, so there is one place where a response stops being raw.
func redactResponse(resp *curl.Response, redactor *secret.ResponseRedactor) *responseView {
	return &responseView{
		Status:   resp.Status,
		Headers:  redactor.Headers(resp.Headers),
		Body:     responseBody(redactor.Body(resp.Body)),
		TimingMS: resp.TimingMS,
	}
}

// validateResponse checks the response against the operation's declared
// contract, on the *redacted* representation.
//
// That is §5a's rule for error paths: validation errors quote the content they
// rejected, so a validator fed the raw body is a leak channel — the one place a
// response secret would reappear after redaction removed it. The cost is that a
// redacted field is validated as `<redacted>`, so redacting a field the schema
// constrains produces a violation the server did not commit. That is the right
// way round: a spurious error is visible and correctable, a leaked credential
// is neither.
//
// A response that cannot be validated at all is a warning, not a failure. The
// call reached the server and came back; whether talaria could then check it
// against the spec is not a reason to change the exit code an agent branches on.
func validateResponse(
	stderr io.Writer,
	doc *spec.Document,
	req *request.Request,
	view *responseView,
) *validate.Result {
	result, err := validate.Response(doc, validationInput(req, view))

	return reportValidation(stderr, result, err)
}

// validateWith is validateResponse against a validator that is already built.
// `run` makes one per suite rather than one per response, because building it
// compiles every schema in the document.
func validateWith(
	stderr io.Writer,
	validator *validate.Validator,
	req *request.Request,
	view *responseView,
) *validate.Result {
	result, err := validator.Response(validationInput(req, view))

	return reportValidation(stderr, result, err)
}

// validationInput is the one conversion from a request and a redacted response
// into what the validator reads, so `call` and `run` cannot check different
// things.
func validationInput(req *request.Request, view *responseView) validate.Input {
	return validate.Input{
		Method:  req.Method,
		URL:     curl.URL(req),
		Status:  view.Status,
		Headers: http.Header(view.Headers),
		Body:    view.Body,
	}
}

// reportValidation turns a validator failure into a warning and no result.
func reportValidation(stderr io.Writer, result *validate.Result, err error) *validate.Result {
	if err != nil {
		fmt.Fprintf(stderr, "warning: the response was not validated: %v\n", err)
		return nil
	}

	return result
}

// callFailure is what --fail-on-error turns an observation into.
//
// Both cases exit 4 (§4). The HTTP status is reported first when both apply:
// a 500 whose body does not match the spec's error schema is a server that is
// down, not a server with a documentation problem.
//
// The message summarises rather than quotes. The errors themselves are already
// on stdout, structured and per-field, which is the surface built for reading
// them; repeating one here would only add a second place for content to escape.
func callFailure(view *responseView, result *validate.Result) error {
	if view.Status >= http.StatusBadRequest {
		return clierr.Validation("the server returned %s", statusText(view.Status))
	}
	if result != nil && len(result.Errors) > 0 {
		return clierr.Validation("the response violates the spec: %s",
			pluralise(len(result.Errors), "validation error"))
	}

	return nil
}

// pluralise renders a count with its noun, adding the s English adds.
func pluralise(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %ss", n, noun)
}

// callPayload renders one call in both shapes from the same values, so the JSON
// block and the printed command cannot disagree about what was sent. A nil resp
// is a dry run: the request block is identical either way, which is what makes
// `--dry-run` a faithful preview rather than a separate code path.
//
// The view is built from the request's *redacted* representation throughout —
// requestView holds Value.String() and curl.Render's symbolic form, never a
// resolved credential. The only code that resolves one is internal/curl, at
// exec time, and it hands back a Response rather than a Request (§5a).
func callPayload(req *request.Request, resp *responseView, result *validate.Result) output.Payload {
	view := callView{
		DryRun: resp == nil,
		Request: requestView{
			Curl:    curl.Render(req),
			Method:  req.Method,
			URL:     curl.URL(req),
			Headers: pairMap(req.Headers),
			Cookies: pairMap(req.Cookies),
		},
	}
	if req.Body != nil {
		view.Request.Body = string(req.Body.Data)
	}
	view.CredentialsWithheld = req.Withheld

	// One line each: the request being described, then the command that makes
	// it. A table would put the curl in a column and pad it into unreadability.
	rows := [][]string{
		{view.Request.Method + " " + view.Request.URL},
		{view.Request.Curl},
	}

	if resp != nil {
		view.Response = resp
		view.Validation = result
		// Status and timing only. The response headers are not summarised here
		// because a Set-Cookie or an X-Auth-Token would land in a human's
		// scrollback unasked; --output json is where the full response lives.
		rows = append(rows, []string{statusLine(resp)})
		// A count, not the messages: a human scanning a call wants to know
		// whether to go look. The messages are in --output json, where they can
		// be read whole rather than truncated into a table cell.
		if result != nil {
			rows = append(rows, []string{validationLine(result)})
		}
	}

	return output.Payload{Data: view, Table: output.Table{Rows: rows}}
}

// statusLine is the pretty renderer's one-line summary of a response.
func statusLine(resp *responseView) string {
	return fmt.Sprintf("%s in %dms", statusText(resp.Status), resp.TimingMS)
}

// statusText renders a status code with its reason phrase, for a code that has
// one.
func statusText(status int) string {
	line := strconv.Itoa(status)
	if text := http.StatusText(status); text != "" {
		line += " " + text
	}

	return line
}

// validationLine is the pretty renderer's one-line summary of the validation
// block.
func validationLine(result *validate.Result) string {
	if len(result.Errors) == 0 {
		return "validation: ok"
	}

	return "validation: " + pluralise(len(result.Errors), "error")
}

// pairMap renders headers or cookies as the object §4's sketch shows. Values
// are the redacted display form: this field is read, not run, so it carries
// <redacted:env:NAME> rather than the shell reference. A name that repeats is
// joined the way HTTP itself joins repeated field values, so nothing is lost.
func pairMap(pairs []request.Pair) map[string]string {
	if len(pairs) == 0 {
		return nil
	}

	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if existing, ok := out[p.Name]; ok {
			out[p.Name] = existing + ", " + p.Value.String()
			continue
		}
		out[p.Name] = p.Value.String()
	}

	return out
}

// operationName is the operation's ID, falling back to method and path for a
// spec that sets no operationId.
func operationName(op operation.Operation) string {
	if op.ID != "" {
		return op.ID
	}

	return strings.TrimSpace(op.Method + " " + op.Path)
}
