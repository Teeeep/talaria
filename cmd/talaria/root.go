package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/secret"
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
		// Args and RunE work together to give a typo exit 2 instead of 1. Left
		// to itself cobra reports an unknown command from Find, as a bare error
		// that exitCode can only read as a request failure — and exit 1 is the
		// code AGENT.md calls transient and worth retrying, so a typo would send
		// an agent into a retry loop. Rejecting the argument in Args instead
		// classifies it, but cobra reaches ValidateArgs only on a *runnable*
		// command, so the root needs a RunE it would otherwise do without.
		Args: unknownCommand,
		// SuggestionsFor, unlike cobra's own unknown-command path, does not
		// default this: left at 0 it would only ever suggest by prefix, and
		// `talaria vrsion` — the shape of a typo — would get nothing back.
		SuggestionsMinimumDistance: 2,
		// What the non-runnable root used to get for free: bare `talaria` prints
		// the help and exits 0. That is how a human finds the commands.
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
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

	// Persistent alongside --base-url because it is the answer to what
	// --base-url does: a credential goes only to a host the spec declares, and
	// this is how a human says otherwise. call, auth check and history replay
	// all need it, and history replay reads it before it reads the entry.
	root.PersistentFlags().StringArray("allow-host", nil,
		"also send credentials to this host, e.g. localhost:9000 (repeatable)")

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
	root.AddCommand(newAuthCmd())
	root.AddCommand(newHistoryCmd())

	return root
}

// invocation is the per-command context every request-making command reads:
// the config file, the profile --profile names, the redaction firewalls that
// profile configures, and the two persistent flags that decide where a request
// goes and which hosts its credentials are bound to.
//
// It is built once per RunE and passed down. Each part used to be derived at
// the point it was needed, so one `call` selected the profile three times and
// constructed two independent pairs of redactors from the same settings —
// objects nothing but a shared constructor kept in step.
type invocation struct {
	Config  *config.Config
	Profile *config.Profile
	// Redactors is the pair of firewalls an entry passes through on its way to
	// disk, extended with whatever the config file added. Request and response
	// share the header list: a name worth hiding on the way back is worth hiding
	// on the way out.
	Redactors corpus.Redactors
	// BaseURL is --base-url and AllowHosts is --allow-host. They are read
	// together because every caller needs both: --base-url without --allow-host
	// is what withholds a credential, and reading one without the other is how a
	// command comes to report a host it will not actually send to.
	BaseURL    string
	AllowHosts []string
}

func newInvocation(cmd *cobra.Command) (*invocation, error) {
	// The config is read whether or not --profile was given: the redaction lists
	// are a security setting, and one that only takes effect when you happen to
	// be using a profile is one that silently does not.
	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}

	prof, err := selectProfile(cmd, cfg)
	if err != nil {
		return nil, err
	}

	baseURL, err := flagString(cmd, "base-url")
	if err != nil {
		return nil, err
	}

	allowHosts, err := cmd.Flags().GetStringArray("allow-host")
	if err != nil {
		return nil, clierr.Usage("%w", err)
	}

	return &invocation{
		Config:  cfg,
		Profile: prof,
		Redactors: corpus.Redactors{
			Request:  secret.NewRedactor(cfg.Redact.Headers...),
			Response: secret.NewResponseRedactor(cfg.Redact.Headers, cfg.Redact.BodyPaths),
		},
		BaseURL:    baseURL,
		AllowHosts: allowHosts,
	}, nil
}

// history is the store, opened with the recording setting the selected profile
// asks for. Reading never consults it — history written before recording was
// switched off is still history — but call and replay write, so it has to be
// right.
func (in *invocation) history() *corpus.Store {
	// The empty dir is the default state directory; internal/corpus locates it.
	return corpus.New("", in.Profile.HistoryEnabled())
}

// selectProfile picks the profile named by --profile out of an already-loaded
// config, or returns nil when no name was given.
func selectProfile(cmd *cobra.Command, cfg *config.Config) (*config.Profile, error) {
	name, err := flagString(cmd, "profile")
	if err != nil || name == "" {
		return nil, err
	}

	return cfg.Profile(name)
}

// flagString reads a string flag, classifying cobra's bare error as the bad
// invocation it is.
func flagString(cmd *cobra.Command, name string) (string, error) {
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		return "", clierr.Usage("%w", err)
	}

	return value, nil
}

// unknownCommand rejects any positional argument on the root command, which by
// then can only be a command that does not exist. It reproduces cobra's own
// wording, and carries the near misses as structured alternatives rather than
// as prose appended to the message (§3.1: "what failed, why, valid
// alternatives") — a typo is the one error an agent can correct unaided.
func unknownCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}

	return clierr.Usage("unknown command %q for %q", args[0], cmd.CommandPath()).
		WithAlternatives(cmd.SuggestionsFor(args[0])...)
}

// groupCommand configures a parent-only command — one that exists to hold
// subcommands and does nothing on its own — so that both ways of stopping short
// of a real command exit 2 with the structured error every other bad invocation
// produces. Cobra's default is help on stdout and exit 0, which tells an agent
// that asked for JSON that it succeeded and then hands it prose (DESIGN.md §4;
// §3.1 makes the exit code the thing agents branch on).
//
// It returns cmd so a constructor can wrap its literal, and it is a helper
// rather than three lines in newAuthCmd so the next command group cannot be
// added without the behaviour.
func groupCommand(cmd *cobra.Command) *cobra.Command {
	// A group is not runnable, and cobra bails out to the help before it reaches
	// ValidateArgs on a command that is not — so, exactly as on the root, Args
	// only classifies the typo if there is a RunE for it to guard.
	cmd.Args = unknownCommand
	// Without this the suggestions are prefix-only; see the root's own comment.
	cmd.SuggestionsMinimumDistance = 2
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		// The subcommands go in valid_alternatives rather than being printed as
		// a help block: stderr carries one JSON object, and prose in front of it
		// is what an agent parsing stderr would choke on. It is the same answer
		// help would have given, in the shape the caller asked for.
		return clierr.Usage("%q requires a subcommand", cmd.CommandPath()).
			WithAlternatives(subcommandNames(cmd)...)
	}

	return cmd
}

// subcommandNames lists the commands under cmd that a caller may invoke, in the
// order cobra shows them. Hidden and deprecated ones are left out: naming them
// as valid alternatives would send an agent straight back into an error.
func subcommandNames(cmd *cobra.Command) []string {
	names := make([]string, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		if sub.IsAvailableCommand() {
			names = append(names, sub.Name())
		}
	}

	return names
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
//
// The signal context is what makes a Ctrl-C or a `kill` orderly rather than
// abrupt. Without it nothing catches the signal, the process dies where it
// stands, and none of the deferred cleanup runs: curl is reparented to init and
// finishes the request talaria was told to stop — a write that carries on after
// the caller cancelled it — while the capture directory holding the raw,
// unredacted response stays on disk.
func run(args []string, stdout, stderr io.Writer) int {
	// Anything a previous SIGKILL left behind goes first: that signal cannot be
	// caught, so the sweep is the only cleanup those files will ever get.
	curl.SweepStale()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runContext(ctx, args, stdout, stderr)
}

// runContext is run with the cancellation source given rather than taken from
// the process's signals, so a test can cancel a call without signalling itself.
//
// The context reaches every command through cobra, which is how internal/curl
// gets the one it kills the subprocess with.
func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := newRootCmd()
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)

	return exitCode(root.ExecuteContext(ctx), stderr)
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
