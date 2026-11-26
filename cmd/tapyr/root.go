package main

import (
	"context"
	"io"
	"log/slog"
	"net"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/selfobs"
)

// env holds the process dependencies shared by all commands.
type env struct {
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	// lookupEnv distinguishes unset from empty variables (secret checks).
	lookupEnv func(string) (string, bool)
	clock     clock.Clock
	// listen opens server listeners (net.ListenConfig.Listen; tests bind
	// free ports).
	listen func(ctx context.Context, network, address string) (net.Listener, error)
	// onRun, when set, observes the runs of tapyr run (tests).
	onRun func(delta int)
	// onInstances, when set, observes the number of scheduled instances of
	// tapyr run (tests).
	onInstances func(n int)
	// reload receives the configuration reload requests of tapyr run
	// (SIGHUP).
	reload <-chan struct{}
	// onReload, when set, observes the outcome of every reload (tests).
	onReload func(err error)
	// logger is the bootstrap logger, built from flags and environment
	// before any command runs.
	logger *slog.Logger
}

// globalFlags are the persistent flags available on every command.
type globalFlags struct {
	configPaths []string
	logLevel    string
	logFormat   string
	// providerPaths are the --provider-path directories of the commands
	// that load provider packages (validate, plan, record, provider list
	// and show).
	providerPaths []string
}

// addProviderPathFlag registers --provider-path on a command that loads a
// configuration, for the provider packages that are neither built in nor in
// the providerPaths of its Settings (a package being written, for example).
func addProviderPathFlag(cmd *cobra.Command, flags *globalFlags) {
	cmd.Flags().StringArrayVar(&flags.providerPaths, "provider-path", nil,
		"directory of provider packages, repeatable, after the providerPaths of the configuration")
}

func newRootCmd(e *env) *cobra.Command {
	flags := &globalFlags{}
	root := &cobra.Command{
		Use:   "tapyr",
		Short: "Declarative API-to-metrics gateway",
		Long: "Tapyr polls metric-oriented vendor APIs within their rate limits and exposes\n" +
			"or pushes the collected data as Prometheus or OTLP metrics.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			logger, err := newBootstrapLogger(e, flags)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			e.logger = logger
			return nil
		},
	}
	root.SetOut(e.stdout)
	root.SetErr(e.stderr)

	pf := root.PersistentFlags()
	pf.StringArrayVar(&flags.configPaths, "config", nil,
		"configuration file or directory, repeatable (env TAPYR_CONFIG)")
	pf.StringVar(&flags.logLevel, "log-level", "", "log level: debug, info, warn, error (env TAPYR_LOG_LEVEL)")
	pf.StringVar(&flags.logFormat, "log-format", "", "log format: json, text (env TAPYR_LOG_FORMAT)")

	root.AddCommand(
		newVersionCmd(e),
		newValidateCmd(e, flags),
		newSchemaCmd(e),
		newRunCmd(e, flags),
		newPlanCmd(e, flags),
		newSimCmd(e),
		newRecordCmd(e, flags),
		newProviderCmd(e, flags),
	)
	return root
}

// newBootstrapLogger builds the logger used until the configuration is
// loaded. Flags win over environment variables.
func newBootstrapLogger(e *env, flags *globalFlags) (*slog.Logger, error) {
	opts := selfobs.LogOptions{Level: slog.LevelInfo, Format: selfobs.FormatJSON, Clock: e.clock}
	if v := firstNonEmpty(flags.logLevel, e.getenv("TAPYR_LOG_LEVEL")); v != "" {
		level, err := selfobs.ParseLogLevel(v)
		if err != nil {
			return nil, err
		}
		opts.Level = level
	}
	if v := firstNonEmpty(flags.logFormat, e.getenv("TAPYR_LOG_FORMAT")); v != "" {
		format, err := selfobs.ParseLogFormat(v)
		if err != nil {
			return nil, err
		}
		opts.Format = format
	}
	return selfobs.NewLogger(e.stderr, opts), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func newProviderCmd(e *env, flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Inspect and lint provider packages",
	}
	cmd.AddCommand(
		newProviderListCmd(e, flags),
		newProviderShowCmd(e, flags),
		newProviderLintCmd(e),
		newProviderNewCmd(e),
	)
	return cmd
}
