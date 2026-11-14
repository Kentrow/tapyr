package collector

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/buildinfo"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/scheduler"
	"github.com/kentrow/tapyr/internal/state"
)

// statusSource implements api.Source for the running collector.
type statusSource struct{ c *Collector }

var _ api.Source = statusSource{}

// configHash identifies a configuration: the SHA-256 of its documents (which
// hold secret references, never secret values).
func configHash(cfg *config.Config) string {
	data, _ := json.Marshal(struct {
		Providers    []*config.Provider
		JobTemplates []*config.JobTemplate
		Accounts     []*config.Account
		Jobs         []*config.Job
		Outputs      []*config.Output
		Settings     *config.Settings
	}{cfg.Providers, cfg.JobTemplates, cfg.Accounts, cfg.Jobs, cfg.Outputs, cfg.Settings})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// recordLoad records a configuration load or reload attempt.
func (c *Collector) recordLoad(cfg *config.Config, err error) {
	c.smu.Lock()
	defer c.smu.Unlock()
	c.lastReload = api.Reload{At: c.clock.Now().UTC(), Success: err == nil}
	if err != nil {
		c.lastReload.Error = c.redact(err.Error())
		return
	}
	c.configHash = configHash(cfg)
}

func (s statusSource) Status(ctx context.Context) api.Status {
	c := s.c
	info := buildinfo.Get()
	now := c.clock.Now()
	c.smu.Lock()
	st := api.Status{
		Version: info.Version, Revision: info.Revision, GoVersion: info.GoVersion, StartedAt: c.started.UTC(),
		UptimeSeconds: now.Sub(c.started).Seconds(), ConfigHash: c.configHash, LastReload: c.lastReload,
	}
	c.smu.Unlock()
	st.ReadinessReasons = c.notReady(ctx)
	st.Ready = len(st.ReadinessReasons) == 0
	if g := c.current(); g != nil {
		st.PlanFeasible = g.lastPlan() == nil || g.lastPlan().Feasible
		st.Instances, _ = g.disc.counts()
	}
	return st
}

// snapshot is the view of a generation the status API answers from: the
// active instances (copied out of the discovery lock) and the schedule.
type snapshot struct {
	g         *generation
	instances []*runtime.Job
	next      map[string]time.Time
	now       time.Time
}

func (s statusSource) snapshot() (*snapshot, bool) {
	g := s.c.current()
	if g == nil {
		return nil, false
	}
	snap := &snapshot{g: g, next: g.sched.Next(), now: s.c.clock.Now()}
	g.disc.do(func(active map[string]*runtime.Job) {
		for _, key := range slices.Sorted(maps.Keys(active)) {
			snap.instances = append(snap.instances, active[key])
		}
	})
	return snap, true
}

func (s statusSource) Providers(ctx context.Context) []api.Provider {
	snap, ok := s.snapshot()
	if !ok {
		return nil
	}
	_, runs := s.lastRuns(ctx, snap)
	return s.providers(snap, s.accounts(snap, runs))
}

// providers lists the providers that have accounts. The health of a
// provider aggregates the health of its accounts: one account failing out
// of several degrades it, every account failing fails it.
func (s statusSource) providers(snap *snapshot, accounts []api.Account) []api.Provider {
	var out []api.Provider
	for _, p := range snap.g.cfg.Providers {
		name := string(p.Metadata.Name)
		ap := api.Provider{Name: name, Version: p.Metadata.Version, Description: p.Metadata.Description}
		var health []string
		for _, a := range accounts {
			if a.Provider == name {
				ap.Accounts++
				health = append(health, a.Health)
			}
		}
		ap.Health = aggregateHealth(health...)
		for _, j := range snap.g.graph.Jobs {
			if j.Provider == name {
				ap.Jobs++
			}
		}
		if ap.Accounts > 0 {
			out = append(out, ap)
		}
	}
	slices.SortFunc(out, func(a, b api.Provider) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// lastRuns reads the last run of every job instance once: the health of
// every job (the aggregate of its instances) and the last runs of every
// account.
func (s statusSource) lastRuns(ctx context.Context, snap *snapshot) (map[string]string, map[string][]lastRun) {
	perJob := map[string][]string{}
	accounts := map[string][]lastRun{}
	for _, j := range snap.instances {
		runs, err := s.c.store.Runs(ctx, j.Name, j.Instance, 1)
		if err != nil || len(runs) == 0 {
			perJob[j.Name] = append(perJob[j.Name], healthUnknown)
			continue
		}
		perJob[j.Name] = append(perJob[j.Name], runHealth(runs[0].Result))
		accounts[j.Account] = append(accounts[j.Account], lastRun{Job: j.Name, Result: runs[0].Result, Error: runs[0].Error})
	}
	jobs := make(map[string]string, len(perJob))
	for name, h := range perJob {
		jobs[name] = aggregateHealth(h...)
	}
	return jobs, accounts
}

func circuitName(state int) string {
	switch state {
	case budget.CircuitOpen:
		return "open"
	case budget.CircuitHalfOpen:
		return "half_open"
	}
	return "closed"
}

func (s statusSource) Accounts(ctx context.Context) []api.Account {
	snap, ok := s.snapshot()
	if !ok {
		return nil
	}
	_, runs := s.lastRuns(ctx, snap)
	return s.accounts(snap, runs)
}

// accounts describes every account, with its health from the last runs of
// its jobs, its credentials and its circuits.
func (s statusSource) accounts(snap *snapshot, runs map[string][]lastRun) []api.Account {
	expiry := map[string]time.Time{}
	reauth := map[string]bool{}
	for _, t := range tokenGauges(s.c.tokens.Sessions()) {
		expiry[t.Provider+"/"+t.Account] = t.Expiry
		reauth[t.Provider+"/"+t.Account] = t.ReauthRequired
	}
	providers := map[string]*config.Provider{}
	for _, p := range snap.g.cfg.Providers {
		providers[string(p.Metadata.Name)] = p
	}
	var out []api.Account
	for _, a := range snap.g.cfg.Accounts {
		name, provider := string(a.Metadata.Name), string(a.Spec.Provider)
		aa := api.Account{Name: name, Provider: provider, Labels: nonNilMap(a.Spec.Labels), Targets: []api.Target{}}
		if p := providers[provider]; p != nil && p.Spec.Auth != nil {
			aa.Auth.Type = string(p.Spec.Auth.Type)
		}
		aa.Auth.Type = cmp.Or(aa.Auth.Type, "none")
		if t := expiry[provider+"/"+name]; !t.IsZero() {
			aa.Auth.TokenExpiry = ptr(t.UTC())
		}
		aa.Auth.ReauthRequired = reauth[provider+"/"+name]
		in := accountInput{Runs: runs[name], Reauth: aa.Auth.ReauthRequired}
		if len(a.Spec.Targets) == 0 {
			aa.Circuit = circuitName(snap.g.engine.CircuitState(name, ""))
			in.Circuits = append(in.Circuits, circuit{State: aa.Circuit})
		}
		for _, t := range a.Spec.Targets {
			c := circuitName(snap.g.engine.CircuitState(name, string(t.Name)))
			aa.Targets = append(aa.Targets, api.Target{Name: string(t.Name), Labels: nonNilMap(t.Labels), Circuit: c})
			in.Circuits = append(in.Circuits, circuit{Target: string(t.Name), State: c})
		}
		aa.Health, aa.HealthReasons = accountHealth(in)
		out = append(out, aa)
	}
	slices.SortFunc(out, func(a, b api.Account) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func ptr[T any](v T) *T { return &v }

// jobModels returns one runtime job per job name: the static jobs, the
// models of fan-outs and the jobs that are not collected, sorted by name.
func jobModels(snap *snapshot) []*runtime.Job {
	seen := map[string]*runtime.Job{}
	for _, j := range slices.Concat(snap.g.graph.Jobs, snap.g.graph.NotCollected) {
		if _, ok := seen[j.Name]; !ok {
			seen[j.Name] = j
		}
	}
	out := make([]*runtime.Job, 0, len(seen))
	for _, name := range slices.Sorted(maps.Keys(seen)) {
		out = append(out, seen[name])
	}
	return out
}

func (s statusSource) Jobs(ctx context.Context) []api.Job {
	snap, ok := s.snapshot()
	if !ok {
		return nil
	}
	var out []api.Job
	for _, j := range jobModels(snap) {
		out = append(out, s.job(ctx, snap, j))
	}
	return out
}

// job describes a job across its instances.
func (s statusSource) job(ctx context.Context, snap *snapshot, j *runtime.Job) api.Job {
	aj := api.Job{
		Name: j.Name, Provider: j.Provider, Account: j.Account, Role: string(j.Role), Priority: string(j.Priority),
		Implicit: j.Implicit, DesiredIntervalSeconds: j.Interval.Seconds(), Outputs: slices.Clone(j.Outputs),
		Labels: map[string]string{}, Collected: j.NotCollected == "", NotCollectedReason: j.NotCollected,
	}
	if aj.Outputs == nil {
		aj.Outputs = []string{}
	}
	if j.ForEach != nil {
		aj.Parent = j.ForEach.ParentJob
	}
	for _, cj := range snap.g.cfg.Jobs {
		if string(cj.Metadata.Name) == j.Name {
			aj.Template = string(cj.Spec.Template)
			maps.Copy(aj.Labels, cj.Spec.Labels)
		}
	}
	if j.Implicit {
		_, tmpl, _ := strings.Cut(j.Name, ".")
		aj.Template = tmpl
	}
	if plan := snap.g.lastPlan(); plan != nil {
		for i := range plan.Jobs {
			if p := &plan.Jobs[i]; p.Name == scheduler.Group(j) {
				aj.StretchReason = p.Describe()
			}
		}
	}
	var next time.Time
	for _, inst := range snap.instances {
		if inst.Name != j.Name {
			continue
		}
		aj.Instances++
		aj.EffectiveIntervalSeconds = max(aj.EffectiveIntervalSeconds, snap.g.sched.Interval(scheduler.Group(inst)).Seconds())
		if t, ok := snap.next[inst.Instance]; ok && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if aj.EffectiveIntervalSeconds == 0 {
		// No instance yet (a fan-out waiting for its parent): the interval
		// planned for the group, else the desired one.
		aj.EffectiveIntervalSeconds = cmp.Or(snap.g.sched.Interval(scheduler.Group(j)), j.Interval).Seconds()
	}
	if !next.IsZero() {
		aj.NextRun = ptr(next.UTC())
	}
	if runs, err := s.c.store.RunsOfJob(ctx, j.Name, 1); err == nil && len(runs) > 0 {
		aj.LastRun = runSummary(runs[0])
	}
	return aj
}

func runSummary(r state.RunRecord) *api.RunSummary {
	return &api.RunSummary{
		RunID: r.RunID, Instance: r.Instance, Result: r.Result, StartedAt: r.Started.UTC(), EndedAt: r.Ended.UTC(), Error: r.Error,
	}
}

func (s statusSource) Job(ctx context.Context, name string) (api.Job, error) {
	snap, ok := s.snapshot()
	if !ok {
		return api.Job{}, api.ErrNotFound
	}
	for _, j := range jobModels(snap) {
		if j.Name != name {
			continue
		}
		aj := s.job(ctx, snap, j)
		aj.Definition = definition(snap.g.cfg, j)
		return aj, nil
	}
	return api.Job{}, api.ErrNotFound
}

// definition is the configuration of a job: its spec (defaults applied)
// and, when it uses one, its template's definition.
func definition(cfg *config.Config, j *runtime.Job) json.RawMessage {
	def := map[string]any{}
	for _, cj := range cfg.Jobs {
		if string(cj.Metadata.Name) == j.Name {
			def["job"] = cj.Spec
		}
	}
	template := ""
	if spec, ok := def["job"].(config.JobSpec); ok {
		template = string(spec.Template)
	} else if j.Implicit {
		_, template, _ = strings.Cut(j.Name, ".")
	}
	for _, t := range cfg.JobTemplates {
		if string(t.Metadata.Name) == template {
			def["template"] = t.Spec
		}
	}
	data, err := json.Marshal(def)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

func (s statusSource) Instances(ctx context.Context, name string) ([]api.Instance, error) {
	snap, ok := s.snapshot()
	if !ok {
		return nil, api.ErrNotFound
	}
	known := false
	for _, j := range jobModels(snap) {
		known = known || j.Name == name
	}
	if !known {
		return nil, api.ErrNotFound
	}
	out := []api.Instance{}
	for _, j := range snap.instances {
		if j.Name != name {
			continue
		}
		inst := api.Instance{Key: j.Instance, Target: j.Target, Vars: map[string]string{}, Series: len(s.c.series.Series(j.Instance))}
		maps.Copy(inst.Vars, j.DiscoveryVars)
		if at, ok := s.c.series.LastSuccess(j.Instance); ok {
			inst.LastSuccess = ptr(at.UTC())
		}
		if t, ok := snap.next[j.Instance]; ok {
			inst.NextRun = ptr(t.UTC())
		}
		if runs, err := s.c.store.Runs(ctx, j.Name, j.Instance, 1); err == nil && len(runs) > 0 {
			inst.LastRun = runSummary(runs[0])
		}
		out = append(out, inst)
	}
	return out, nil
}

func (s statusSource) Runs(ctx context.Context, name, instance string, limit int) ([]api.Run, error) {
	snap, ok := s.snapshot()
	if !ok {
		return nil, api.ErrNotFound
	}
	if !slices.ContainsFunc(jobModels(snap), func(j *runtime.Job) bool { return j.Name == name }) {
		return nil, api.ErrNotFound
	}
	var records []state.RunRecord
	var err error
	if instance != "" {
		records, err = s.c.store.Runs(ctx, name, instance, limit)
	} else {
		records, err = s.c.store.RunsOfJob(ctx, name, limit)
	}
	if err != nil {
		return nil, err
	}
	out := make([]api.Run, 0, len(records))
	for _, r := range records {
		out = append(out, api.Run{
			RunSummary: *runSummary(r), DurationSeconds: r.Ended.Sub(r.Started).Seconds(),
			Requests: r.Requests, Retries: r.Retries, Pages: r.Pages, Items: r.Items, Samples: r.Samples,
		})
	}
	return out, nil
}

func (s statusSource) Budgets(context.Context) []api.Budget {
	snap, ok := s.snapshot()
	if !ok {
		return nil
	}
	g := snap.g
	jobs := append(slices.Clone(g.graph.Jobs), snap.instances...)
	now := s.c.clock.Now()
	var out []api.Budget
	for _, q := range g.engine.Quotas() {
		b := api.Budget{
			Provider: q.Provider, Limit: q.Limit, Kind: string(config.LimitQuota), Scope: q.Scope, Accounts: scopeAccounts(q.Provider, q.Scope, jobs),
			Max: ptr(q.Max), Usable: ptr(q.Usable), Reserve: ptr(q.Reserve), Used: ptr(q.Used), Remaining: ptr(q.Remaining),
			ExternalUsage: ptr(q.ExternalUsage), ReserveRemaining: ptr(min(q.Reserve, max(0, q.Usable-q.Used))),
			WindowStart: ptr(q.WindowStart.UTC()), WindowEnd: ptr(q.WindowEnd.UTC()), Rolling: q.Rolling, LimitMismatch: q.Mismatch,
		}
		if q.ObservedRemaining >= 0 {
			b.RemainingObserved = ptr(q.ObservedRemaining)
		}
		if eta := exhaustionETA(q, now); eta > 0 {
			b.ExhaustionETASeconds = ptr(eta.Seconds())
		}
		out = append(out, b)
	}
	for _, l := range g.engine.Limits() {
		b := api.Budget{Provider: l.Provider, Limit: l.Limit, Kind: string(l.Kind), Scope: l.Scope, Accounts: scopeAccounts(l.Provider, l.Scope, jobs)}
		if l.Kind == config.LimitRate {
			b.RatePerSecond, b.Burst, b.Tokens = ptr(l.Rate), ptr(l.Burst), ptr(l.Tokens)
		} else {
			b.Max, b.InUse = ptr(l.Max), ptr(l.InUse)
		}
		if !l.PenaltyUntil.IsZero() {
			b.PenaltyUntil = ptr(l.PenaltyUntil.UTC())
		}
		out = append(out, b)
	}
	return out
}

func (s statusSource) Timeline(_ context.Context, limit, scope string) (api.Timeline, error) {
	g := s.c.current()
	provider, name, ok := strings.Cut(limit, "/")
	if g == nil || !ok {
		return api.Timeline{}, api.ErrNotFound
	}
	t, ok := g.engine.Timeline(provider, name, scope)
	if !ok {
		return api.Timeline{}, api.ErrNotFound
	}
	window := func(w budget.TimelineWindow) api.TimelineWindow {
		aw := api.TimelineWindow{Start: w.Start.UTC(), End: w.End.UTC(), Buckets: []api.Bucket{}}
		for _, b := range w.Buckets {
			aw.Buckets = append(aw.Buckets, api.Bucket{Start: b.Start.UTC(), Count: b.Count})
		}
		return aw
	}
	at := api.Timeline{Provider: t.Provider, Limit: t.Limit, Scope: t.Scope, BucketSeconds: t.Bucket.Seconds(), Current: window(t.Current)}
	if t.Previous != nil {
		at.Previous = ptr(window(*t.Previous))
	}
	return at, nil
}

func (s statusSource) Plan(context.Context) (api.Plan, error) {
	g := s.c.current()
	if g == nil {
		return api.Plan{}, api.ErrNotFound
	}
	g.pmu.Lock()
	res, at := g.plan, g.planAt
	g.pmu.Unlock()
	if res == nil {
		return api.Plan{}, api.ErrNotFound
	}
	p := api.Plan{At: at.UTC(), Feasible: res.Feasible, Jobs: []api.PlanJob{}, Scopes: []api.PlanScope{}}
	for i := range res.Jobs {
		j := &res.Jobs[i]
		p.Jobs = append(p.Jobs, api.PlanJob{
			Name: j.Name, Provider: j.Provider, Account: j.Account, Priority: string(j.Priority),
			DesiredIntervalSeconds: j.Desired.Seconds(), EffectiveIntervalSeconds: j.Effective.Seconds(), Stretch: j.Stretch,
			Reason: j.Describe(), Instances: j.Fanout, PagesPerRun: j.PagesPerRun,
		})
	}
	for _, sc := range res.Scopes {
		ps := api.PlanScope{
			Limit: sc.Limit, Scope: sc.Key, PeriodSeconds: sc.Period.Seconds(), Budget: sc.Budget, Reserve: sc.Reserve,
			Projected: sc.Projected, Feasible: sc.Feasible, Jobs: slices.Clone(sc.Jobs),
		}
		if ps.Jobs == nil {
			ps.Jobs = []string{}
		}
		if !sc.ExhaustionAt.IsZero() {
			ps.ExhaustionAt = ptr(sc.ExhaustionAt.UTC())
		}
		p.Scopes = append(p.Scopes, ps)
	}
	return p, nil
}

func (s statusSource) Graph(ctx context.Context) api.Graph {
	gr := api.Graph{Nodes: []api.Node{}, Edges: []api.Edge{}}
	snap, ok := s.snapshot()
	if !ok {
		return gr
	}
	health, runs := s.lastRuns(ctx, snap)
	accounts := s.accounts(snap, runs)
	node := func(kind, name, label, h string, instances int) string {
		id := kind + "/" + name
		gr.Nodes = append(gr.Nodes, api.Node{ID: id, Kind: kind, Label: label, Health: cmp.Or(h, "unknown"), Instances: instances})
		return id
	}
	edge := func(from, to string) { gr.Edges = append(gr.Edges, api.Edge{From: from, To: to}) }
	for _, p := range s.providers(snap, accounts) {
		node("provider", p.Name, p.Name, p.Health, 0)
	}
	for _, a := range accounts {
		id := node("account", a.Name, a.Name, a.Health, 0)
		edge("provider/"+a.Provider, id)
		for _, t := range a.Targets {
			th := "ok"
			if t.Circuit == "open" {
				th = "degraded"
			}
			edge(id, node("target", a.Name+"/"+t.Name, t.Name, th, 0))
		}
	}
	var outputs []string
	s.c.omu.RLock()
	for name := range s.c.outputs {
		outputs = append(outputs, name)
	}
	s.c.omu.RUnlock()
	slices.Sort(outputs)
	for _, name := range outputs {
		node("output", name, name, "ok", 0)
	}
	for _, j := range jobModels(snap) {
		if j.NotCollected != "" {
			continue
		}
		count := 0
		for _, inst := range snap.instances {
			if inst.Name == j.Name {
				count++
			}
		}
		id := node("job", j.Name, j.Name, health[j.Name], count)
		if j.ForEach != nil && j.ForEach.ParentJob != "" {
			edge("job/"+j.ForEach.ParentJob, id)
		} else {
			edge("account/"+j.Account, id)
		}
		for _, name := range outputs {
			if j.Role != config.RoleInventory && (len(j.Outputs) == 0 || slices.Contains(j.Outputs, name)) {
				edge(id, "output/"+name)
			}
		}
	}
	return gr
}

func (s statusSource) Outputs(context.Context) []api.Output {
	s.c.omu.RLock()
	defer s.c.omu.RUnlock()
	out := []api.Output{}
	for _, name := range slices.Sorted(maps.Keys(s.c.outputs)) {
		ro := s.c.outputs[name]
		st := ro.out.State()
		ao := api.Output{
			Name: name, Type: string(ro.spec.Type), Queue: st.Queue, Capacity: st.Capacity, Sent: st.Sent, Dropped: st.Dropped,
			LastError: st.LastError,
		}
		if !st.LastSuccess.IsZero() {
			ao.LastSuccess = ptr(st.LastSuccess.UTC())
		}
		if !st.LastErrorAt.IsZero() {
			ao.LastErrorAt = ptr(st.LastErrorAt.UTC())
		}
		out = append(out, ao)
	}
	return out
}

func (s statusSource) Debug(ctx context.Context, job, instance string) (api.Debug, error) {
	return s.c.debug.get(ctx, job, instance)
}
