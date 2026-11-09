package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/clock"
)

// TestDiscoveryChainInventoryChange is the discovery acceptance test: a
// 3-level chain (sites, networks per site, devices per network) against the
// inventory-change simulator, Tapyr and the simulator sharing one fake
// clock. After 2 hours a site closes, another opens, a network goes away and
// another appears: the series follow.
func TestDiscoveryChainInventoryChange(t *testing.T) {
	if testing.Short() {
		t.Skip("4 simulated hours")
	}
	midnight := time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(midnight)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "example-sim", "--scenario", "inventory-change")

	dir := t.TempDir()
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: fleet }
spec:
  provider: example-sim
  params: { baseURL: "http://%s/api/v1" }
  secrets: { apiKey: { env: TAPYR_FLEET_API_KEY } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: network-devices }
spec: { account: fleet, template: example-sim-network-devices, interval: 5m }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: discovery }
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
	addr, stopRun := backgroundHooks(t, map[string]string{"TAPYR_FLEET_API_KEY": "sim-api-key"}, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", dir)

	// Idle: every run in flight waits on the clock (see clockBound), so
	// only advancing it makes progress.
	quiet := func() {
		deadline := time.Now().Add(10 * time.Second)
		for !clockBound(clk, &inFlight, &instances) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector not idle: %d runs in flight, %d timers, %d instances", inFlight.Load(), clk.Waiters(), instances.Load())
		}
	}
	advanceTo := func(at time.Time) {
		for clk.Now().Before(at) {
			quiet()
			clk.Advance(10 * time.Second)
		}
		quiet()
	}
	metrics := func() string {
		t.Helper()
		_, body := httpGet(t, "http://"+addr+"/metrics")
		return body
	}

	// Before the change: 3 sites, 5 networks, one device instance each, plus
	// the 3 sites and 3 network instances.
	advanceTo(midnight.Add(time.Hour))
	before := metrics()
	if diff := cmp.Diff([]string{"site-a", "site-b", "site-c"}, labelValues(before, "example_site_devices", "site_id")); diff != "" {
		t.Errorf("sites before the change (-want +got):\n%s", diff)
	}
	wantBefore := []string{"net-a1", "net-a2", "net-a3", "net-b1", "net-c1"}
	if diff := cmp.Diff(wantBefore, labelValues(before, "example_network_info", "network_id")); diff != "" {
		t.Errorf("networks before the change (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantBefore, labelValues(before, "example_network_device_up", "network_id")); diff != "" {
		t.Errorf("devices before the change (-want +got):\n%s", diff)
	}
	if got := instances.Load(); got != 1+3+5 {
		t.Errorf("%d instances before the change, want 9", got)
	}

	// After the change (sites at 2h, networks within their 30m interval,
	// devices within 5m): site-c and its subtree are retired, site-d and
	// net-b2 discovered, net-a3 retired.
	advanceTo(midnight.Add(3 * time.Hour))
	after := metrics()
	if diff := cmp.Diff([]string{"site-a", "site-b", "site-d"}, labelValues(after, "example_site_devices", "site_id")); diff != "" {
		t.Errorf("sites after the change (-want +got):\n%s", diff)
	}
	wantAfter := []string{"net-a1", "net-a2", "net-b1", "net-b2", "net-d1"}
	if diff := cmp.Diff(wantAfter, labelValues(after, "example_network_info", "network_id")); diff != "" {
		t.Errorf("networks after the change (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantAfter, labelValues(after, "example_network_device_up", "network_id")); diff != "" {
		t.Errorf("devices after the change (-want +got):\n%s", diff)
	}
	if got := instances.Load(); got != 1+3+5 {
		t.Errorf("%d instances after the change, want 9", got)
	}

	res := stopRun()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 40))
	}
	t.Logf("%d device series before, %d after; %d runs of network-devices instances",
		strings.Count(before, "example_network_device_up{"), strings.Count(after, "example_network_device_up{"),
		strings.Count(res.stderr, `msg="run finished" component=run provider=example-sim account=fleet job=network-devices`))
	if failed := grepLines(res.stderr, "run failed"); failed != "" {
		t.Errorf("runs failed:\n%s", failed)
	}
	if res := stopSim(); res.code != exitOK {
		t.Errorf("sim exited with %d", res.code)
	}
}

var labelPair = regexp.MustCompile(`(\w+)="([^"]*)"`)

// labelValues returns the sorted distinct values of a label over the series
// of a metric in a Prometheus text exposition.
func labelValues(exposition, metric, label string) []string {
	var out []string
	for line := range strings.SplitSeq(exposition, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		for _, m := range labelPair.FindAllStringSubmatch(line, -1) {
			if m[1] == label && !slices.Contains(out, m[2]) {
				out = append(out, m[2])
			}
		}
	}
	slices.Sort(out)
	return out
}

// collectorIdle reports whether every run has finished and only the idle
// timers are pending: one per scheduled instance, the ledger flush and the
// replan tickers. The timers are read first, then the runs: the scheduler
// counts a run as in flight before it re-arms the timer of its instance, so
// a count of timers that includes that timer always sees the run. In the
// other order, a run starting between the two reads looks idle.
func collectorIdle(clk *clock.Fake, inFlight, instances *atomic.Int64) bool {
	timers := int64(clk.Waiters())
	return timers == instances.Load()+2 && inFlight.Load() == 0
}

// clockBound reports whether the collector can only make progress when the
// fake clock advances. Every scheduled instance waits on a timer, as do the
// ledger flush and replan tickers; a run in flight adds its deadline timer,
// and a second one while it waits on the clock (rate tokens of the budget
// engine, a backoff, simulated latency). New fan-out instances start
// together, so their runs can wait on a rate limit. A run in an HTTP
// request, or an instance between two timers, lowers the count: the
// equality never holds early. The timers are read first, see collectorIdle.
func clockBound(clk *clock.Fake, inFlight, instances *atomic.Int64) bool {
	timers := int64(clk.Waiters())
	return timers == instances.Load()+2+2*inFlight.Load()
}
