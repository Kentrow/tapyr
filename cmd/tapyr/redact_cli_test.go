package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunRedactsSecrets runs tapyr run against an API that echoes the
// credentials it receives in its error responses: the excerpt of the error
// is redacted in the logs (assertNoSecrets checks every line) and in what
// the status API serves (the runs, the last run of the job).
func TestRunRedactsSecrets(t *testing.T) {
	for _, tt := range []struct{ name, auth, scheme string }{
		{"bearer", `{ type: bearer, bearer: { token: { secret: apiKey } } }`, "Bearer"},
		{"header with a format", `{ type: header, header: { name: Authorization, value: { secret: apiKey }, format: "Token {value}" } }`, "Token"},
	} {
		t.Run(tt.name, func(t *testing.T) { testRunRedactsSecrets(t, tt.auth, tt.scheme) })
	}
}

func testRunRedactsSecrets(t *testing.T, auth, scheme string) {
	requests := make(chan struct{}, 16)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error": "invalid header %s"}`, r.Header.Get("Authorization"))
		select {
		case requests <- struct{}{}:
		default:
		}
	}))
	defer api.Close()
	dir := t.TempDir()
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: echo, version: 0.1.0 }
spec:
  baseURL: %q
  secrets: [{ name: apiKey, required: true }]
  auth: %s
---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: echo }
spec: { provider: echo, secrets: { apiKey: { env: TAPYR_ECHO_API_KEY } } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: items }
spec:
  account: echo
  interval: 1m
  request: { path: /items }
  extract:
    items: response.body
    metrics: [{ name: item_up, value: "1.0" }]
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: echo }
spec:
  server: { listen: "127.0.0.1:0" }
  state: { path: %q }
  log: { format: json }
`, api.URL, auth, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	addr, stop := background(t, map[string]string{"TAPYR_ECHO_API_KEY": "echo-api-key-1234"}, "run", "--config", dir)
	select {
	case <-requests:
	case <-time.After(10 * time.Second):
		t.Fatal("no request received")
	}
	// The failed run reaches the status API with its error redacted.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, runs := httpGet(t, "http://"+addr+"/api/v1/jobs/items/runs")
		if strings.Contains(runs, "invalid header") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no failed run in the status API: %s", runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, path := range []string{"/api/v1/jobs/items/runs", "/api/v1/jobs/items", "/api/v1/jobs/items/instances", "/api/v1/status"} {
		_, body := httpGet(t, "http://"+addr+path)
		if strings.Contains(body, "echo-api-key-1234") {
			t.Errorf("%s serves the secret: %s", path, body)
		}
		if path == "/api/v1/jobs/items/runs" && !strings.Contains(body, "invalid header "+scheme+" [REDACTED]") {
			t.Errorf("%s does not serve the redacted error: %s", path, body)
		}
		// The failed run still counts the request it sent.
		if path == "/api/v1/jobs/items/runs" && !strings.Contains(strings.ReplaceAll(body, " ", ""), `"requests":1`) {
			t.Errorf("%s does not count the request of the failed run: %s", path, body)
		}
	}
	// The shutdown waits for the run in flight, which logs its failure.
	res := stop()
	if !strings.Contains(res.stderr, "invalid header "+scheme+" [REDACTED]") {
		t.Errorf("the redacted error is not logged:\n%s", tail(res.stderr, 10))
	}
}
