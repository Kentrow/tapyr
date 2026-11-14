package collector

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/budget/planner"
	"github.com/kentrow/tapyr/internal/collect"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/model"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/scheduler"
	"github.com/kentrow/tapyr/internal/selfobs"
	"github.com/kentrow/tapyr/internal/series"
	"github.com/kentrow/tapyr/internal/state"
)

// generation is the pipeline of one compiled graph: budget engine,
// executor, discovery and scheduler.
type generation struct {
	c        *Collector
	cfg      *config.Config
	graph    *runtime.Graph
	engine   *budget.Engine
	executor *collect.Executor
	disc     *discovery
	// headers are the response headers kept by the live debug endpoint, by
	// provider.
	headers map[string][]string
	sched   *scheduler.Scheduler
	cancel  context.CancelFunc
	loops   sync.WaitGroup

	pmu    sync.Mutex
	plan   *planner.Result
	planAt time.Time
	// planSig summarizes the plan applied, to publish its changes.
	planSig string

	// projection is the state of the projection check: whether it is on,
	// the runs that ask for it (pending, buffered by one), and, owned by
	// replanLoop, when it last ran and the scopes that triggered a replan.
	projection struct {
		on      bool
		pending chan struct{}
		checked time.Time
		scopes  map[string]*projectionScope
	}
}

// Plan recomputation reasons (tapyr_plan_recomputations_total).
const (
	reasonStart        = "start"
	reasonReload       = "reload"
	reasonInterval     = "interval"
	reasonLimitLearned = "limit_learned"
	reasonInventory    = "inventory"
	reasonProjection   = "projection"
)

// The projection check (docs/spec/04-budget-engine.md, Replan on
// projection) runs at most once per projectionSpacing. A quota scope
// triggers a replan when the consumption projected by the current intervals
// exceeds its remaining budget by more than projectionMargin, at most once
// per projectionCooldown.
// A scope still over budget after its replan (a plan that cannot fit) is
// disarmed until its projection fits again, so that it never replans in a
// loop.
const (
	projectionSpacing  = time.Minute
	projectionMargin   = 0.1
	projectionCooldown = 5 * time.Minute
)

// projectionScope is the projection state of a quota scope.
type projectionScope struct {
	triggered time.Time
	disarmed  bool
}

// newGeneration builds the pipeline of a graph; it is started by start.
func (c *Collector) newGeneration(cfg *config.Config, graph *runtime.Graph) (*generation, error) {
	engine, err := budget.NewEngine(budget.Config{
		Providers: cfg.Providers, Accounts: cfg.Accounts,
		RetryReserve: graph.Settings.Budget.RetryReserve.Fraction(),
		Breaker:      graph.Settings.CircuitBreaker,
		Observer:     c.metrics,
	}, c.clock, c.logger)
	if err != nil {
		return nil, err
	}
	g := &generation{c: c, cfg: cfg, graph: graph, engine: engine, headers: KeptHeaders(cfg.Providers)}
	for p, hs := range g.headers {
		g.headers[p] = append(hs, debugHeaders...)
	}
	g.disc = newDiscovery(graph.Jobs, c.store, func(instance string) {
		c.retire(instance, c.series.Remove(instance))
	}, c.logger)
	g.disc.observe = c.opts.OnInstances
	for _, j := range graph.NotCollectedJobs() {
		c.logger.Warn("job not collected", "component", "run", "provider", j.Provider, "account", j.Account, "job", j.Name,
			"reason", j.NotCollected, "setting", "settings.collection.skipUnconsumed")
	}
	clients := c.accountClients(graph)
	g.executor = collect.New(collect.Options{
		Client:                c.client,
		ClientFor:             func(account string) *http.Client { return clients[account] },
		Budget:                engine,
		Clock:                 c.clock,
		Logger:                c.logger,
		MaxConcurrentRequests: graph.Settings.Collection.MaxConcurrentRequests,
		Retry:                 graph.Settings.Retry,
		Observer:              c.metrics,
	})
	return g, nil
}

// start schedules the instances of a generation and makes it current.
// resume holds the next due times of the instances of the previous
// generation, which keep their schedule.
func (c *Collector) start(ctx context.Context, g *generation, resume map[string]time.Time, reason string) {
	opts := scheduler.Options{
		Clock: c.clock, Jitter: g.graph.Settings.Collection.Jitter.Fraction(), Logger: c.logger, Runner: g.runJob,
		OnRun: c.opts.OnRun, Sequence: &c.runs,
	}
	if resume != nil {
		opts.Resume = func(instance string) (time.Time, bool) {
			t, ok := resume[instance]
			return t, ok
		}
	}
	g.sched = scheduler.New(nil, opts)
	g.disc.attach(ctx, g.sched, c.clock.Now())
	g.projection.on = g.graph.Settings.Budget.ReplanOnProjection == nil || *g.graph.Settings.Budget.ReplanOnProjection
	g.projection.pending = make(chan struct{}, 1)
	g.replan(ctx, reason)
	c.tokens.SetSender(g.executor)
	c.gmu.Lock()
	c.gen = g
	c.gmu.Unlock()
	gctx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	g.loops.Go(func() { g.sched.Run(gctx) })
	g.loops.Go(func() { g.replanLoop(gctx) })
}

// stop stops scheduling and waits for the runs in flight, canceled when ctx
// ends first.
func (g *generation) stop(ctx context.Context, canceledMsg string) {
	g.cancel()
	g.loops.Wait()
	if err := g.sched.Wait(ctx); err != nil {
		g.c.logger.Warn(canceledMsg, "component", "run", "error", err)
	}
}

// replanLoop recomputes the effective intervals every replanInterval,
// whenever a response header changes the max of a quota, whenever discovery
// adds or removes instances, and when the current plan is projected to
// exhaust a quota before its reset, with the remaining budget and time of
// the current windows.
func (g *generation) replanLoop(ctx context.Context) {
	var tick <-chan time.Time
	if interval := g.graph.Settings.Budget.ReplanInterval.Std(); interval > 0 {
		ticker := g.c.clock.NewTicker(interval)
		defer ticker.Stop()
		tick = ticker.C()
	}
	onRun := g.c.opts.OnRun
	for {
		select {
		case <-ctx.Done():
			select {
			case <-g.projection.pending:
				if onRun != nil {
					onRun(-1)
				}
			default:
			}
			return
		case <-tick:
			g.replan(ctx, reasonInterval)
		case <-g.engine.LimitLearned():
			g.replan(ctx, reasonLimitLearned)
		case <-g.disc.changed:
			g.replan(ctx, reasonInventory)
		case <-g.projection.pending:
			// No timer: a run ending within projectionSpacing of the last
			// check is checked with the next one.
			if now := g.c.clock.Now(); now.Sub(g.projection.checked) >= projectionSpacing {
				g.projection.checked = now
				g.checkProjection(ctx, now)
			}
			if onRun != nil {
				onRun(-1)
			}
		}
	}
}

// requestProjection asks replanLoop for a projection check, once a run
// ended: its requests counted against the quotas, and its responses
// reported their remaining. The check counts as work in flight for OnRun
// (tests), from before the run ends until it is done.
func (g *generation) requestProjection() {
	if !g.projection.on {
		return
	}
	onRun := g.c.opts.OnRun
	if onRun != nil {
		onRun(+1)
	}
	select {
	case g.projection.pending <- struct{}{}:
	default: // a check is already pending
		if onRun != nil {
			onRun(-1)
		}
	}
}

// checkProjection replans when the current intervals are projected to spend
// more than the remaining budget of a quota scope before its reset: another
// client started spending the same quota, or Tapyr needs more pages or
// retries than planned. Only a tightening triggers it; a relaxation waits for
// the next replanInterval.
func (g *generation) checkProjection(ctx context.Context, now time.Time) {
	jobs, err := PlanInputs(ctx, g.disc.planEntries(), g.engine, g.c.store)
	if err != nil {
		g.c.logger.Warn("projection check", "component", "planner", "error", err)
		return
	}
	intervals := make(map[string]time.Duration, len(jobs))
	for _, j := range jobs {
		intervals[j.Name] = g.sched.Interval(j.Name)
	}
	if g.projection.scopes == nil {
		g.projection.scopes = map[string]*projectionScope{}
	}
	var over []planner.ScopePlan
	for _, s := range planner.Evaluate(jobs, intervals, g.options(now)) {
		st := g.projection.scopes[s.ID]
		if st == nil {
			st = &projectionScope{}
			g.projection.scopes[s.ID] = st
		}
		if st.disarmed {
			st.disarmed = s.Projected > s.Budget
			continue
		}
		if s.Projected > s.Budget*(1+projectionMargin) && now.Sub(st.triggered) >= projectionCooldown {
			st.triggered = now
			over = append(over, s)
		}
	}
	if len(over) == 0 {
		return
	}
	for _, s := range over {
		g.c.logger.Info("replanning: the current plan would exhaust a quota before its reset", "component", "planner",
			"limit", s.Limit, "scope", s.Key, "budget", math.Round(s.Budget), "projected", math.Round(s.Projected))
	}
	res := g.replan(ctx, reasonProjection)
	if res == nil {
		return
	}
	for _, s := range res.Scopes {
		if st := g.projection.scopes[s.ID]; st != nil && !s.Feasible {
			st.disarmed = true
		}
	}
}

// options are the options of an online plan at now.
func (g *generation) options(now time.Time) planner.Options {
	return planner.Options{
		MaxStretch: g.graph.Settings.Budget.MaxStretch, Online: true, Now: now,
		ReplanInterval: g.graph.Settings.Budget.ReplanInterval.Std(),
	}
}

// replan applies an online plan to the scheduler and returns it, or nil when
// planning failed.
func (g *generation) replan(ctx context.Context, reason string) *planner.Result {
	logger := g.c.logger
	jobs, err := PlanInputs(ctx, g.disc.planEntries(), g.engine, g.c.store)
	if err != nil {
		logger.Warn("planning", "component", "planner", "error", err)
		return nil
	}
	g.c.metrics.PlanRecomputed(reason)
	res := planner.Plan(jobs, g.options(g.c.clock.Now()))
	var sig strings.Builder
	for _, j := range res.Jobs {
		fmt.Fprintf(&sig, "%s=%s;", j.Name, j.Effective)
	}
	for _, s := range res.Scopes {
		fmt.Fprintf(&sig, "%s=%t;", s.ID, s.Feasible)
	}
	g.pmu.Lock()
	g.plan, g.planAt = res, g.c.clock.Now()
	changed := sig.String() != g.planSig
	g.planSig = sig.String()
	g.pmu.Unlock()
	if changed {
		g.c.events.Publish(api.EventPlanChanged, map[string]any{"feasible": res.Feasible})
	}
	for _, j := range res.Jobs {
		if prev := g.sched.Interval(j.Name); prev != j.Effective {
			g.sched.SetInterval(j.Name, j.Effective)
			logger.Info("effective interval changed", "component", "planner", "provider", j.Provider, "account", j.Account,
				"job", j.Name, "desired", j.Desired.String(), "previous", prev.String(), "effective", j.Effective.String(),
				"reason", j.Describe())
		}
	}
	for _, s := range res.Scopes {
		if !s.Feasible {
			logger.Warn("plan infeasible: critical jobs alone exceed the remaining quota", "component", "planner",
				"limit", s.Limit, "scope", s.Key, "budget", s.Budget, "projected", s.Projected)
		}
	}
	return res
}

// runJob executes one run: requests, mapping, then outputs and history.
func (g *generation) runJob(ctx context.Context, run scheduler.Run) {
	defer g.requestProjection() // last: once the run is recorded
	c := g.c
	job := run.Job
	log := c.logger.With("component", "run", "provider", job.Provider, "account", job.Account, "job", job.Name,
		"instance", job.Instance, "run_id", run.ID)
	started := c.clock.Now()
	record := state.RunRecord{Job: job.Name, Instance: job.Instance, RunID: run.ID, Started: started, Result: "success"}
	obs := selfobs.Run{Provider: job.Provider, Account: job.Account, Job: job.Name}
	var res *collect.Result
	var samples []model.Sample
	defer func() {
		record.Ended = c.clock.Now()
		// The error may quote the response (an excerpt of its body), and an
		// API may echo the credentials it received: it is stored and served
		// redacted.
		record.Error = c.redact(record.Error)
		obs.Result, obs.Duration, obs.Pages = record.Result, record.Ended.Sub(started), record.Pages
		c.metrics.RunFinished(obs)
		if err := c.store.AddRun(context.WithoutCancel(ctx), record); err != nil {
			log.Warn("recording the run", "error", err)
		}
		c.debug.keep(job, run.ID, started, record.Result, record.Error, res, samples, g.headers[job.Provider])
		c.events.Publish(api.EventRunFinished, map[string]any{
			"job": job.Name, "instance": job.Instance, "provider": job.Provider, "account": job.Account, "run_id": run.ID,
			"result": record.Result, "duration_ms": record.Ended.Sub(started).Milliseconds(), "samples": record.Samples,
		})
		c.publishCircuit(g, job)
	}()

	res, err := g.executor.Run(ctx, job, started, run.Deadline)
	if res != nil {
		record.Requests, record.Retries, record.Pages = res.Requests, res.Retries, len(res.Pages)
	}
	if err != nil {
		record.Result, record.Error = failure(err)
		attrs := []any{"result", record.Result, "error", err, "duration_ms", c.clock.Since(started).Milliseconds()}
		if ce, ok := errors.AsType[*collect.Error](err); ok {
			attrs = append(attrs, "class", string(ce.Class), "endpoint", ce.Endpoint, "status", ce.Status)
		}
		log.Warn("run failed", attrs...)
		return
	}
	out, err := job.Extract.Evaluate(&mapping.Input{
		Pages: res.Pages, Vars: job.Vars, Account: job.AccountInfo, Target: job.TargetInfo, Now: started, Labels: job.Labels,
	}, job.Limits)
	if err != nil {
		record.Result, record.Error = "failed", err.Error()
		log.Warn("run failed", "result", record.Result, "error", err, "class", string(collect.ClassDecode))
		return
	}
	out.Samples = c.accum.Apply(job.Instance, out.Samples)
	samples = out.Samples
	record.Items, record.Samples = out.Items, len(out.Samples)
	obs.Truncated, obs.MappingErrors = res.Truncated, out.ErrorsByMetric
	for _, mErr := range out.Errors {
		log.Warn("mapping error", "error", mErr)
	}
	if job.Role != config.RoleMetrics {
		if res.Truncated {
			log.Warn("inventory not updated: the run stopped at maxPages", "items", len(out.Inventory))
		} else if err := g.disc.update(ctx, job, out.Inventory, started); err != nil {
			log.Warn("updating the inventory", "error", err)
		}
	}
	deduplicated := 0
	if !g.disc.deliver(job.Instance, func() {
		c.omu.RLock()
		defer c.omu.RUnlock()
		var targets []series.Output
		for _, name := range slices.Sorted(maps.Keys(c.outputs)) {
			if len(job.Outputs) == 0 || slices.Contains(job.Outputs, name) {
				ro := c.outputs[name]
				targets = append(targets, series.Output{Name: name, Dedup: ro.dedup, Processor: ro.proc})
			}
		}
		for name, d := range c.series.Success(job.Instance, out.Samples, targets, started) {
			deduplicated += d.Deduplicated
			if d.Filtered > 0 {
				c.metrics.OutputFiltered(name, d.Filtered)
			}
			if d.Duplicates > 0 {
				c.metrics.OutputDropped(name, "duplicate", d.Duplicates)
				log.Warn("samples dropped: the processing rules of the output made them identical", "output", name,
					"samples", d.Duplicates)
			}
			c.outputs[name].out.Enqueue(model.Batch{
				Instance: job.Instance, Job: job.Name, RunID: run.ID, Provider: job.Provider, Account: job.Account, At: started,
				Samples: d.Samples, Stale: d.Stale,
			})
		}
	}) {
		log.Info("samples dropped: the instance was removed during the run", "samples", len(out.Samples))
		return
	}
	obs.Deduplicated = deduplicated
	log.Info("run finished", "requests", res.Requests, "retries", res.Retries, "pages", len(res.Pages), "items", out.Items,
		"samples", len(out.Samples), "deduplicated", deduplicated, "mapping_errors", out.MappingErrors, "truncated", res.Truncated,
		"duration_ms", c.clock.Since(started).Milliseconds())
}

// publishCircuit publishes the change of the circuit of a job's account
// (and target) seen since its last run.
func (c *Collector) publishCircuit(g *generation, job *runtime.Job) {
	state := g.engine.CircuitState(job.Account, job.Target)
	key := job.Account + "/" + job.Target
	c.cmu.Lock()
	prev, seen := c.circuits[key]
	c.circuits[key] = state
	c.cmu.Unlock()
	if seen && prev != state || !seen && state != budget.CircuitClosed {
		c.events.Publish(api.EventCircuitChanged, map[string]any{
			"provider": job.Provider, "account": job.Account, "target": job.Target, "state": circuitName(state),
		})
	}
}

// failure maps a run error to its result and summary.
func failure(err error) (result, summary string) {
	if reason, ok := budget.IsRefusal(err); ok {
		if reason == budget.ReasonCircuitOpen {
			return "skipped_circuit", err.Error()
		}
		return "skipped_budget", err.Error()
	}
	return "failed", err.Error()
}
