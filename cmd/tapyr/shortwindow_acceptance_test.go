package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
)

// TestShortWindowExhaustion is a regression test: a quota of one-minute
// windows exhausted for a few seconds (a 429, or another client bursting)
// was planned over the seconds left in the window, with no budget, and
// every job was stretched to its maximum until the next replan, 15 minutes
// later. On the public demo, ThousandEyes then collected almost nothing.
// Against the org-burst simulator (another integration of the organization
// bursting through three minutes), the jobs keep their desired intervals:
// only the runs of those minutes are refused.
func TestShortWindowExhaustion(t *testing.T) {
	if testing.Short() {
		t.Skip("2 simulated hours")
	}
	start := time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "cisco-thousandeyes", "--scenario", "org-burst")
	defer stopSim()
	dir := t.TempDir()
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: te }
spec:
  provider: cisco-thousandeyes
  params: { baseURL: "http://%s/v7" }
  secrets: { apiToken: { env: TAPYR_TE_TOKEN } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: tests }
spec: { account: te, template: thousandeyes-tests }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: agents }
spec: { account: te, template: thousandeyes-agents }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: network-results }
spec: { account: te, template: thousandeyes-network-results }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: te }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var inFlight, instances atomic.Int64
	_, stopRun := backgroundHooks(t, map[string]string{"TAPYR_TE_TOKEN": "sim-thousandeyes-token"}, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", dir)
	for clk.Now().Before(start.Add(2 * time.Hour)) {
		deadline := time.Now().Add(10 * time.Second)
		for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		clk.Advance(5 * time.Second)
	}

	_, body := httpGet(t, "http://"+simAddr+"/_sim/status")
	var status struct {
		Stats struct {
			Refused map[string]int64 `json:"rate_limited"`
			Routes  map[string]int64 `json:"routes"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	results := status.Stats.Routes["GET /test-results/{testId}/network"]
	agents := status.Stats.Routes["GET /agents"]
	t.Logf("simulator: %d network results, %d agents requests, refusals %v", results, agents, status.Stats.Refused)
	res := stopRun()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 40))
	}
	// The bursts reached Tapyr: some of its requests were answered 429.
	if status.Stats.Refused["quota_exhausted"] == 0 {
		t.Error("no request of Tapyr refused by the bursts")
	}
	// The network results of 3 tests every 5 minutes for 2 hours: 72
	// requests, less those of the burst minutes; agents every 15 minutes: 8.
	// Stretched for 15 minutes after each burst, results would lose 27.
	if results < 66 || agents < 7 {
		t.Errorf("%d network results requests (want about 72), %d agents requests (want about 8)", results, agents)
	}
	if lines := grepLines(res.stderr, `msg="plan infeasible`); lines != "" {
		t.Errorf("the plan became infeasible:\n%s", lines)
	}
	if t.Failed() {
		t.Logf("logs:\n%s", tail(res.stderr, 60))
	}
}
