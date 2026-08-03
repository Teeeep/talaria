package main

import (
	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
)

// searchView is the JSON payload. The query is echoed back so a transcript of
// several searches stays readable without the invocations next to it.
type searchView struct {
	Query   string             `json:"query"`
	Results []operation.Result `json:"results"`
}

func newSearchCmd() *cobra.Command {
	var kind string

	cmd := &cobra.Command{
		Use:   "search [spec] <query>",
		Short: "Find operations, schemas and parameters by substring",
		Long: "Search a spec for a term across operation ids, paths, summaries and\n" +
			"descriptions, component schema names and parameter names. Matching is\n" +
			"case-insensitive, and results are ranked with name matches first.\n\n" +
			"A term that matches nothing is not an error: an empty result set is the\n" +
			"answer that this API has no such concept.",
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			// Validated before the spec is loaded: a bad --kind is a bad
			// invocation whatever the spec turns out to contain.
			searchKind, err := operation.ParseKind(kind)
			if err != nil {
				return clierr.Usage("%w", err).WithAlternatives(operation.Kinds()...)
			}

			// The query is always last: with one argument the spec comes from
			// --spec or the environment, with two it is the first.
			query := args[len(args)-1]

			index, err := loadIndex(cmd, args[:len(args)-1])
			if err != nil {
				return err
			}

			return output.New(format, cmd.OutOrStdout()).
				Render(searchPayload(query, index.Search(query, searchKind)))
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "",
		"restrict results to one kind: operation|schema|param (default: all three)")

	return cmd
}

func searchPayload(query string, results []operation.Result) output.Payload {
	rows := make([][]string, 0, len(results))
	for _, r := range results {
		rows = append(rows, []string{string(r.Kind), r.Name, r.Where, r.Summary})
	}

	return output.Payload{
		Data:  searchView{Query: query, Results: results},
		Table: output.Table{Rows: rows},
	}
}
