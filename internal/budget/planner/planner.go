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
	// Reason explains a stretch: the scope that required it, as
	// "quota <limit> <scope>" or "rate <limit> <scope>"; Constraint holds
	// the same, for Describe.
	Reason      string
	Constraint  Constraint
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
	// Accounts are the accounts of the jobs counted in the scope, sorted:
	// they name a scope keyed by a credential, whose key is a hash.
	Accounts []string
	// Shortfall says why the scope does not fit, nil when it does.
	Shortfall *Shortfall
}

// Constraint is the limit that set the effective interval of a job.
type Constraint struct {
	// Kind is quota or rate.
	Kind  config.LimitKind
	Limit string
	Key   string
	// Accounts are the accounts counted in the scope, as in ScopePlan.
	Accounts []string
}

// Shortfall says why a quota scope does not fit, and what would make it
// fit: what its jobs need when the planner slows them down as much as it
// may (the critical jobs at their desired interval, the others stretched
// to the maximum), and how much the critical jobs would have to slow down.
type Shortfall struct {
	// Needed are the calls of the jobs over the period, at the most the
	// planner may slow them down; Budget is what the scope leaves.
	Needed float64
	Budget float64
	Period time.Duration
	// Online plans cover what is left of the window, offline plans a full
	// window.
	Online bool
	// Critical are the critical jobs of the scope, most calls first.
	Critical []CriticalJob
	// Factor is how much every critical job would have to slow down for
	// the scope to fit (above 1), or 0 when slowing them down cannot be
	// enough: the other jobs alone, at the maximum stretch, exceed the
	// budget, or there is no critical job.
	Factor float64
	// FirstFactor is how much the first critical job (the one with the
	// most calls) would have to slow down, alone, for the scope to fit, or
	// 0 when slowing it down alone cannot be enough.
	FirstFactor float64
	// MaxStretch is the most the planner may slow a job down.
	MaxStretch float64
}

// CriticalJob is a critical job of a scope that does not fit.
type CriticalJob struct {
	Name    string
	Desired time.Duration
	// Calls are its calls over the period, at its desired interval.
	Calls float64
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
	constraint := make([]Constraint, len(jobs))
	for i := range stretch {
		stretch[i] = 1
	}

	scopes, order := quotaScopes(jobs, opts)
	accounts := scopeAccounts(jobs)

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
				constraint[i] = Constraint{Kind: config.LimitQuota, Limit: s.plan.Limit, Key: s.plan.Key, Accounts: s.plan.Accounts}
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
				constraint[i] = Constraint{Kind: config.LimitRate, Limit: ps.Limit, Key: ps.Key, Accounts: accounts[ps.ID]}
			}
		}
		res.Jobs = append(res.Jobs, JobPlan{
			Name: j.Name, Provider: j.Provider, Account: j.Account, Priority: j.Priority,
			Desired: j.Desired, Effective: eff, Stretch: float64(eff) / float64(j.Desired), Reason: reason[i], Constraint: constraint[i],
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
		if !s.plan.Feasible {
			s.plan.Shortfall = shortfall(jobs, s, opts)
		}
		res.Scopes = append(res.Scopes, s.plan)
	}
	return res
}

// shortfall says why a scope does not fit (see Shortfall).
func shortfall(jobs []Job, s *scope, opts Options) *Shortfall {
	sf := &Shortfall{Budget: s.plan.Budget, Period: s.plan.Period, Online: opts.Online, MaxStretch: opts.MaxStretch}
	var critical, others float64
	for _, i := range s.jobs {
		if jobs[i].Priority != config.PriorityCritical {
			others += consumption(jobs, s, i, opts.MaxStretch)
			continue
		}
		calls := consumption(jobs, s, i, 1)
		critical += calls
		sf.Critical = append(sf.Critical, CriticalJob{Name: jobs[i].Name, Desired: jobs[i].Desired, Calls: calls})
	}
	slices.SortStableFunc(sf.Critical, func(a, b CriticalJob) int {
		if a.Calls != b.Calls {
			return cmpFloat(b.Calls, a.Calls)
		}
		return strings.Compare(a.Name, b.Name)
	})
	sf.Needed = critical + others
	if room := sf.Budget - others; critical > 0 && room > 0 {
		sf.Factor = math.Max(1, critical/room)
		first := sf.Critical[0].Calls
		if left := room - (critical - first); left > 0 {
			sf.FirstFactor = math.Max(1, first/left)
		}
	}
	return sf
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
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
	accounts := scopeAccounts(jobs)
	for id, s := range scopes {
		s.plan.Accounts = accounts[id]
	}
	slices.Sort(order)
	return scopes, order
}

// scopeAccounts returns the accounts of the jobs counted in each scope,
// quota or rate, by scope ID, sorted.
func scopeAccounts(jobs []Job) map[string][]string {
	out := map[string][]string{}
	for i := range jobs {
		for _, ps := range jobs[i].Scopes {
			if a := jobs[i].Account; a != "" && !slices.Contains(out[ps.ID], a) {
				out[ps.ID] = append(out[ps.ID], a)
			}
		}
	}
	for _, a := range out {
		slices.Sort(a)
	}
	return out
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

// Describe says in words why a job runs less often than desired: the limit
// it has to fit, named as its provider declares it, with its scope.
func (j *JobPlan) Describe() string {
	if j.Effective == j.Desired {
		return ""
	}
	c := j.Constraint
	switch c.Kind {
	case config.LimitQuota:
		return "to fit the " + LimitName(c.Limit) + " quota" + ScopeWords(c.Key, c.Accounts)
	case config.LimitRate:
		return "for a run to fit the " + LimitName(c.Limit) + " rate limit" + ScopeWords(c.Key, c.Accounts)
	}
	return strings.TrimSpace(j.Reason)
}

// LimitName is the name of a limit as its provider declares it, without
// the provider the budget engine prefixes it with.
func LimitName(limit string) string {
	if _, name, ok := strings.Cut(limit, "/"); ok {
		return name
	}
	return limit
}

// ScopeWords names a scope key in words, as a complement: " of account
// lab", " of account lab and target eu", or "" for a scope shared by every
// account of the provider. A credential is named by the accounts that use
// it (" of accounts north and south"), its key being a hash; by the hash
// when no account is known.
func ScopeWords(key string, accounts []string) string {
	if key == "" {
		return ""
	}
	var parts []string
	for part := range strings.SplitSeq(key, ",") {
		dim, value, ok := strings.Cut(part, "=")
		if !ok {
			if part == "instance" {
				// The whole process: every account of this Tapyr.
				part = "the Tapyr process"
			}
			parts = append(parts, part)
			continue
		}
		if dim == "credential" && len(accounts) > 0 {
			parts = append(parts, accountWords(accounts))
			continue
		}
		parts = append(parts, dim+" "+value)
	}
	return " of " + strings.Join(parts, " and ")
}

// accountWords names accounts in words: "account lab", "accounts north and
// south", "accounts a, b and c".
func accountWords(accounts []string) string {
	switch len(accounts) {
	case 0:
		return ""
	case 1:
		return "account " + accounts[0]
	}
	return "accounts " + strings.Join(accounts[:len(accounts)-1], ", ") + " and " + accounts[len(accounts)-1]
}
