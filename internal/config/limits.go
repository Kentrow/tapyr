package config

import "github.com/invopop/jsonschema"

// LimitKind selects the behavior of a limit.
type LimitKind string

// Limit kinds.
const (
	LimitRate        LimitKind = "rate"
	LimitConcurrency LimitKind = "concurrency"
	LimitQuota       LimitKind = "quota"
)

// Limit is a constraint of the provider API, enforced by the budget engine
// for every request (see docs/spec/04-budget-engine.md).
type Limit struct {
	// Name of the limit, unique within the provider.
	Name Name `json:"name" jsonschema:"required"`
	// Description is free text for humans, ideally with the source of the limit.
	Description string `json:"description,omitempty"`
	// Kind of limit: rate (token bucket), concurrency or quota (window).
	Kind LimitKind `json:"kind" jsonschema:"required,enum=rate,enum=concurrency,enum=quota"`
	// Rate is the refill rate of a rate limit, for example 10/s.
	Rate *Rate `json:"rate,omitempty"`
	// Burst is the bucket capacity of a rate limit. Defaults to the rate per
	// second, minimum 1.
	Burst int `json:"burst,omitempty" jsonschema:"minimum=1"`
	// Max is the maximum of a concurrency limit, or of a quota per window.
	// A quota may use "auto" to learn it from response headers.
	Max *IntOrAuto `json:"max,omitempty"`
	// InitialMax is the conservative quota maximum used with max: auto
	// until the first limit header is observed.
	InitialMax int64 `json:"initialMax,omitempty" jsonschema:"minimum=1"`
	// Window of a quota: a calendar period or a rolling duration.
	Window *Window `json:"window,omitempty"`
	// Scope partitions the limit (one counter per scope key).
	Scope *Scope `json:"scope,omitempty"`
	// AppliesTo restricts the requests the limit counts.
	AppliesTo *AppliesTo `json:"appliesTo,omitempty"`
	// Sync reads quota state from response headers.
	Sync *QuotaSync `json:"sync,omitempty"`
	// Source of the limit. Optional, and required on every limit by
	// tapyr provider lint --catalog.
	Source *LimitSource `json:"source,omitempty"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (Limit) JSONSchemaExtend(s *jsonschema.Schema) {
	requireWhen(s, "kind", string(LimitRate), "rate")
	requireWhen(s, "kind", string(LimitConcurrency), "max")
	requireWhen(s, "kind", string(LimitQuota), "max", "window")
}

// Window is the window of a quota: exactly one of period or rolling.
type Window struct {
	// Period of a fixed calendar window.
	Period string `json:"period,omitempty" jsonschema:"enum=minute,enum=hour,enum=day,enum=week,enum=month"`
	// ResetAt is the time of day (HH:MM, 24h) at which day, week and month
	// windows reset.
	ResetAt string `json:"resetAt,omitempty" jsonschema:"default=00:00"`
	// Timezone of the calendar (IANA name).
	Timezone string `json:"timezone,omitempty" jsonschema:"default=UTC"`
	// WeekStart is the first day of week windows.
	WeekStart string `json:"weekStart,omitempty" jsonschema:"enum=monday,enum=tuesday,enum=wednesday,enum=thursday,enum=friday,enum=saturday,enum=sunday,default=monday"`
	// MonthDay is the day of month windows reset (1 to 31; the last day of
	// shorter months).
	MonthDay int `json:"monthDay,omitempty" jsonschema:"minimum=1,maximum=31,default=1"`
	// Rolling is the duration of a rolling window (1-minute precision).
	Rolling Duration `json:"rolling,omitempty"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (Window) JSONSchemaExtend(s *jsonschema.Schema) {
	setPattern(s, "resetAt", resetAtPattern)
}

// Scope partitions a limit.
type Scope struct {
	// By lists the scope dimensions: instance, provider, account, target,
	// credential, endpoint or var:<name>. Empty means instance.
	By []string `json:"by,omitempty"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (Scope) JSONSchemaExtend(s *jsonschema.Schema) {
	if p, ok := s.Properties.Get("by"); ok && p.Items != nil {
		p.Items.Pattern = `^(instance|provider|account|target|credential|endpoint|var:[A-Za-z_][A-Za-z0-9_]*)$`
		p.UniqueItems = true
	}
}

// AppliesTo restricts the requests a limit counts.
type AppliesTo struct {
	// Endpoints are globs on the templated path: * matches within one
	// segment, ** matches any number of segments.
	Endpoints []string `json:"endpoints,omitempty"`
	// Methods the limit counts.
	Methods []string `json:"methods,omitempty"`
	// RequestsWithVar counts only requests where this variable is set.
	RequestsWithVar Identifier `json:"requestsWithVar,omitempty"`
	// AuthRequests selects regular requests and token requests: include
	// (both), only (token requests only) or exclude (regular requests only).
	AuthRequests string `json:"authRequests,omitempty" jsonschema:"enum=include,enum=only,enum=exclude,default=include"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (AppliesTo) JSONSchemaExtend(s *jsonschema.Schema) {
	if p, ok := s.Properties.Get("methods"); ok && p.Items != nil {
		p.Items.Enum = []any{"GET", "HEAD", "POST"}
	}
	if p, ok := s.Properties.Get("endpoints"); ok && p.Items != nil {
		p.Items.Pattern = `^/`
	}
}

// QuotaSync names the response headers carrying quota state. Header names
// are matched case-insensitively; missing headers are ignored.
type QuotaSync struct {
	// LimitHeader carries the quota maximum.
	LimitHeader string `json:"limitHeader,omitempty"`
	// RemainingHeader carries the remaining quota.
	RemainingHeader string `json:"remainingHeader,omitempty"`
	// ResetHeader carries the reset time (epoch seconds or delta seconds).
	ResetHeader string `json:"resetHeader,omitempty"`
}

// LimitSource says where a limit comes from.
type LimitSource struct {
	// Kind is vendor-docs (the vendor documentation, with its url), measured
	// (against the real API, with its date) or assumed (nothing documented).
	Kind string `json:"kind" jsonschema:"required,enum=vendor-docs,enum=measured,enum=assumed"`
	// URL of the vendor documentation, or of the notes of a measurement.
	URL string `json:"url,omitempty" jsonschema:"format=uri"`
	// Date the source was checked or the measurement made, YYYY-MM-DD or
	// YYYY-MM.
	Date string `json:"date,omitempty"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (LimitSource) JSONSchemaExtend(s *jsonschema.Schema) {
	setPattern(s, "date", datePattern)
}
