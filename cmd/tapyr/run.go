package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/auth/plugins"
	"github.com/kentrow/tapyr/internal/collector"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/selfobs"
	"github.com/kentrow/tapyr/internal/state"
	"github.com/kentrow/tapyr/ui"
)

// saltKey is the metadata key of the credential salt.
const saltKey = "credential_salt"

// instanceIDKey is the metadata key of the instance identifier
// (service.instance.id of OTLP).
const instanceIDKey = "instance_id"

func newRunCmd(e *env, flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the collector",
		Long: "Load and compile the configuration, then collect: run every job at its\n" +
			"interval through the budget engine and expose the samples on /metrics.\n" +
			"SIGHUP reloads the configuration; SIGINT and SIGTERM shut down gracefully.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := loadConfig(e, flags)
			if err != nil {
				return err
			}
			// Compile once to report every problem (secrets included) before
			// touching the state store, then again with the credential salt.
			// Secrets are compiled twice at startup: short ones are counted
			// once each.
			redactor, short := selfobs.NewRedactor(), map[string]bool{}
			var smu sync.Mutex
			secrets := &config.SecretSource{LookupEnv: e.lookupEnv, ReadFile: os.ReadFile, OnResolve: func(v string) {
				if !redactor.Add(v) {
					smu.Lock()
					short[v] = true
					smu.Unlock()
				}
			}}
			check, err := offlineAuth(e.clock)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			if _, err := compileGraph(e, cfg, runtime.Options{Secrets: secrets, Auth: check}); err != nil {
				return err
			}
			store, err := state.OpenSQLite(ctx, cfg.Settings.Spec.State.Path, false)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			defer store.Close()
			salt, err := credentialSalt(ctx, store)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			instance, err := instanceID(ctx, store)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			logger, err := settingsLogger(e, flags, &cfg.Settings.Spec.Log, redactor)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			smu.Lock()
			if len(short) > 0 {
				logger.Warn("secrets too short to be redacted from logs", "component", "run", "secrets", len(short),
					"min_length", selfobs.MinRedactedLength)
			}
			smu.Unlock()
			c, err := collector.New(collector.Options{
				Clock: e.clock, Listen: e.listen, Store: store, Logger: logger, Redactor: redactor, Secrets: secrets,
				Salt: salt, InstanceID: instance, StateKey: e.getenv("TAPYR_STATE_KEY"), Plugins: plugins.Builtin(),
				Load: func() (*config.Config, error) { return loadConfig(e, flags) },
				Compile: func(cfg *config.Config, opts runtime.Options) (*runtime.Graph, error) {
					return compileGraph(e, cfg, opts)
				},
				UI: ui.Dist(), Reload: e.reload, OnReload: e.onReload, OnRun: e.onRun, OnInstances: e.onInstances,
			})
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			graph, err := c.Compile(cfg)
			if err != nil {
				return err
			}
			if err := c.Run(ctx, cfg, graph); err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			return nil
		},
	}
}

// credentialSalt returns the salt of credential keys, created on first use.
func credentialSalt(ctx context.Context, store state.Store) ([]byte, error) {
	v, ok, err := store.Meta(ctx, saltKey)
	if err != nil {
		return nil, err
	}
	if ok {
		return hex.DecodeString(v)
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, store.SetMeta(ctx, saltKey, hex.EncodeToString(salt))
}

// instanceID returns the identifier of this Tapyr instance, created on
// first use.
func instanceID(ctx context.Context, store state.Store) (string, error) {
	v, ok, err := store.Meta(ctx, instanceIDKey)
	if err != nil || ok {
		return v, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	v = hex.EncodeToString(id)
	return v, store.SetMeta(ctx, instanceIDKey, v)
}

// settingsLogger applies the Settings log configuration where neither a
// flag nor an environment variable sets it.
func settingsLogger(e *env, flags *globalFlags, s *config.LogSettings, redactor *selfobs.Redactor) (*slog.Logger, error) {
	level := firstNonEmpty(flags.logLevel, e.getenv("TAPYR_LOG_LEVEL"), s.Level)
	format := firstNonEmpty(flags.logFormat, e.getenv("TAPYR_LOG_FORMAT"), s.Format)
	l, err := selfobs.ParseLogLevel(level)
	if err != nil {
		return nil, err
	}
	f, err := selfobs.ParseLogFormat(format)
	if err != nil {
		return nil, err
	}
	return selfobs.NewLogger(e.stderr, selfobs.LogOptions{Level: l, Format: f, Clock: e.clock, Redactor: redactor}), nil
}
