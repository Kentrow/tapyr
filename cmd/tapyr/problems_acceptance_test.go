package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/clock"
)

func stubURL(r *http.Request) string { return "http://" + r.Host }

// TestProblems checks that the errors of the runs come with a sentence, an
// action and a link to the guide (spec 09, "Messages"), from one stub API
// that answers by API key: a rejected key (the response echoes it), a
// server error, and a quota the provider reports as used up. The status
// API gives the problem of every run, of the account, and the live debug
// keeps the response that failed, redacted.
func TestProblems(t *testing.T) {
	const rejected = "rejected-key-value"
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("Authorization") {
		case "Bearer " + rejected:
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error":"invalid key %s"}`, rejected)
		case "Bearer flaky-key":
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"maintenance"}`)
		default:
			// The first page uses the last call of the day: the second is
			// refused by the budget engine before it is sent.
			w.Header().Set("X-RateLimit-Limit-Day", "5000")
			w.Header().Set("X-RateLimit-Remaining-Day", "0")
			w.Header().Set("Link", `<`+stubURL(r)+`/devices?page=2>; rel="next"`)
			fmt.Fprint(w, `[{"serial":"SIM-0001","siteId":"site-a","status":"online","clients":3}]`)
		}
	}))
	defer stub.Close()

	start := time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	dir := t.TempDir()
	var docs strings.Builder
	for _, account := range []string{"lab", "flaky", "spent"} {
		fmt.Fprintf(&docs, `apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: %[1]s }
spec:
  provider: example-sim
  params: { baseURL: %[2]q }
  secrets: { apiKey: { env: TAPYR_%[3]s_API_KEY } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: devices-%[1]s }
spec: { account: %[1]s, template: example-sim-devices, interval: 2m }
---
`, account, stub.URL, strings.ToUpper(account))
	}
	fmt.Fprintf(&docs, `apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: s }
spec:
  server: { listen: "127.0.0.1:0", debug: { enabled: true } }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(docs.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"TAPYR_LAB_API_KEY": rejected, "TAPYR_FLAKY_API_KEY": "flaky-key", "TAPYR_SPENT_API_KEY": "spent-key"}
	var inFlight, instances atomic.Int64
	addr, stop := backgroundHooks(t, vars, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", dir)
	for clk.Now().Before(start.Add(5 * time.Minute)) {
		deadline := time.Now().Add(10 * time.Second)
		// clockBound, not collectorIdle: a run retrying the server error
		// waits on the clock for its backoff.
		for !clockBound(clk, &inFlight, &instances) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		clk.Advance(10 * time.Second)
	}

	validator, err := api.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	// get reads a path of the status API, checks the response against the
	// OpenAPI description, and checks that the rejected key never shows.
	get := func(operation, path string, v any) string {
		t.Helper()
		status, body := httpGet(t, "http://"+addr+path)
		if err := validator.Validate(operation, status, []byte(body)); err != nil {
			t.Errorf("%s: %v\n%s", path, err, body)
		}
		if err := json.Unmarshal([]byte(body), v); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, body)
		}
		if strings.Contains(body, rejected) {
			t.Errorf("%s: the rejected key leaks:\n%s", path, body)
		}
		return body
	}
	const guide = "https://docs.tapyr.eu/dev/troubleshooting/#"

	// A rejected key: the sentence names the account and the reference
	// of its secret, never its value.
	var lab api.Job
	get("GET /api/v1/jobs/{name}", "/api/v1/jobs/devices-lab", &lab)
	if p := lab.LastRun.Problem; p == nil || p.Class != "auth" || p.Title != "The provider rejected the credentials of lab." ||
		p.Action != "Check the secret TAPYR_LAB_API_KEY (environment variable), then reload Tapyr." || p.DocsURL != guide+"authentication-fails-401-403" {
		t.Errorf("devices-lab: %+v", lab.LastRun)
	}
	var accounts api.List[api.Account]
	get("GET /api/v1/accounts", "/api/v1/accounts", &accounts)
	for _, a := range accounts.Items {
		want := map[string]string{"lab": "auth", "flaky": "", "spent": "quota_exhausted"}[a.Name]
		switch {
		case a.Name == "flaky" && (a.Problem == nil || a.Problem.Class != "server_error" && a.Problem.Class != "circuit_open"):
			t.Errorf("account flaky: %+v", a.Problem)
		case want != "" && (a.Problem == nil || a.Problem.Class != want):
			t.Errorf("account %s: problem %+v, want %s", a.Name, a.Problem, want)
		}
	}
	var debug api.Debug
	get("GET /api/v1/debug/jobs/{name}/last", "/api/v1/debug/jobs/devices-lab/last", &debug)
	if debug.Problem == nil || debug.Problem.Class != "auth" || len(debug.Pages) != 1 || debug.Pages[0].Status != 401 ||
		!strings.Contains(debug.Pages[0].Body, "invalid key") {
		t.Errorf("live debug of devices-lab: %+v", debug)
	}

	// A server error, retried, then the circuit opens: each run says so.
	var runs api.List[api.Run]
	get("GET /api/v1/jobs/{name}/runs", "/api/v1/jobs/devices-flaky/runs", &runs)
	classes := map[string]bool{}
	for _, r := range runs.Items {
		if r.Problem == nil {
			t.Errorf("devices-flaky: run without a problem: %+v", r)
			continue
		}
		classes[r.Problem.Class] = true
		if r.Problem.Class == "server_error" && r.Problem.Title != "The provider failed to answer (HTTP 503)." {
			t.Errorf("devices-flaky: %+v", r.Problem)
		}
	}
	if !classes["server_error"] {
		t.Errorf("devices-flaky: no server error among %v", classes)
	}

	// A quota the provider reports as used up: the run cannot get its
	// second page, until the reset.
	var spent api.Job
	get("GET /api/v1/jobs/{name}", "/api/v1/jobs/devices-spent", &spent)
	if r := spent.LastRun; r == nil || r.Result != "skipped_budget" || r.Problem == nil || r.Problem.Class != "quota_exhausted" ||
		r.Problem.Title != "The daily quota of spent is used up." || r.Problem.Until == nil ||
		!r.Problem.Until.Equal(time.Date(2026, time.November, 4, 0, 0, 0, 0, time.UTC)) || r.Problem.DocsURL != guide+"quota-problems" {
		t.Errorf("devices-spent: %+v %+v", spent.LastRun, spent.LastRun.Problem)
	}

	if res := stop(); res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
	}
}
