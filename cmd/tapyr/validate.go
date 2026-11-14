package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/auth/plugins"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/provider"
	"github.com/kentrow/tapyr/internal/runtime"
)

func newValidateCmd(e *env, flags *globalFlags) *cobra.Command {
	var checkSecrets bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate the configuration",
		Long: "Load the configuration, validate every document against its JSON Schema,\n" +
			"check references and semantic rules, and report every error as\n" +
			"file:line:column. Exit code 2 when the configuration is invalid.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, err := loadConfig(e, flags)
			if err != nil {
				return err
			}
			tokens, err := offlineAuth(e.clock)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			graph, err := compileGraph(e, cfg, runtime.Options{Auth: tokens})
			if err != nil {
				return err
			}
			warnNotCollected(e, graph)
			if checkSecrets {
				diags := config.CheckSecrets(cfg, config.SecretSource{LookupEnv: e.lookupEnv, ReadFile: os.ReadFile})
				if len(diags) > 0 {
					return invalidConfig(e, diags)
				}
			}
			_, err = fmt.Fprintf(e.stdout, "configuration is valid: %s\n", summary(cfg))
			return err
		},
	}
	cmd.Flags().BoolVar(&checkSecrets, "check-secrets", false,
		"also check that every referenced secret (environment variable or file) is set and not empty")
	addProviderPathFlag(cmd, flags)
	return cmd
}

// configPaths returns the --config flags, else TAPYR_CONFIG split with the
// OS path list separator.
func configPaths(e *env, flags *globalFlags) []string {
	if len(flags.configPaths) > 0 {
		return flags.configPaths
	}
	if v := e.getenv("TAPYR_CONFIG"); v != "" {
		return filepath.SplitList(v)
	}
	return nil
}

// loadConfig loads and validates the configuration. Diagnostics are printed
// to stderr and turned into exit code 2.
func loadConfig(e *env, flags *globalFlags) (*config.Config, error) {
	paths := configPaths(e, flags)
	if len(paths) == 0 {
		return nil, &exitError{code: exitFailure, err: errors.New("no configuration path given (use --config or TAPYR_CONFIG)")}
	}
	loader, builtin, err := newLoader()
	if err != nil {
		return nil, err
	}
	var extra []*config.Package
	for _, dir := range flags.providerPaths {
		found, err := provider.FromPath(loader, dir)
		if err != nil {
			return nil, &exitError{code: exitInvalidConfig, err: err}
		}
		extra = append(extra, found...)
	}
	cfg, err := provider.LoadConfig(loader, paths, append(extra, builtin...))
	if err != nil {
		if diags, ok := errors.AsType[config.Diagnostics](err); ok {
			return nil, invalidConfig(e, diags)
		}
		return nil, &exitError{code: exitInvalidConfig, err: err}
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(e.stderr, "warning: %s\n", w)
	}
	return cfg, nil
}

// compileGraph compiles the configuration into the runtime graph.
// Compilation errors are printed to stderr and turned into exit code 2.
func compileGraph(e *env, cfg *config.Config, opts runtime.Options) (*runtime.Graph, error) {
	graph, err := runtime.Compile(cfg, opts)
	if err != nil {
		if diags, ok := errors.AsType[config.Diagnostics](err); ok {
			return nil, invalidConfig(e, diags)
		}
		return nil, &exitError{code: exitFailure, err: err}
	}
	return graph, nil
}

// warnNotCollected prints a warning for every job that is not collected
// (ADR 0013): a processing rule broader than intended shows at once.
func warnNotCollected(e *env, graph *runtime.Graph) {
	for _, j := range graph.NotCollectedJobs() {
		fmt.Fprintf(e.stderr, "warning: job %q (account %s) is not collected: %s (settings.collection.skipUnconsumed)\n",
			j.Name, j.Account, j.NotCollected)
	}
}

// newLoader returns the configuration loader and the built-in packages.
func newLoader() (*config.Loader, []*config.Package, error) {
	loader, err := config.NewLoader()
	if err != nil {
		return nil, nil, &exitError{code: exitFailure, err: fmt.Errorf("initializing the configuration loader: %w", err)}
	}
	builtin, err := provider.Builtin(loader)
	if err != nil {
		return nil, nil, &exitError{code: exitFailure, err: fmt.Errorf("loading built-in provider packages: %w", err)}
	}
	return loader, builtin, nil
}

func invalidConfig(e *env, diags config.Diagnostics) error {
	for _, d := range diags {
		fmt.Fprintln(e.stderr, d)
	}
	noun := "errors"
	if len(diags) == 1 {
		noun = "error"
	}
	fmt.Fprintf(e.stderr, "configuration is invalid: %d %s\n", len(diags), noun)
	return &exitError{code: exitInvalidConfig, silent: true, err: diags}
}

func summary(cfg *config.Config) string {
	return fmt.Sprintf("%s, %s, %s, %s, %s",
		plural(len(cfg.Providers), "provider"), plural(len(cfg.JobTemplates), "job template"),
		plural(len(cfg.Accounts), "account"), plural(len(cfg.Jobs), "job"), plural(len(cfg.Outputs), "output"))
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// offlineAuth is the token manager of the commands that compile a
// configuration without running it (validate, plan, provider lint): it
// knows the auth plugins, to validate their configuration, and never
// requests a token.
func offlineAuth(clk clock.Clock) (*auth.Manager, error) {
	return auth.NewManager(auth.ManagerOptions{Clock: clk, Plugins: plugins.Builtin()})
}
