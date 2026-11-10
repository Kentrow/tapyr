package config

import (
	"math"
	"net/http"
	"slices"
	"time"
)

// Defaults documented in docs/spec/03-configuration.md and in the schemas.
const (
	defaultMaxPages         = 100
	defaultMaxInstances     = 10000
	defaultRateLimitedPause = 2 * time.Second
	defaultSettingsName     = "default"
)

// applyDefaults fills unset fields. It runs after semantic validation, so
// references are known to resolve.
func applyDefaults(cfg *Config) {
	templates := make(map[Name]*JobTemplate, len(cfg.JobTemplates))
	for _, t := range cfg.JobTemplates {
		templates[t.Metadata.Name] = t
		defaultDefinition(&t.Spec.JobDefinition)
	}
	for _, p := range cfg.Providers {
		defaultProvider(&p.Spec)
	}
	for _, a := range cfg.Accounts {
		if a.Spec.Budget == nil {
			a.Spec.Budget = &AccountBudget{}
		}
		setDefault(&a.Spec.Budget.Share, ptr(1.0))
	}
	for _, j := range cfg.Jobs {
		spec := &j.Spec
		if t, ok := templates[spec.Template]; ok {
			if spec.Interval == 0 {
				spec.Interval = t.Spec.DefaultInterval
			}
			if spec.Priority == "" {
				spec.Priority = t.Spec.DefaultPriority
			}
		} else {
			defaultDefinition(&spec.JobDefinition)
		}
		if spec.Priority == "" {
			spec.Priority = PriorityNormal
		}
	}
	for _, o := range cfg.Outputs {
		defaultOutput(&o.Spec)
	}
	if cfg.Settings == nil {
		cfg.Settings = &Settings{
			APIVersion: APIVersion,
			Kind:       string(KindSettings),
			Metadata:   ObjectMeta{Name: defaultSettingsName},
		}
	}
	defaultSettings(&cfg.Settings.Spec)
}

func defaultProvider(spec *ProviderSpec) {
	if spec.Auth == nil {
		spec.Auth = &Auth{Type: AuthNone}
	}
	if s := spec.Auth.SessionLogin; s != nil {
		setDefault(&s.Request.Method, http.MethodPost)
		setDefault(&s.RefreshBefore, Duration(30*time.Second))
		if s.Apply.Header != "" {
			setDefault(&s.Apply.Format, "{token}")
		}
	}
	if o := spec.Auth.OAuth2ClientCredentials; o != nil {
		setDefault(&o.AuthStyle, "header")
	}
	if spec.Pagination != nil {
		defaultPagination(spec.Pagination)
	}
	for i := range spec.Limits {
		defaultLimit(&spec.Limits[i])
	}
	if spec.Errors == nil {
		spec.Errors = &ErrorSemantics{}
	}
	e := spec.Errors
	if e.RateLimited == nil {
		e.RateLimited = &RateLimitedErrors{}
	}
	if len(e.RateLimited.Statuses) == 0 {
		e.RateLimited.Statuses = []int{429}
	}
	setDefault(&e.RateLimited.RetryAfterHeader, "Retry-After")
	setDefault(&e.RateLimited.DefaultPenalty, Duration(defaultRateLimitedPause))
	setDefault(&e.RateLimited.Penalize, "narrowest")
	if e.AuthFailed == nil || len(e.AuthFailed.Statuses) == 0 {
		e.AuthFailed = &StatusMatcher{Statuses: []int{401}}
	}
	if e.Retryable == nil || len(e.Retryable.Statuses) == 0 {
		e.Retryable = &StatusMatcher{Statuses: []int{500, 502, 503, 504}}
	}
}

// LimitWithDefaults returns a copy of l with the defaults applied (burst,
// scope, window reset), leaving l unchanged: provider packages are loaded
// without them, defaults apply when a configuration is assembled.
func LimitWithDefaults(l Limit) Limit {
	if l.Scope != nil {
		s := *l.Scope
		s.By = slices.Clone(s.By)
		l.Scope = &s
	}
	if l.AppliesTo != nil {
		a := *l.AppliesTo
		l.AppliesTo = &a
	}
	if l.Window != nil {
		w := *l.Window
		l.Window = &w
	}
	defaultLimit(&l)
	return l
}

func defaultLimit(l *Limit) {
	if l.Kind == LimitRate && l.Rate != nil && l.Burst == 0 {
		l.Burst = max(1, int(math.Ceil(l.Rate.PerSecond())))
	}
	if l.Scope == nil {
		l.Scope = &Scope{}
	}
	if len(l.Scope.By) == 0 {
		l.Scope.By = []string{"instance"}
	}
	if l.AppliesTo == nil {
		l.AppliesTo = &AppliesTo{}
	}
	setDefault(&l.AppliesTo.AuthRequests, "include")
	if w := l.Window; w != nil && w.Period != "" {
		setDefault(&w.Timezone, "UTC")
		switch w.Period {
		case "day", "week", "month":
			setDefault(&w.ResetAt, "00:00")
		}
		if w.Period == "week" {
			setDefault(&w.WeekStart, "monday")
		}
		if w.Period == "month" {
			setDefault(&w.MonthDay, 1)
		}
	}
}

func defaultPagination(p *Pagination) {
	setDefault(&p.MaxPages, defaultMaxPages)
	if p.Page != nil {
		setDefault(&p.Page.Start, ptr(1))
	}
	if p.LinkHeader != nil {
		setDefault(&p.LinkHeader.Rel, "next")
	}
}

func defaultDefinition(d *JobDefinition) {
	setDefault(&d.Role, RoleMetrics)
	if d.Request != nil {
		setDefault(&d.Request.Method, http.MethodGet)
	}
	if d.Pagination != nil {
		defaultPagination(d.Pagination)
	}
	if d.ForEach != nil {
		setDefault(&d.ForEach.MaxInstances, defaultMaxInstances)
	}
	if e := d.Extract; e != nil {
		for i := range e.Metrics {
			setDefault(&e.Metrics[i].Type, MetricGauge)
		}
		for i := range e.Post {
			setDefault(&e.Post[i].Type, MetricGauge)
		}
	}
}

func defaultOutput(spec *OutputSpec) {
	switch spec.Type {
	case OutputPrometheus:
		if spec.Prometheus == nil {
			spec.Prometheus = &PrometheusOutput{}
		}
		return
	case OutputRemoteWrite:
		setDefault(&spec.RemoteWrite.Timeout, Duration(10*time.Second))
	case OutputOTLP:
		setDefault(&spec.OTLP.Protocol, "http/protobuf")
		setDefault(&spec.OTLP.Timeout, Duration(10*time.Second))
	}
	if spec.Queue == nil {
		spec.Queue = &Queue{}
	}
	setDefault(&spec.Queue.Capacity, 50000)
	setDefault(&spec.Queue.MaxBatch, 5000)
	setDefault(&spec.Queue.FlushInterval, Duration(5*time.Second))
}

func defaultSettings(s *SettingsSpec) {
	setDefault(&s.Server.Listen, "127.0.0.1:9790")
	setDefault(&s.Server.UI.Enabled, ptr(true))
	setDefault(&s.Server.Debug.MaxBodyBytes, 1<<20)
	setDefault(&s.State.Path, "./data/tapyr.db")
	setDefault(&s.Collection.MaxConcurrentRequests, 32)
	setDefault(&s.Collection.DefaultTimeout, Duration(30*time.Second))
	setDefault(&s.Collection.Jitter, ptr(Percent(0.1)))
	setDefault(&s.Budget.RetryReserve, ptr(Percent(0.1)))
	setDefault(&s.Budget.ReplanInterval, Duration(15*time.Minute))
	setDefault(&s.Budget.ReplanOnProjection, ptr(true))
	setDefault(&s.Budget.MaxStretch, 12)
	setDefault(&s.Retry.MaxAttempts, 4)
	setDefault(&s.Retry.InitialBackoff, Duration(time.Second))
	setDefault(&s.Retry.MaxBackoff, Duration(60*time.Second))
	setDefault(&s.Retry.Multiplier, 2)
	setDefault(&s.CircuitBreaker.FailureThreshold, 5)
	setDefault(&s.CircuitBreaker.Cooldown, Duration(2*time.Minute))
	setDefault(&s.CircuitBreaker.MaxCooldown, Duration(30*time.Minute))
	setDefault(&s.Mapping.MaxCost, 1_000_000)
	setDefault(&s.Mapping.MaxItemsPerRun, 100_000)
	setDefault(&s.Mapping.MaxSamplesPerRun, 200_000)
	setDefault(&s.SelfMetrics.Interval, Duration(30*time.Second))
	setDefault(&s.Mapping.LabelConflict, "override")
	setDefault(&s.Log.Level, "info")
	setDefault(&s.Log.Format, "json")
}

// setDefault sets *field to value when it holds the zero value.
func setDefault[T comparable](field *T, value T) {
	var zero T
	if *field == zero {
		*field = value
	}
}
