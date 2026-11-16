package clock

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Fake is a manually driven Clock for tests. Time only moves when Advance,
// Set or Jump is called. Timers and tickers fire in deadline order, and Now
// returns each deadline while the corresponding waiter fires, so daily and
// monthly logic can be exercised in milliseconds.
//
// Channel deliveries are non-blocking with a buffer of one value, like the
// time package: a ticker whose channel is full drops ticks. AfterFunc
// callbacks run in their own goroutine, as with the real clock.
//
// Fake is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	seq     uint64
	waiters []*fakeWaiter
	// changed is closed and replaced every time the set of waiters changes,
	// which lets BlockUntil wait without polling.
	changed chan struct{}
}

// NewFake returns a Fake clock set to start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start, changed: make(chan struct{})}
}

type fakeWaiter struct {
	clock    *Fake
	deadline time.Time
	seq      uint64 // tie-breaker: creation order for equal deadlines
	period   time.Duration
	ch       chan time.Time
	fn       func()
	active   bool
}

// Now returns the current fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since returns the fake time elapsed since t.
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Until returns the fake duration until t.
func (f *Fake) Until(t time.Time) time.Duration { return t.Sub(f.Now()) }

// NewTimer creates a fake Timer. A non-positive duration fires immediately.
func (f *Fake) NewTimer(d time.Duration) Timer {
	w := &fakeWaiter{clock: f, ch: make(chan time.Time, 1)}
	f.schedule(w, d)
	return w
}

// After is shorthand for NewTimer(d).C().
func (f *Fake) After(d time.Duration) <-chan time.Time {
	return f.NewTimer(d).C()
}

// AfterFunc calls fn in its own goroutine once the fake time reaches the
// deadline. A non-positive duration runs fn immediately.
func (f *Fake) AfterFunc(d time.Duration, fn func()) Timer {
	w := &fakeWaiter{clock: f, fn: fn}
	f.schedule(w, d)
	return w
}

// NewTicker creates a fake Ticker. It panics if d <= 0.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	w := &fakeWaiter{clock: f, ch: make(chan time.Time, 1), period: d}
	f.schedule(w, d)
	return fakeTicker{w}
}

// Sleep blocks until the fake time advances by d or ctx is done.
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := f.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C():
		return nil
	}
}

// Advance moves the fake time forward by d, firing every waiter whose
// deadline is reached, in order. A negative d is ignored.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		return
	}
	f.advanceTo(f.Now().Add(d))
}

// Set moves the fake time to t. Moving forward fires waiters like Advance;
// moving backward only changes the time (a simulated wall clock jump).
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	if t.Before(f.now) {
		f.now = t
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()
	f.advanceTo(t)
}

// Jump moves the fake time to t at once, then fires the waiters that are
// due, all at t: a host resumed from a suspension, whose timers fire late.
// Unlike Set, a goroutine woken by a timer never sees a time between the
// old one and t. Moving backward only changes the time, like Set.
func (f *Fake) Jump(t time.Time) {
	f.mu.Lock()
	f.now = t
	f.mu.Unlock()
	f.advanceTo(t)
}

// Waiters returns the number of pending timers, tickers and sleepers.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// Next returns the deadline of the earliest pending waiter, or false when
// none is pending.
func (f *Fake) Next() (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.waiters) == 0 {
		return time.Time{}, false
	}
	return f.waiters[0].deadline, true
}

// BlockUntil blocks until at least n waiters are pending, or ctx is done. It
// lets a test wait for goroutines to reach Sleep or a timer before calling
// Advance.
func (f *Fake) BlockUntil(ctx context.Context, n int) error {
	return f.blockUntil(ctx, n, func(*fakeWaiter) bool { return true })
}

// BlockUntilTimers is BlockUntil counting the timers, tickers and sleepers
// only: AfterFunc callbacks are left out. It lets a test wait for loops to
// arm their timers while work in flight holds AfterFunc deadlines, which
// BlockUntil would count in their place.
func (f *Fake) BlockUntilTimers(ctx context.Context, n int) error {
	return f.blockUntil(ctx, n, func(w *fakeWaiter) bool { return w.fn == nil })
}

func (f *Fake) blockUntil(ctx context.Context, n int, counted func(*fakeWaiter) bool) error {
	for {
		f.mu.Lock()
		count := 0
		for _, w := range f.waiters {
			if counted(w) {
				count++
			}
		}
		if count >= n {
			f.mu.Unlock()
			return nil
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (f *Fake) advanceTo(target time.Time) {
	for {
		f.mu.Lock()
		if len(f.waiters) == 0 || f.waiters[0].deadline.After(target) {
			if target.After(f.now) {
				f.now = target
			}
			f.mu.Unlock()
			return
		}
		w := f.waiters[0]
		if w.deadline.After(f.now) {
			f.now = w.deadline
		}
		f.removeLocked(w)
		if w.period > 0 {
			f.insertLocked(w, w.deadline.Add(w.period))
		}
		now := f.now
		f.mu.Unlock()
		w.fire(now)
	}
}

func (f *Fake) schedule(w *fakeWaiter, d time.Duration) {
	f.mu.Lock()
	if d <= 0 {
		now := f.now
		f.mu.Unlock()
		w.fire(now)
		return
	}
	f.insertLocked(w, f.now.Add(d))
	f.mu.Unlock()
}

func (f *Fake) insertLocked(w *fakeWaiter, deadline time.Time) {
	f.seq++
	w.deadline, w.seq, w.active = deadline, f.seq, true
	i, _ := slices.BinarySearchFunc(f.waiters, w, func(a, b *fakeWaiter) int {
		if c := a.deadline.Compare(b.deadline); c != 0 {
			return c
		}
		switch {
		case a.seq < b.seq:
			return -1
		case a.seq > b.seq:
			return 1
		}
		return 0
	})
	f.waiters = slices.Insert(f.waiters, i, w)
	f.notifyLocked()
}

func (f *Fake) removeLocked(w *fakeWaiter) bool {
	if !w.active {
		return false
	}
	w.active = false
	if i := slices.Index(f.waiters, w); i >= 0 {
		f.waiters = slices.Delete(f.waiters, i, i+1)
	}
	f.notifyLocked()
	return true
}

func (f *Fake) notifyLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (w *fakeWaiter) fire(now time.Time) {
	if w.fn != nil {
		go w.fn()
		return
	}
	select {
	case w.ch <- now:
	default: // channel full: drop, like time.Ticker
	}
}

// drain discards a value delivered but not yet received, so Stop and Reset
// never leave a stale value behind.
func (w *fakeWaiter) drain() {
	if w.ch == nil {
		return
	}
	select {
	case <-w.ch:
	default:
	}
}

func (w *fakeWaiter) C() <-chan time.Time {
	if w.ch == nil {
		return nil
	}
	return w.ch
}

func (w *fakeWaiter) Stop() bool {
	f := w.clock
	f.mu.Lock()
	defer f.mu.Unlock()
	stopped := f.removeLocked(w)
	w.drain()
	return stopped
}

func (w *fakeWaiter) Reset(d time.Duration) bool {
	return w.reset(d, keepPeriod)
}

// keepPeriod tells reset to leave the waiter period unchanged.
const keepPeriod time.Duration = -1

// reset re-arms the waiter. The period is read and updated under the clock
// lock because advanceTo reads it while firing.
func (w *fakeWaiter) reset(d, period time.Duration) bool {
	f := w.clock
	f.mu.Lock()
	wasActive := f.removeLocked(w)
	w.drain()
	if period != keepPeriod {
		w.period = period
	}
	f.mu.Unlock()
	f.schedule(w, d)
	return wasActive
}

type fakeTicker struct{ w *fakeWaiter }

func (t fakeTicker) C() <-chan time.Time { return t.w.ch }
func (t fakeTicker) Stop()               { t.w.Stop() }

func (t fakeTicker) Reset(d time.Duration) {
	if d <= 0 {
		panic("clock: non-positive interval for Ticker.Reset")
	}
	t.w.reset(d, d)
}
