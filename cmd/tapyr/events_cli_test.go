package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/clock"
)

// TestEventsAndDebug runs tapyr run against the simulator with the live
// debug endpoint enabled: the event stream reports runs and reloads, and
// the debug endpoint returns the last pages of an instance, redacted, with
// its samples.
func TestEventsAndDebug(t *testing.T) {
	simAddr, stopSim := background(t, nil, "sim", "--provider", "example-sim")
	defer stopSim()
	dir := t.TempDir()
	write := func(debug bool) {
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
spec: { account: fleet, template: example-sim-devices, interval: 2s, priority: critical }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: events }
spec:
  server: { listen: "127.0.0.1:0", debug: { enabled: %t, maxBodyBytes: 200 } }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, debug, filepath.Join(dir, "tapyr.db"))
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(true)
	reload := make(chan struct{})
	reloaded := make(chan error, 1)
	vars := map[string]string{"TAPYR_FLEET_API_KEY": "sim-api-key"}
	addr, stop := backgroundHooks(t, vars, &env{clock: clock.Real(), reload: reload, onReload: func(err error) { reloaded <- err }},
		"run", "--config", filepath.Join(dir, "config.yaml"))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/v1/events", http.NoBody)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed below, read by the goroutine
	if err != nil {
		t.Fatal(err)
	}
	stream := resp.Body
	defer stream.Close()
	events := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(stream)
		var event string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event != "":
				events <- event + " " + strings.TrimPrefix(line, "data: ")
				event = ""
			}
		}
		close(events)
	}()
	next := func(typ string) map[string]any {
		t.Helper()
		for {
			select {
			case e, ok := <-events:
				if !ok {
					t.Fatalf("the stream closed before a %s event", typ)
				}
				name, data, _ := strings.Cut(e, " ")
				if name != typ {
					continue
				}
				var fields map[string]any
				if err := json.Unmarshal([]byte(data), &fields); err != nil {
					t.Fatal(err)
				}
				return fields
			case <-ctx.Done():
				t.Fatalf("no %s event", typ)
			}
		}
	}
	run := next(api.EventRunFinished)
	if run["job"] != "devices" || run["result"] != "success" || run["samples"] == float64(0) || run["time"] == nil {
		t.Errorf("run_finished = %v", run)
	}

	// The capture of the last run: pages truncated to 200 bytes, samples.
	_, body := httpGet(t, "http://"+addr+"/api/v1/debug/jobs/devices/last")
	var capture api.Debug
	if err := json.Unmarshal([]byte(body), &capture); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if capture.Instance != "devices" || len(capture.Pages) == 0 || len(capture.Samples) == 0 || len(capture.Instances) != 1 {
		t.Fatalf("capture = %s", body)
	}
	if p := capture.Pages[0]; p.Status != 200 || p.Headers["Content-Type"] == "" || p.Headers["X-Ratelimit-Remaining-Day"] == "" ||
		len(p.Body) > 200 || !p.Truncated {
		t.Errorf("page = %+v", p)
	}
	if strings.Contains(body, "sim-api-key") {
		t.Error("the capture holds the API key")
	}
	if status, _ := httpGet(t, "http://"+addr+"/api/v1/debug/jobs/nope/last"); status != http.StatusNotFound {
		t.Errorf("unknown job: %d", status)
	}

	// A reload is an event; disabling the debug endpoint drops the captures.
	write(false)
	reload <- struct{}{}
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	if r := next(api.EventConfigReloaded); r["result"] != "success" {
		t.Errorf("config_reloaded = %v", r)
	}
	if status, body := httpGet(t, "http://"+addr+"/api/v1/debug/jobs/devices/last"); status != http.StatusNotFound ||
		!strings.Contains(body, "debug_disabled") {
		t.Errorf("disabled: %d %s", status, body)
	}
}
