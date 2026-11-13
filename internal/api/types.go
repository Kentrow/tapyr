package api

import (
	"encoding/json"
	"time"
)

// The types of the API responses. Field names are snake_case and stable;
// times are RFC 3339 (ISO 8601) in UTC; durations are seconds. They never
// carry secrets.

// Status is GET /api/v1/status.
type Status struct {
	Version          string    `json:"version"`
	Revision         string    `json:"revision"`
	GoVersion        string    `json:"go_version"`
	StartedAt        time.Time `json:"started_at"`
	UptimeSeconds    float64   `json:"uptime_seconds"`
	ConfigHash       string    `json:"config_hash"`
	LastReload       Reload    `json:"last_reload"`
	Ready            bool      `json:"ready"`
	ReadinessReasons []string  `json:"readiness_reasons"`
	PlanFeasible     bool      `json:"plan_feasible"`
	Instances        int       `json:"instances"`
}

// Reload is the last configuration load or reload.
type Reload struct {
	At      time.Time `json:"at"`
	Success bool      `json:"success"`
	Error   string    `json:"error,omitempty"`
}

// Provider is one provider in use.
type Provider struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Accounts    int    `json:"accounts"`
	Jobs        int    `json:"jobs"`
	// Health aggregates the health of its accounts: ok or failing when
	// every account is, degraded when they differ (one account failing out
	// of several), unknown before any run.
	Health string `json:"health"`
}

// Account is one account, without its secrets.
type Account struct {
	Name     string            `json:"name"`
	Provider string            `json:"provider"`
	Labels   map[string]string `json:"labels"`
	Targets  []Target          `json:"targets"`
	// Circuit is closed, half_open or open (accounts without targets).
	Circuit string     `json:"circuit,omitempty"`
	Auth    AuthStatus `json:"auth"`
	// Health is ok, degraded, failing or unknown (no run yet), from the last
	// runs of its jobs, its credentials and its circuits.
	Health string `json:"health"`
	// HealthReasons say why the account is not ok (the redacted errors of
	// its jobs, a re-bootstrap required, an open circuit); empty when ok.
	HealthReasons []string `json:"health_reasons"`
}

// Target is one target of a multi-target account.
type Target struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	Circuit string            `json:"circuit"`
}

// AuthStatus describes the authentication of an account.
type AuthStatus struct {
	Type           string     `json:"type"`
	TokenExpiry    *time.Time `json:"token_expiry,omitempty"`
	ReauthRequired bool       `json:"reauth_required"`
}

// Job is one job, with its instances summarized.
type Job struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Template string `json:"template,omitempty"`
	Role     string `json:"role"`
	Priority string `json:"priority"`
	// Implicit marks a parent instantiated for a forEach.template.
	Implicit                 bool              `json:"implicit"`
	Parent                   string            `json:"parent,omitempty"`
	DesiredIntervalSeconds   float64           `json:"desired_interval_seconds"`
	EffectiveIntervalSeconds float64           `json:"effective_interval_seconds"`
	StretchReason            string            `json:"stretch_reason,omitempty"`
	Instances                int               `json:"instances"`
	Outputs                  []string          `json:"outputs"`
	Labels                   map[string]string `json:"labels"`
	LastRun                  *RunSummary       `json:"last_run,omitempty"`
	NextRun                  *time.Time        `json:"next_run,omitempty"`
	// Definition is the effective job definition (template expanded,
	// defaults applied), in job details only.
	Definition json.RawMessage `json:"definition,omitempty"`
}

// RunSummary is the outcome of a run.
type RunSummary struct {
	RunID     string    `json:"run_id"`
	Instance  string    `json:"instance"`
	Result    string    `json:"result"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Error     string    `json:"error,omitempty"`
}

// Instance is one instance of a job.
type Instance struct {
	Key         string            `json:"key"`
	Target      string            `json:"target,omitempty"`
	Vars        map[string]string `json:"vars"`
	LastSuccess *time.Time        `json:"last_success,omitempty"`
	Series      int               `json:"series"`
	NextRun     *time.Time        `json:"next_run,omitempty"`
	LastRun     *RunSummary       `json:"last_run,omitempty"`
}

// Run is one recorded run.
type Run struct {
	RunSummary
	DurationSeconds float64 `json:"duration_seconds"`
	Requests        int     `json:"requests"`
	Retries         int     `json:"retries"`
	Pages           int     `json:"pages"`
	Items           int     `json:"items"`
	Samples         int     `json:"samples"`
}

// Budget is one limit for one scope key.
type Budget struct {
	Provider string `json:"provider"`
	Limit    string `json:"limit"`
	// Kind is quota, rate or concurrency.
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
	// Accounts are the accounts whose requests can count in the scope: the
	// one or those its account, target or credential dimension names, every
	// account of the provider for a scope without one (instance, provider,
	// discovery variables).
	Accounts []string `json:"accounts"`
	// Quota limits.
	Max                  *int64     `json:"max,omitempty"`
	Usable               *int64     `json:"usable,omitempty"`
	Reserve              *int64     `json:"reserve,omitempty"`
	Used                 *int64     `json:"used,omitempty"`
	Remaining            *int64     `json:"remaining,omitempty"`
	RemainingObserved    *int64     `json:"remaining_observed,omitempty"`
	ExternalUsage        *int64     `json:"external_usage,omitempty"`
	ReserveRemaining     *int64     `json:"reserve_remaining,omitempty"`
	WindowStart          *time.Time `json:"window_start,omitempty"`
	WindowEnd            *time.Time `json:"window_end,omitempty"`
	Rolling              bool       `json:"rolling,omitempty"`
	ExhaustionETASeconds *float64   `json:"exhaustion_eta_seconds,omitempty"`
	LimitMismatch        bool       `json:"limit_mismatch,omitempty"`
	// Rate limits.
	RatePerSecond *float64 `json:"rate_per_second,omitempty"`
	Burst         *float64 `json:"burst,omitempty"`
	Tokens        *float64 `json:"tokens,omitempty"`
	// Concurrency limits (Max is shared with quotas).
	InUse *int64 `json:"in_use,omitempty"`
	// PenaltyUntil is the end of a 429 penalty.
	PenaltyUntil *time.Time `json:"penalty_until,omitempty"`
}

// Timeline is the consumption of a quota scope per bucket.
type Timeline struct {
	Provider      string          `json:"provider"`
	Limit         string          `json:"limit"`
	Scope         string          `json:"scope"`
	BucketSeconds float64         `json:"bucket_seconds"`
	Current       TimelineWindow  `json:"current"`
	Previous      *TimelineWindow `json:"previous,omitempty"`
}

// TimelineWindow is one window of a timeline; buckets without consumption
// are omitted.
type TimelineWindow struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Buckets []Bucket  `json:"buckets"`
}

// Bucket is the consumption from Start for the bucket width.
type Bucket struct {
	Start time.Time `json:"start"`
	Count int64     `json:"count"`
}

// Plan is the plan applied by the running collector.
type Plan struct {
	At       time.Time   `json:"at"`
	Feasible bool        `json:"feasible"`
	Jobs     []PlanJob   `json:"jobs"`
	Scopes   []PlanScope `json:"scopes"`
}

// PlanJob is the plan of one job.
type PlanJob struct {
	Name                     string  `json:"name"`
	Provider                 string  `json:"provider"`
	Account                  string  `json:"account"`
	Priority                 string  `json:"priority"`
	DesiredIntervalSeconds   float64 `json:"desired_interval_seconds"`
	EffectiveIntervalSeconds float64 `json:"effective_interval_seconds"`
	Stretch                  float64 `json:"stretch"`
	Reason                   string  `json:"reason,omitempty"`
	Instances                int     `json:"instances"`
	PagesPerRun              float64 `json:"pages_per_run"`
}

// PlanScope is the plan of one quota scope.
type PlanScope struct {
	Limit         string     `json:"limit"`
	Scope         string     `json:"scope"`
	PeriodSeconds float64    `json:"period_seconds"`
	Budget        float64    `json:"budget"`
	Reserve       float64    `json:"reserve"`
	Projected     float64    `json:"projected"`
	Feasible      bool       `json:"feasible"`
	ExhaustionAt  *time.Time `json:"exhaustion_at,omitempty"`
	Jobs          []string   `json:"jobs"`
}

// Graph is the configuration as nodes and edges.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Node is one node of the graph: provider, account, target, job (a group of
// instances) or output.
type Node struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// Health is ok, degraded, failing or unknown.
	Health string `json:"health"`
	// Instances counts the instances of a job node.
	Instances int `json:"instances,omitempty"`
}

// Edge links two nodes.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Output is one output and its queue.
type Output struct {
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Queue       int        `json:"queue"`
	Capacity    int        `json:"capacity"`
	Sent        int64      `json:"sent"`
	Dropped     int64      `json:"dropped"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
}

// List is a page of a list endpoint.
type List[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// ErrorBody is the body of error responses.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes an error.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Debug is the capture of the last run of a job instance.
type Debug struct {
	Job      string        `json:"job"`
	Instance string        `json:"instance"`
	RunID    string        `json:"run_id"`
	Time     time.Time     `json:"time"`
	Result   string        `json:"result"`
	Error    string        `json:"error,omitempty"`
	Pages    []DebugPage   `json:"pages"`
	Samples  []DebugSample `json:"samples"`
	// Instances lists the instances of the job with a capture.
	Instances []string `json:"instances"`
}

// DebugPage is a response of a run: the body is redacted and truncated.
type DebugPage struct {
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	Truncated bool              `json:"truncated"`
}

// DebugSample is a sample produced by the run.
type DebugSample struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}
