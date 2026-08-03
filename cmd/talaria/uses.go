package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
)

// usesView is the JSON payload. The schema name is under "name" rather than
// "schema" because "schema" is the envelope's own version field.
type usesView struct {
	Name       string          `json:"name"`
	Operations []operation.Use `json:"operations"`
}

func newUsesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uses [spec] <schema>",
		Short: "List the operations that reach a component schema",
		Long: "Find every operation that touches a schema, in a parameter, a request\n" +
			"body or a response. References are followed: an operation returning a\n" +
			"PetList is reported for Pet, marked as an indirect use.",
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			// The schema name is always last, as the operationId is for describe.
			name := args[len(args)-1]

			index, err := loadIndex(cmd, args[:len(args)-1])
			if err != nil {
				return err
			}

			uses, err := index.Uses(name)
			if err != nil {
				return err
			}

			return output.New(format, cmd.OutOrStdout()).Render(usesPayload(name, uses))
		},
	}

	return cmd
}

func usesPayload(name string, uses []operation.Use) output.Payload {
	rows := make([][]string, 0, len(uses))
	for _, u := range uses {
		rows = append(rows, []string{u.Method, u.Path, u.ID, strings.Join(u.Where, ","), reachLabel(u.Direct)})
	}

	return output.Payload{
		Data:  usesView{Name: name, Operations: uses},
		Table: output.Table{Rows: rows},
	}
}

// reachLabel names how the operation gets to the schema. It earns its column:
// "indirect" is the reader's cue that the schema is nested inside whatever the
// endpoint actually declares, so the field will not be at the top level.
func reachLabel(direct bool) string {
	if direct {
		return "direct"
	}

	return "indirect"
}
