package push

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/output/buffer"
)

// downSender is a backend that refuses every send while down, and records
// the batches it accepts once up.
type downSender struct {
	mu      sync.Mutex
	down    bool
	batches [][]float64
}

func (s *downSender) Send(_ context.Context, points []Point) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return Result{}, &Error{Class: ClassTransient, Err: errors.New("connection refused")}
	}
	values := make([]float64, len(points))
	for i, p := range points {
		values[i] = p.Value
	}
	s.batches = append(s.batches, values)
	return Result{}, nil
}

func (s *downSender) setDown(down bool) { s.mu.Lock(); s.down = down; s.mu.Unlock() }

// values returns every value accepted, in order.
func (s *downSender) values() []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []float64
	for _, b := range s.batches {
		out = append(out, b...)
	}
	return out
}

// diskQueue is a queue on a disk buffer, running until stop.
type diskQueue struct {
	q    *Queue
	obs  *counts
	stop func()
}

func startDiskQueue(t *testing.T, dir string, clk *clock.Fake, sender Sender, capacity, maxBatch int, maxAge time.Duration) *diskQueue {
	t.Helper()
	b, err := buffer.Open(buffer.Options{Dir: dir, MaxSize: 64 << 20, MaxAge: maxAge})
	if err != nil {
		t.Fatal(err)
	}
	obs := &counts{dropped: map[string]int{}, failures: map[string]int{}}
	q := New(Options{
		Name: "out", Capacity: capacity, MaxBatch: maxBatch, FlushInterval: 5 * time.Second,
		Sender: sender, Clock: clk, Observer: obs, Buffer: b, MaxAge: maxAge,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = q.Run(ctx)
	}()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return &diskQueue{q: q, obs: obs, stop: stop}
}

// advanceUntil advances the fake clock by steps until cond holds.
func advanceUntil(t *testing.T, clk *clock.Fake, step time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		clk.Advance(step)
		time.Sleep(time.Millisecond)
	}
}

// TestDiskSurvivesRestart checks the promise of the buffer: the points
// queued while the backend is down are sent by the next run of Tapyr.
func TestDiskSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(start)
	down := &downSender{down: true}
	first := startDiskQueue(t, dir, clk, down, 100, 10, 0)
	first.q.Add(points(1, 2, 3))
	first.q.Add(points(4))
	first.stop() // Tapyr stops with the backend still down

	up := &downSender{}
	second := startDiskQueue(t, dir, clk, up, 100, 10, 0)
	advanceUntil(t, clk, time.Second, "the points of the previous run", func() bool { return len(up.values()) == 4 })
	if got := up.values(); !cmp.Equal(got, []float64{1, 2, 3, 4}) {
		t.Errorf("sent = %v", got)
	}
	second.stop()

	// Acknowledged: a third run sends nothing again.
	again := &downSender{}
	third := startDiskQueue(t, dir, clk, again, 100, 10, 0)
	clk.Advance(10 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if got := again.values(); len(got) != 0 {
		t.Errorf("sent again = %v", got)
	}
	if s := third.q.Buffered(); s.Records != 0 {
		t.Errorf("buffer = %+v", s)
	}
}

// TestDiskBacklogBeyondCapacity checks that a full memory queue leaves the
// points on disk instead of dropping them, and sends them in order.
func TestDiskBacklogBeyondCapacity(t *testing.T) {
	clk := clock.NewFake(start)
	backend := &downSender{down: true}
	dq := startDiskQueue(t, t.TempDir(), clk, backend, 4, 4, 0)
	for v := 1; v <= 10; v++ {
		dq.q.Add(points(float64(v)))
	}
	if st := dq.q.State(); st.Queue > 4 || st.Dropped != 0 {
		t.Errorf("state = %+v: the memory queue holds at most its capacity and drops nothing", st)
	}
	if s := dq.q.Buffered(); s.Points != 10 {
		t.Errorf("buffered = %+v", s)
	}
	backend.setDown(false)
	advanceUntil(t, clk, time.Second, "the backlog", func() bool { return len(backend.values()) == 10 })
	if got := backend.values(); !cmp.Equal(got, []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Errorf("sent = %v, want every point in order", got)
	}
	dq.obs.mu.Lock()
	defer dq.obs.mu.Unlock()
	if len(dq.obs.dropped) != 0 {
		t.Errorf("dropped = %v", dq.obs.dropped)
	}
}

// TestDiskMaxAge checks that points older than the maximum age are dropped
// instead of sent, and counted.
func TestDiskMaxAge(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(start)
	first := startDiskQueue(t, dir, clk, &downSender{down: true}, 100, 10, 2*time.Hour)
	first.q.Add(points(1, 2, 3))
	first.stop()

	clk.Advance(3 * time.Hour) // Tapyr was down for three hours
	backend := &downSender{}
	second := startDiskQueue(t, dir, clk, backend, 100, 10, 2*time.Hour)
	second.q.Add(points(4))
	advanceUntil(t, clk, time.Second, "the new point", func() bool { return len(backend.values()) == 1 })
	if got := backend.values(); !cmp.Equal(got, []float64{4}) {
		t.Errorf("sent = %v, want only the new point", got)
	}
	advanceUntil(t, clk, time.Second, "the drop", func() bool {
		second.obs.mu.Lock()
		defer second.obs.mu.Unlock()
		return second.obs.dropped["too_old"] == 3
	})
}

// TestDiskNotWritable checks that a buffer that cannot be written leaves
// the queue working in memory.
func TestDiskNotWritable(t *testing.T) {
	clk := clock.NewFake(start)
	backend := &downSender{}
	dq := startDiskQueue(t, t.TempDir(), clk, backend, 100, 10, 0)
	if err := dq.q.opts.Buffer.Close(); err != nil { // every write fails from now on
		t.Fatal(err)
	}
	dq.q.Add(points(1, 2))
	advanceUntil(t, clk, time.Second, "the points kept in memory", func() bool { return len(backend.values()) == 2 })
	dq.q.mu.Lock()
	failing := dq.q.diskFailing
	dq.q.mu.Unlock()
	if !failing {
		t.Error("the queue does not know its buffer is failing")
	}
}
