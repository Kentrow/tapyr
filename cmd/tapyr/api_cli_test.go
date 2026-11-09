package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/clock"
)

// TestStatusAPI runs a discovery chain against the simulator for 12
// simulated minutes, then reads every endpoint of the status API through
// basic auth: every response validates against the OpenAPI description.
func TestStatusAPI(t *testing.T) {
	start := time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	simAddr, stopSim := backgroundWith(t, nil, clk, "sim", "--provider", "example-sim")
	defer stopSim()
	dir := t.TempDir()
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: fleet }
spec:
  provider: example-sim
  params: { baseURL: "http://%s/api/v1" }
  secrets: { apiKey: { env: TAPYR_FLEET_API_KEY } }
  labels: { tenant: acme }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: network-devices }
spec: { account: fleet, template: example-sim-network-devices, interval: 5m }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: devices }
spec: { account: fleet, template: example-sim-devices, interval: 2m, outputs: [prom] }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: api }
spec:
  server:
    listen: "127.0.0.1:0"
    basicAuth: { username: { env: TAPYR_UI_USER }, password: { env: TAPYR_UI_PASSWORD }, protectMetrics: true }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"TAPYR_FLEET_API_KEY": "sim-api-key", "TAPYR_UI_USER": "admin", "TAPYR_UI_PASSWORD": "ui-password"}
	var inFlight, instances atomic.Int64
	addr, stop := backgroundHooks(t, vars, &env{
		clock:       clk,
		onRun:       func(d int) { inFlight.Add(int64(d)) },
		onInstances: func(n int) { instances.Store(int64(n)) },
	}, "run", "--config", dir)
	for clk.Now().Before(start.Add(12 * time.Minute)) {
		deadline := time.Now().Add(10 * time.Second)
		for !collectorIdle(clk, &inFlight, &instances) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		clk.Advance(10 * time.Second)
	}

	v, err := api.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr
	call := func(path string, auth bool) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+path, http.NoBody)
		if auth {
			req.SetBasicAuth("admin", "ui-password")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	check := func(operation, path string, want int) []byte {
		t.Helper()
		status, body := call(path, true)
		if status != want {
			t.Errorf("%s: %d, want %d: %s", path, status, want, body)
			return nil
		}
		if err := v.Validate(operation, status, body); err != nil {
			t.Errorf("%s: %v\n%s", path, err, body)
		}
		return body
	}

	// Basic auth protects the API and the metrics, not the health checks.
	for path, want := range map[string]int{"/api/v1/status": 401, "/metrics": 401, "/metrics/self": 401, "/healthz": 200, "/readyz": 200} {
		if status, _ := call(path, false); status != want {
			t.Errorf("%s without credentials: %d, want %d", path, status, want)
		}
	}
	var status api.Status
	if err := json.Unmarshal(check("GET /api/v1/status", "/api/v1/status", 200), &status); err != nil ||
		!status.Ready || !status.LastReload.Success || len(status.ConfigHash) != 64 || status.Instances < 4 {
		t.Errorf("status = %+v (%v)", status, err)
	}
	check("GET /api/v1/providers", "/api/v1/providers", 200)
	check("GET /api/v1/accounts", "/api/v1/accounts", 200)
	check("GET /api/v1/accounts/{name}", "/api/v1/accounts/fleet", 200)
	check("GET /api/v1/accounts/{name}", "/api/v1/accounts/nope", 404)
	var jobs api.List[api.Job]
	if err := json.Unmarshal(check("GET /api/v1/jobs", "/api/v1/jobs", 200), &jobs); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, j := range jobs.Items {
		names[j.Name] = true
		if j.EffectiveIntervalSeconds <= 0 {
			t.Errorf("job %s: effective interval %v", j.Name, j.EffectiveIntervalSeconds)
		}
		check("GET /api/v1/jobs/{name}", "/api/v1/jobs/"+url.PathEscape(j.Name), 200)
		var insts api.List[api.Instance]
		if err := json.Unmarshal(check("GET /api/v1/jobs/{name}/instances", "/api/v1/jobs/"+url.PathEscape(j.Name)+"/instances", 200), &insts); err != nil {
			t.Fatal(err)
		}
		check("GET /api/v1/jobs/{name}/runs", "/api/v1/jobs/"+url.PathEscape(j.Name)+"/runs?limit=5", 200)
		if len(insts.Items) > 0 {
			check("GET /api/v1/jobs/{name}/runs", "/api/v1/jobs/"+url.PathEscape(j.Name)+"/runs?instance="+url.QueryEscape(insts.Items[0].Key), 200)
		}
	}
	// The discovery chain: an implicit parent per template, then the models.
	for _, want := range []string{"devices", "network-devices", "fleet.example-sim-sites", "fleet.example-sim-networks"} {
		if !names[want] {
			t.Errorf("job %s missing from %v", want, names)
		}
	}
	check("GET /api/v1/jobs/{name}", "/api/v1/jobs/nope", 404)
	var budgets api.List[api.Budget]
	if err := json.Unmarshal(check("GET /api/v1/budgets", "/api/v1/budgets", 200), &budgets); err != nil {
		t.Fatal(err)
	}
	timelines := 0
	for _, b := range budgets.Items {
		if b.Kind != "quota" {
			continue
		}
		body := check("GET /api/v1/budgets/{limit}/timeline",
			"/api/v1/budgets/"+url.PathEscape(b.Provider+"/"+b.Limit)+"/timeline?scope="+url.QueryEscape(b.Scope), 200)
		var tl api.Timeline
		if err := json.Unmarshal(body, &tl); err == nil && len(tl.Current.Buckets) > 0 {
			timelines++
		}
	}
	if timelines == 0 {
		t.Errorf("no timeline with consumption in %d budgets", len(budgets.Items))
	}
	check("GET /api/v1/plan", "/api/v1/plan", 200)
	var graph api.Graph
	if err := json.Unmarshal(check("GET /api/v1/graph", "/api/v1/graph", 200), &graph); err != nil || len(graph.Nodes) < 6 || len(graph.Edges) < 5 {
		t.Errorf("graph = %+v", graph)
	}
	check("GET /api/v1/outputs", "/api/v1/outputs", 200)
	check("GET /api/v1/debug/jobs/{name}/last", "/api/v1/debug/jobs/devices/last", 404) // debug disabled

	if res := stop(); res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
	}
}

// TestServerTLS serves the status API over HTTPS.
func TestServerTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := selfSigned(t, dir)
	config := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: tls }
spec:
  server: { listen: "127.0.0.1:0", tls: { certFile: %q, keyFile: %q } }
  state: { path: %q }
  log: { format: text }
`, certFile, keyFile, filepath.Join(dir, "tapyr.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	addr, stop := background(t, nil, "run", "--config", filepath.Join(dir, "config.yaml"))
	pool := x509.NewCertPool()
	certPEM, _ := os.ReadFile(certFile)
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+addr+"/api/v1/status", http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS == nil {
		t.Errorf("HTTPS status = %d", resp.StatusCode)
	}
	if status, _ := httpGet(t, "http://"+addr+"/healthz"); status != http.StatusBadRequest {
		t.Errorf("plain HTTP on the TLS listener = %d", status)
	}
	if res := stop(); res.code != exitOK {
		t.Fatalf("run exited with %d:\n%s", res.code, tail(res.stderr, 20))
	}
}

func selfSigned(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tapyr test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
