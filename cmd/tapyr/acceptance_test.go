package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
)

// TestTwentyFourHoursQuotaTight checks the budget engine end to end: 24 simulated
// hours against the quota-tight simulator, Tapyr and the simulator sharing
// one fake clock.
func TestTwentyFourHoursQuotaTight(t *testing.T) {
	if testing.Short() {
		t.Skip("24 simulated hours")
	}
	midnight := time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(midnight)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "example-sim", "--scenario", "quota-tight")

	dir := t.TempDir()
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: tight }
spec:
  provider: example-sim
  params: { baseURL: "http://%s/api/v1" }
  secrets: { apiKey: { env: TAPYR_TIGHT_API_KEY } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: tight-devices }
spec: { account: tight, template: example-sim-devices, interval: 10m, priority: critical }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: tight-sites }
spec: { account: tight, template: example-sim-sites, interval: 5m, priority: low }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: tight }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"TAPYR_TIGHT_API_KEY": "sim-api-key"}
	var inFlight atomic.Int64
	_, stopRun := backgroundEnv(t, vars, clk, func(d int) { inFlight.Add(int64(d)) }, "run", "--config", dir)

	// Drive the shared clock until just before the end of the daily window.
	// Before every step, wait until the collector is idle: no run in flight
	// and only its idle timers pending (two scheduler loops, the ledger flush
	// and the replan tickers), so every loop has re-armed. Advancing faster
	// than the collector can follow would make the scheduler skip slots, as
	// it does for a suspended host.
	const idleWaiters = 4
	quiet := func() {
		deadline := time.Now().Add(10 * time.Second)
		for (clk.Waiters() != idleWaiters || inFlight.Load() != 0) && time.Now().Before(deadline) { // timers first, see collectorIdle
			time.Sleep(100 * time.Microsecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector not idle: %d runs in flight, %d timers", inFlight.Load(), clk.Waiters())
		}
	}
	end := midnight.Add(24*time.Hour - 30*time.Second)
	for clk.Now().Before(end) {
		quiet()
		clk.Advance(10 * time.Second)
	}
	quiet()

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
	devices, sites := status.Stats.Routes["GET /devices"], status.Stats.Routes["GET /sites"]
	measured := devices + sites
	t.Logf("simulator: %d device requests, %d site requests, refusals %v", devices, sites, status.Stats.Refused)

	// Zero 429 caused by a quota (and none at all: no fault is injected).
	if n := status.Stats.Refused["quota_exhausted"]; n != 0 {
		t.Errorf("%d requests refused by the quota", n)
	}
	for reason, n := range status.Stats.Refused {
		if n != 0 {
			t.Errorf("%d requests refused by the simulator (%s)", n, reason)
		}
	}
	// The critical job stays on schedule: 144 runs of 3 pages.
	if devices != 144*3 {
		t.Errorf("critical job: %d device requests, want about 432 (144 runs)", devices)
	}
	// The low-priority job is stretched to fit the quota (unstretched it
	// would need 576 requests on its own).
	if sites > 200 || measured > 600 {
		t.Errorf("low-priority job: %d site requests, %d in total for a quota of 600", sites, measured)
	}

	res := stopRun()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 40))
	}
	if !strings.Contains(res.stderr, `msg="effective interval changed" component=planner provider=example-sim account=tight job=tight-sites`) {
		t.Errorf("the low-priority job was not stretched:\n%s", tail(res.stderr, 40))
	}
	if lines := grepLines(res.stderr, "job=tight-devices"); strings.Contains(lines, "run failed") {
		t.Errorf("runs of the critical job failed:\n%s", grepLines(lines, "run failed"))
	}

	// tapyr plan, with the learned quota and the run history, projects the
	// consumption within 15% of what the simulator measured.
	plan := planJSONOf(t, clk, dir, vars)
	var projected float64
	for _, s := range plan.Scopes {
		if s.Limit == "example-sim/daily" {
			projected = s.Projected
		}
	}
	if projected == 0 || math.Abs(float64(measured)-projected)/projected > 0.15 {
		t.Errorf("plan projects %.0f calls per day, the simulator measured %d (more than 15%% apart)", projected, measured)
	}
	t.Logf("plan projects %.0f calls per day, measured %d", projected, measured)
	if res := stopSim(); res.code != exitOK {
		t.Errorf("sim exited with %d", res.code)
	}
}

// planJSONOf runs tapyr plan --output json with the shared clock.
func planJSONOf(t *testing.T, clk clock.Clock, dir string, vars map[string]string) planJSON {
	t.Helper()
	var stdout, stderr bytes.Buffer
	lookupEnv := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	code := run(context.Background(), []string{"plan", "--config", dir, "--output", "json"},
		&env{stdout: &stdout, stderr: &stderr, lookupEnv: lookupEnv, clock: clk, listen: (&net.ListenConfig{}).Listen})
	if code != exitOK && code != exitInfeasible {
		t.Fatalf("plan exited with %d: %s", code, stderr.String())
	}
	var out planJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("plan output: %v\n%s", err, stdout.String())
	}
	return out
}

func grepLines(s, substr string) string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// arubaDay runs 24 simulated hours of Tapyr against the
// aruba-central-classic simulator with a scenario: a critical job every 10
// minutes and three low-priority jobs every 2 minutes, about 2300 calls a
// day at their desired intervals. It returns the requests the simulator
// served by route, its refusals, the self-metrics and logs of the run, and
// the daily consumption projected by tapyr plan afterwards.
func arubaDay(t *testing.T, scenario string) (routes, refused map[string]int64, self, stderr string, projected float64) {
	t.Helper()
	midnight := time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(midnight)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "aruba-central-classic", "--scenario", scenario)

	dir := t.TempDir()
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: campus }
spec:
  provider: aruba-central-classic
  params: { baseURL: "http://%s" }
  secrets:
    clientId: { env: TAPYR_ARUBA_CLIENT_ID }
    clientSecret: { env: TAPYR_ARUBA_CLIENT_SECRET }
    refreshToken: { env: TAPYR_ARUBA_REFRESH_TOKEN }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: aps }
spec: { account: campus, template: aruba-aps, interval: 10m, priority: critical }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: switches }
spec: { account: campus, template: aruba-switches, interval: 2m, priority: low }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: gateways }
spec: { account: campus, template: aruba-gateways, interval: 2m, priority: low }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: sites }
spec: { account: campus, template: aruba-sites, interval: 2m, priority: low }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: aruba }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"TAPYR_ARUBA_CLIENT_ID": "sim-client", "TAPYR_ARUBA_CLIENT_SECRET": "sim-client-secret",
		"TAPYR_ARUBA_REFRESH_TOKEN": "sim-refresh-token", "TAPYR_STATE_KEY": "acceptance-state-key",
	}
	var inFlight, instances atomic.Int64
	addr, stopRun := backgroundHooks(t, vars, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", dir)

	// Drive the shared clock minute by minute until just before the end of
	// the daily window, waiting before every step until the collector is
	// idle: no run in flight, one timer per instance and two loop timers.
	end := midnight.Add(24*time.Hour - time.Minute)
	for {
		deadline := time.Now().Add(10 * time.Second)
		for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector not idle at %s: %d runs in flight, %d timers", clk.Now().Format(time.TimeOnly), inFlight.Load(), clk.Waiters())
		}
		if !clk.Now().Before(end) {
			break
		}
		clk.Advance(time.Minute)
	}
	_, self = httpGet(t, "http://"+addr+"/metrics/self")

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
	t.Logf("simulator: routes %v, refusals %v", status.Stats.Routes, status.Stats.Refused)
	res := stopRun()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 40))
	}
	for _, s := range planJSONOf(t, clk, dir, vars).Scopes {
		if s.Limit == "aruba-central-classic/daily" {
			projected = s.Projected
		}
	}
	if res := stopSim(); res.code != exitOK {
		t.Errorf("sim exited with %d", res.code)
	}
	return status.Stats.Routes, status.Stats.Refused, self, res.stderr, projected
}

// checkNo429 fails on any refusal of the simulator and on any unexpected
// 429 counted by Tapyr.
func checkNo429(t *testing.T, refused map[string]int64, self string) {
	t.Helper()
	for reason, n := range refused {
		if n != 0 {
			t.Errorf("%d requests refused by the simulator (%s)", n, reason)
		}
	}
	if lines := grepLines(self, "tapyr_budget_unexpected_429_total{"); lines != "" && !strings.HasSuffix(strings.TrimSpace(lines), " 0") {
		t.Errorf("unexpected 429:\n%s", lines)
	}
}

// TestTwentyFourHoursArubaPlan checks the success criterion of v0.1.0 on
// plan accuracy against the aruba-central-classic simulator: tapyr plan,
// with the learned quota and the run history, projects the consumption of
// a day within 15% of what the simulator measured.
func TestTwentyFourHoursArubaPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("24 simulated hours")
	}
	routes, refused, self, stderr, projected := arubaDay(t, "default")
	checkNo429(t, refused, self)
	if failed := grepLines(stderr, "run failed"); failed != "" {
		t.Errorf("runs failed:\n%s", failed)
	}
	var measured int64
	for _, n := range routes {
		measured += n
	}
	if projected == 0 || math.Abs(float64(measured)-projected)/projected > 0.15 {
		t.Errorf("plan projects %.0f calls per day, the simulator measured %d (more than 15%% apart)", projected, measured)
	}
	t.Logf("plan projects %.0f calls per day, measured %d", projected, measured)
}

// TestTwentyFourHoursArubaQuotaTight checks the success criterion of
// v0.1.0 on undersized quotas against the aruba-central-classic simulator:
// its quota-tight scenario shares the 5000 daily calls with another
// integration spending 150 an hour, which leaves Tapyr about 900 with its
// retry reserve, much less than the 2300 its jobs would spend at their
// desired intervals. No 429, the critical job on schedule, the
// low-priority jobs stretched.
func TestTwentyFourHoursArubaQuotaTight(t *testing.T) {
	if testing.Short() {
		t.Skip("24 simulated hours")
	}
	routes, refused, self, stderr, _ := arubaDay(t, "quota-tight")
	checkNo429(t, refused, self)
	if aps := routes["GET /monitoring/v2/aps"]; aps != 144 {
		t.Errorf("critical job: %d requests, want 144", aps)
	}
	if failed := grepLines(grepLines(stderr, "job=aps"), "run failed"); failed != "" {
		t.Errorf("runs of the critical job failed:\n%s", failed)
	}
	low := routes["GET /monitoring/v1/switches"] + routes["GET /monitoring/v1/gateways"] + routes["GET /central/v2/sites"]
	if low > 1000 {
		t.Errorf("low-priority jobs: %d requests, not stretched enough", low)
	}
	if !strings.Contains(stderr, `msg="effective interval changed" component=planner provider=aruba-central-classic account=campus job=sites`) {
		t.Errorf("the low-priority jobs were not stretched:\n%s", tail(stderr, 40))
	}
	if strings.Contains(grepLines(stderr, "msg=\"effective interval changed\""), "job=aps") {
		t.Error("the critical job was stretched")
	}
}
