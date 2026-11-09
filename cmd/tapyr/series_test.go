package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
)

// TestStaleAfterWithdrawsSeries checks that the series of an instance whose
// runs keep failing leave the exposition after staleAfter, and come back
// with the next successful run.
func TestStaleAfterWithdrawsSeries(t *testing.T) {
	var failing atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id": "a"}, {"id": "b"}]`))
	}))
	defer api.Close()

	start := time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	dir := t.TempDir()
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: flaky, version: 0.1.0 }
spec:
  baseURL: %q
  auth: { type: none }
---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: flaky }
spec: { provider: flaky }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: items }
spec:
  account: flaky
  interval: 1m
  staleAfter: 3m
  request: { path: /items }
  extract:
    items: response.body
    metrics: [{ name: item_up, value: "1.0", labels: { id: item.id } }]
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: stale }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  retry: { maxAttempts: 1 }
  circuitBreaker: { failureThreshold: 100 }
  log: { format: text }
`, api.URL, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var inFlight, instances atomic.Int64
	addr, stopRun := backgroundHooks(t, nil, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", dir)
	advanceTo := func(at time.Time) {
		for {
			deadline := time.Now().Add(10 * time.Second)
			for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
				time.Sleep(100 * time.Microsecond)
			}
			if time.Now().After(deadline) {
				t.Fatalf("collector not idle: %d runs in flight, %d timers", inFlight.Load(), clk.Waiters())
			}
			if !clk.Now().Before(at) {
				return
			}
			clk.Advance(5 * time.Second)
		}
	}
	// series waits until the exposition holds want series: the flush loop
	// withdraws them while the clock is already idle.
	series := func(want int) int {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, body := httpGet(t, "http://"+addr+"/metrics")
			if n := strings.Count(body, "item_up{"); n == want || time.Now().After(deadline) {
				return n
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	advanceTo(start.Add(30 * time.Second))
	if n := series(2); n != 2 {
		t.Fatalf("%d series after the first run, want 2", n)
	}
	// Runs fail from 1m: the series stay until 3m after the last success.
	failing.Store(true)
	advanceTo(start.Add(3 * time.Minute))
	if n := series(2); n != 2 {
		t.Errorf("%d series within staleAfter, want 2", n)
	}
	advanceTo(start.Add(3*time.Minute + 10*time.Second))
	if n := series(0); n != 0 {
		t.Errorf("%d series after staleAfter, want 0", n)
	}
	failing.Store(false)
	advanceTo(start.Add(4*time.Minute + 30*time.Second))
	if n := series(2); n != 2 {
		t.Errorf("%d series after recovery, want 2", n)
	}

	res := stopRun()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
	}
	if !strings.Contains(res.stderr, `msg="series withdrawn: no successful run within staleAfter"`) {
		t.Errorf("no withdrawal logged:\n%s", tail(res.stderr, 20))
	}
}
