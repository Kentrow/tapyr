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

	"github.com/kentrow/tapyr/internal/clock"
)

// TestAuthCodeEndToEnd runs the example-oauth package as a user would (ADR 0016):
// tapyr auth login against its simulator writes the bootstrap refresh
// token, tapyr run collects for 24 simulated hours while the access token
// (6 hours) is renewed and the refresh token rotated, a restart continues
// from the stored pair although the secret file is spent, a new login
// replaces the pair, and a refresh token the API no longer knows sets the
// re-bootstrap condition.
func TestAuthCodeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("24 simulated hours")
	}
	start := time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "example-oauth")
	defer func() { stopSim() }()
	dir := t.TempDir()
	ls := &loginSetup{
		config: filepath.Join(dir, "config.yaml"), providers: t.TempDir(), secretFile: filepath.Join(dir, "lab-refresh-token"),
		vars: map[string]string{"TAPYR_LAB_CLIENT_ID": "sim-client", "TAPYR_LAB_CLIENT_SECRET": "sim-client-secret"},
	}
	writeConfig := func(sim string) {
		t.Helper()
		config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: home }
spec:
  provider: example-oauth
  params: { baseURL: "http://%[1]s/api/v1", oauthBaseURL: "http://%[1]s" }
  secrets:
    clientId: { env: TAPYR_LAB_CLIENT_ID }
    clientSecret: { env: TAPYR_LAB_CLIENT_SECRET }
    refreshToken: { file: %[2]q }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: usage }
spec: { account: home, template: example-oauth-usage }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: e2e }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %[3]q }
  collection: { jitter: 0%% }
  log: { format: text }
`, sim, ls.secretFile, filepath.Join(dir, "tapyr.db"))
		if err := os.WriteFile(ls.config, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(simAddr)
	simStats := func(addr string) (routes map[string]int64, issued int64) {
		t.Helper()
		_, body := httpGet(t, "http://"+addr+"/_sim/status")
		var status struct {
			Stats struct {
				Routes map[string]int64 `json:"routes"`
				Issued int64            `json:"tokens_issued"`
			} `json:"stats"`
		}
		if err := json.Unmarshal([]byte(body), &status); err != nil {
			t.Fatal(err)
		}
		return status.Stats.Routes, status.Stats.Issued
	}
	login := func(args ...string) {
		t.Helper()
		res := runLogin(t, ls, clk, pasteBrowser(t, func(r string) string { return r }), args...)
		if res.code != 0 {
			t.Fatalf("login: exit %d\n%s%s", res.code, res.stdout, res.stderr)
		}
	}
	var inFlight, instances atomic.Int64
	reload := make(chan struct{}, 1)
	reloaded := make(chan error, 1)
	advance := func(d time.Duration) {
		t.Helper()
		until := clk.Now().Add(d)
		for clk.Now().Before(until) {
			deadline := time.Now().Add(10 * time.Second)
			for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
				time.Sleep(100 * time.Microsecond)
			}
			clk.Advance(time.Minute)
		}
	}
	collect := func(d time.Duration) (string, func() result) {
		t.Helper()
		inFlight.Store(0)
		instances.Store(0)
		addr, stop := backgroundHooks(t, ls.vars, &env{
			clock:       clk,
			onRun:       func(d int) { inFlight.Add(int64(d)) },
			onInstances: func(n int) { instances.Store(int64(n)) },
			reload:      reload,
			onReload:    func(err error) { reloaded <- err },
		}, "run", "--config", ls.config)
		advance(d)
		return addr, stop
	}

	// 1. The consent, then 24 hours of collection.
	login()
	bootstrap, err := os.ReadFile(ls.secretFile)
	if err != nil {
		t.Fatal(err)
	}
	_, stop := collect(24 * time.Hour)
	res := stop()
	if res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 40))
	}
	routes, issued := simStats(simAddr)
	stats := routes["GET /users/{userId}/usage"]
	// 48 usage runs a day, about 5 access tokens (one every 5h50) besides
	// the one of the login.
	if stats < 45 || stats > 50 {
		t.Errorf("%d usage requests in 24 hours, want about 48", stats)
	}
	if issued < 5 || issued > 7 {
		t.Errorf("%d tokens issued in 24 hours (login included), want about 6", issued)
	}
	if lines := grepLines(res.stderr, "level=ERROR"); lines != "" {
		t.Errorf("errors during the day:\n%s", lines)
	}

	// 2. A restart: the secret file holds the spent bootstrap token, the
	// stored pair carries on.
	if after, _ := os.ReadFile(ls.secretFile); string(after) != string(bootstrap) {
		t.Fatal("the secret file changed")
	}
	_, stop = collect(7 * time.Hour)
	if res := stop(); grepLines(res.stderr, "level=ERROR") != "" || grepLines(res.stderr, "refresh token was refused") != "" {
		t.Errorf("after a restart:\n%s", tail(res.stderr, 30))
	}
	if routes2, issued2 := simStats(simAddr); routes2["GET /users/{userId}/usage"] < stats+13 || issued2 <= issued {
		t.Errorf("after a restart: %d usage requests (was %d), %d tokens (was %d)", routes2["GET /users/{userId}/usage"], stats, issued2, issued)
	}

	// 3. A new login replaces the stored pair.
	login("--force")
	_, stop = collect(time.Hour)
	if res := stop(); !strings.Contains(res.stderr, "the bootstrap refresh token changed") || grepLines(res.stderr, "level=ERROR") != "" {
		t.Errorf("after a new login:\n%s", tail(res.stderr, 30))
	}

	// 4. The API forgets the refresh token (a revoked access, here a new
	// simulator): the account needs a new consent, and says so.
	stopSim()
	simAddr, stopSim = backgroundWith(t, nil, clk, "sim", "--provider", "example-oauth")
	writeConfig(simAddr)
	addr, stop := collect(30 * time.Minute)
	_, metrics := httpGet(t, "http://"+addr+"/metrics/self")
	_, accounts := httpGet(t, "http://"+addr+"/api/v1/accounts")

	// 5. A new consent while tapyr run runs, then a reload: the condition
	// clears without a restart.
	login("--force")
	reload <- struct{}{}
	if err := <-reloaded; err != nil {
		t.Fatalf("reload: %v", err)
	}
	advance(30 * time.Minute)
	_, metricsAfter := httpGet(t, "http://"+addr+"/metrics/self")
	_, accountsAfter := httpGet(t, "http://"+addr+"/api/v1/accounts")
	res = stop()
	if !strings.Contains(metricsAfter, `tapyr_auth_reauth_required{account="home",provider="example-oauth"} 0`) {
		t.Errorf("the re-bootstrap condition stays after a new login and a reload:\n%s", grepLines(metricsAfter, "tapyr_auth"))
	}
	if !strings.Contains(accountsAfter, `"reauth_required": false`) {
		t.Errorf("the status API still asks for a new consent after a new login and a reload")
	}
	if !strings.Contains(metrics, `tapyr_auth_reauth_required{account="home",provider="example-oauth"} 1`) {
		t.Errorf("no re-bootstrap condition in the self-metrics:\n%s", grepLines(metrics, "tapyr_auth"))
	}
	if !strings.Contains(accounts, `"reauth_required": true`) {
		t.Errorf("no re-bootstrap condition in the status API: %s", accounts)
	}
	if !strings.Contains(res.stderr, "run tapyr auth login") {
		t.Errorf("the logs do not name the command:\n%s", tail(res.stderr, 30))
	}
	for _, leak := range []string{"sim-refresh-", "sim-token-"} {
		if strings.Contains(res.stderr+metrics+accounts, leak) {
			t.Errorf("a token appears in the logs, metrics or status API (%s)", leak)
		}
	}
}
