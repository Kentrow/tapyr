package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/clock"
)

// TestWaiting checks that the status API tells waiting from failing: right
// after start, every job waits (for its first run, or for its parent to
// list its items), none once they have run, and, when the parent of a
// discovery chain fails, its children wait for it with its error.
func TestWaiting(t *testing.T) {
	start := time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC)

	t.Run("start then runs", func(t *testing.T) {
		clk := clock.NewFake(start)
		addr, stop, settle := waitingRun(t, clk, "sim-api-key")
		jobs := waitingJobs(t, addr)
		want := map[string]string{
			"devices":                    "first_run",
			"fleet.example-sim-sites":    "first_run",
			"fleet.example-sim-networks": "parent fleet.example-sim-sites",
			"network-devices":            "parent fleet.example-sim-networks",
		}
		for name, reason := range want {
			j, ok := jobs[name]
			if !ok || j.Waiting == nil {
				t.Errorf("%s: not waiting: %+v", name, j)
				continue
			}
			if got := strings.TrimSpace(j.Waiting.Reason + " " + j.Waiting.Parent); got != reason {
				t.Errorf("%s: waiting %q, want %q", name, got, reason)
			}
			// A fan-out waits until its first parent with instances runs:
			// the sites, here, for the networks and their devices.
			if j.Waiting.Until == nil || j.Waiting.Until.Before(start) {
				t.Errorf("%s: waiting until %v", name, j.Waiting.Until)
			}
		}
		if until := jobs["network-devices"].Waiting.Until; until == nil || !until.Equal(*jobs["fleet.example-sim-sites"].NextRun) {
			t.Errorf("network-devices: until %v, the sites run next at %v", until, jobs["fleet.example-sim-sites"].NextRun)
		}
		if nodes := waitingNodes(t, addr); nodes["network-devices"] != "parent" || nodes["devices"] != "first_run" {
			t.Errorf("graph: waiting %v", nodes)
		}

		settle(12 * time.Minute)
		for name, j := range waitingJobs(t, addr) {
			if j.Waiting != nil || j.LastRun == nil {
				t.Errorf("%s: after the runs, waiting %+v, last run %+v", name, j.Waiting, j.LastRun)
			}
		}
		if nodes := waitingNodes(t, addr); len(nodes) != 0 {
			t.Errorf("graph: after the runs, waiting %v", nodes)
		}
		if res := stop(); res.code != exitOK {
			t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
		}
	})

	t.Run("parent failing", func(t *testing.T) {
		clk := clock.NewFake(start)
		addr, stop, settle := waitingRun(t, clk, "rejected-key")
		settle(8 * time.Minute)
		jobs := waitingJobs(t, addr)
		if j := jobs["fleet.example-sim-sites"]; j.Waiting != nil || j.LastRun == nil || j.LastRun.Result != "failed" {
			t.Fatalf("sites: waiting %+v, last run %+v", j.Waiting, j.LastRun)
		}
		failedAt := jobs["fleet.example-sim-sites"].LastRun.EndedAt
		for name, parent := range map[string]string{
			"fleet.example-sim-networks": "fleet.example-sim-sites",
			"network-devices":            "fleet.example-sim-networks",
		} {
			w := jobs[name].Waiting
			switch {
			case w == nil:
				t.Errorf("%s: not waiting", name)
			case w.Reason != "parent_failing" || w.Parent != parent || w.FailingJob != "fleet.example-sim-sites":
				t.Errorf("%s: waiting %+v", name, w)
			case !strings.Contains(w.Error, "401") || w.FailedAt == nil || !w.FailedAt.Equal(failedAt) || w.Until == nil:
				t.Errorf("%s: error %q at %v until %v, want a 401 at %v and the next run of the sites", name, w.Error, w.FailedAt, w.Until, failedAt)
			case w.Problem == nil || w.Problem.Class != "auth" || w.Problem.Title != "The provider rejected the credentials of fleet.":
				t.Errorf("%s: problem %+v", name, w.Problem)
			}
		}
		if strings.Contains(fmt.Sprint(jobs["network-devices"].Waiting), "rejected-key") {
			t.Errorf("the key leaks: %+v", jobs["network-devices"].Waiting)
		}
		if nodes := waitingNodes(t, addr); nodes["network-devices"] != "parent_failing" || nodes["fleet.example-sim-networks"] != "parent_failing" {
			t.Errorf("graph: waiting %v", nodes)
		}
		if res := stop(); res.code != exitOK {
			t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
		}
	})
}

// waitingRun starts tapyr run against the example-sim simulator with a
// static job and a discovery chain of three levels (sites, networks,
// network devices). The jitter spreads the first runs, so that none runs
// before the clock moves. settle advances the clock until the given time
// after the start.
func waitingRun(t *testing.T, clk *clock.Fake, key string) (addr string, stop func() result, settle func(time.Duration)) {
	t.Helper()
	start := clk.Now()
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "example-sim")
	t.Cleanup(func() { stopSim() })
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
metadata: { name: devices }
spec: { account: fleet, template: example-sim-devices, interval: 2m }
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
metadata: { name: s }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 10%% }
  log: { format: text }
`, simAddr, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var inFlight, instances atomic.Int64
	addr, stop = backgroundHooks(t, map[string]string{"TAPYR_FLEET_API_KEY": key}, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", dir)
	settle = func(until time.Duration) {
		for clk.Now().Before(start.Add(until)) {
			deadline := time.Now().Add(10 * time.Second)
			for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
				time.Sleep(100 * time.Microsecond)
			}
			clk.Advance(10 * time.Second)
		}
		deadline := time.Now().Add(10 * time.Second)
		for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
	}
	return addr, stop, settle
}

func waitingJobs(t *testing.T, addr string) map[string]api.Job {
	t.Helper()
	_, body := httpGet(t, "http://"+addr+"/api/v1/jobs")
	var jobs api.List[api.Job]
	if err := json.Unmarshal([]byte(body), &jobs); err != nil {
		t.Fatalf("jobs: %v\n%s", err, body)
	}
	out := map[string]api.Job{}
	for _, j := range jobs.Items {
		out[j.Name] = j
	}
	return out
}

// waitingNodes returns the waiting reason of the job nodes of the graph
// that wait.
func waitingNodes(t *testing.T, addr string) map[string]string {
	t.Helper()
	_, body := httpGet(t, "http://"+addr+"/api/v1/graph")
	var graph api.Graph
	if err := json.Unmarshal([]byte(body), &graph); err != nil {
		t.Fatalf("graph: %v\n%s", err, body)
	}
	out := map[string]string{}
	for _, n := range graph.Nodes {
		if n.Kind == "job" && n.Waiting != "" {
			out[n.Label] = n.Waiting
		}
	}
	return out
}
