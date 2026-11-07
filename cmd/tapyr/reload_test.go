package main

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
)

// TestReloadUnderLoad is the reload acceptance test: 30 simulated
// minutes with a configuration reload every 2m30s, sent while runs are due,
// alternating between a configuration with and without an extra job. No run
// of the jobs that stay is lost or added, the removed job is retired, and
// reloads leak no goroutines.
func TestReloadUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("30 simulated minutes")
	}
	start := time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "example-sim")

	dir := t.TempDir()
	base := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
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
spec: { account: fleet, template: example-sim-devices, interval: 2m, priority: critical }
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
metadata: { name: reload }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, filepath.Join(dir, "tapyr.db"))
	extra := `---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: devices-extra }
spec: { account: fleet, template: example-sim-devices, interval: 3m, labels: { copy: "yes" } }
`
	path := filepath.Join(dir, "config.yaml")
	write := func(withExtra bool) {
		t.Helper()
		content := base
		if withExtra {
			content += extra
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(false)

	reload := make(chan struct{}, 1)
	reloaded := make(chan error, 1)
	var inFlight, instances atomic.Int64
	addr, stopRun := backgroundHooks(t, map[string]string{"TAPYR_FLEET_API_KEY": "sim-api-key"}, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
		reload:      reload,
		onReload:    func(err error) { reloaded <- err },
	}, "run", "--config", path)

	quiet := func() {
		deadline := time.Now().Add(10 * time.Second)
		for !clockBound(clk, inFlight.Load(), instances.Load()) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector not idle at %s: %d runs in flight, %d timers, %d instances",
				clk.Now().Sub(start), inFlight.Load(), clk.Waiters(), instances.Load())
		}
	}
	end := start.Add(30*time.Minute - 10*time.Second)
	reloads, goroutines := 0, 0
	for step := 1; clk.Now().Before(end); step++ {
		quiet()
		if step == 30 {
			// Baseline after the first reloads, connections and caches warm.
			// Every scheduled instance has a loop goroutine: discovery adds
			// instances over time.
			goroutines = ownGoroutines() - int(instances.Load())
		}
		clk.Advance(10 * time.Second)
		if step%15 == 12 {
			// At 2m, 4m30, 7m...: the critical job is due every other time;
			// the reload is sent while its runs start.
			reloads++
			write(reloads%2 == 1)
			reload <- struct{}{}
			select {
			case err := <-reloaded:
				if err != nil {
					t.Fatalf("reload %d: %v", reloads, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("reload %d did not complete", reloads)
			}
		}
	}
	quiet()
	// The goroutines of the last runs (dials, request cancellation) may
	// still be exiting once the collector is idle: a leak is a count that
	// stays above the baseline.
	after := ownGoroutines() - int(instances.Load())
	for deadline := time.Now().Add(5 * time.Second); after > goroutines+2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		after = ownGoroutines() - int(instances.Load())
	}
	if after > goroutines+2 {
		t.Errorf("%d goroutines besides the instance loops after %d reloads, %d at the baseline:\n%s",
			after, reloads, goroutines, goroutineStacks())
	}
	_, metrics := httpGet(t, "http://"+addr+"/metrics")
	if strings.Contains(metrics, `copy="yes"`) {
		t.Error("the series of the removed job are still exposed")
	}
	if !strings.Contains(metrics, "example_network_device_up{") || !strings.Contains(metrics, "example_device_up{") {
		t.Errorf("metrics lost across reloads:\n%s", metrics)
	}

	res := stopRun()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 30))
	}
	// Every 2 minutes from 0 to 28: 15 runs, whatever the reloads.
	if n := strings.Count(res.stderr, `msg="run finished" component=run provider=example-sim account=fleet job=devices instance=devices `); n != 15 {
		t.Errorf("%d runs of the critical job, want 15", n)
	}
	if n := strings.Count(res.stderr, `msg="configuration reloaded"`); n != reloads || reloads != 12 {
		t.Errorf("%d reloads logged, %d sent", n, reloads)
	}
	if n := strings.Count(res.stderr, "retired_instances=1"); n != reloads/2 {
		t.Errorf("%d reloads retired the extra job, want %d", n, reloads/2)
	}
	for _, bad := range []string{"run failed", "runs canceled", "run skipped"} {
		if lines := grepLines(res.stderr, bad); lines != "" {
			t.Errorf("%s:\n%s", bad, lines)
		}
	}
	if res := stopSim(); res.code != exitOK {
		t.Errorf("sim exited with %d", res.code)
	}
}

// TestReloadRejectsInvalidConfig checks that a reload with an invalid
// configuration is rejected and the running configuration kept.
func TestReloadRejectsInvalidConfig(t *testing.T) {
	simAddr, stopSim := background(t, nil, "sim", "--provider", "example-sim")
	defer stopSim()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	valid := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: fleet }
spec:
  provider: example-sim
  params: { baseURL: "http://%s/api/v1" }
  secrets: { apiKey: { env: TAPYR_FLEET_API_KEY } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: sites }
spec: { account: fleet, template: example-sim-sites }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: reload }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	reload := make(chan struct{}, 1)
	reloaded := make(chan error, 1)
	addr, stopRun := backgroundHooks(t, map[string]string{"TAPYR_FLEET_API_KEY": "sim-api-key"}, &env{
		clock: clock.Real(), reload: reload, onReload: func(err error) { reloaded <- err },
	}, "run", "--config", path)

	for name, content := range map[string]string{
		"invalid YAML":    "kind: [",
		"unknown account": strings.Replace(valid, "account: fleet,", "account: nope,", 1),
		"missing secret":  strings.Replace(valid, "TAPYR_FLEET_API_KEY", "TAPYR_UNSET_KEY", 1),
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		reload <- struct{}{}
		select {
		case err := <-reloaded:
			if err == nil {
				t.Errorf("%s: reload accepted", name)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: reload did not complete", name)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, body := httpGet(t, "http://"+addr+"/metrics"); strings.Contains(body, "example_site_info") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the running configuration stopped collecting")
		}
		time.Sleep(20 * time.Millisecond)
	}
	res := stopRun()
	if res.code != exitOK || strings.Count(res.stderr, `msg="configuration reload rejected: the running configuration is kept"`) != 3 {
		t.Errorf("exit %d:\n%s", res.code, tail(res.stderr, 20))
	}
}

// ownGoroutines counts the goroutines of the process, except those of HTTP
// keep-alive connections: their number follows the connections the
// clients happen to keep, not a leak.
func ownGoroutines() int {
	count := 0
	for _, g := range strings.Split(goroutineStacks(), "\n\n") {
		if strings.Contains(g, "net/http.(*persistConn)") || strings.Contains(g, "net/http.(*conn).serve") ||
			strings.Contains(g, "net/http.(*connReader).backgroundRead") {
			continue
		}
		count++
	}
	return count
}

// goroutineStacks returns the stacks of every goroutine.
func goroutineStacks() string {
	buf := make([]byte, 1<<20)
	for {
		n := goruntime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}
