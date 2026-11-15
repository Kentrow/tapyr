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

// TestNotCollected checks ADR 0013 end to end: the only output keeps the
// metrics of devices alone, so network-devices and its implicit parents
// (sites, networks) are not collected. The simulator receives no request
// for them, the CLI warns, tapyr plan lists them apart, and the status API
// and the self-metrics report them.
func TestNotCollected(t *testing.T) {
	start := time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "example-sim")
	defer stopSim()
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
spec:
  type: prometheus
  processing:
    - { action: keep, sourceLabels: [__name__], regex: "example_devices?(_.*)?" }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: s }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"TAPYR_FLEET_API_KEY": "sim-api-key"}
	notCollected := map[string]string{
		"fleet.example-sim-networks": "no output keeps its metrics",
		"fleet.example-sim-sites":    "no output keeps its metrics",
		"network-devices":            "no output keeps its metrics",
	}

	// tapyr validate and tapyr plan warn, and the plan lists them apart.
	res := execute(t, vars, "validate", "--config", dir)
	if res.code != exitOK {
		t.Fatalf("validate exited with %d: %s", res.code, res.stderr)
	}
	for name := range notCollected {
		if !strings.Contains(res.stderr, fmt.Sprintf("warning: job %q (account fleet) is not collected: no output keeps its metrics", name)) {
			t.Errorf("validate: no warning for %s:\n%s", name, res.stderr)
		}
	}
	plan := executeWith(t, vars, clk, "plan", "--config", dir, "--output", "json")
	var out planJSON
	if err := json.Unmarshal([]byte(plan.stdout), &out); err != nil {
		t.Fatalf("plan: %v\n%s", err, plan.stdout)
	}
	got := map[string]string{}
	for _, j := range out.NotCollected {
		got[j.Name] = j.Reason
	}
	if fmt.Sprint(got) != fmt.Sprint(notCollected) || len(out.Jobs) != 1 || out.Jobs[0].Name != "devices" {
		t.Errorf("plan: not collected %v, planned %+v", got, out.Jobs)
	}

	// tapyr run calls the API for devices only.
	var inFlight, instances atomic.Int64
	addr, stop := backgroundHooks(t, vars, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", dir)
	for clk.Now().Before(start.Add(12 * time.Minute)) {
		deadline := time.Now().Add(10 * time.Second)
		for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		clk.Advance(10 * time.Second)
	}
	_, body := httpGet(t, "http://"+simAddr+"/_sim/status")
	var status struct {
		Stats struct {
			Routes map[string]int64 `json:"routes"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	for route, n := range status.Stats.Routes {
		if n > 0 && route != "GET /devices" {
			t.Errorf("simulator: %d requests to %s", n, route)
		}
	}
	if status.Stats.Routes["GET /devices"] == 0 {
		t.Errorf("simulator: no request to /devices: %v", status.Stats.Routes)
	}

	_, body = httpGet(t, "http://"+addr+"/api/v1/jobs")
	var jobs api.List[api.Job]
	if err := json.Unmarshal([]byte(body), &jobs); err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs.Items {
		if want, ok := notCollected[j.Name]; ok == j.Collected || j.NotCollectedReason != want {
			t.Errorf("job %s: collected %t (%q), want %t (%q)", j.Name, j.Collected, j.NotCollectedReason, !ok, want)
		}
	}
	if len(jobs.Items) != 4 {
		t.Errorf("status API: %d jobs, want 4", len(jobs.Items))
	}
	_, self := httpGet(t, "http://"+addr+"/metrics/self")
	for _, line := range []string{
		`tapyr_job_collected{account="fleet",job="devices",provider="example-sim"} 1`,
		`tapyr_job_collected{account="fleet",job="network-devices",provider="example-sim"} 0`,
	} {
		if !strings.Contains(self, line) {
			t.Errorf("self-metrics: no %s:\n%s", line, grepLines(self, "tapyr_job_collected"))
		}
	}
	res = stop()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
	}
	if !strings.Contains(res.stderr, `msg="job not collected"`) || !strings.Contains(res.stderr, "job=network-devices") {
		t.Errorf("run: no warning for network-devices:\n%s", grepLines(res.stderr, "not collected"))
	}
}
