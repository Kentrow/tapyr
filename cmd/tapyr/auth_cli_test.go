package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/state"
)

// tokenPackage is a provider package with OAuth2 client credentials, served
// by its simulator.
var tokenPackage = map[string]string{
	"provider.yaml": `apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: token-sim, version: 0.1.0 }
spec:
  baseURL: '{{ .params.baseURL | default "http://127.0.0.1:1/api" }}'
  params: [{ name: baseURL }]
  secrets: [{ name: clientSecret, required: true }]
  auth:
    type: oauth2_client_credentials
    oauth2ClientCredentials:
      tokenURL: '{{ .params.baseURL | default "http://127.0.0.1:1/api" }}/oauth/token'
      clientId: tapyr
      clientSecret: { secret: clientSecret }
  limits:
    - { name: daily, kind: quota, max: 5000, window: { period: day }, scope: { by: [account] } }
`,
	"templates/items.yaml": `apiVersion: tapyr/v1alpha1
kind: JobTemplate
metadata: { name: token-sim-items }
spec:
  provider: token-sim
  defaultInterval: 1m
  request: { path: /items }
  extract:
    items: response.body
    metrics: [{ name: token_sim_item_up, value: "1.0", labels: { id: item.id } }]
`,
	"sim/sim.yaml": `apiVersion: tapyr/v1alpha1
kind: Simulation
metadata: { name: token-sim }
spec:
  basePath: /api
  auth:
    accept: { clients: [{ clientId: tapyr, clientSecret: sim-secret }] }
    tokenLifetime: 5m
  routes: [{ path: /items, response: { file: items.json } }]
`,
	"sim/items.json": `[{"id": "a"}, {"id": "b"}]`,
}

// TestOAuth2AgainstSimulator runs tapyr run against the simulator of an
// OAuth2 package for 30 simulated minutes: tokens are renewed before they
// expire, token requests count in the quota, and the stored token is
// encrypted with TAPYR_STATE_KEY.
func TestOAuth2AgainstSimulator(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "providers", "token-sim")
	for name, content := range tokenPackage {
		path := filepath.Join(pkgDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "token-sim", "--provider-path", filepath.Join(dir, "providers"))

	dbPath := filepath.Join(dir, "tapyr.db")
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: acme }
spec:
  provider: token-sim
  params: { baseURL: "http://%s/api" }
  secrets: { clientSecret: { env: TAPYR_ACME_CLIENT_SECRET } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: items }
spec: { account: acme, template: token-sim-items }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: tokens }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  providerPaths: [%q]
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, dbPath, filepath.Join(dir, "providers"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"TAPYR_ACME_CLIENT_SECRET": "sim-secret", "TAPYR_STATE_KEY": "e2e-state-key"}
	var inFlight, instances atomic.Int64
	addr, stopRun := backgroundHooks(t, vars, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", filepath.Join(dir, "config.yaml"))
	for {
		deadline := time.Now().Add(10 * time.Second)
		for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector not idle: %d runs in flight, %d timers", inFlight.Load(), clk.Waiters())
		}
		if !clk.Now().Before(start.Add(30 * time.Minute)) {
			break
		}
		clk.Advance(10 * time.Second)
	}

	if _, body := httpGet(t, "http://"+addr+"/metrics"); strings.Count(body, "token_sim_item_up{") != 2 {
		t.Errorf("metrics:\n%s", body)
	}
	_, body := httpGet(t, "http://"+simAddr+"/_sim/status")
	var status struct {
		Stats struct {
			Refused map[string]int64 `json:"rate_limited"`
			Routes  map[string]int64 `json:"routes"`
			Tokens  int64            `json:"tokens_issued"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	// 5-minute tokens renewed 30s before expiry over 30 minutes: 7 tokens,
	// and no request rejected for an expired token.
	if st := status.Stats; st.Tokens != 7 || st.Routes["POST /api/oauth/token"] != 7 || st.Routes["GET /items"] != 31 {
		t.Errorf("simulator stats = %+v", st)
	}

	res := stopRun()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
	}
	if failed := grepLines(res.stderr, "run failed"); failed != "" {
		t.Errorf("runs failed:\n%s", failed)
	}
	if strings.Contains(res.stderr, "sim-secret") || strings.Contains(res.stderr, "sim-token-") {
		t.Error("a secret or a token appears in the logs")
	}
	if stopSim().code != exitOK {
		t.Error("sim did not stop cleanly")
	}

	// The stored token is encrypted.
	store, err := state.OpenSQLite(context.Background(), dbPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snaps, err := store.LoadSeries(context.Background())
	if err != nil || len(snaps) != 1 {
		t.Errorf("series snapshot: %v %v", snaps, err)
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("sim-token-")) {
		t.Error("the token is stored in clear")
	}
}
