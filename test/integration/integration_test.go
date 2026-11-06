//go:build integration

// Package integration checks the outputs against real backends: the samples
// of tapyr run land in VictoriaMetrics (remote write), in an OpenTelemetry
// Collector (OTLP over HTTP and gRPC) and in Prometheus (a scrape of
// /metrics and /metrics/self, remote write and OTLP), and series that go
// away are marked stale in all of them. A Prometheus without out-of-order
// window refuses the samples with old source timestamps, and gets the
// others. Run it with make integration, which starts the backends of
// compose.yaml.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

var (
	vmURL    = env("TAPYR_IT_VM_URL", "http://127.0.0.1:8428")
	otlpHTTP = env("TAPYR_IT_OTLP_HTTP", "http://127.0.0.1:4318")
	otlpGRPC = env("TAPYR_IT_OTLP_GRPC", "http://127.0.0.1:4317")
	otelOut  = env("TAPYR_IT_OTEL_OUT", "out/metrics.json")
	promURL  = env("TAPYR_IT_PROM_URL", "http://127.0.0.1:9090")
	// strictURL is the Prometheus without out-of-order window.
	strictURL = env("TAPYR_IT_PROM_STRICT_URL", "http://127.0.0.1:9091")
	// listen is where the tapyr under test serves /metrics: on every
	// interface, at the port Prometheus scrapes (prometheus.yml).
	listen    = env("TAPYR_IT_LISTEN", "0.0.0.0:19790")
	binary    string
	runMarker = fmt.Sprintf("it-%d", time.Now().UnixNano()) // isolates the samples of this run
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tapyr-it-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "tapyr")
	build := exec.CommandContext(context.Background(), "go", "build", "-o", binary, "../../cmd/tapyr")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building tapyr:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// start runs the tapyr binary until the test ends.
func start(t *testing.T, logs *bytes.Buffer, env []string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), binary, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("tapyr %s:\n%s", args[0], logs.String())
		}
	})
	return cmd
}

func config(simAddr, state string, withSites bool) string {
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
  remoteWrite: { url: %q }
  queue: { flushInterval: 1s }
  externalLabels: { tapyr_run: %s }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: otel-http }
spec:
  type: otlp
  otlp: { endpoint: %q, resourceAttributes: { tapyr.run: %s, tapyr.protocol: http } }
  queue: { flushInterval: 1s }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: otel-grpc }
spec:
  type: otlp
  otlp: { endpoint: %q, protocol: grpc, resourceAttributes: { tapyr.run: %s, tapyr.protocol: grpc } }
  queue: { flushInterval: 1s }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prometheus }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom-rw }
spec:
  type: remote_write
  remoteWrite: { url: %q }
  queue: { flushInterval: 1s }
  externalLabels: { tapyr_run: %s }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom-otlp }
spec:
  type: otlp
  otlp: { endpoint: %q, resourceAttributes: { tapyr.run: %s, tapyr.protocol: prom-otlp } }
  queue: { flushInterval: 1s }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: strict-rw }
spec:
  type: remote_write
  remoteWrite: { url: %q }
  queue: { flushInterval: 1s }
  externalLabels: { tapyr_run: %s }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: strict-otlp }
spec:
  type: otlp
  otlp: { endpoint: %q, resourceAttributes: { tapyr.run: %s, tapyr.protocol: strict-otlp } }
  queue: { flushInterval: 1s }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: it }
spec:
  server: { listen: %q }
  state: { path: %q }
  collection: { jitter: 0%% }
  log: { format: text }
`, simAddr, vmURL+"/api/v1/write", runMarker, otlpHTTP, runMarker, otlpGRPC, runMarker,
		promURL+"/api/v1/write", runMarker, promURL+"/api/v1/otlp/v1/metrics", runMarker,
		strictURL+"/api/v1/write", runMarker, strictURL+"/api/v1/otlp/v1/metrics", runMarker, listen, state)
	if withSites {
		cfg += `---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: sites }
spec: { account: it, template: example-sim-sites, interval: 10s }
`
	}
	return cfg
}

// vmSeries returns the number of series of a PromQL instant query to
// VictoriaMetrics.
func vmSeries(t *testing.T, query string) int {
	t.Helper()
	return series(t, vmURL, query)
}

// promSeries returns the number of series of a PromQL instant query to
// Prometheus.
func promSeries(t *testing.T, query string) int {
	t.Helper()
	return series(t, promURL, query)
}

func series(t *testing.T, base, query string) int {
	t.Helper()
	resp, err := httpGet(base + "/api/v1/query?query=" + url.QueryEscape(query))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return len(body.Data.Result)
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Second)
	}
}

// otelPoints reads the points of this run written by the collector, as
// "protocol metric stale" keys.
func otelPoints(t *testing.T) map[string]int {
	t.Helper()
	f, err := os.Open(otelOut)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var u pmetric.JSONUnmarshaler
	for sc.Scan() {
		md, err := u.UnmarshalMetrics(sc.Bytes())
		if err != nil {
			t.Fatalf("decoding %s: %v", otelOut, err)
		}
		for i := range md.ResourceMetrics().Len() {
			rm := md.ResourceMetrics().At(i)
			attrs := rm.Resource().Attributes()
			if v, _ := attrs.Get("tapyr.run"); v.AsString() != runMarker {
				continue
			}
			if v, _ := attrs.Get("service.name"); v.AsString() != "tapyr" {
				t.Errorf("service.name = %q", v.AsString())
			}
			protocol, _ := attrs.Get("tapyr.protocol")
			for j := range rm.ScopeMetrics().Len() {
				metrics := rm.ScopeMetrics().At(j).Metrics()
				for k := range metrics.Len() {
					m := metrics.At(k)
					var dps pmetric.NumberDataPointSlice
					if m.Type() == pmetric.MetricTypeSum {
						dps = m.Sum().DataPoints()
					} else {
						dps = m.Gauge().DataPoints()
					}
					for l := range dps.Len() {
						key := protocol.AsString() + " " + m.Name()
						if dps.At(l).Flags().NoRecordedValue() {
							key += " stale"
						}
						out[key]++
					}
				}
			}
		}
	}
	return out
}

func TestPushOutputs(t *testing.T) {
	dir := t.TempDir()
	simAddr := freePort(t)
	var simLogs, runLogs bytes.Buffer
	start(t, &simLogs, nil, "sim", "--provider", "example-sim", "--listen", simAddr)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(config(simAddr, filepath.Join(dir, "tapyr.db"), true)), 0o600); err != nil {
		t.Fatal(err)
	}
	run := start(t, &runLogs, []string{"TAPYR_IT_API_KEY=sim-api-key"}, "run", "--config", path)

	// The samples land in VictoriaMetrics and in the collector, by both
	// protocols.
	devices := fmt.Sprintf(`example_device_up{tapyr_run=%q}`, runMarker)
	sites := fmt.Sprintf(`example_site_info{tapyr_run=%q}`, runMarker)
	eventually(t, "the devices and sites in VictoriaMetrics", 60*time.Second, func() bool {
		return vmSeries(t, devices) > 0 && vmSeries(t, sites) > 0
	})
	eventually(t, "the devices and sites in the collector", 60*time.Second, func() bool {
		p := otelPoints(t)
		return p["http example_device_up"] > 0 && p["grpc example_device_up"] > 0 && p["http example_site_info"] > 0
	})
	// Prometheus gets them three ways: its scrape of /metrics, remote write
	// (the external label) and OTLP (the promoted resource attributes).
	scraped := `example_%s{job="tapyr-metrics"}`
	pushed := `example_%s{tapyr_run="` + runMarker + `",tapyr_protocol=""}`
	otlp := `example_%s{tapyr_run="` + runMarker + `",tapyr_protocol="prom-otlp"}`
	eventually(t, "the devices and sites in Prometheus", 60*time.Second, func() bool {
		for _, q := range []string{scraped, pushed, otlp} {
			if promSeries(t, fmt.Sprintf(q, "device_up")) == 0 || promSeries(t, fmt.Sprintf(q, "site_info")) == 0 {
				return false
			}
		}
		return true
	})
	// The Prometheus without out-of-order window refuses the counter with
	// the source timestamps of the fixtures, days old, and gets every
	// sample stamped with the collection time, sent in the same batches.
	strictPushed := `example_%s{tapyr_run="` + runMarker + `",tapyr_protocol=""}`
	strictOTLP := `example_%s{tapyr_run="` + runMarker + `",tapyr_protocol="strict-otlp"}`
	eventually(t, "the devices and sites in the Prometheus without out-of-order window", 60*time.Second, func() bool {
		for _, q := range []string{strictPushed, strictOTLP} {
			for _, m := range []string{"device_up", "device_clients", "device_info", "devices", "site_info"} {
				if series(t, strictURL, fmt.Sprintf(q, m)) == 0 {
					return false
				}
			}
		}
		return true
	})
	for _, q := range []string{strictPushed, strictOTLP} {
		if q := fmt.Sprintf(q, "device_received_bytes_total"); series(t, strictURL, q) > 0 {
			t.Errorf("%s: samples with old source timestamps accepted without out-of-order window", q)
		}
	}

	// The scrape of /metrics/self keeps the job label of the Tapyr job.
	eventually(t, "the self-metrics in Prometheus", 30*time.Second, func() bool {
		return promSeries(t, `tapyr_job_runs_total{job="devices"}`) > 0
	})
	if n := promSeries(t, `{__name__=~"tapyr_.+", exported_job!=""}`); n > 0 {
		t.Errorf("%d self-metric series renamed the job label to exported_job", n)
	}

	// Without the sites job, its series are marked stale: they leave the
	// instant queries of VictoriaMetrics, and the collector receives points
	// without a recorded value.
	if err := os.WriteFile(path, []byte(config(simAddr, filepath.Join(dir, "tapyr.db"), false)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the sites marked stale in VictoriaMetrics", 60*time.Second, func() bool { return vmSeries(t, sites) == 0 })
	eventually(t, "the stale points in the collector", 60*time.Second, func() bool {
		p := otelPoints(t)
		return p["http example_site_info stale"] > 0 && p["grpc example_site_info stale"] > 0
	})
	if vmSeries(t, devices) == 0 {
		t.Error("the devices left VictoriaMetrics")
	}
	eventually(t, "the sites marked stale in Prometheus", 60*time.Second, func() bool {
		for _, q := range []string{scraped, pushed, otlp} {
			if promSeries(t, fmt.Sprintf(q, "site_info")) > 0 {
				return false
			}
		}
		return true
	})
	for _, q := range []string{scraped, pushed, otlp} {
		if promSeries(t, fmt.Sprintf(q, "device_up")) == 0 {
			t.Errorf("%s left Prometheus", fmt.Sprintf(q, "device_up"))
		}
	}

	// The self-metrics count what the outputs sent and dropped.
	_, port, _ := net.SplitHostPort(listen)
	resp, err := httpGet("http://127.0.0.1:" + port + "/metrics/self")
	if err != nil {
		t.Fatal(err)
	}
	self, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, output := range []string{"vm", "otel-http", "otel-grpc", "prom-rw", "prom-otlp", "strict-rw", "strict-otlp"} {
		if !strings.Contains(string(self), fmt.Sprintf(`tapyr_output_samples_sent_total{output=%q}`, output)) {
			t.Errorf("no samples sent by %s", output)
		}
	}
	// Only the samples the Prometheus without out-of-order window refused
	// are dropped, as out_of_order.
	for _, line := range strings.Split(grep(string(self), "tapyr_output_samples_dropped_total{"), "\n") {
		strict := strings.Contains(line, `output="strict-`) && strings.Contains(line, `reason="out_of_order"`)
		if line != "" && !strict {
			t.Errorf("samples dropped: %s", line)
		}
	}
	for _, output := range []string{"strict-rw", "strict-otlp"} {
		if !strings.Contains(string(self), fmt.Sprintf(`tapyr_output_samples_dropped_total{output=%q,reason="out_of_order"}`, output)) {
			t.Errorf("no out_of_order samples counted for %s", output)
		}
	}
}

func httpGet(u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func grep(s, substr string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
