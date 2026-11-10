package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
)

// TestArubaCentralRotation runs the aruba-central-classic package against
// its simulator for 5 simulated hours, then restarts tapyr run for 2 more:
// access tokens are renewed with rotating refresh tokens (each valid once),
// the second process resumes from the pair stored, encrypted, in the state
// store, and no run fails.
func TestArubaCentralRotation(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "aruba-central-classic")
	defer stopSim()
	dbPath := filepath.Join(dir, "tapyr.db")
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: campus }
spec:
  provider: aruba-central-classic
  params: { baseURL: "http://%s" }
  secrets:
    clientId: { env: TAPYR_ARUBA_CLIENT_ID }
    clientSecret: { env: TAPYR_ARUBA_CLIENT_SECRET }
    refreshToken: { env: TAPYR_ARUBA_REFRESH_TOKEN }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: aps }
spec: { account: campus, template: aruba-aps, interval: 15m }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: aruba }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, dbPath)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"TAPYR_ARUBA_CLIENT_ID": "sim-client", "TAPYR_ARUBA_CLIENT_SECRET": "sim-client-secret",
		"TAPYR_ARUBA_REFRESH_TOKEN": "sim-refresh-token", "TAPYR_STATE_KEY": "e2e-state-key",
	}
	tokens := func() int64 {
		_, body := httpGet(t, "http://"+simAddr+"/_sim/status")
		var status struct {
			Stats struct {
				Tokens int64 `json:"tokens_issued"`
			} `json:"stats"`
		}
		if err := json.Unmarshal([]byte(body), &status); err != nil {
			t.Fatal(err)
		}
		return status.Stats.Tokens
	}
	var self, ready string
	runFor := func(d time.Duration) (string, result) {
		var inFlight, instances atomic.Int64
		addr, stop := backgroundHooks(t, vars, &env{
			clock:       clk,
			onRun:       func(d int) { inFlight.Add(int64(d)) },
			onInstances: func(n int) { instances.Store(int64(n)) },
		}, "run", "--config", filepath.Join(dir, "config.yaml"))
		until := clk.Now().Add(d)
		for {
			deadline := time.Now().Add(10 * time.Second)
			for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
				time.Sleep(100 * time.Microsecond)
			}
			if time.Now().After(deadline) {
				t.Fatalf("collector not idle: %d runs in flight, %d timers", inFlight.Load(), clk.Waiters())
			}
			if !clk.Now().Before(until) {
				break
			}
			clk.Advance(time.Minute)
		}
		_, body := httpGet(t, "http://"+addr+"/metrics")
		_, self = httpGet(t, "http://"+addr+"/metrics/self")
		_, ready = httpGet(t, "http://"+addr+"/readyz")
		return body, stop()
	}

	// 2-hour tokens refreshed 10 minutes before they expire: at 0, 1h50 and
	// 3h40.
	body, res := runFor(5 * time.Hour)
	if strings.Count(body, "aruba_ap_up{") != 24 {
		t.Errorf("metrics:\n%s", body)
	}
	if n := tokens(); n != 3 {
		t.Errorf("%d tokens issued in 5 hours, want 3", n)
	}
	if failed := grepLines(res.stderr, "run failed"); failed != "" {
		t.Errorf("runs failed:\n%s", failed)
	}
	// The restart resumes from the stored pair: the bootstrap refresh token
	// is spent and would be rejected.
	_, res = runFor(2 * time.Hour)
	if failed := grepLines(res.stderr, "run failed"); failed != "" || strings.Contains(res.stderr, "re-bootstrap") {
		t.Errorf("after the restart:\n%s", tail(res.stderr, 20))
	}
	if n := tokens(); n != 4 {
		t.Errorf("%d tokens issued after 7 hours, want 4", n)
	}
	// tapyr record, next to tapyr run, uses the stored pair without
	// refreshing it. Regression: it started from the spent bootstrap pair,
	// and a refresh would have spent the refresh token of tapyr run.
	rec := executeWith(t, vars, clk, "record", "--config", filepath.Join(dir, "config.yaml"), "--job", "aps", "--out", filepath.Join(t.TempDir(), "recording"))
	if rec.code != 0 {
		t.Fatalf("record: exit %d\n%s%s", rec.code, rec.stdout, rec.stderr)
	}
	if n := tokens(); n != 4 {
		t.Errorf("%d tokens issued after the recording, want 4", n)
	}
	// Once the stored access token expires (at 8h), it says so instead of
	// renewing it.
	clk.Advance(61 * time.Minute)
	rec = executeWith(t, vars, clk, "record", "--config", filepath.Join(dir, "config.yaml"), "--job", "aps", "--out", filepath.Join(t.TempDir(), "recording"))
	if rec.code == 0 || !strings.Contains(rec.stdout+rec.stderr, "retry while tapyr run holds a valid access token") || tokens() != 4 {
		t.Errorf("record with an expired token: exit %d, %d tokens issued\n%s%s", rec.code, tokens(), rec.stdout, rec.stderr)
	}
	// A changed bootstrap secret replaces the stored pair; a rejected
	// refresh token stops the account and raises the condition.
	vars["TAPYR_ARUBA_REFRESH_TOKEN"] = "revoked-refresh-token"
	_, res = runFor(10 * time.Minute)
	if !strings.Contains(self, `tapyr_auth_reauth_required{account="campus",provider="aruba-central-classic"} 1`) {
		t.Errorf("no re-bootstrap condition in the self-metrics:\n%s", grepLines(self, "tapyr_auth"))
	}
	if !strings.Contains(ready, `"ready":true`) || !strings.Contains(ready, "account campus (provider aruba-central-classic) needs a manual re-bootstrap") {
		t.Errorf("/readyz = %s", ready)
	}
	if !strings.Contains(res.stderr, "the refresh token was rejected") || strings.Contains(res.stderr, "revoked-refresh-token") {
		t.Errorf("logs:\n%s", tail(res.stderr, 10))
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("sim-refresh-")) || bytes.Contains(data, []byte("sim-access-")) {
		t.Error("the token pair is stored in clear")
	}
}
