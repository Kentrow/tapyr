package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProcessingAgainstSimulator checks the processing rules of an output
// end to end (ADR 0012): tapyr run against the example simulator exposes on
// /metrics only what the rules of the prometheus output keep, and counts
// what they filtered.
func TestProcessingAgainstSimulator(t *testing.T) {
	simAddr, stopSim := background(t, nil, "sim", "--provider", "example-sim")
	defer stopSim()
	dir := t.TempDir()
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: e2e }
spec:
  provider: example-sim
  params: { baseURL: "http://%s/api/v1" }
  secrets: { apiKey: { env: TAPYR_E2E_API_KEY } }
  labels: { tenant: e2e }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: e2e-devices }
spec: { account: e2e, template: example-sim-devices, labels: { env: test } }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec:
  type: prometheus
  processing:
    - { action: drop, sourceLabels: [__name__], regex: "example_device_info|.*_total" }
    - { action: drop, sourceLabels: [__name__, site_id], regex: "example_device_up;site-b" }
    - { action: labeldrop, regex: "env|tenant" }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: e2e }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
`, simAddr, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	addr, stopRun := background(t, map[string]string{"TAPYR_E2E_API_KEY": "sim-api-key"}, "run", "--config", dir, "--log-format", "text")
	defer stopRun()

	var body string
	deadline := time.Now().Add(10 * time.Second)
	for {
		var code int
		code, body = httpGet(t, "http://"+addr+"/metrics")
		if code == http.StatusOK && strings.Contains(body, "example_devices") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics not exposed in time (status %d):\n%s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, gone := range []string{"example_device_info", "_total", `env="test"`, `tenant="e2e"`, `site_id="site-b"`} {
		if strings.Contains(body, gone) {
			t.Errorf("/metrics still holds %s:\n%s", gone, body)
		}
	}
	for _, kept := range []string{`example_device_up{serial="SIM-0001",site_id="site-a"} 1`, `example_device_clients{serial="SIM-0001"} 12`, "example_devices 5"} {
		if !strings.Contains(body, kept) {
			t.Errorf("/metrics lacks %s:\n%s", kept, body)
		}
	}
	// 4 info, 4 counters and the 2 statuses of site-b.
	_, self := httpGet(t, "http://"+addr+"/metrics/self")
	if !strings.Contains(self, `tapyr_output_samples_filtered_total{output="prom"} 10`) {
		t.Errorf("filtered samples:\n%s", grepLines(self, "tapyr_output_samples_"))
	}
}
