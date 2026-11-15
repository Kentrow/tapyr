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

// TestSeriesLimit checks ADR 0014 end to end: the pull output caps its
// series at 5, fewer than the devices job produces. The exposition holds 5
// series, the samples of the others are dropped and counted, the status
// API and the self-metrics report the series, the limit and what it holds
// back, the log warns, and tapyr plan estimates what the output would
// receive without the limit.
func TestSeriesLimit(t *testing.T) {
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
kind: Output
metadata: { name: prom }
spec:
  type: prometheus
  limits: { maxSeries: 5 }
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
	var inFlight, instances atomic.Int64
	addr, stop := backgroundHooks(t, vars, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", dir)
	for clk.Now().Before(start.Add(5 * time.Minute)) {
		deadline := time.Now().Add(10 * time.Second)
		for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		clk.Advance(10 * time.Second)
	}

	_, exposition := httpGet(t, "http://"+addr+"/metrics")
	series := 0
	for line := range strings.SplitSeq(exposition, "\n") {
		if strings.HasPrefix(line, "example_") {
			series++
		}
	}
	if series != 5 {
		t.Errorf("exposition: %d series, want 5:\n%s", series, exposition)
	}

	_, body := httpGet(t, "http://"+addr+"/api/v1/outputs")
	var outputs api.List[api.Output]
	if err := json.Unmarshal([]byte(body), &outputs); err != nil || len(outputs.Items) != 1 {
		t.Fatalf("outputs: %v %s", err, body)
	}
	o := outputs.Items[0]
	if o.Series != 5 || o.MaxSeries != 5 || o.SeriesRefused == 0 || o.SamplesRefused == 0 || len(o.UnmeasuredJobs) != 0 {
		t.Errorf("output: %+v", o)
	}

	_, self := httpGet(t, "http://"+addr+"/metrics/self")
	for _, want := range []string{
		`tapyr_output_series{output="prom"} 5`,
		`tapyr_output_series_limit{output="prom"} 5`,
		`tapyr_output_samples_dropped_total{output="prom",reason="series_limit"} `,
	} {
		if !strings.Contains(self, want) {
			t.Errorf("self-metrics: no %s:\n%s", want, grepLines(self, "tapyr_output_"))
		}
	}

	res := stop()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
	}
	if !strings.Contains(res.stderr, `msg="output series limit reached: new series are held back" component=run`) {
		t.Errorf("run: no warning:\n%s", grepLines(res.stderr, "series limit"))
	}

	// tapyr plan reads the series of the last runs from the state store:
	// what the output would receive without its limit.
	plan := executeWith(t, vars, clk, "plan", "--config", dir, "--output", "json")
	var out planJSON
	if err := json.Unmarshal([]byte(plan.stdout), &out); err != nil {
		t.Fatalf("plan: %v\n%s", err, plan.stdout)
	}
	if len(out.Outputs) != 1 || out.Outputs[0].Series <= 5 || out.Outputs[0].MaxSeries != 5 || out.Outputs[0].SamplesPerMinute <= 0 {
		t.Errorf("plan outputs: %+v", out.Outputs)
	}
	table := executeWith(t, vars, clk, "plan", "--config", dir)
	if !strings.Contains(table.stdout, "new series would be held back") {
		t.Errorf("plan table:\n%s", table.stdout)
	}
}
