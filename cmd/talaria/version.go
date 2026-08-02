package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the talaria version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Explicitly OutOrStdout: cobra's cmd.Print* family defaults to
			// stderr, and the version belongs on stdout.
			fmt.Fprintf(cmd.OutOrStdout(), "talaria %s\n", version)
			return nil
		},
	}
}
