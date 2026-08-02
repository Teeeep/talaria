package main

import (
	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/spec"
)

// maxPrettyWidth bounds one pretty `list` line. Progressive disclosure is the
// product (DESIGN.md §3.2): a line that wraps in an 80–100 column terminal, or
// in an agent's transcript, costs more context than the operation is worth, so
// the summary is truncated to fit rather than allowed to wrap.
const maxPrettyWidth = 100

// minSummaryWidth is the shortest summary worth printing. Below it the method,
// path and id have already eaten the line, and two words followed by an
// ellipsis inform nobody — the summary is dropped instead.
const minSummaryWidth = 12

// summaryColumn is the index of the summary in a table row — the one column
// that can be shortened to make a line fit.
const summaryColumn = 3

// listEntry is one operation as `list` reports it: the addressing information
// plus just enough prose to choose between two endpoints. Everything else is
// what `describe` is for.
type listEntry struct {
	ID      string   `json:"id"`
	Method  string   `json:"method"`
	Path    string   `json:"path"`
	Tags    []string `json:"tags"`
	Summary string   `json:"summary"`
}

// listView is the JSON payload. It is a struct rather than a bare slice so the
// operations sit at the top level next to the schema field.
type listView struct {
	Operations []listEntry `json:"operations"`
}

func newListCmd() *cobra.Command {
	var tag string

	cmd := &cobra.Command{
		Use:   "list [spec]",
		Short: "List a spec's operations, one compact line each",
		Long: "List every operation in a spec: method, path, operationId and a short\n" +
			"summary. Operations the spec did not name get a synthesised id, which is\n" +
			"as callable as an authored one.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			index, err := loadIndex(cmd, args)
			if err != nil {
				return err
			}

			ops := index.Operations()
			if tag != "" {
				ops = index.ByTag(tag)
			}

			return output.New(format, cmd.OutOrStdout()).Render(listPayload(ops, format))
		},
	}

	// Local rather than persistent: only the commands that enumerate operations
	// have a tag to filter on.
	cmd.Flags().StringVar(&tag, "tag", "", "only operations carrying this tag")

	return cmd
}

// listPayload builds the JSON view and the table together. The table is
// format-aware because only the pretty renderer has a width budget: TSV is
// piped into other programs, which want the summary whole.
func listPayload(ops []operation.Operation, format output.Format) output.Payload {
	view := listView{Operations: make([]listEntry, 0, len(ops))}
	rows := make([][]string, 0, len(ops))

	for _, op := range ops {
		tags := op.Tags
		if tags == nil {
			// An untagged operation renders "tags": [], not null: agents parse
			// this, and a field that changes type between specs is a trap.
			tags = []string{}
		}
		view.Operations = append(view.Operations, listEntry{
			ID:      op.ID,
			Method:  op.Method,
			Path:    op.Path,
			Tags:    tags,
			Summary: op.Summary,
		})

		rows = append(rows, []string{op.Method, op.Path, op.ID, op.Summary})
	}

	if format == output.FormatPretty {
		fitSummaries(rows)
	}

	// No headers: §3.2 asks for one compact line per operation, and a header
	// line is one more thing every consumer has to skip. `--output json` is
	// where the fields are labelled.
	return output.Payload{
		Data:  view,
		Table: output.Table{Rows: rows},
	}
}

// fitSummaries truncates the summary column in place so no rendered line
// exceeds maxPrettyWidth. The budget is what is left after the three columns
// that cannot be shortened, measured at the width tabwriter will pad them to —
// the widest cell in each — because that padding is what a row-local
// measurement would miss.
func fitSummaries(rows [][]string) {
	const padding = 2 // tabwriter is constructed with two spaces between columns.

	fixed := 0
	for col := 0; col < summaryColumn; col++ {
		width := 0
		for _, row := range rows {
			width = max(width, len([]rune(row[col])))
		}
		fixed += width + padding
	}

	budget := maxPrettyWidth - fixed
	for _, row := range rows {
		if budget < minSummaryWidth {
			row[summaryColumn] = ""

			continue
		}
		row[summaryColumn] = truncate(row[summaryColumn], budget)
	}
}

// truncate shortens s to at most max runes, marking the cut with an ellipsis so
// a reader can tell the summary continues.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}

	return string(runes[:max-1]) + "…"
}

// loadIndex resolves the spec from the positional argument, --spec or the
// environment, loads it and indexes its operations. Every spec-reading command
// starts this way, so the precedence and the error codes are decided once.
func loadIndex(cmd *cobra.Command, args []string) (*operation.Index, error) {
	var arg string
	if len(args) > 0 {
		arg = args[0]
	}

	flag, err := cmd.Flags().GetString("spec")
	if err != nil {
		return nil, clierr.Usage("%w", err)
	}

	ref, err := spec.Resolve(arg, flag)
	if err != nil {
		return nil, err
	}

	doc, err := spec.Load(ref)
	if err != nil {
		return nil, err
	}

	return operation.NewIndexFor(doc), nil
}
