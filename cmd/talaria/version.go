package main

import (
	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/output"
)

// versionView is the JSON payload. A struct rather than a bare string so the
// version sits at the top level next to the schema field, where an agent
// checking compatibility can read it without unwrapping anything.
type versionView struct {
	Version string `json:"version"`
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the talaria version",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			// One cell rather than two columns: the pretty rendering stays the
			// bare `talaria <version>` line, and tabwriter has nothing to pad.
			return output.New(format, cmd.OutOrStdout()).Render(output.Payload{
				Data:  versionView{Version: version},
				Table: output.Table{Rows: [][]string{{"talaria " + version}}},
			})
		},
	}
}
