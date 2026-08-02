package main

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/spec"
)

// version is the binary's version string. It defaults to "dev" and is
// overridden at build time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

// newRootCmd builds the talaria command tree. Every command is constructed
// per-call rather than living in a package-level var so tests can run against
// an isolated tree with their own output writers.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "talaria",
		Short: "Point it at any API doc; your agent works the API and never sees your credentials",
		Long: "talaria turns any OpenAPI/Swagger spec into an explorable API client, a\n" +
			"spec-driven tester, and a digital twin — without exposing credentials to\n" +
			"the agent driving it.",
		Version: version,
		// Agents parse stderr. Cobra's default behaviour of dumping the usage
		// block on every error, and of printing the error itself on top of the
		// caller doing so, is noise.
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Registered once, here, so every present and future subcommand accepts it
	// (DESIGN.md §3.1: "--output json on every command"). The empty default
	// means "decide from the terminal"; see output.Resolve.
	root.PersistentFlags().String("output", "",
		"output format: json|pretty|tsv (default: pretty on a terminal, json when piped)")

	// Also persistent: every spec-reading command takes the same spec the same
	// way, and an agent that exports $TALARIA_SPEC once should not have to
	// repeat itself on each call (DESIGN.md §4). Precedence between the three
	// sources lives in spec.Resolve, not here.
	root.PersistentFlags().String("spec", "",
		"OpenAPI/Swagger spec to use: file path or URL (default: $"+spec.EnvSpec+")")

	// Persistent for the same reason: call, run and auth check all take them
	// (DESIGN.md §4), and registering either per-command would shadow the other
	// registration silently. Commands that read no config simply ignore them.
	root.PersistentFlags().String("profile", "",
		"named profile from the config file: base-url + headers + auth")
	root.PersistentFlags().String("base-url", "",
		"override the spec's server URL")

	// A bad flag is a bad invocation, not a failed request; without this it
	// would reach the translator as a bare error and exit 1.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return clierr.Usage("%s", err)
	})

	// The Args counterpart of SetFlagErrorFunc lives per-command, in usageArgs;
	// cobra has no tree-wide hook for it.

	// Validated once for the whole tree, so an unusable --output fails before a
	// command does any work — including on commands that ignore the format.
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		_, err := resolveFormat(cmd)
		return err
	}

	root.AddCommand(newVersionCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newDescribeCmd())
	root.AddCommand(newSearchCmd())
	root.AddCommand(newUsesCmd())
	root.AddCommand(newCallCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newAuthCmd())
	root.AddCommand(newHistoryCmd())

	return root
}

// usageArgs wraps a positional-argument validator so a wrong argument count
// exits 2 like every other bad invocation. Cobra returns a bare error here, and
// SetFlagErrorFunc does not cover it, so without this the caller would see the
// unclassified-failure code 1.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return clierr.Usage("%w", err)
		}
		return nil
	}
}

// resolveFormat resolves --output for cmd against the TTY-ness of its stdout.
// internal/output deliberately returns a bare error; it becomes a usage error
// here, where the exit-code contract lives, carrying the valid values so an
// agent can correct itself without reading the help text.
func resolveFormat(cmd *cobra.Command) (output.Format, error) {
	explicit, err := cmd.Flags().GetString("output")
	if err != nil {
		return "", clierr.Usage("%w", err)
	}

	format, err := output.Resolve(explicit, output.IsTTY(cmd.OutOrStdout()))
	if err != nil {
		return "", clierr.Usage("%w", err).WithAlternatives(output.Formats()...)
	}
	return format, nil
}

// run executes the talaria command tree and returns the process exit code.
// It deliberately does not call os.Exit — main is the only place that does —
// so every exit code in the CLI is assertable in-process.
func run(args []string, stdout, stderr io.Writer) int {
	root := newRootCmd()
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)

	return exitCode(root.Execute(), stderr)
}

// exitCode renders err as structured JSON on stderr and returns the code to
// exit with. An error that was never classified is a request failure (1).
func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return int(clierr.CodeOK)
	}

	clierr.Render(stderr, err)
	return int(clierr.From(err).Code)
}
