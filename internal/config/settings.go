package config

import "github.com/invopop/jsonschema"

// Settings holds global settings. At most one document; every field has a
// default.
type Settings struct {
	APIVersion string       `json:"apiVersion" jsonschema:"required"`
	Kind       string       `json:"kind" jsonschema:"required"`
	Metadata   ObjectMeta   `json:"metadata" jsonschema:"required"`
	Spec       SettingsSpec `json:"spec" jsonschema:"required"`

	Source *Source `json:"-"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (Settings) JSONSchemaExtend(s *jsonschema.Schema) { extendEnvelope(s, KindSettings) }

func (s *Settings) kind() Kind      { return KindSettings }
func (s *Settings) name() string    { return string(s.Metadata.Name) }
func (s *Settings) source() *Source { return s.Source }

// SettingsSpec is the specification of Settings.
type SettingsSpec struct {
	// Server is the HTTP server (metrics, health, status API, UI).
	Server ServerSettings `json:"server,omitzero"`
	// State is the persistent state store.
	State StateSettings `json:"state,omitzero"`
	// ProviderPaths lists directories of user provider packages, in
	// addition to the built-in packages.
	ProviderPaths []string `json:"providerPaths,omitempty"`
	// Collection configures request execution.
	Collection CollectionSettings `json:"collection,omitzero"`
	// Budget configures the budget engine and the planner.
	Budget BudgetSettings `json:"budget,omitzero"`
	// Retry configures retries of transient errors.
	Retry RetrySettings `json:"retry,omitzero"`
	// CircuitBreaker configures the per-account circuit breakers.
	CircuitBreaker CircuitBreakerSettings `json:"circuitBreaker,omitzero"`
	// Mapping configures CEL evaluation limits.
	Mapping MappingSettings `json:"mapping,omitzero"`
	// SelfMetrics configures Tapyr's own metrics.
	SelfMetrics SelfMetricsSettings `json:"selfMetrics,omitzero"`
	// Log configures logging. Flags and environment variables win.
	Log LogSettings `json:"log,omitzero"`
}

// ServerSettings configures the HTTP server.
type ServerSettings struct {
	// Listen address.
	Listen string `json:"listen,omitempty" jsonschema:"default=127.0.0.1:9790"`
	// BasicAuth protects /api and /ui.
	BasicAuth *ServerBasicAuth `json:"basicAuth,omitempty"`
	// TLS serves HTTPS with the given certificate. A reverse proxy is the
	// recommended way to terminate TLS.
	TLS *ServerTLS `json:"tls,omitempty"`
	// UI is the embedded read-only web UI.
	UI UISettings `json:"ui,omitzero"`
	// Debug is the live debug endpoint exposing raw responses.
	Debug DebugSettings `json:"debug,omitzero"`
}

// ServerBasicAuth protects the server with HTTP basic authentication.
type ServerBasicAuth struct {
	// Username secret.
	Username SecretRef `json:"username" jsonschema:"required"`
	// Password secret.
	Password SecretRef `json:"password" jsonschema:"required"`
	// ProtectMetrics also protects /metrics and /metrics/self.
	ProtectMetrics bool `json:"protectMetrics,omitempty"`
}

// ServerTLS holds the server certificate files, relative to the declaring file.
type ServerTLS struct {
	// CertFile is the PEM certificate chain.
	CertFile string `json:"certFile" jsonschema:"required,minLength=1"`
	// KeyFile is the PEM private key.
	KeyFile string `json:"keyFile" jsonschema:"required,minLength=1"`
}

// UISettings configures the web UI.
type UISettings struct {
	// Enabled serves the UI under /ui/.
	Enabled *bool `json:"enabled,omitempty" jsonschema:"default=true"`
}

// DebugSettings configures the live debug endpoint.
type DebugSettings struct {
	// Enabled exposes the last raw responses (redacted) in the status API.
	Enabled bool `json:"enabled,omitempty"`
	// MaxBodyBytes truncates the raw responses kept for debugging.
	MaxBodyBytes int `json:"maxBodyBytes,omitempty" jsonschema:"minimum=1,default=1048576"`
}

// StateSettings configures the state store.
type StateSettings struct {
	// Path of the SQLite database, relative to the declaring file. The
	// built-in default is relative to the working directory.
	Path string `json:"path,omitempty" jsonschema:"default=./data/tapyr.db"`
}

// CollectionSettings configures request execution.
type CollectionSettings struct {
	// MaxConcurrentRequests bounds in-flight requests across all providers.
	MaxConcurrentRequests int `json:"maxConcurrentRequests,omitempty" jsonschema:"minimum=1,default=32"`
	// DefaultTimeout of a request when the provider sets none.
	DefaultTimeout Duration `json:"defaultTimeout,omitempty" jsonschema:"default=30s"`
	// Jitter spreads scheduled runs, from 0% to 50% of the interval.
	Jitter *Percent `json:"jitter,omitempty" jsonschema:"default=10%"`
	// SkipUnconsumed does not collect the jobs whose metrics no output
	// keeps (the processing rules of every output receiving them drop them
	// by name), nor the parents whose inventory no collected job takes: no
	// API call, no quota spent (ADR 0013). Set it to false to keep every
	// job running, for example as an authentication probe or for the live
	// debug.
	SkipUnconsumed *bool `json:"skipUnconsumed,omitempty" jsonschema:"default=true"`
}

// BudgetSettings configures the budget engine and the planner.
type BudgetSettings struct {
	// RetryReserve is the share of every quota reserved for retries, from 0% to 50%.
	RetryReserve *Percent `json:"retryReserve,omitempty" jsonschema:"default=10%"`
	// ReplanInterval is the period of mid-window replanning.
	ReplanInterval Duration `json:"replanInterval,omitempty" jsonschema:"default=15m"`
	// ReplanOnProjection replans at once when the consumption projected by
	// the current plan until the end of a quota window exceeds its remaining
	// budget, for example when another client starts spending the same
	// quota, instead of waiting for the next replanInterval.
	ReplanOnProjection *bool `json:"replanOnProjection,omitempty" jsonschema:"default=true"`
	// MaxStretch caps the interval stretch factor applied to a priority
	// class before the planner moves to the next class.
	MaxStretch float64 `json:"maxStretch,omitempty" jsonschema:"minimum=1,default=12"`
}

// RetrySettings configures retries with exponential backoff and full jitter.
type RetrySettings struct {
	// MaxAttempts per request, including the first attempt.
	MaxAttempts int `json:"maxAttempts,omitempty" jsonschema:"minimum=1,default=4"`
	// InitialBackoff before the first retry.
	InitialBackoff Duration `json:"initialBackoff,omitempty" jsonschema:"default=1s"`
	// MaxBackoff caps the backoff.
	MaxBackoff Duration `json:"maxBackoff,omitempty" jsonschema:"default=60s"`
	// Multiplier of the backoff between attempts.
	Multiplier float64 `json:"multiplier,omitempty" jsonschema:"minimum=1,default=2"`
}

// CircuitBreakerSettings configures the per-account circuit breakers.
type CircuitBreakerSettings struct {
	// FailureThreshold is the number of consecutive transient failures that
	// opens the circuit.
	FailureThreshold int `json:"failureThreshold,omitempty" jsonschema:"minimum=1,default=5"`
	// Cooldown before the half-open probe.
	Cooldown Duration `json:"cooldown,omitempty" jsonschema:"default=2m"`
	// MaxCooldown caps the cooldown, which doubles on repeated openings.
	MaxCooldown Duration `json:"maxCooldown,omitempty" jsonschema:"default=30m"`
}

// MappingSettings configures CEL evaluation limits.
type MappingSettings struct {
	// MaxCost is the CEL cost limit per expression evaluation.
	MaxCost int64 `json:"maxCost,omitempty" jsonschema:"minimum=1,default=1000000"`
	// MaxItemsPerRun caps the items extracted by a run.
	MaxItemsPerRun int `json:"maxItemsPerRun,omitempty" jsonschema:"minimum=1,default=100000"`
	// MaxSamplesPerRun caps the samples produced by a run.
	MaxSamplesPerRun int `json:"maxSamplesPerRun,omitempty" jsonschema:"minimum=1,default=200000"`
	// LabelConflict resolves labels set both by the mapping and by
	// account, target or job labels: override (job > target > account >
	// mapping) or error.
	LabelConflict string `json:"labelConflict,omitempty" jsonschema:"enum=override,enum=error,default=override"`
}

// SelfMetricsSettings configures Tapyr's own metrics.
type SelfMetricsSettings struct {
	// Outputs that also receive the self-metrics (always served on /metrics/self).
	Outputs []Name `json:"outputs,omitempty"`
	// Interval between two pushes of the self-metrics to Outputs.
	Interval Duration `json:"interval,omitempty" jsonschema:"default=30s"`
}

// LogSettings configures logging.
type LogSettings struct {
	// Level: debug, info, warn or error.
	Level string `json:"level,omitempty" jsonschema:"enum=debug,enum=info,enum=warn,enum=error,default=info"`
	// Format: json or text.
	Format string `json:"format,omitempty" jsonschema:"enum=json,enum=text,default=json"`
}
