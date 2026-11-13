// Package planner turns desired job intervals into effective intervals that
// fit every quota (see docs/spec/04-budget-engine.md, "Planner"). It is
// pure: the same code serves tapyr plan (offline, a full window) and the
// running collector (online, the remaining budget and time of the window).
package planner

import (
	"math"
	"slices"
	"strings"
	"time"

	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/config"
)

// DefaultRetryOverhead is the retry ratio assumed without run history.
const DefaultRetryOverhead = 0.05

// Job is a planner input.
type Job struct {
	Name     string
	Provider string
	Account  string
	Priority config.Priority
	Desired  time.Duration
	// PagesPerRun is the expected number of requests per run and instance.
	PagesPerRun float64
	// RetryOverhead is the expected ratio of retries to requests.
	RetryOverhead float64
	// Fanout is the number of instances of the job (1 without discovery).
	Fanout int
	Scopes []budget.PlanScope
}

// callsPerRun is the expected number of budgeted requests per run of one
// instance, retries included.
func (j *Job) callsPerRun() float64 { return j.PagesPerRun * (1 + j.RetryOverhead) }

// Options configures a plan.
type Options struct {
	// MaxStretch caps the stretch of a priority class.
	MaxStretch float64
	// Online plans with the remaining budget and the remaining time of the
	// current windows; offline plans a full window.
	Online bool
	Now    time.Time
	// ReplanInterval is the period of the online replans: a fixed window
	// shorter than it is planned over a full window, since a plan lasts
	// through several of them. Zero plans every window over its remaining
	// time.
	ReplanInterval time.Duration
}

// JobPlan is the plan of one job.
type JobPlan struct {
	Name      string
	Provider  string
	Account   string
	Priority  config.Priority
	Desired   time.Duration
	Effective time.Duration
	Stretch   float64
	// Reason explains a stretch: the scope that required it.
	Reason      string
	CallsPerRun float64
	Fanout      int
	PagesPerRun float64
	// Calls are the projected calls per scope over the planned period.
	Calls map[string]float64
}

// ScopePlan is the plan of one quota scope.
type ScopePlan struct {
	ID      string
	Limit   string
	Key     string
	Period  time.Duration
	Budget  float64
	Reserve float64
	// Projected are the calls projected over the period with effective
	// intervals.
	Projected float64
	Feasible  bool
	// ExhaustionAt is when the projection exhausts the budget, when it
	// does (infeasible plans only).
	ExhaustionAt time.Time
	Jobs         []string
}

// Result is a plan.
type Result struct {
	Jobs     []JobPlan
	Scopes   []ScopePlan
	Feasible bool
}

// classes are the priority classes that may be stretched, lowest first.
var classes = []config.Priority{config.PriorityLow, config.PriorityNormal, config.PriorityHigh}

type scope struct {
	plan ScopePlan
	jobs []int // indexes of the contributing jobs
	// perJob holds the calls per run of every contributing job on this
	// scope (fan-out included unless the scope is per instance).
	perJob map[int]float64
}

// Plan computes effective intervals.
func Plan(jobs []Job, opts Options) *Result {
	if opts.MaxStretch < 1 {
		opts.MaxStretch = 1
	}
	stretch := make([]float64, len(jobs))
	reason := make([]string, len(jobs))
	for i := range stretch {
		stretch[i] = 1
	}

	scopes, order := quotaScopes(jobs, opts)

	// Stretch per scope, independently; a job keeps the largest stretch.
	feasible := true
	for _, id := range order {
		s := scopes[id]
		need := solve(jobs, s, opts.MaxStretch)
		if need == nil {
			s.plan.Feasible = false
			feasible = false
			need = maxStretchAll(jobs, s, opts.MaxStretch)
		}
		for i, k := range need {
			if k > stretch[i] {
				stretch[i], reason[i] = k, "quota "+s.plan.Limit+" "+s.plan.Key
			}
		}
	}

	res := &Result{Feasible: feasible}
	for i := range jobs {
		j := &jobs[i]
		eff := time.Duration(math.Ceil(float64(j.Desired)*stretch[i]/float64(time.Second))) * time.Second
		eff = max(eff, j.Desired)
		// Rate feasibility: a run must fit in its interval.
		for _, ps := range j.Scopes {
			if ps.Kind != config.LimitRate || ps.Rate <= 0 {
				continue
			}
			calls := j.callsPerRun()
			if !ps.PerInstance {
				calls *= float64(max(1, j.Fanout))
			}
			if minimum := time.Duration(math.Ceil(calls/ps.Rate)) * time.Second; minimum > eff {
				eff, reason[i] = minimum, "rate "+ps.Limit+" "+ps.Key
			}
		}
		res.Jobs = append(res.Jobs, JobPlan{
			Name: j.Name, Provider: j.Provider, Account: j.Account, Priority: j.Priority,
			Desired: j.Desired, Effective: eff, Stretch: float64(eff) / float64(j.Desired), Reason: reason[i],
			CallsPerRun: j.callsPerRun(), Fanout: max(1, j.Fanout), PagesPerRun: j.PagesPerRun,
			Calls: map[string]float64{},
		})
	}
	for _, id := range order {
		s := scopes[id]
		for _, i := range s.jobs {
			calls := s.perJob[i] * s.plan.Period.Seconds() / res.Jobs[i].Effective.Seconds()
			res.Jobs[i].Calls[id] = calls
			s.plan.Projected += calls
			s.plan.Jobs = append(s.plan.Jobs, jobs[i].Name)
		}
		if s.plan.Projected > s.plan.Budget+1e-9 {
			s.plan.Feasible = false
			res.Feasible = false
			if s.plan.Projected > 0 {
				s.plan.ExhaustionAt = opts.Now.Add(time.Duration(s.plan.Budget / s.plan.Projected * float64(s.plan.Period)))
			}
		}
		res.Scopes = append(res.Scopes, s.plan)
	}
	return res
}

// quotaScopes groups the jobs by quota scope, with the calls per run of
// every contributing job, and returns the scope IDs sorted.
func quotaScopes(jobs []Job, opts Options) (map[string]*scope, []string) {
	scopes := map[string]*scope{}
	var order []string
	for i := range jobs {
		j := &jobs[i]
		fanout := float64(max(1, j.Fanout))
		for _, ps := range j.Scopes {
			if ps.Kind != config.LimitQuota {
				continue
			}
			s, ok := scopes[ps.ID]
			if !ok {
				s = &scope{plan: newScopePlan(ps, opts), perJob: map[int]float64{}}
				scopes[ps.ID] = s
				order = append(order, ps.ID)
			}
			calls := j.callsPerRun()
			if !ps.PerInstance {
				calls *= fanout
			}
			s.jobs = append(s.jobs, i)
			s.perJob[i] = calls
		}
	}
	slices.Sort(order)
	return scopes, order
}

// Evaluate projects the consumption of every quota scope over its period
// with the given effective intervals, against the budget of opts, without
// planning: a job missing from intervals runs at its desired interval. It
// tells whether the current plan still fits fresh budgets (the projection
// replan of the collector). Projected above Budget makes a scope
// infeasible.
func Evaluate(jobs []Job, intervals map[string]time.Duration, opts Options) []ScopePlan {
	scopes, order := quotaScopes(jobs, opts)
	out := make([]ScopePlan, 0, len(order))
	for _, id := range order {
		s := scopes[id]
		for _, i := range s.jobs {
			interval, ok := intervals[jobs[i].Name]
			if !ok || interval <= 0 {
				interval = jobs[i].Desired
			}
			s.plan.Projected += s.perJob[i] * s.plan.Period.Seconds() / interval.Seconds()
			s.plan.Jobs = append(s.plan.Jobs, jobs[i].Name)
		}
		if s.plan.Projected > s.plan.Budget+1e-9 {
			s.plan.Feasible = false
			if s.plan.Projected > 0 {
				s.plan.ExhaustionAt = opts.Now.Add(time.Duration(s.plan.Budget / s.plan.Projected * float64(s.plan.Period)))
			}
		}
		out = append(out, s.plan)
	}
	return out
}

func newScopePlan(ps budget.PlanScope, opts Options) ScopePlan {
	p := ScopePlan{ID: ps.ID, Limit: ps.Limit, Key: ps.Key, Reserve: float64(ps.Reserve), Feasible: true}
	switch {
	case opts.Online && !ps.Rolling && ps.WindowLength < opts.ReplanInterval:
		// The window is shorter than the replan interval: the plan paces
		// several of them, and what is left of the current one says nothing
		// of the next (a minute window exhausted by a 429, or by another
		// client bursting, would stretch every job for a whole replan
		// interval). Plan a full window, as offline; the engine still
		// refuses what the current window cannot take, until its reset.
		p.Budget = float64(ps.Usable - ps.Reserve)
		p.Period = ps.WindowLength
	case opts.Online:
		p.Budget = math.Max(0, float64(ps.Remaining-ps.Reserve))
		p.Period = ps.WindowLength
		if !ps.Rolling {
			p.Period = max(0, ps.WindowEnd.Sub(opts.Now))
			// Other clients keep consuming until the reset: their
			// consumption so far, extrapolated at the same pace, is not
			// Tapyr's to spend. The elapsed time counts as at least 1/24 of
			// the window, so that the first minutes of a window do not
			// extrapolate a burst over the whole day.
			if ps.External > 0 {
				elapsed := max(opts.Now.Sub(ps.WindowStart), ps.WindowLength/24)
				p.Budget = math.Max(0, p.Budget-float64(ps.External)*p.Period.Seconds()/elapsed.Seconds())
			}
		}
	default:
		p.Budget = float64(ps.Usable - ps.Reserve)
		p.Period = ps.WindowLength
	}
	return p
}

// consumption returns the calls of job i on scope s over its period.
func consumption(jobs []Job, s *scope, i int, k float64) float64 {
	return s.perJob[i] * s.plan.Period.Seconds() / (jobs[i].Desired.Seconds() * k)
}

// solve returns the stretch of every contributing job that fits the scope,
// or nil when critical jobs alone do not fit.
func solve(jobs []Job, s *scope, maxStretch float64) map[int]float64 {
	k := map[int]float64{}
	for _, i := range s.jobs {
		k[i] = 1
	}
	total := func() float64 {
		var sum float64
		for _, i := range s.jobs {
			sum += consumption(jobs, s, i, k[i])
		}
		return sum
	}
	if total() <= s.plan.Budget {
		return k
	}
	for _, class := range classes {
		var members []int
		for _, i := range s.jobs {
			if jobs[i].Priority == class || (class == config.PriorityNormal && jobs[i].Priority == "") {
				members = append(members, i)
			}
		}
		if len(members) == 0 {
			continue
		}
		var classAtOne, others float64
		for _, i := range s.jobs {
			if slices.Contains(members, i) {
				classAtOne += consumption(jobs, s, i, 1)
			} else {
				others += consumption(jobs, s, i, k[i])
			}
		}
		room := s.plan.Budget - others
		if room > 0 && classAtOne/room <= maxStretch {
			factor := math.Max(1, classAtOne/room)
			for _, i := range members {
				k[i] = factor
			}
			return k
		}
		for _, i := range members {
			k[i] = maxStretch
		}
	}
	if total() <= s.plan.Budget*(1+1e-9) {
		return k
	}
	return nil
}

func maxStretchAll(jobs []Job, s *scope, maxStretch float64) map[int]float64 {
	k := map[int]float64{}
	for _, i := range s.jobs {
		if jobs[i].Priority != config.PriorityCritical {
			k[i] = maxStretch
		}
	}
	return k
}

// Describe returns the reason of a job plan in words.
func (j *JobPlan) Describe() string {
	if j.Effective == j.Desired {
		return ""
	}
	return strings.TrimSpace(j.Reason)
}
