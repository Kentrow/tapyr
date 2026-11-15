package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/budget/planner"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/scheduler"
	"github.com/kentrow/tapyr/internal/selfobs"
)

// livenessTimeout is how long the flush loop may go without ticking before
// /healthz reports the process unhealthy.
const livenessTimeout = 30 * time.Second

// selfState is the scrape-time view of the running configuration behind the
// self-metric gauges.
func (c *Collector) selfState() selfobs.State {
	g := c.current()
	if g == nil {
		return selfobs.State{}
	}
	now := c.clock.Now()
	st := selfobs.State{
		Quotas: quotaGauges(g.engine.Quotas(), now),
		Plan:   planGauges(g.lastPlan()),
		Tokens: tokenGauges(c.tokens.Sessions()),
	}
	next := g.sched.Next()
	type key struct{ provider, account, job string }
	jobs := map[key]*selfobs.JobGauge{}
	never := map[key]bool{}
	circuits := map[[3]string]bool{}
	g.disc.do(func(active map[string]*runtime.Job) {
		for instance, j := range active {
			k := key{j.Provider, j.Account, j.Name}
			jg, ok := jobs[k]
			if !ok {
				jg = &selfobs.JobGauge{Provider: j.Provider, Account: j.Account, Job: j.Name, Desired: j.Interval}
				jobs[k] = jg
			}
			jg.Instances++
			jg.Effective = max(jg.Effective, g.sched.Interval(scheduler.Group(j)))
			jg.Series += len(c.series.Series(instance))
			if at, ok := c.series.LastSuccess(instance); !ok {
				never[k] = true
			} else if jg.LastSuccess.IsZero() || at.Before(jg.LastSuccess) {
				jg.LastSuccess = at
			}
			if due, ok := next[instance]; ok && now.After(due) {
				jg.Lag = max(jg.Lag, now.Sub(due))
			}
			circuits[[3]string{j.Provider, j.Account, j.Target}] = true
		}
	})
	for k, jg := range jobs {
		if never[k] {
			jg.LastSuccess = time.Time{}
		}
		st.Jobs = append(st.Jobs, *jg)
	}
	slices.SortFunc(st.Jobs, func(a, b selfobs.JobGauge) int {
		return strings.Compare(a.Provider+"/"+a.Account+"/"+a.Job, b.Provider+"/"+b.Account+"/"+b.Job)
	})
	for _, j := range g.graph.NotCollectedJobs() {
		st.NotCollected = append(st.NotCollected, selfobs.JobRef{Provider: j.Provider, Account: j.Account, Job: j.Name})
	}
	for _, k := range slices.SortedFunc(func(yield func([3]string) bool) {
		for k := range circuits {
			if !yield(k) {
				return
			}
		}
	}, func(a, b [3]string) int { return strings.Compare(strings.Join(a[:], "/"), strings.Join(b[:], "/")) }) {
		st.Circuits = append(st.Circuits, selfobs.CircuitGauge{
			Provider: k[0], Account: k[1], Target: k[2], State: g.engine.CircuitState(k[1], k[2]),
		})
	}
	c.omu.RLock()
	for name, ro := range c.outputs {
		s := ro.out.State()
		st.Outputs = append(st.Outputs, selfobs.OutputGauge{Output: name, Queue: s.Queue, LastSuccess: s.LastSuccess})
	}
	c.omu.RUnlock()
	slices.SortFunc(st.Outputs, func(a, b selfobs.OutputGauge) int { return strings.Compare(a.Output, b.Output) })
	return st
}

// scopeLabel returns the self-metric label of a scope key: the key itself
// when its dimensions are bounded by configuration, else the dimension
// names with the variables written var:<name> and without their values
// (aggregated is then true).
func scopeLabel(key string) (label string, aggregated bool) {
	if key == "" {
		return "", false
	}
	parts := strings.Split(key, ",")
	for i, p := range parts {
		name, _, _ := strings.Cut(p, "=")
		switch name {
		case "instance", "provider", "account", "target", "credential", "endpoint":
			continue
		}
		parts[i], aggregated = "var:"+name, true
	}
	if !aggregated {
		return key, false
	}
	for i, p := range parts {
		if name, _, _ := strings.Cut(p, "="); !strings.HasPrefix(p, "var:") {
			parts[i] = name
		}
	}
	return strings.Join(parts, ","), true
}

// quotaGauges turns quota states into gauges: one per bounded scope key, and
// for scopes partitioned by a variable the most constrained key.
func quotaGauges(quotas []budget.QuotaState, now time.Time) []selfobs.QuotaGauge {
	var out []selfobs.QuotaGauge
	index := map[string]int{}
	for _, q := range quotas {
		label, _ := scopeLabel(q.Scope)
		qg := selfobs.QuotaGauge{
			Provider: q.Provider, Limit: q.Limit, Scope: label,
			Max: q.Max, Used: q.Used, Remaining: q.Remaining, ObservedRemaining: q.ObservedRemaining,
			ExternalUsage: q.ExternalUsage, ReserveRemaining: min(q.Reserve, max(0, q.Usable-q.Used)),
			WindowEnd: q.WindowEnd, ExhaustionETA: exhaustionETA(q, now), Mismatch: q.Mismatch,
		}
		id := q.Provider + "/" + q.Limit + "|" + label
		if i, seen := index[id]; seen {
			if remainingFraction(qg) < remainingFraction(out[i]) {
				qg.Mismatch = qg.Mismatch || out[i].Mismatch
				out[i] = qg
			} else {
				out[i].Mismatch = out[i].Mismatch || qg.Mismatch
			}
			continue
		}
		index[id] = len(out)
		out = append(out, qg)
	}
	return out
}

func remainingFraction(q selfobs.QuotaGauge) float64 {
	if q.Max <= 0 {
		return 0
	}
	return float64(q.Remaining) / float64(q.Max)
}

// exhaustionETA extrapolates the consumption rate since the start of a fixed
// window; it is zero when the quota lasts until the window ends (and for
// rolling windows, which never end).
func exhaustionETA(q budget.QuotaState, now time.Time) time.Duration {
	elapsed := now.Sub(q.WindowStart)
	if q.Rolling || q.Used <= 0 || elapsed <= 0 {
		return 0
	}
	if q.Remaining <= 0 {
		return time.Nanosecond // exhausted now
	}
	// The remaining quota runs out before the window ends when
	// remaining / rate < left, with rate = used / elapsed.
	left := q.WindowEnd.Sub(now)
	if float64(q.Remaining)*float64(elapsed) >= float64(q.Used)*float64(left) {
		return 0
	}
	return time.Duration(float64(q.Remaining) / float64(q.Used) * float64(elapsed))
}

// planGauges turns the scopes of a plan into feasibility gauges, a scope
// label being feasible when all its keys are.
func planGauges(res *planner.Result) []selfobs.PlanGauge {
	if res == nil {
		return nil
	}
	var out []selfobs.PlanGauge
	index := map[string]int{}
	for _, s := range res.Scopes {
		provider, limit, _ := strings.Cut(s.Limit, "/")
		label, _ := scopeLabel(s.Key)
		id := s.Limit + "|" + label
		if i, seen := index[id]; seen {
			out[i].Feasible = out[i].Feasible && s.Feasible
			continue
		}
		index[id] = len(out)
		out = append(out, selfobs.PlanGauge{Provider: provider, Limit: limit, Scope: label, Feasible: s.Feasible})
	}
	return out
}

// tokenGauges reports, per account, the earliest expiry of its tokens.
func tokenGauges(sessions []auth.SessionInfo) []selfobs.TokenGauge {
	var out []selfobs.TokenGauge
	index := map[[2]string]int{}
	for _, s := range sessions {
		k := [2]string{s.Provider, s.Account}
		i, seen := index[k]
		if !seen {
			index[k] = len(out)
			out = append(out, selfobs.TokenGauge{Provider: s.Provider, Account: s.Account, Expiry: s.Expiry, ReauthRequired: s.Reauth})
			continue
		}
		out[i].ReauthRequired = out[i].ReauthRequired || s.Reauth
		if s.Expiry.IsZero() || (!out[i].Expiry.IsZero() && s.Expiry.Before(out[i].Expiry)) {
			out[i].Expiry = s.Expiry
		}
	}
	return out
}

// lastPlan returns the plan last applied, nil before the first.
func (g *generation) lastPlan() *planner.Result {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	return g.plan
}

// serveHealth answers /healthz: unhealthy when the flush loop stopped
// ticking.
func (c *Collector) serveHealth(w http.ResponseWriter, _ *http.Request) {
	last := time.Unix(0, c.heartbeat.Load())
	if since := c.clock.Since(last); since > livenessTimeout {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"healthy": false, "reason": "the flush loop has not ticked for " + since.Round(time.Second).String(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"healthy": true})
}

// serveReady answers /readyz with the reasons the collector is not ready.
func (c *Collector) serveReady(w http.ResponseWriter, r *http.Request) {
	reasons := c.notReady(r.Context())
	status := http.StatusOK
	if len(reasons) > 0 {
		status = http.StatusServiceUnavailable
	}
	// Accounts needing a re-bootstrap are reported without failing the
	// readiness: the other accounts keep collecting.
	conditions := []string{}
	var sessions []auth.SessionInfo
	if c.tokens != nil {
		sessions = c.tokens.Sessions()
	}
	for _, t := range tokenGauges(sessions) {
		if t.ReauthRequired {
			conditions = append(conditions, fmt.Sprintf("account %s (provider %s) needs a manual re-bootstrap of its credentials", t.Account, t.Provider))
		}
	}
	writeJSON(w, status, map[string]any{"ready": len(reasons) == 0, "reasons": reasons, "conditions": conditions})
}

// notReady lists what keeps the collector from being ready.
func (c *Collector) notReady(ctx context.Context) []string {
	reasons := []string{}
	if c.current() == nil {
		reasons = append(reasons, "configuration not loaded")
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := c.store.Ping(pctx); err != nil {
		reasons = append(reasons, "state store not answering")
	}
	if !c.outputsStarted.Load() {
		reasons = append(reasons, "outputs not started")
	}
	return reasons
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
