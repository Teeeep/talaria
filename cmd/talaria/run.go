package main

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/gen"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
	"github.com/Teeeep/talaria/internal/spec"
	"github.com/Teeeep/talaria/internal/validate"
)

// runSeed is what the data generator draws from. It is a constant rather than a
// flag because a smoke test whose request bodies change between runs fails once,
// passes next time and teaches nobody anything (DESIGN.md §5a).
const runSeed = 1

// contentTypeHeader is the header a generated body's media type travels in.
const contentTypeHeader = "Content-Type"

// The three outcomes one operation can have. Skipped is deliberately not a
// failure: a DELETE left alone without --allow-mutations, or an operation whose
// path parameter nothing could supply, is correct behaviour rather than a
// broken API.
const (
	outcomePassed  = "passed"
	outcomeFailed  = "failed"
	outcomeSkipped = "skipped"
)

// runResult is one operation's line in the report.
type runResult struct {
	OperationID string `json:"operation_id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Outcome     string `json:"outcome"`
	// Reason says why a skip or a failure happened. A passing operation has
	// none: there is nothing to explain.
	Reason string `json:"reason,omitempty"`
	// Status and TimingMS are absent for an operation that was never executed.
	// TimingMS is a pointer so a call to a local server that legitimately
	// rounded to 0ms is still reported as timed.
	Status     int              `json:"status,omitempty"`
	TimingMS   *int64           `json:"timing_ms,omitempty"`
	Validation *validate.Result `json:"validation,omitempty"`
}

// runSummary is the count an agent, or a CI job, reads first.
type runSummary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// runView is the whole report.
type runView struct {
	Results []runResult `json:"results"`
	Summary runSummary  `json:"summary"`
}

func newRunCmd() *cobra.Command {
	var (
		tags           []string
		ids            []string
		fixturesDir    string
		report         string
		allowMutations bool
		failOnError    bool
		timeout        float64
	)

	// One warner for the whole command, as on `call`: a run that puts a
	// credential in the query string makes the point once, not once per
	// operation.
	warner := secret.NewQueryKeyWarner()

	cmd := &cobra.Command{
		Use:   "run [spec]",
		Short: "Smoke-test a spec's operations and report what each one did",
		Long: "Call every operation the filters select, check each response against the\n" +
			"spec, and report one line per operation. Mutating operations are skipped\n" +
			"unless --allow-mutations is given. Request data comes from the spec's own\n" +
			"examples first, then --fixtures, then generation.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveReportFormat(cmd, report)
			if err != nil {
				return err
			}

			doc, index, err := loadSpec(cmd, args)
			if err != nil {
				return err
			}

			ops, err := selectOperations(index, tags, ids)
			if err != nil {
				return err
			}

			runner, err := newRunner(cmd, doc, fixturesDir, allowMutations, timeoutOptions(timeout), warner)
			if err != nil {
				return err
			}

			// Sequential, in spec order. Determinism beats speed here, and
			// parallel calls against a real API are a surprise nobody asked for.
			results := make([]runResult, 0, len(ops))
			for _, op := range ops {
				results = append(results, runner.execute(op))
			}

			view := runView{Results: results, Summary: summarise(results)}

			// Rendered before the exit code is decided, for the reason `call`
			// renders first: the flags change what the process exits with, not
			// what the caller gets to read.
			if err := output.New(format, cmd.OutOrStdout()).Render(runPayload(view)); err != nil {
				return err
			}

			return runner.verdict(view, failOnError)
		},
	}

	// All local to run: no other command enumerates operations, reports on a
	// suite, or has test data to supply.
	cmd.Flags().StringArrayVar(&tags, "tag", nil,
		"only operations carrying this tag (repeatable)")
	cmd.Flags().StringArrayVar(&ids, "operation", nil,
		"only this operationId (repeatable); combined with --tag as a union")
	cmd.Flags().StringVar(&fixturesDir, "fixtures", "",
		"directory of <operationId>.json test data, consulted after spec examples")
	cmd.Flags().StringVar(&report, "report", "",
		"report format: json|pretty|tsv|junit (overrides --output)")
	cmd.Flags().BoolVar(&allowMutations, "allow-mutations", false,
		"include operations whose method is not GET, HEAD or OPTIONS")
	cmd.Flags().BoolVar(&failOnError, "fail-on-error", false,
		"exit 4 if any operation failed")
	// Per operation, not per suite: the bound that matters is the one that keeps
	// a single wedged endpoint from costing the report.
	cmd.Flags().Float64Var(&timeout, "timeout", curl.DefaultMaxTime.Seconds(),
		"give up on each operation's request after this many seconds")

	return cmd
}

// resolveReportFormat picks the format the report is written in. --report is
// run's own flag and wins over the persistent --output: an agent that set
// --output globally and then asked run for JSON must get JSON, not a format it
// asked for two flags ago.
//
// It also accepts junit, which --output does not: a suite of operations is the
// only thing there is to render as a test report, and `run` is the only command
// that produces one.
func resolveReportFormat(cmd *cobra.Command, report string) (output.Format, error) {
	if report == "" {
		return resolveFormat(cmd)
	}

	format, err := output.ParseReportFormat(report)
	if err != nil {
		return "", clierr.Usage("%w", err).WithAlternatives(output.ReportFormats()...)
	}

	return format, nil
}

// selectOperations applies --tag and --operation as a union, in spec order. No
// filters at all means the whole spec.
//
// A filter that selects nothing is a usage error rather than an empty pass: a
// mistyped tag that exits 0 with nothing tested is a green CI run that proves
// nothing, which is the one result a smoke test must never produce.
func selectOperations(index *operation.Index, tags, ids []string) ([]operation.Operation, error) {
	if len(tags) == 0 && len(ids) == 0 {
		return index.Operations(), nil
	}

	keep := map[string]bool{}
	for _, tag := range tags {
		for _, op := range index.ByTag(tag) {
			keep[operationKey(op)] = true
		}
	}
	// Looked up rather than matched, so a misspelt id gets Lookup's suggestions
	// instead of silently selecting nothing.
	for _, id := range ids {
		op, err := index.Lookup(id)
		if err != nil {
			return nil, err
		}
		keep[operationKey(op)] = true
	}

	var ops []operation.Operation
	for _, op := range index.Operations() {
		if keep[operationKey(op)] {
			ops = append(ops, op)
		}
	}
	if len(ops) == 0 {
		return nil, clierr.Usage("no operation matches %s",
			strings.Join(append(quoted("--tag", tags), quoted("--operation", ids)...), " or "))
	}

	return ops, nil
}

// operationKey identifies an operation independently of its id, so the two
// filters can be unioned even for operations the spec never named.
func operationKey(op operation.Operation) string { return op.Method + " " + op.Path }

// quoted renders one filter's values for an error message.
func quoted(flag string, values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, fmt.Sprintf("%s %q", flag, value))
	}

	return out
}

// runner holds everything one suite reuses across operations: the compiled
// validator, the seeded generator, the history store and the redactors. Built
// once so the per-operation loop only makes requests.
type runner struct {
	doc       *spec.Document
	profile   *config.Profile
	baseURL   string
	gen       *gen.Generator
	validator *validate.Validator
	store     *corpus.Store
	redactors corpus.Redactors
	warner    *secret.QueryKeyWarner
	opts      curl.Options
	stderr    io.Writer

	allowMutations bool
	// missing collects the security schemes that stopped an operation from
	// running, in the display form the exit-5 error reports.
	missing []string
}

func newRunner(
	cmd *cobra.Command,
	doc *spec.Document,
	fixturesDir string,
	allowMutations bool,
	opts curl.Options,
	warner *secret.QueryKeyWarner,
) (*runner, error) {
	// The whole fixtures directory is read up front, before any request goes
	// out: a typo in a fixture should not surface halfway through a suite.
	fixtures, err := gen.LoadFixtures(fixturesDir)
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}

	profile, err := selectProfile(cmd, cfg)
	if err != nil {
		return nil, err
	}

	store, err := openHistory(cmd, cfg)
	if err != nil {
		return nil, err
	}

	baseURL, err := cmd.Flags().GetString("base-url")
	if err != nil {
		return nil, clierr.Usage("%w", err)
	}

	generator := gen.New(runSeed)
	generator.Fixtures = fixtures

	return &runner{
		doc:            doc,
		profile:        profile,
		baseURL:        baseURL,
		gen:            generator,
		validator:      validate.New(doc),
		store:          store,
		redactors:      newRedactors(cfg),
		warner:         warner,
		opts:           opts,
		stderr:         cmd.ErrOrStderr(),
		allowMutations: allowMutations,
	}, nil
}

// execute runs one operation and reports what happened. It never returns an
// error: one operation's problem is a line in the report, not the end of the
// suite, and the exit code is decided once at the end from the whole of it.
func (r *runner) execute(op operation.Operation) runResult {
	res := runResult{
		OperationID: operationName(op),
		Method:      op.Method,
		Path:        op.Path,
	}

	if op.IsMutation() && !r.allowMutations {
		return res.skip("a %s request may change server state: pass --allow-mutations to include it",
			op.Method)
	}

	creds, err := config.Resolve(op, r.doc, r.profile)
	if err != nil {
		return res.skip("%s", clierr.From(err).Message)
	}
	// Checked before the request is built rather than left to fail at exec
	// time, so an agent gets the variable to export and the server gets no
	// request it was always going to reject.
	if missing := missingCredentials(creds); len(missing) > 0 {
		r.noteMissing(missing)

		return res.skip("no credential for security %s",
			pluralise(len(missing), "scheme")+" "+strings.Join(missing, ", "))
	}

	data := r.gen.DataFor(op)
	req, err := request.Build(request.Inputs{
		Op:      op,
		Doc:     r.doc,
		Profile: r.profile,
		Creds:   creds,
		BaseURL: r.baseURL,
		Params:  flagPairs(data.Params),
		Headers: flagPairs(headersFor(data)),
		Body:    bodyFlag(data.Body),
		// Already built from this run's config, and shared with the entry that
		// goes to history so the two surfaces cannot disagree.
		Redactor: r.redactors.Request,
		// No stdin: `run` makes many requests and stdin can only be read once,
		// so there is nothing here for `--body -` to mean.
	})
	if err != nil {
		// A request that cannot be built is data talaria could not supply, not
		// an API that misbehaved, so it is a skip carrying the builder's own
		// message — which names the parameter.
		return res.skip("%s", clierr.From(err).Message)
	}

	warnQueryCredentials(r.stderr, r.warner, req)

	resp, execErr := curl.ExecuteWith(req, r.opts)
	// Recorded either way, as `call` records: a request that never completed is
	// still something that was tried.
	recordCall(r.stderr, r.store, corpus.SourceRun, req, resp, r.redactors)
	if execErr != nil {
		return res.fail("%s", clierr.From(execErr).Message)
	}

	view := redactResponse(resp, r.redactors.Response)
	timing := view.TimingMS
	res.Status, res.TimingMS = view.Status, &timing
	res.Validation = validateWith(r.stderr, r.validator, req, view)

	return res.judge()
}

// noteMissing records the credentials blocking an operation, without repeating
// one that already blocked another.
func (r *runner) noteMissing(missing []string) {
	for _, name := range missing {
		if !slicesContains(r.missing, name) {
			r.missing = append(r.missing, name)
		}
	}
}

// verdict turns the finished report into an exit code.
//
// A missing credential exits 5 whether or not --fail-on-error was given, as
// `auth check` does: it means the suite did not test the operation at all,
// which is a different thing from an API that answered badly, and an agent
// acts on it by exporting a variable rather than by reading the report.
func (r *runner) verdict(view runView, failOnError bool) error {
	if len(r.missing) > 0 {
		return clierr.CredentialMissing("no credential for security %s %s",
			pluralise(len(r.missing), "scheme"), strings.Join(r.missing, ", "))
	}
	if failOnError && view.Summary.Failed > 0 {
		return clierr.Validation("%s failed", pluralise(view.Summary.Failed, "operation"))
	}

	return nil
}

// skip marks an operation as not run, and why.
func (res runResult) skip(format string, a ...any) runResult {
	res.Outcome, res.Reason = outcomeSkipped, fmt.Sprintf(format, a...)

	return res
}

// fail marks an operation as run and wrong, and why.
func (res runResult) fail(format string, a ...any) runResult {
	res.Outcome, res.Reason = outcomeFailed, fmt.Sprintf(format, a...)

	return res
}

// judge decides the outcome of an operation that reached the server. The HTTP
// status is reported first when both apply, for `call --fail-on-error`'s
// reason: a 500 whose body does not match the error schema is a server that is
// down, not a server with a documentation problem.
func (res runResult) judge() runResult {
	switch {
	case res.Status >= http.StatusBadRequest:
		return res.fail("the server returned %s", statusText(res.Status))
	case res.Validation != nil && len(res.Validation.Errors) > 0:
		return res.fail("the response violates the spec: %s",
			pluralise(len(res.Validation.Errors), "validation error"))
	default:
		res.Outcome = outcomePassed

		return res
	}
}

// missingCredentials names the credentials an operation needs and does not
// have, in the form `scheme (set $VAR)` — the same phrasing `auth check` uses,
// because it answers the same question.
func missingCredentials(creds []config.Credential) []string {
	var missing []string
	for _, cred := range creds {
		if !cred.Present() {
			missing = append(missing, cred.Scheme+" (set "+cred.Ref.Symbolic()+")")
		}
	}

	return missing
}

// flagPairs renders generated data as the `name=value` strings request.Build
// parses, sorted so two runs of the same spec build byte-identical requests.
func flagPairs(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for name, value := range values {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)

	return out
}

// headersFor is the headers one request sends: the fixture's, plus the media
// type the body was generated as.
//
// Without that header the request builder falls back to the operation's first
// declared media type, which is not necessarily the one the body was generated
// from — a spec listing application/xml before application/json, or a converted
// Swagger 2.0 formData operation, then sends JSON bytes under a media type it
// is not. A fixture header wins, as a user-set --header wins in `call`.
func headersFor(data gen.Data) map[string]string {
	headers := make(map[string]string, len(data.Headers)+1)
	for name, value := range data.Headers {
		headers[name] = value
	}

	if len(data.Body) == 0 || data.ContentType == "" {
		return headers
	}

	for name := range headers {
		if strings.EqualFold(name, contentTypeHeader) {
			return headers
		}
	}
	headers[contentTypeHeader] = data.ContentType

	return headers
}

// bodyFlag renders a generated body as the --body value it stands in for, so
// the media type travels with it in headersFor rather than being re-derived
// from the spec. Generated and fixture bodies are always JSON, so neither can
// collide with --body's `@file` and `-` spellings.
func bodyFlag(body []byte) []string {
	if len(body) == 0 {
		return nil
	}

	return []string{string(body)}
}

// summarise counts the outcomes.
func summarise(results []runResult) runSummary {
	summary := runSummary{Total: len(results)}
	for _, res := range results {
		switch res.Outcome {
		case outcomePassed:
			summary.Passed++
		case outcomeFailed:
			summary.Failed++
		default:
			summary.Skipped++
		}
	}

	return summary
}

// runPayload builds the JSON view and the table from the same results.
//
// The table is one row per operation and then the summary, which is the shape a
// human reads top to bottom and a `grep -c failed` reads too. The last row is a
// single cell, so tabwriter leaves it unpadded rather than aligning a sentence
// into the status column.
func runPayload(view runView) output.Payload {
	rows := make([][]string, 0, len(view.Results)+1)
	for _, res := range view.Results {
		rows = append(rows, []string{
			res.Outcome,
			res.Method,
			res.Path,
			res.OperationID,
			statusCell(res.Status),
			res.Reason,
		})
	}
	rows = append(rows, []string{summaryLine(view.Summary)})

	return output.Payload{
		Data:  view,
		Table: output.Table{Rows: rows},
		JUnit: junitSuite(view),
	}
}

// junitSuite is the same results as a test suite, for `--report junit`.
//
// A skip stays a skip rather than becoming a failure: a DELETE left alone
// without --allow-mutations is correct behaviour, and a CI job that went red
// over it would be turned off within a week.
func junitSuite(view runView) output.Suite {
	cases := make([]output.Case, 0, len(view.Results))
	for _, res := range view.Results {
		c := output.Case{Name: res.OperationID, Seconds: res.seconds()}
		switch res.Outcome {
		case outcomeFailed:
			c.Failure = res.Reason
		case outcomeSkipped:
			c.Skipped = res.Reason
		}

		cases = append(cases, c)
	}

	return output.Suite{Name: "talaria run", Cases: cases}
}

// seconds is the operation's timing in the unit JUnit's time attribute is
// defined in. An operation that never reached a server took no measured time
// rather than an unknown one: it is reported as 0, with the <skipped> child
// saying why.
func (res runResult) seconds() float64 {
	if res.TimingMS == nil {
		return 0
	}

	return float64(*res.TimingMS) / 1000
}

// summaryLine is the one line a human is looking for.
func summaryLine(s runSummary) string {
	return fmt.Sprintf("%s: %d passed, %d failed, %d skipped",
		pluralise(s.Total, "operation"), s.Passed, s.Failed, s.Skipped)
}

// slicesContains reports whether values already holds value.
func slicesContains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}

	return false
}
