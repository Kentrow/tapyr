package scheduler

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/runtime"
)

var epoch = time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)

const waitTimeout = 5 * time.Second

// recorder collects the runs started by the scheduler.
type recorder struct {
	mu   sync.Mutex
	runs []Run
	ch   chan Run
}

func newRecorder() *recorder { return &recorder{ch: make(chan Run, 100)} }

func (r *recorder) runner(_ context.Context, run Run) {
	r.mu.Lock()
	r.runs = append(r.runs, run)
	r.mu.Unlock()
	r.ch <- run
}

func (r *recorder) next(t *testing.T) Run {
	t.Helper()
	select {
	case run := <-r.ch:
		return run
	case <-time.After(waitTimeout):
		t.Fatal("timed out waiting for a run")
		return Run{}
	}
}

func (r *recorder) none(t *testing.T) {
	t.Helper()
	select {
	case run := <-r.ch:
		t.Fatalf("unexpected run %+v", run)
	case <-time.After(20 * time.Millisecond):
	}
}

func start(t *testing.T, s *Scheduler) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Fatal("Run did not return after cancellation")
		}
	}
}

// settle waits until the scheduler loops wait on n timers.
func settle(t *testing.T, clk *clock.Fake, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := clk.BlockUntil(ctx, n); err != nil {
		t.Fatalf("waiting for %d timers: %v", n, err)
	}
}

func TestRunsAtPhasePlusIntervals(t *testing.T) {
	clk := clock.NewFake(epoch)
	rec := newRecorder()
	job := &runtime.Job{Name: "devices", Instance: "devices", Interval: time.Minute}
	s := New([]*runtime.Job{job}, Options{Clock: clk, Rand: rand.New(rand.NewPCG(1, 2)), Jitter: 0.5, Runner: rec.runner})
	phase := time.Duration(rand.New(rand.NewPCG(1, 2)).Float64() * 0.5 * float64(time.Minute))
	if phase <= 0 || phase >= 30*time.Second {
		t.Fatalf("phase %v outside [0, 30s)", phase)
	}
	stop := start(t, s)
	defer stop()

	settle(t, clk, 1)
	clk.Advance(phase - time.Millisecond)
	rec.none(t)
	clk.Advance(time.Millisecond)
	first := rec.next(t)
	if !first.Scheduled.Equal(epoch.Add(phase)) || first.ID != "000001" || first.Job != job {
		t.Fatalf("first run = %+v", first)
	}
	for k := 1; k <= 3; k++ {
		idle(t, s, clk, "devices")
		clk.Advance(time.Minute)
		run := rec.next(t)
		if want := epoch.Add(phase + time.Duration(k)*time.Minute); !run.Scheduled.Equal(want) {
			t.Fatalf("run %d scheduled at %v, want %v (drift)", k, run.Scheduled, want)
		}
	}
}

func TestZeroJitterRunsImmediately(t *testing.T) {
	clk := clock.NewFake(epoch)
	rec := newRecorder()
	s := New([]*runtime.Job{{Name: "a", Instance: "a", Interval: time.Hour}, {Name: "b", Instance: "b", Interval: time.Hour}}, Options{Clock: clk, Runner: rec.runner})
	stop := start(t, s)
	defer stop()
	got := map[string]bool{rec.next(t).Job.Name: true, rec.next(t).Job.Name: true}
	if !got["a"] || !got["b"] {
		t.Fatalf("runs = %v", got)
	}
}

// TestFanOutPhasesSpread is a regression test: with FNV-1a, keys that
// differ only by their last characters got phases within a second of each
// other, and 100 instances fell in 4 tenths of the interval.
func TestFanOutPhasesSpread(t *testing.T) {
	s := New(nil, Options{})
	var tenths [10]int
	for i := range 100 {
		org := strconv.Itoa(i)
		j := &runtime.Job{Name: "devices", Instance: "devices[orgId=" + org + "]", Interval: time.Hour, DiscoveryVars: map[string]string{"orgId": org}}
		p := s.phase(j)
		if p < 0 || p >= time.Hour {
			t.Fatalf("phase %v outside the interval", p)
		}
		tenths[p*10/time.Hour]++
	}
	for i, n := range tenths {
		if n == 0 || n > 25 {
			t.Errorf("100 instances per tenth of the interval: %v (tenth %d)", tenths, i)
			break
		}
	}
}

func TestOverlappingRunIsSkipped(t *testing.T) {
	clk := clock.NewFake(epoch)
	release := make(chan struct{})
	started := make(chan Run, 10)
	runner := func(_ context.Context, run Run) {
		started <- run
		<-release
	}
	job := &runtime.Job{Name: "slow", Instance: "slow", Interval: time.Minute}
	s := New([]*runtime.Job{job}, Options{Clock: clk, Runner: runner})
	stop := start(t, s)
	defer stop()

	<-started
	// The run keeps going: the next slot is skipped.
	settle(t, clk, 2) // the loop timer and the run deadline timer
	clk.Advance(time.Minute)
	waitFor(t, func() bool { return s.Skipped("slow") == 1 })
	close(release)
	waitFor(t, func() bool { return !s.running("slow") })
	settle(t, clk, 1)
	clk.Advance(time.Minute)
	select {
	case run := <-started:
		if !run.Scheduled.Equal(epoch.Add(2 * time.Minute)) {
			t.Fatalf("run after skip scheduled at %v", run.Scheduled)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no run after the skipped one")
	}
}

func TestRunDeadline(t *testing.T) {
	clk := clock.NewFake(epoch)
	canceled := make(chan time.Time, 1)
	runner := func(ctx context.Context, _ Run) {
		<-ctx.Done()
		canceled <- clk.Now()
	}
	tests := []struct {
		interval, deadline time.Duration
	}{
		{time.Minute, time.Minute},
		{time.Hour, 10 * time.Minute},
	}
	for _, tt := range tests {
		clk.Set(epoch)
		s := New([]*runtime.Job{{Name: "j", Instance: "j", Interval: tt.interval}}, Options{Clock: clk, Runner: runner})
		ctx, cancel := context.WithCancel(context.Background())
		go s.Run(ctx)
		settle(t, clk, 2)
		cancel()
		clk.Advance(tt.deadline - time.Second)
		select {
		case <-canceled:
			t.Fatalf("interval %v: run canceled before its deadline", tt.interval)
		case <-time.After(20 * time.Millisecond):
		}
		clk.Advance(time.Second)
		select {
		case at := <-canceled:
			if !at.Equal(epoch.Add(tt.deadline)) {
				t.Errorf("interval %v: canceled at %v", tt.interval, at)
			}
		case <-time.After(waitTimeout):
			t.Fatalf("interval %v: run not canceled at its deadline", tt.interval)
		}
		if err := s.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWait(t *testing.T) {
	clk := clock.NewFake(epoch)
	release := make(chan struct{})
	stopped := make(chan error, 1)
	runner := func(ctx context.Context, _ Run) {
		select {
		case <-release:
			stopped <- nil
		case <-ctx.Done():
			stopped <- ctx.Err()
		}
	}

	t.Run("waits for runs in flight", func(t *testing.T) {
		s := New([]*runtime.Job{{Name: "j", Instance: "j", Interval: time.Hour}}, Options{Clock: clk, Runner: runner})
		stop := start(t, s)
		settle(t, clk, 2)
		stop()
		waited := make(chan error, 1)
		go func() { waited <- s.Wait(context.Background()) }()
		close(release)
		if err := <-waited; err != nil {
			t.Fatal(err)
		}
		if err := <-stopped; err != nil {
			t.Fatalf("run ended with %v", err)
		}
	})

	t.Run("cancels runs after the timeout", func(t *testing.T) {
		s := New([]*runtime.Job{{Name: "j", Instance: "j", Interval: time.Hour}}, Options{Clock: clk, Runner: func(ctx context.Context, _ Run) {
			<-ctx.Done()
			stopped <- ctx.Err()
		}})
		stop := start(t, s)
		settle(t, clk, 2)
		stop()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait() = %v", err)
		}
		if err := <-stopped; !errors.Is(err, context.Canceled) {
			t.Fatalf("run ended with %v", err)
		}
	})
}

func TestCatchUpSkipsMissedSlots(t *testing.T) {
	clk := clock.NewFake(epoch)
	rec := newRecorder()
	s := New([]*runtime.Job{{Name: "j", Instance: "j", Interval: time.Minute}}, Options{Clock: clk, Runner: rec.runner})
	stop := start(t, s)
	defer stop()
	rec.next(t)
	idle(t, s, clk, "j")
	// The host was suspended for 5 minutes and 30 seconds: the time jumps,
	// then the timer fires late (Set would fire it at its deadline, and the
	// loop could read any time in between).
	clk.Jump(epoch.Add(5*time.Minute + 30*time.Second))
	run := rec.next(t)
	if !run.Scheduled.Equal(epoch.Add(time.Minute)) {
		t.Fatalf("late run scheduled at %v", run.Scheduled)
	}
	waitFor(t, func() bool { return s.Skipped("j") == 4 })
	idle(t, s, clk, "j")
	clk.Set(epoch.Add(6 * time.Minute))
	if run := rec.next(t); !run.Scheduled.Equal(epoch.Add(6 * time.Minute)) {
		t.Fatalf("next run scheduled at %v, want the next slot", run.Scheduled)
	}
}

// idle waits until no run of job is in flight and the scheduler loop waits
// on its timer: the next Advance then triggers exactly the next slot.
func idle(t *testing.T, s *Scheduler, clk *clock.Fake, job string) {
	t.Helper()
	waitFor(t, func() bool { return !s.running(job) })
	settle(t, clk, 1)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSetInterval(t *testing.T) {
	clk := clock.NewFake(epoch)
	rec := newRecorder()
	s := New([]*runtime.Job{{Name: "j", Instance: "j", Interval: time.Minute}}, Options{Clock: clk, Runner: rec.runner})
	stop := start(t, s)
	defer stop()
	rec.next(t) // at epoch
	idle(t, s, clk, "j")
	s.SetInterval("j", 5*time.Minute)
	s.SetInterval("unknown", time.Minute)
	if s.Interval("j") != 5*time.Minute {
		t.Fatalf("Interval() = %v", s.Interval("j"))
	}
	idle(t, s, clk, "j")
	clk.Advance(time.Minute)
	rec.none(t)
	idle(t, s, clk, "j")
	clk.Advance(4 * time.Minute)
	if run := rec.next(t); !run.Scheduled.Equal(epoch.Add(5 * time.Minute)) {
		t.Fatalf("run after SetInterval scheduled at %v, want one new interval after the previous run", run.Scheduled)
	}
}

func TestOnRun(t *testing.T) {
	clk := clock.NewFake(epoch)
	var mu sync.Mutex
	var deltas []int
	release := make(chan struct{})
	s := New([]*runtime.Job{{Name: "j", Instance: "j", Interval: time.Hour}}, Options{
		Clock:  clk,
		Runner: func(context.Context, Run) { <-release },
		OnRun: func(d int) {
			mu.Lock()
			deltas = append(deltas, d)
			mu.Unlock()
		},
	})
	stop := start(t, s)
	defer stop()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(deltas) == 1 })
	close(release)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(deltas) == 2 && deltas[0] == 1 && deltas[1] == -1
	})
}

func TestDynamicInstances(t *testing.T) {
	clk := clock.NewFake(epoch)
	rec := newRecorder()
	s := New(nil, Options{Clock: clk, Runner: rec.runner})
	stop := start(t, s)
	defer stop()

	// Fan-out instances spread over the interval, in a stable way.
	children := map[string]time.Time{}
	for _, org := range []string{"1", "2", "3"} {
		s.Add(&runtime.Job{
			Name: "devices", Instance: "devices[orgId=" + org + "]", Interval: time.Hour,
			DiscoveryVars: map[string]string{"orgId": org},
		})
	}
	s.Add(&runtime.Job{Name: "devices", Instance: "devices[orgId=1]", Interval: time.Hour}) // duplicate: kept once
	if got := s.Instances()["devices"]; got != 3 {
		t.Fatalf("instances = %d", got)
	}
	for range 3 {
		settle(t, clk, 1)
		clk.Advance(time.Minute)
		for len(children) < 3 {
			select {
			case run := <-rec.ch:
				children[run.Job.Instance] = run.Scheduled
				continue
			case <-time.After(20 * time.Millisecond):
			}
			break
		}
		if len(children) == 3 {
			break
		}
		for range 59 {
			clk.Advance(time.Minute)
			select {
			case run := <-rec.ch:
				children[run.Job.Instance] = run.Scheduled
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	if len(children) != 3 {
		t.Fatalf("runs = %v", children)
	}
	distinct := map[time.Time]bool{}
	for _, at := range children {
		distinct[at] = true
		if at.Before(epoch) || !at.Before(epoch.Add(time.Hour+time.Minute)) {
			t.Errorf("phase outside the interval: %v", at)
		}
	}
	if len(distinct) < 2 {
		t.Errorf("fan-out instances not spread: %v", children)
	}

	s.Remove("devices[orgId=2]")
	s.Remove("unknown")
	if got := s.Instances()["devices"]; got != 2 {
		t.Fatalf("instances after Remove = %d", got)
	}
}

func TestResume(t *testing.T) {
	clk := clock.NewFake(epoch)
	rec := newRecorder()
	jobs := []*runtime.Job{
		{Name: "a", Instance: "a", Interval: time.Hour},
		{Name: "b", Instance: "b", Interval: time.Hour},
		{Name: "c", Instance: "c", Interval: time.Hour},
		{Name: "d", Instance: "d", Interval: time.Minute},
	}
	var seq atomic.Uint64
	first := New(jobs[:2], Options{Clock: clk, Runner: rec.runner, Sequence: &seq})
	stop := start(t, first)
	rec.next(t)
	rec.next(t)
	settle(t, clk, 2)
	stop()
	if err := first.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := first.Next()
	if len(next) != 2 || !next["a"].Equal(epoch.Add(time.Hour)) {
		t.Fatalf("Next() = %v", next)
	}

	// 20 minutes later, a new scheduler keeps the schedule of a; b was due
	// during the gap and runs at once; c, unknown, starts at its phase; d,
	// whose interval shrank, runs one interval from now.
	clk.Advance(20 * time.Minute)
	now := clk.Now()
	next["b"] = epoch.Add(10 * time.Minute)
	next["d"] = epoch.Add(2 * time.Hour)
	second := New(jobs, Options{Clock: clk, Runner: rec.runner, Sequence: &seq, Resume: func(key string) (time.Time, bool) {
		t, ok := next[key]
		return t, ok
	}})
	stop = start(t, second)
	defer stop()
	got := map[string]Run{}
	for range 2 {
		run := rec.next(t)
		got[run.Job.Name] = run
	}
	if !got["b"].Scheduled.Equal(now) || !got["c"].Scheduled.Equal(now) {
		t.Fatalf("runs after resume = %v", got)
	}
	// Run IDs continue the shared sequence.
	if ids := []string{got["b"].ID, got["c"].ID}; !slices.Contains(ids, "000003") || !slices.Contains(ids, "000004") {
		t.Errorf("run IDs = %v", ids)
	}
	settle(t, clk, 4)
	clk.Advance(time.Minute)
	if run := rec.next(t); run.Job.Name != "d" || !run.Scheduled.Equal(now.Add(time.Minute)) {
		t.Fatalf("capped run = %+v", run)
	}
	settle(t, clk, 4)
	clk.Advance(39 * time.Minute)
	for range 39 {
		if run := rec.next(t); run.Job.Name == "a" {
			if !run.Scheduled.Equal(epoch.Add(time.Hour)) {
				t.Fatalf("resumed run = %+v", run)
			}
			return
		}
	}
	t.Fatal("a did not keep its schedule")
}

// running reports whether a run of the instance is in flight.
func (s *Scheduler) running(key string) bool {
	s.mu.Lock()
	inst, ok := s.instances[key]
	s.mu.Unlock()
	return ok && inst.inFlight.Load()
}
