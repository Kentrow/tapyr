// Package scheduler triggers job runs at their intervals (see
// docs/spec/02-architecture.md, "Scheduler"). Every job instance gets a
// phase and runs at phase + k x interval, without drift: a random phase in
// [0, jitter x interval) for configured instances, a phase spread over the
// interval for the instances of a fan-out. A run still in flight when the
// next one is due makes the scheduler skip that next run. Instances can be
// added and removed while the scheduler runs (discovery).
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/runtime"
)

// maxRunDeadline caps the deadline of a run (its interval otherwise).
const maxRunDeadline = 10 * time.Minute

// Run describes one triggered run.
type Run struct {
	Job *runtime.Job
	ID  string
	// Scheduled is the time the run was due.
	Scheduled time.Time
	// Deadline is when the run context is canceled: start + min(interval,
	// 10m), on the injected clock.
	Deadline time.Time
}

// Runner executes a run. ctx is canceled at the run deadline or on forced
// shutdown.
type Runner func(ctx context.Context, run Run)

// Options configures a Scheduler.
type Options struct {
	Clock clock.Clock
	// Rand draws the phases; inject a seeded source in tests.
	Rand *rand.Rand
	// Jitter is the fraction of the interval the phases spread over.
	Jitter float64
	Logger *slog.Logger
	Runner Runner
	// OnRun, when set, is called with +1 before a run starts and -1 once it
	// has returned (tests pacing a fake clock use it to detect idleness).
	OnRun func(delta int)
	// Resume, when set, returns when the next run of an instance was due in
	// a previous scheduler (before a reload, see Next): the instance keeps
	// that schedule instead of starting at its phase. A time already past
	// runs at once; one more than an interval away (the interval shrank) is
	// brought to one interval from now.
	Resume func(instance string) (next time.Time, ok bool)
	// Sequence numbers the runs; share one across schedulers for run IDs
	// unique to the process. Defaults to a sequence of the scheduler.
	Sequence *atomic.Uint64
}

// Scheduler triggers the runs of a set of job instances.
type Scheduler struct {
	opts   Options
	seq    *atomic.Uint64
	runs   sync.WaitGroup
	cancel context.CancelFunc
	runCtx context.Context

	mu        sync.Mutex
	ctx       context.Context // set by Run
	loops     sync.WaitGroup
	instances map[string]*instance
	pending   []string // instances added before Run
	skipped   map[string]int
	groups    map[string]*group
}

type instance struct {
	job      *runtime.Job
	cancel   context.CancelFunc
	inFlight atomic.Bool
	// next is when the next run is due, in Unix nanoseconds (0 until the
	// loop starts).
	next atomic.Int64
}

// group holds the interval shared by the instances of a job and target.
type group struct {
	interval time.Duration
	changed  chan struct{}
}

// Group returns the scheduling group of an instance: its job and target.
// Instances of a fan-out share the group of their model.
func Group(j *runtime.Job) string {
	if j.Target == "" {
		return j.Name
	}
	return j.Name + "[target=" + j.Target + "]"
}

// New returns a scheduler for jobs.
func New(jobs []*runtime.Job, opts Options) *Scheduler {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) //nolint:gosec // G404: phases need no cryptographic randomness.
	}
	runCtx, cancel := context.WithCancel(context.Background())
	if opts.Sequence == nil {
		opts.Sequence = new(atomic.Uint64)
	}
	s := &Scheduler{
		opts: opts, runCtx: runCtx, cancel: cancel, seq: opts.Sequence,
		instances: map[string]*instance{}, skipped: map[string]int{}, groups: map[string]*group{},
	}
	for _, j := range jobs {
		s.Add(j)
	}
	return s
}

// Add schedules a job instance; an instance with the same key is kept.
// Instances added while the scheduler runs start at once (with their phase).
func (s *Scheduler) Add(j *runtime.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[j.Instance]; ok {
		return
	}
	g := Group(j)
	if _, ok := s.groups[g]; !ok {
		s.groups[g] = &group{interval: j.Interval, changed: make(chan struct{})}
	}
	inst := &instance{job: j}
	s.instances[j.Instance] = inst
	if s.ctx == nil {
		s.pending = append(s.pending, j.Instance)
		return
	}
	s.start(inst)
}

// Remove stops scheduling an instance; a run in flight finishes.
func (s *Scheduler) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[key]
	if !ok {
		return
	}
	delete(s.instances, key)
	if inst.cancel != nil {
		inst.cancel()
	}
}

// Next returns when the next run of every started instance is due, by
// instance key (see Options.Resume).
func (s *Scheduler) Next() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]time.Time{}
	for key, inst := range s.instances {
		if ns := inst.next.Load(); ns != 0 {
			out[key] = time.Unix(0, ns)
		}
	}
	return out
}

// Instances returns the number of scheduled instances per group.
func (s *Scheduler) Instances() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, inst := range s.instances {
		out[Group(inst.job)]++
	}
	return out
}

// start launches the loop of an instance; s.mu must be held.
func (s *Scheduler) start(inst *instance) {
	ctx, cancel := context.WithCancel(s.ctx)
	inst.cancel = cancel
	phase := s.phase(inst.job)
	s.loops.Go(func() { s.loop(ctx, inst, phase) })
}

// phase spreads the instances of a fan-out over the interval (stable per
// instance key); configured instances get a random phase within the jitter.
// The hash mixes every byte of the key: instance keys often differ only by
// their last characters (devices[orgId=1], devices[orgId=2]).
func (s *Scheduler) phase(j *runtime.Job) time.Duration {
	if len(j.DiscoveryVars) > 0 {
		sum := sha256.Sum256([]byte(j.Instance))
		return time.Duration(float64(binary.BigEndian.Uint64(sum[:8])) / float64(^uint64(0)) * float64(j.Interval))
	}
	return time.Duration(s.opts.Rand.Float64() * s.opts.Jitter * float64(j.Interval))
}

// Run schedules the instances until ctx is done. It returns once scheduling
// has stopped; runs in flight continue, see Wait.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	for _, key := range s.pending {
		if inst, ok := s.instances[key]; ok && inst.cancel == nil {
			s.start(inst)
		}
	}
	s.pending = nil
	s.mu.Unlock()
	<-ctx.Done()
	s.loops.Wait()
}

// Wait waits for the runs in flight. When ctx ends first, the runs are
// canceled and ctx.Err() is returned.
func (s *Scheduler) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.runs.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.cancel()
		return nil
	case <-ctx.Done():
		s.cancel()
		<-done
		return ctx.Err()
	}
}

// Skipped returns the number of runs of a group skipped because the
// previous run was still in flight or the scheduler fell behind.
func (s *Scheduler) Skipped(group string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skipped[group]
}

// SetInterval changes the effective interval of a group (planner). The next
// run of every instance is due one new interval after its previous one.
func (s *Scheduler) SetInterval(name string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d <= 0 {
		return
	}
	g, ok := s.groups[name]
	if !ok {
		// A fan-out group without instances yet: they will use d.
		s.groups[name] = &group{interval: d, changed: make(chan struct{})}
		return
	}
	if g.interval == d {
		return
	}
	g.interval = d
	close(g.changed)
	g.changed = make(chan struct{})
}

// Interval returns the effective interval of a group.
func (s *Scheduler) Interval(group string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := s.groups[group]; ok {
		return g.interval
	}
	return 0
}

func (s *Scheduler) intervalOf(group string) (time.Duration, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[group]
	return g.interval, g.changed
}

func (s *Scheduler) loop(ctx context.Context, inst *instance, phase time.Duration) {
	clk := s.opts.Clock
	job := inst.job
	g := Group(job)
	next := clk.Now().Add(phase)
	last := time.Time{}
	if s.opts.Resume != nil {
		if due, ok := s.opts.Resume(job.Instance); ok {
			interval, _ := s.intervalOf(g)
			now := clk.Now()
			switch next = due; {
			case next.Before(now):
				next = now
			case next.After(now.Add(interval)):
				next = now.Add(interval)
			}
		}
	}
	for {
		inst.next.Store(next.UnixNano())
		interval, changed := s.intervalOf(g)
		timer := clk.NewTimer(clk.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
			if !last.IsZero() {
				next = last.Add(s.Interval(g))
			}
			continue
		case <-timer.C():
		}
		last = next
		if inst.inFlight.Load() {
			s.skip(job, next, "previous run still in flight")
		} else {
			inst.inFlight.Store(true)
			run := Run{Job: job, ID: fmt.Sprintf("%06d", s.seq.Add(1)), Scheduled: next}
			if s.opts.OnRun != nil {
				s.opts.OnRun(+1)
			}
			s.runs.Go(func() {
				defer func() {
					inst.inFlight.Store(false)
					if s.opts.OnRun != nil {
						s.opts.OnRun(-1)
					}
				}()
				s.execute(run, interval)
			})
		}
		next = next.Add(interval)
		// Catch up after a pause (for example a suspended host) without a
		// burst: the missed slots are skipped.
		if now := clk.Now(); next.Before(now) {
			missed := now.Sub(next)/interval + 1
			next = next.Add(missed * interval)
			for range missed {
				s.skip(job, next, "scheduler fell behind")
			}
		}
	}
}

func (s *Scheduler) skip(job *runtime.Job, at time.Time, reason string) {
	s.mu.Lock()
	s.skipped[Group(job)]++
	s.mu.Unlock()
	s.opts.Logger.Warn("run skipped", "component", "scheduler", "provider", job.Provider, "account", job.Account,
		"job", job.Name, "reason", reason, "scheduled", at)
}

// execute runs with a deadline of min(interval, 10m), measured on the
// injected clock.
func (s *Scheduler) execute(run Run, interval time.Duration) {
	deadline := min(interval, maxRunDeadline)
	run.Deadline = s.opts.Clock.Now().Add(deadline)
	ctx, cancel := context.WithCancel(s.runCtx)
	defer cancel()
	t := s.opts.Clock.AfterFunc(deadline, cancel)
	defer t.Stop()
	s.opts.Runner(ctx, run)
}
