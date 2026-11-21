//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestDiskBufferOutage checks the promise of the disk buffer (ADR 0015)
// against VictoriaMetrics: the samples collected while the backend cannot
// be reached, and kept on disk across a restart of Tapyr, land in it with
// their own timestamps once it can.
func TestDiskBufferOutage(t *testing.T) {
	marker := runMarker + "-buffer"
	dir := t.TempDir()
	simAddr, backend, api := freePort(t), freePort(t), freePort(t)
	var simLogs bytes.Buffer
	start(t, &simLogs, nil, "sim", "--provider", "example-sim", "--listen", simAddr)
	path := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: it }
spec:
  provider: example-sim
  params: { baseURL: "http://%s/api/v1" }
  secrets: { apiKey: { env: TAPYR_IT_API_KEY } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: devices }
spec: { account: it, template: example-sim-devices, interval: 10s }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: vm }
spec:
  type: remote_write
  remoteWrite: { url: "http://%s/api/v1/write" }
  externalLabels: { tapyr_run: %q }
  queue: { flushInterval: 1s, disk: {} }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: it }
spec:
  server: { listen: %q }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, backend, marker, api, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := []string{"TAPYR_IT_API_KEY=sim-api-key"}

	// The backend cannot be reached: the samples wait on disk, then Tapyr
	// stops.
	var firstLogs bytes.Buffer
	first := start(t, &firstLogs, vars, "run", "--config", path)
	eventually(t, "samples in the disk buffer", 60*time.Second, func() bool { return buffered(api) >= 10 })
	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	restart := time.Now()
	if n := vmSamples(t, marker); n != 0 {
		t.Fatalf("%d samples in VictoriaMetrics before it could be reached", n)
	}

	// The backend comes back, through a proxy at the address of the output;
	// Tapyr starts again and sends what the first run left.
	target, err := url.Parse(vmURL)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", backend)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &http.Server{Handler: httputil.NewSingleHostReverseProxy(target), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = proxy.Serve(ln) }()
	t.Cleanup(func() { _ = proxy.Close() })
	var secondLogs bytes.Buffer
	start(t, &secondLogs, vars, "run", "--config", path)
	eventually(t, "the samples of the first run in VictoriaMetrics", 60*time.Second, func() bool {
		first, _ := vmOldest(t, marker)
		return !first.IsZero() && first.Before(restart)
	})
	if !bytes.Contains(secondLogs.Bytes(), []byte("sending the samples left by the previous run")) {
		t.Errorf("no replay in the log:\n%s", secondLogs.String())
	}
	eventually(t, "an empty buffer", 30*time.Second, func() bool { return buffered(api) == 0 })
}

// buffered returns the samples in the disk buffer of the output vm, from
// the status API of the tapyr at addr (-1 when it does not answer).
func buffered(addr string) int {
	resp, err := httpGet("http://" + addr + "/api/v1/outputs")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var body struct {
		Items []struct {
			Name   string `json:"name"`
			Buffer *struct {
				Samples int `json:"samples"`
			} `json:"buffer"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return -1
	}
	for _, o := range body.Items {
		if o.Name == "vm" && o.Buffer != nil {
			return o.Buffer.Samples
		}
	}
	return -1
}

// vmExport reads the raw samples of example_device_up for a run marker.
func vmExport(t *testing.T, marker string) []time.Time {
	t.Helper()
	q := url.Values{"match[]": {fmt.Sprintf(`example_device_up{tapyr_run=%q}`, marker)}}
	resp, err := httpGet(vmURL + "/api/v1/export?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []time.Time
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var line struct {
			Timestamps []int64 `json:"timestamps"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("export: %v", err)
		}
		for _, ms := range line.Timestamps {
			out = append(out, time.UnixMilli(ms))
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	return out
}

func vmSamples(t *testing.T, marker string) int {
	t.Helper()
	return len(vmExport(t, marker))
}

// vmOldest returns the oldest sample of a run marker and the count.
func vmOldest(t *testing.T, marker string) (time.Time, int) {
	t.Helper()
	samples := vmExport(t, marker)
	var oldest time.Time
	for _, ts := range samples {
		if oldest.IsZero() || ts.Before(oldest) {
			oldest = ts
		}
	}
	return oldest, len(samples)
}
