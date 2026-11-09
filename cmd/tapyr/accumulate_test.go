package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/clock"
)

// TestAccumulate checks counter reconstruction end to end: an API that
// returns every window twice is summed into a counter counting each window
// once, and the total survives a restart.
func TestAccumulate(t *testing.T) {
	var calls atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		// Calls 1 and 2 see window 1, calls 3 and 4 window 2...
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id": "a", "window": %d, "bytes": 10}]`, (n+1)/2)
	}))
	defer api.Close()
	dir := t.TempDir()
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: windows, version: 0.1.0 }
spec:
  baseURL: %q
  auth: { type: none }
---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: windows }
spec: { provider: windows }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: uplinks }
spec:
  account: windows
  interval: 1m
  request: { path: /uplinks }
  extract:
    items: response.body
    metrics:
      - { name: uplink_sent_bytes_total, type: counter, value: "double(item.bytes)", accumulate: true,
          windowField: item.window, labels: { id: item.id } }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: windows }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, api.URL, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC))
	counter := regexp.MustCompile(`uplink_sent_bytes_total\{id="a"\} (\S+)`)

	// collect runs tapyr run until n more API calls were made and returns
	// the counter exposed after each, as value@call.
	collect := func(n int) []string {
		t.Helper()
		// Before the start: the first run may call the API at once.
		seen := calls.Load()
		var inFlight, instances atomic.Int64
		addr, stop := backgroundHooks(t, nil, &env{
			clock:       clk,
			onRun:       func(d int) { inFlight.Add(int64(d)) },
			onInstances: func(n int) { instances.Store(int64(n)) },
		}, "run", "--config", dir)
		var got []string
		for len(got) < n {
			deadline := time.Now().Add(10 * time.Second)
			for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
				time.Sleep(100 * time.Microsecond)
			}
			if c := calls.Load(); c > seen {
				seen = c
				_, body := httpGet(t, "http://"+addr+"/metrics")
				m := counter.FindStringSubmatch(body)
				if m == nil {
					t.Fatalf("no counter after call %d:\n%s", c, body)
				}
				got = append(got, fmt.Sprintf("%s@%d", m[1], c))
				if len(got) == n {
					break
				}
			}
			clk.Advance(time.Minute)
		}
		if res := stop(); res.code != exitOK {
			t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
		}
		return got
	}
	if got := collect(4); !cmp.Equal(got, []string{"10@1", "10@2", "20@3", "20@4"}) {
		t.Errorf("counter = %v", got)
	}
	// After a restart, the total continues.
	if got := collect(2); !cmp.Equal(got, []string{"30@5", "30@6"}) {
		t.Errorf("counter after the restart = %v", got)
	}
}
