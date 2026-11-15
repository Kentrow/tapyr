package selfobs

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// State is the scrape-time view of the running configuration behind the
// self-metric gauges.
type State struct {
	Quotas   []QuotaGauge
	Plan     []PlanGauge
	Jobs     []JobGauge
	Tokens   []TokenGauge
	Circuits []CircuitGauge
	Outputs  []OutputGauge

	// NotCollected are the jobs that are not collected (ADR 0013).
	NotCollected []JobRef
}

// QuotaGauge is one quota limit for one scope label.
type QuotaGauge struct {
	Provider, Limit, Scope string
	// Max is the effective max for the window, after learning and share.
	Max, Used, Remaining int64
	// ObservedRemaining is negative when never observed.
	ObservedRemaining int64
	ExternalUsage     int64
	ReserveRemaining  int64
	WindowEnd         time.Time
	// ExhaustionETA is zero when no exhaustion is projected in the window.
	ExhaustionETA time.Duration
	// Mismatch reports headers disagreeing with the declared max.
	Mismatch bool
}

// PlanGauge is the feasibility of one quota scope in the last plan.
type PlanGauge struct {
	Provider, Limit, Scope string
	Feasible               bool
}

// JobGauge describes a job across its instances.
type JobGauge struct {
	Provider, Account, Job string
	// LastSuccess is the oldest last success across instances; zero when an
	// instance never succeeded.
	LastSuccess        time.Time
	Desired, Effective time.Duration
	// Lag is the largest delay of an instance behind its schedule.
	Lag       time.Duration
	Instances int
	Series    int
}

// JobRef names a job.
type JobRef struct {
	Provider, Account, Job string
}

// TokenGauge describes the token session of an account.
type TokenGauge struct {
	Provider, Account string
	// Expiry is zero when no token is held.
	Expiry         time.Time
	ReauthRequired bool
}

// CircuitGauge is the circuit breaker state of an account and target: 0
// closed, 1 half-open, 2 open.
type CircuitGauge struct {
	Provider, Account, Target string
	State                     int
}

// OutputGauge describes an output.
type OutputGauge struct {
	Output string
	// Queue is the number of samples waiting to be sent.
	Queue int
	// LastSuccess is zero before the first successful send.
	LastSuccess time.Time
}

func desc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(name, help, labels, nil)
}

var (
	quotaLabels = []string{"provider", "limit", "scope"}
	jobLabels   = []string{"provider", "account", "job"}

	descBudgetLimit     = desc("tapyr_budget_limit", "Effective max of the quota window, after learning and share.", quotaLabels...)
	descBudgetUsed      = desc("tapyr_budget_used", "Quota consumed by Tapyr in the current window.", quotaLabels...)
	descBudgetRemaining = desc("tapyr_budget_remaining", "Remaining quota, internal estimate.", quotaLabels...)
	descBudgetObserved  = desc("tapyr_budget_remaining_observed", "Remaining quota reported by the provider headers.", quotaLabels...)
	descBudgetExternal  = desc("tapyr_budget_external_usage", "Estimated consumption of other clients in the window.", quotaLabels...)
	descBudgetReserve   = desc("tapyr_budget_reserve_remaining", "Retry reserve left in the window.", quotaLabels...)
	descBudgetWindowEnd = desc("tapyr_budget_window_end_timestamp_seconds", "End of the current quota window.", quotaLabels...)
	descBudgetETA       = desc("tapyr_budget_exhaustion_eta_seconds",
		"Projected time to quota exhaustion, when it comes before the window ends.", quotaLabels...)
	descBudgetMismatch = desc("tapyr_budget_limit_mismatch", "1 when the quota headers disagree with the declared max.", "provider", "limit")
	descPlanFeasible   = desc("tapyr_plan_feasible", "1 when the critical jobs fit in the remaining quota.", quotaLabels...)

	descJobLastSuccess = desc("tapyr_job_last_success_timestamp_seconds", "Oldest last successful run across the instances.", jobLabels...)
	descJobDesired     = desc("tapyr_job_desired_interval_seconds", "Configured interval of the job.", jobLabels...)
	descJobEffective   = desc("tapyr_job_effective_interval_seconds", "Interval applied by the planner.", jobLabels...)
	descJobLag         = desc("tapyr_job_lag_seconds", "Largest delay of an instance behind its schedule.", jobLabels...)
	descJobInstances   = desc("tapyr_job_instances", "Scheduled instances of the job (fan-out size).", jobLabels...)
	descJobSeries      = desc("tapyr_job_series", "Series produced by the last successful runs.", jobLabels...)
	descJobCollected   = desc("tapyr_job_collected",
		"1 when the job is collected, 0 when no output keeps its metrics or no collected job takes its inventory.", jobLabels...)

	descTokenExpiry = desc("tapyr_auth_token_expiry_timestamp_seconds", "Expiry of the token held for the account.", "provider", "account")
	descReauth      = desc("tapyr_auth_reauth_required", "1 when the account needs a manual re-bootstrap.", "provider", "account")
	descCircuit     = desc("tapyr_circuit_state", "Circuit breaker state: 0 closed, 1 half-open, 2 open.", "provider", "account", "target")

	descOutputQueue       = desc("tapyr_output_queue_samples", "Samples waiting in the output queue.", "output")
	descOutputLastSuccess = desc("tapyr_output_last_success_timestamp_seconds", "Time of the last successful send.", "output")
)

// stateCollector exposes a State as gauges at scrape time.
type stateCollector struct {
	state func() State
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		descBudgetLimit, descBudgetUsed, descBudgetRemaining, descBudgetObserved, descBudgetExternal, descBudgetReserve,
		descBudgetWindowEnd, descBudgetETA, descBudgetMismatch, descPlanFeasible,
		descJobLastSuccess, descJobDesired, descJobEffective, descJobLag, descJobInstances, descJobSeries, descJobCollected,
		descTokenExpiry, descReauth, descCircuit, descOutputQueue, descOutputLastSuccess,
	} {
		ch <- d
	}
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.state()
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	mismatch := map[[2]string]bool{}
	for _, q := range s.Quotas {
		l := []string{q.Provider, q.Limit, q.Scope}
		gauge(descBudgetLimit, float64(q.Max), l...)
		gauge(descBudgetUsed, float64(q.Used), l...)
		gauge(descBudgetRemaining, float64(q.Remaining), l...)
		if q.ObservedRemaining >= 0 {
			gauge(descBudgetObserved, float64(q.ObservedRemaining), l...)
		}
		gauge(descBudgetExternal, float64(q.ExternalUsage), l...)
		gauge(descBudgetReserve, float64(q.ReserveRemaining), l...)
		gauge(descBudgetWindowEnd, seconds(q.WindowEnd), l...)
		if q.ExhaustionETA > 0 {
			gauge(descBudgetETA, q.ExhaustionETA.Seconds(), l...)
		}
		key := [2]string{q.Provider, q.Limit}
		mismatch[key] = mismatch[key] || q.Mismatch
	}
	for key, v := range mismatch {
		gauge(descBudgetMismatch, bool01(v), key[0], key[1])
	}
	for _, p := range s.Plan {
		gauge(descPlanFeasible, bool01(p.Feasible), p.Provider, p.Limit, p.Scope)
	}
	for _, j := range s.Jobs {
		l := []string{j.Provider, j.Account, j.Job}
		if !j.LastSuccess.IsZero() {
			gauge(descJobLastSuccess, seconds(j.LastSuccess), l...)
		}
		gauge(descJobDesired, j.Desired.Seconds(), l...)
		gauge(descJobEffective, j.Effective.Seconds(), l...)
		gauge(descJobLag, j.Lag.Seconds(), l...)
		gauge(descJobInstances, float64(j.Instances), l...)
		gauge(descJobSeries, float64(j.Series), l...)
		gauge(descJobCollected, 1, l...)
	}
	for _, j := range s.NotCollected {
		gauge(descJobCollected, 0, j.Provider, j.Account, j.Job)
	}
	for _, t := range s.Tokens {
		if !t.Expiry.IsZero() {
			gauge(descTokenExpiry, seconds(t.Expiry), t.Provider, t.Account)
		}
		gauge(descReauth, bool01(t.ReauthRequired), t.Provider, t.Account)
	}
	for _, cs := range s.Circuits {
		gauge(descCircuit, float64(cs.State), cs.Provider, cs.Account, cs.Target)
	}
	for _, o := range s.Outputs {
		gauge(descOutputQueue, float64(o.Queue), o.Output)
		if !o.LastSuccess.IsZero() {
			gauge(descOutputLastSuccess, seconds(o.LastSuccess), o.Output)
		}
	}
}

func seconds(t time.Time) float64 { return float64(t.UnixMilli()) / 1e3 }

func bool01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
