package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/clock"
)

// TestRemovedOutputBuffer checks the end of a disk buffer: an output whose
// backend is down keeps its samples on disk; once a reload removes the
// output, its buffer is deleted and the log says how many samples were
// never delivered. Without a buffer, the reload that removes an output
// whose backend is down completes too: its flush is bounded (it used to
// wait for the backend forever).
func TestRemovedOutputBuffer(t *testing.T) {
	t.Run("disk", func(t *testing.T) { testRemovedOutput(t, true) })
	t.Run("memory", func(t *testing.T) { testRemovedOutput(t, false) })
}

func testRemovedOutput(t *testing.T, disk bool) {
	simAddr, stopSim := background(t, nil, "sim", "--provider", "example-sim")
	defer stopSim()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write := func(withSpool bool) {
		t.Helper()
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
spec: { account: fleet, template: example-sim-devices, interval: 1m }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: buffer }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, filepath.Join(dir, "tapyr.db"))
		if withSpool {
			// Nothing listens on port 1: every send fails.
			config += `---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: spool }
spec:
  type: remote_write
  remoteWrite: { url: "http://127.0.0.1:1/api/v1/write" }
  queue: { flushInterval: 1s, disk: {} }
`
			if !disk {
				config = strings.Replace(config, "disk: {} ", "", 1)
			}
		}
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(true)
	reload := make(chan struct{})
	reloaded := make(chan error, 1)
	addr, stop := backgroundHooks(t, map[string]string{"TAPYR_FLEET_API_KEY": "sim-api-key"},
		&env{clock: clock.Real(), reload: reload, onReload: func(err error) { reloaded <- err }}, "run", "--config", path)

	// The samples wait in the queue, and on disk with a buffer.
	queued := func() int {
		_, body := httpGet(t, "http://"+addr+"/api/v1/outputs")
		var outputs api.List[api.Output]
		if err := json.Unmarshal([]byte(body), &outputs); err != nil {
			t.Fatalf("outputs: %v", err)
		}
		for _, o := range outputs.Items {
			if o.Name == "spool" && o.Buffer != nil {
				return o.Buffer.Samples
			}
			if o.Name == "spool" && !disk {
				return o.Queue
			}
		}
		return 0
	}
	waitUntil(t, 20*time.Second, "samples queued for spool", func() bool { return queued() > 0 })
	spool := filepath.Join(dir, "buffer", "spool")
	if _, err := os.Stat(spool); disk && err != nil {
		t.Fatalf("buffer directory: %v", err)
	}

	// The output goes away: so does its buffer.
	write(false)
	reload <- struct{}{}
	select {
	case err := <-reloaded:
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the reload waits for the backend of the removed output")
	}
	if !disk {
		stop()
		return
	}
	waitUntil(t, 10*time.Second, "the buffer of spool deleted", func() bool {
		_, err := os.Stat(spool)
		return errors.Is(err, os.ErrNotExist)
	})
	res := stop()
	if !strings.Contains(res.stderr, `msg="disk buffer of a removed output deleted with samples never delivered"`) {
		t.Errorf("no warning in the log:\n%s", grepLines(res.stderr, "buffer"))
	}
}

// waitUntil polls cond until it holds or the timeout passes.
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
