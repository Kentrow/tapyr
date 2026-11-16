package clock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)

// waitTimeout bounds how long a test waits for a goroutine to react. It uses
// real time on purpose: it only guards against hangs, never drives behavior.
const waitTimeout = 5 * time.Second

func receive(t *testing.T, ch <-chan time.Time) time.Time {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatal("timed out waiting for a value")
		return time.Time{}
	}
}

func assertEmpty(t *testing.T, ch <-chan time.Time) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected value %v", v)
	default:
	}
}

func TestFakeNowAdvanceSet(t *testing.T) {
	f := NewFake(epoch)
	if got := f.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() = %v, want %v", got, epoch)
	}
	f.Advance(90 * time.Second)
	if got, want := f.Now(), epoch.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("Now() after Advance = %v, want %v", got, want)
	}
	if got := f.Since(epoch); got != 90*time.Second {
		t.Fatalf("Since() = %v, want 90s", got)
	}
	if got := f.Until(epoch.Add(time.Hour)); got != time.Hour-90*time.Second {
		t.Fatalf("Until() = %v", got)
	}
	target := epoch.Add(24 * time.Hour)
	f.Set(target)
	if got := f.Now(); !got.Equal(target) {
		t.Fatalf("Now() after Set = %v, want %v", got, target)
	}
	// Moving backwards is allowed (simulates a wall clock jump) and fires nothing.
	f.Set(epoch)
	if got := f.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() after backwards Set = %v, want %v", got, epoch)
	}
}

func TestFakeJumpFiresLateAtTarget(t *testing.T) {
	f := NewFake(epoch)
	first, second, later := f.NewTimer(time.Minute), f.NewTimer(2*time.Minute), f.NewTimer(time.Hour)
	target := epoch.Add(5*time.Minute + 30*time.Second)
	f.Jump(target)
	for i, timer := range []Timer{first, second} {
		select {
		case at := <-timer.C():
			if !at.Equal(target) {
				t.Errorf("timer %d fired at %v, want %v", i, at, target)
			}
		default:
			t.Errorf("timer %d did not fire", i)
		}
	}
	select {
	case <-later.C():
		t.Error("a timer due after the target fired")
	default:
	}
	if got := f.Now(); !got.Equal(target) {
		t.Fatalf("Now() after Jump = %v, want %v", got, target)
	}
	f.Jump(epoch)
	if got := f.Now(); !got.Equal(epoch) || f.Waiters() != 1 {
		t.Fatalf("backward Jump: Now() = %v, %d waiters", got, f.Waiters())
	}
}

func TestFakeTimerFiresAtDeadline(t *testing.T) {
	tests := []struct {
		name    string
		timer   time.Duration
		advance []time.Duration
		fireAt  int // index of the Advance call that fires the timer, -1 for never
	}{
		{name: "exact deadline", timer: time.Minute, advance: []time.Duration{time.Minute}, fireAt: 0},
		{name: "one nanosecond short", timer: time.Minute, advance: []time.Duration{time.Minute - 1, 1}, fireAt: 1},
		{name: "overshoot", timer: time.Minute, advance: []time.Duration{time.Hour}, fireAt: 0},
		{name: "several steps", timer: 10 * time.Second, advance: []time.Duration{3 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second}, fireAt: 3},
		{name: "never reached", timer: time.Hour, advance: []time.Duration{time.Minute}, fireAt: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFake(epoch)
			timer := f.NewTimer(tt.timer)
			for i, d := range tt.advance {
				f.Advance(d)
				if i == tt.fireAt {
					// The value is the deadline, not the time Advance ended at.
					if got, want := receive(t, timer.C()), epoch.Add(tt.timer); !got.Equal(want) {
						t.Fatalf("timer value = %v, want %v", got, want)
					}
				} else {
					assertEmpty(t, timer.C())
				}
			}
		})
	}
}

func TestFakeTimersFireInDeadlineOrder(t *testing.T) {
	f := NewFake(epoch)
	var (
		mu    sync.Mutex
		order []int
		wg    sync.WaitGroup
	)
	// Callbacks record the fake time they observe, proving Advance stops at
	// every deadline instead of jumping to the end.
	seen := make([]time.Time, 3)
	for i, d := range []time.Duration{3 * time.Second, time.Second, 2 * time.Second} {
		wg.Add(1)
		timer := f.NewTimer(d)
		go func() {
			defer wg.Done()
			v := <-timer.C()
			mu.Lock()
			order = append(order, i)
			seen[i] = v
			mu.Unlock()
		}()
	}
	f.Advance(time.Second)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 1 })
	f.Advance(time.Second)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 2 })
	f.Advance(time.Second)
	wg.Wait()

	want := []int{1, 2, 0}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("fire order = %v, want %v", order, want)
		}
	}
	for i, d := range []time.Duration{3 * time.Second, time.Second, 2 * time.Second} {
		if !seen[i].Equal(epoch.Add(d)) {
			t.Errorf("timer %d value = %v, want %v", i, seen[i], epoch.Add(d))
		}
	}
}

func TestFakeAdvanceObservesEachDeadline(t *testing.T) {
	f := NewFake(epoch)
	ticker := f.NewTicker(time.Minute)
	defer ticker.Stop()
	timer := f.NewTimer(90 * time.Second)

	f.Advance(2 * time.Minute)
	// The ticker channel holds one value (later ticks are dropped, like
	// time.Ticker), and that value is the first tick.
	if got, want := receive(t, ticker.C()), epoch.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("first tick = %v, want %v", got, want)
	}
	assertEmpty(t, ticker.C())
	if got, want := receive(t, timer.C()), epoch.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("timer value = %v, want %v", got, want)
	}
}

func TestFakeTimerStop(t *testing.T) {
	f := NewFake(epoch)
	timer := f.NewTimer(time.Second)
	if !timer.Stop() {
		t.Fatal("Stop() on an active timer returned false")
	}
	if timer.Stop() {
		t.Fatal("second Stop() returned true")
	}
	f.Advance(time.Hour)
	assertEmpty(t, timer.C())

	fired := f.NewTimer(time.Second)
	f.Advance(time.Second)
	if fired.Stop() {
		t.Fatal("Stop() on a fired timer returned true")
	}
	// Like Go 1.23+ timers, no stale value is received after Stop.
	assertEmpty(t, fired.C())
}

func TestFakeTimerReset(t *testing.T) {
	f := NewFake(epoch)
	timer := f.NewTimer(time.Second)
	if !timer.Reset(time.Minute) {
		t.Fatal("Reset() on an active timer returned false")
	}
	f.Advance(time.Second)
	assertEmpty(t, timer.C())
	f.Advance(time.Minute - time.Second)
	if got, want := receive(t, timer.C()), epoch.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("timer value = %v, want %v", got, want)
	}

	// Reset after firing, with the value still unread: the stale value is
	// discarded and the timer fires again at the new deadline.
	f.Advance(time.Hour)
	if timer.Reset(time.Second) {
		t.Fatal("Reset() on an expired timer returned true")
	}
	f.Advance(time.Second)
	if got, want := receive(t, timer.C()), epoch.Add(time.Minute+time.Hour+time.Second); !got.Equal(want) {
		t.Fatalf("timer value after Reset = %v, want %v", got, want)
	}
	assertEmpty(t, timer.C())
}

func TestFakeNonPositiveTimerFiresImmediately(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		f := NewFake(epoch)
		timer := f.NewTimer(d)
		if got := receive(t, timer.C()); !got.Equal(epoch) {
			t.Fatalf("NewTimer(%v) value = %v, want %v", d, got, epoch)
		}
		if got := receive(t, f.After(d)); !got.Equal(epoch) {
			t.Fatalf("After(%v) value = %v, want %v", d, got, epoch)
		}
	}
}

func TestFakeTicker(t *testing.T) {
	f := NewFake(epoch)
	ticker := f.NewTicker(10 * time.Second)

	for i := 1; i <= 3; i++ {
		f.Advance(10 * time.Second)
		if got, want := receive(t, ticker.C()), epoch.Add(time.Duration(i)*10*time.Second); !got.Equal(want) {
			t.Fatalf("tick %d = %v, want %v", i, got, want)
		}
	}

	ticker.Reset(time.Minute)
	f.Advance(10 * time.Second)
	assertEmpty(t, ticker.C())
	f.Advance(50 * time.Second)
	if got, want := receive(t, ticker.C()), epoch.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("tick after Reset = %v, want %v", got, want)
	}

	ticker.Stop()
	f.Advance(time.Hour)
	assertEmpty(t, ticker.C())
}

func TestFakeTickerPanicsOnNonPositivePeriod(t *testing.T) {
	f := NewFake(epoch)
	for _, d := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewTicker(%v) did not panic", d)
				}
			}()
			f.NewTicker(d)
		}()
		func() {
			ticker := f.NewTicker(time.Second)
			defer ticker.Stop()
			defer func() {
				if recover() == nil {
					t.Errorf("Ticker.Reset(%v) did not panic", d)
				}
			}()
			ticker.Reset(d)
		}()
	}
}

func TestFakeAfterFunc(t *testing.T) {
	f := NewFake(epoch)
	done := make(chan time.Time, 1)
	timer := f.AfterFunc(time.Minute, func() { done <- f.Now() })
	if timer.C() != nil {
		t.Fatal("AfterFunc timer has a non-nil channel")
	}
	f.Advance(59 * time.Second)
	assertEmpty(t, done)
	f.Advance(time.Second)
	if got, want := receive(t, done), epoch.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("callback observed %v, want %v", got, want)
	}

	stopped := f.AfterFunc(time.Second, func() { t.Error("stopped callback ran") })
	if !stopped.Stop() {
		t.Fatal("Stop() on a pending AfterFunc returned false")
	}
	f.Advance(time.Hour)

	// A callback may use the clock without deadlocking.
	again := make(chan time.Time, 1)
	f.AfterFunc(time.Second, func() {
		f.AfterFunc(time.Second, func() { again <- f.Now() })
	})
	f.Advance(time.Second)
	if err := f.BlockUntil(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	f.Advance(time.Second)
	receive(t, again)
}

func TestFakeSleep(t *testing.T) {
	t.Run("returns after advance", func(t *testing.T) {
		f := NewFake(epoch)
		errc := make(chan error, 1)
		go func() { errc <- f.Sleep(context.Background(), time.Minute) }()
		if err := f.BlockUntil(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		f.Advance(time.Minute)
		if err := waitErr(t, errc); err != nil {
			t.Fatalf("Sleep() = %v, want nil", err)
		}
		if n := f.Waiters(); n != 0 {
			t.Fatalf("Waiters() = %d after Sleep returned, want 0", n)
		}
	})
	t.Run("honors cancellation", func(t *testing.T) {
		f := NewFake(epoch)
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- f.Sleep(ctx, time.Hour) }()
		if err := f.BlockUntil(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := waitErr(t, errc); !errors.Is(err, context.Canceled) {
			t.Fatalf("Sleep() = %v, want context.Canceled", err)
		}
		if n := f.Waiters(); n != 0 {
			t.Fatalf("Waiters() = %d after canceled Sleep, want 0", n)
		}
	})
	t.Run("non-positive duration", func(t *testing.T) {
		f := NewFake(epoch)
		if err := f.Sleep(context.Background(), 0); err != nil {
			t.Fatalf("Sleep(0) = %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.Sleep(ctx, -time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("Sleep(-1s) on canceled ctx = %v, want context.Canceled", err)
		}
	})
}

func TestFakeBlockUntilHonorsContext(t *testing.T) {
	f := NewFake(epoch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.BlockUntil(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("BlockUntil() = %v, want context.Canceled", err)
	}
}

func TestFakeBlockUntilTimers(t *testing.T) {
	f := NewFake(epoch)
	f.AfterFunc(time.Minute, func() {})
	// The callback is a waiter, not a timer.
	if err := f.BlockUntil(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.BlockUntilTimers(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BlockUntilTimers() with an AfterFunc only = %v, want context.DeadlineExceeded", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- f.BlockUntilTimers(context.Background(), 2) }()
	f.NewTimer(time.Minute)
	f.NewTicker(time.Minute)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("BlockUntilTimers() = %v", err)
	}
}

func TestFakeConcurrentUse(t *testing.T) {
	f := NewFake(epoch)
	const sleepers = 50
	var wg sync.WaitGroup
	for i := range sleepers {
		wg.Go(func() {
			if err := f.Sleep(context.Background(), time.Duration(i+1)*time.Second); err != nil {
				t.Errorf("Sleep() = %v", err)
			}
		})
	}
	if err := f.BlockUntil(context.Background(), sleepers); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 100 {
				_ = f.Now()
				_ = f.Waiters()
			}
		})
	}
	for range sleepers {
		f.Advance(time.Second)
	}
	wg.Wait()
	readers.Wait()
}

func waitErr(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(waitTimeout):
		t.Fatal("timed out waiting for Sleep to return")
		return nil
	}
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

func TestFakeNext(t *testing.T) {
	f := NewFake(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	if _, ok := f.Next(); ok {
		t.Fatal("Next() with no waiter")
	}
	later := f.NewTimer(2 * time.Second)
	f.NewTimer(time.Second)
	next, ok := f.Next()
	if !ok || !next.Equal(f.Now().Add(time.Second)) {
		t.Fatalf("Next() = %v, %t", next, ok)
	}
	later.Stop()
	f.Set(next)
	if _, ok := f.Next(); ok {
		t.Fatal("Next() after the last timer fired")
	}
}
