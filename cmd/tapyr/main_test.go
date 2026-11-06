package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
)

var (
	examples = filepath.Join("..", "..", "examples", "config")
	invalid  = filepath.Join("testdata", "invalid.yaml")
)

type result struct {
	code   int
	stdout string
	stderr string
}

// execute runs the CLI with an isolated environment.
func execute(t *testing.T, vars map[string]string, args ...string) result {
	t.Helper()
	return executeWith(t, vars, clock.NewFake(time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC)), args...)
}

// executeWith runs the CLI with an isolated environment and a clock.
func executeWith(t *testing.T, vars map[string]string, clk clock.Clock, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	lookupEnv := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	code := run(context.Background(), args, &env{stdout: &stdout, stderr: &stderr, lookupEnv: lookupEnv, clock: clk, listen: (&net.ListenConfig{}).Listen})
	res := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
	assertNoSecrets(t, vars, args, res)
	return res
}

// secretVar matches the names of the environment variables holding
// secrets in tests.
var secretVar = regexp.MustCompile(`KEY|SECRET|PASSWORD|TOKEN`)

// assertNoSecrets fails the test when the output of a command contains the
// value of a secret of its environment, or, for tapyr run, a token issued
// by the simulator (see "Redaction" in 07-self-observability.md).
func assertNoSecrets(t *testing.T, vars map[string]string, args []string, res result) {
	t.Helper()
	out := res.stdout + res.stderr
	for name, value := range vars {
		if secretVar.MatchString(name) && value != "" && strings.Contains(out, value) {
			t.Errorf("the value of %s appears in the output of tapyr %s", name, strings.Join(args, " "))
		}
	}
	if len(args) > 0 && args[0] == "run" && strings.Contains(out, "sim-token-") {
		t.Errorf("a token appears in the output of tapyr %s", strings.Join(args, " "))
	}
}

func TestCLI(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		args       []string
		wantCode   int
		wantStdout []string
		wantStderr []string
	}{
		{
			name:       "version",
			args:       []string{"version"},
			wantCode:   exitOK,
			wantStdout: []string{"tapyr ", "revision:", "go:", "platform:"},
		},
		{
			name:       "help lists every command",
			args:       []string{"--help"},
			wantCode:   exitOK,
			wantStdout: []string{"run", "plan", "sim", "record", "provider", "version", "--config"},
		},
		{
			name:       "unknown command",
			args:       []string{"frobnicate"},
			wantCode:   exitFailure,
			wantStderr: []string{`unknown command "frobnicate"`, "tapyr --help"},
		},
		{
			name:       "unknown flag",
			args:       []string{"version", "--frobnicate"},
			wantCode:   exitFailure,
			wantStderr: []string{"unknown flag: --frobnicate"},
		},
		{
			name:       "version takes no arguments",
			args:       []string{"version", "extra"},
			wantCode:   exitFailure,
			wantStderr: []string{"unknown command"},
		},
		{
			name:       "run resolves secrets before starting",
			args:       []string{"run", "--config", examples},
			wantCode:   exitInvalidConfig,
			wantStderr: []string{"spec.secrets.apiKey: environment variable TAPYR_EXAMPLE_API_KEY is not set"},
		},

		{
			name:       "run rejects an invalid configuration",
			args:       []string{"run", "--config", invalid},
			wantCode:   exitInvalidConfig,
			wantStderr: []string{"testdata/invalid.yaml:8:3: spec.intervall: unknown field \"intervall\""},
		},
		{
			name:       "validate examples",
			args:       []string{"validate", "--config", examples},
			wantCode:   exitOK,
			wantStdout: []string{"configuration is valid: ", " job templates, 1 account, 3 jobs, 1 output"},
		},
		{
			name:       "validate reads TAPYR_CONFIG as a path list",
			env:        map[string]string{"TAPYR_CONFIG": examples + string(filepath.ListSeparator) + filepath.Join(examples, "config.yaml")},
			args:       []string{"validate"},
			wantCode:   exitOK,
			wantStdout: []string{"configuration is valid"},
		},
		{
			name:     "--config wins over TAPYR_CONFIG",
			env:      map[string]string{"TAPYR_CONFIG": invalid},
			args:     []string{"validate", "--config", examples},
			wantCode: exitOK,
		},
		{
			name:     "validate invalid configuration",
			args:     []string{"validate", "--config", examples, "--config", invalid},
			wantCode: exitInvalidConfig,
			wantStderr: []string{
				"testdata/invalid.yaml:8:3: spec.intervall: unknown field \"intervall\"",
				"configuration is invalid: 1 error",
			},
		},
		{
			name:       "validate reports CEL errors with positions",
			args:       []string{"validate", "--config", examples, "--config", filepath.Join("testdata", "bad-cel.yaml")},
			wantCode:   exitInvalidConfig,
			wantStderr: []string{"testdata/bad-cel.yaml:13:16: spec.extract.metrics[0].value: invalid expression: undeclared reference to 'itme'"},
		},

		{
			name:       "validate missing path",
			args:       []string{"validate", "--config", "testdata/nope"},
			wantCode:   exitInvalidConfig,
			wantStderr: []string{"testdata/nope: no such file or directory"},
		},
		{
			name:       "validate without configuration",
			args:       []string{"validate"},
			wantCode:   exitFailure,
			wantStderr: []string{"no configuration path given"},
		},
		{
			name:       "check secrets reports unset variables",
			args:       []string{"validate", "--config", examples, "--check-secrets"},
			wantCode:   exitInvalidConfig,
			wantStderr: []string{"spec.secrets.apiKey: environment variable TAPYR_EXAMPLE_API_KEY is not set"},
		},
		{
			name:       "check secrets passes when set",
			env:        map[string]string{"TAPYR_EXAMPLE_API_KEY": "sim-api-key"},
			args:       []string{"validate", "--config", examples, "--check-secrets"},
			wantCode:   exitOK,
			wantStdout: []string{"configuration is valid"},
		},
		{
			name:       "provider list",
			args:       []string{"provider", "list"},
			wantCode:   exitOK,
			wantStdout: []string{"NAME", "example-sim", "0.1.0", "the built-in package"},
		},
		{
			name:       "provider list with an unknown output",
			args:       []string{"provider", "list", "--output", "yaml"},
			wantCode:   exitFailure,
			wantStderr: []string{`invalid --output "yaml" (want table or json)`},
		},
		{
			name:       "schema prints the combined schema",
			args:       []string{"schema"},
			wantCode:   exitOK,
			wantStdout: []string{`"$id": "https://raw.githubusercontent.com/kentrow/tapyr/main/schemas/v1alpha1/tapyr.json"`},
		},
		{
			name:       "schema of one kind, case-insensitive",
			args:       []string{"schema", "--kind", "provider"},
			wantCode:   exitOK,
			wantStdout: []string{`"title": "Provider"`},
		},
		{
			name:       "schema of an unknown kind",
			args:       []string{"schema", "--kind", "Exporter"},
			wantCode:   exitFailure,
			wantStderr: []string{`unknown kind "Exporter"`},
		},
		{
			name:       "record needs jobs and an output",
			args:       []string{"record", "--job", "devices"},
			wantCode:   exitFailure,
			wantStderr: []string{"--job and --out are required"},
		},
		{
			name:       "record help",
			args:       []string{"record", "--help"},
			wantCode:   exitOK,
			wantStdout: []string{"anonymized"},
		},
		{
			name:       "provider lint of a missing directory",
			args:       []string{"provider", "lint", "./providers/example"},
			wantCode:   exitInvalidConfig,
			wantStderr: []string{"provider path ./providers/example"},
		},
		{
			name:       "plan examples",
			args:       []string{"plan", "--config", examples},
			wantCode:   exitOK,
			wantStdout: []string{"example-sim/daily", "account=demo", "job demo-devices", "desired 2m", "effective 2m", "720 runs x 3 pages"},
		},
		{
			name:       "plan json",
			args:       []string{"plan", "--config", examples, "--output", "json"},
			wantCode:   exitOK,
			wantStdout: []string{`"feasible": true`, `"name": "demo-sites"`, `"effective_seconds": 300`},
		},
		{
			name:       "plan infeasible",
			args:       []string{"plan", "--config", examples, "--config", filepath.Join("testdata", "infeasible.yaml")},
			wantCode:   exitInfeasible,
			wantStdout: []string{"INFEASIBLE", "job too-often", "The plan is infeasible"},
		},
		{
			name:       "plan of a package that is not built in",
			args:       []string{"plan", "--config", filepath.Join("testdata", "plan-only.yaml"), "--provider-path", filepath.Join("testdata", "providers")},
			wantCode:   exitOK,
			wantStdout: []string{"plan-only/daily", "job things", "144 runs x 2 pages"},
		},
		{
			name:       "validate of a package that is not built in",
			args:       []string{"validate", "--config", filepath.Join("testdata", "plan-only.yaml"), "--provider-path", filepath.Join("testdata", "providers")},
			wantCode:   exitOK,
			wantStdout: []string{"configuration is valid"},
		},
		{
			name:       "validate without its provider path",
			args:       []string{"validate", "--config", filepath.Join("testdata", "plan-only.yaml")},
			wantCode:   exitInvalidConfig,
			wantStderr: []string{`unknown provider "plan-only"`},
		},
		{
			name:       "validate with a missing provider path",
			args:       []string{"validate", "--config", filepath.Join("testdata", "plan-only.yaml"), "--provider-path", filepath.Join("testdata", "nope")},
			wantCode:   exitInvalidConfig,
			wantStderr: []string{"testdata/nope"},
		},
		{
			name:       "plan rejects invalid fan-out hints",
			args:       []string{"plan", "--config", examples, "--fanout", "sites"},
			wantCode:   exitFailure,
			wantStderr: []string{`invalid --fanout "sites"`},
		},
		{
			name:       "plan warns about unused fan-out hints",
			args:       []string{"plan", "--config", examples, "--fanout", "example-sim-devices=3"},
			wantCode:   exitOK,
			wantStderr: []string{"warning: --fanout example-sim-devices matches no fan-out job"},
		},
		{
			name:       "sim of an unknown provider",
			args:       []string{"sim", "--provider", "nope"},
			wantCode:   exitFailure,
			wantStderr: []string{`unknown provider "nope" (providers with a simulator: aruba-central-classic, example-sim,`},
		},
		{
			name:       "sim requires a provider",
			args:       []string{"sim"},
			wantCode:   exitFailure,
			wantStderr: []string{"give either --provider or --all"},
		},
		{
			name:       "sim --all with an unknown scenario",
			args:       []string{"sim", "--all", "--scenario", "nope"},
			wantCode:   exitFailure,
			wantStderr: []string{`no simulator has a "nope" scenario`},
		},
		{
			name:       "sim --all with an invalid base",
			args:       []string{"sim", "--all", "--listen-base", "8081"},
			wantCode:   exitFailure,
			wantStderr: []string{`invalid --listen-base "8081"`},
		},
		{
			name:       "invalid log level flag",
			args:       []string{"version", "--log-level", "loud"},
			wantCode:   exitFailure,
			wantStderr: []string{`invalid log level "loud"`},
		},
		{
			name:       "invalid log format from environment",
			env:        map[string]string{"TAPYR_LOG_FORMAT": "xml"},
			args:       []string{"version"},
			wantCode:   exitFailure,
			wantStderr: []string{`invalid log format "xml"`},
		},
		{
			name:       "flag wins over environment",
			env:        map[string]string{"TAPYR_LOG_FORMAT": "xml"},
			args:       []string{"version", "--log-format", "text"},
			wantCode:   exitOK,
			wantStdout: []string{"tapyr "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := execute(t, tt.env, tt.args...)
			if got.code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", got.code, tt.wantCode, got.stdout, got.stderr)
			}
			for _, s := range tt.wantStdout {
				if !strings.Contains(got.stdout, s) {
					t.Errorf("stdout does not contain %q:\n%s", s, got.stdout)
				}
			}
			for _, s := range tt.wantStderr {
				if !strings.Contains(got.stderr, s) {
					t.Errorf("stderr does not contain %q:\n%s", s, got.stderr)
				}
			}
		})
	}
}

func TestSchemaOut(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "schemas")
	got := execute(t, nil, "schema", "--out", dir)
	if got.code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 9 {
		t.Fatalf("wrote %d files, want 9 (one per kind plus tapyr.json)", len(entries))
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(b) {
			t.Errorf("%s is not valid JSON", e.Name())
		}
	}
}

func TestCheckSecretsNeverPrintsValues(t *testing.T) {
	const secret = "do-not-print-this-value"
	got := execute(t, map[string]string{"TAPYR_EXAMPLE_API_KEY": secret}, "validate", "--config", examples, "--check-secrets")
	if strings.Contains(got.stdout+got.stderr, secret) {
		t.Fatal("secret value printed")
	}
}

// TestDeploymentExamples keeps the configurations of the deployment
// examples valid.
func TestDeploymentExamples(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "examples", "compose", "tapyr"),
		filepath.Join("..", "..", "examples", "kubernetes", "config.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			if got := execute(t, nil, "validate", "--config", path); got.code != exitOK {
				t.Fatalf("exit code = %d, stderr: %s", got.code, got.stderr)
			}
		})
	}
}

// background runs the CLI until stop is called. Servers listen on free
// ports; the address of the first listener is returned.
func background(t *testing.T, vars map[string]string, args ...string) (addr string, stop func() result) {
	t.Helper()
	return backgroundWith(t, vars, clock.Real(), args...)
}

// backgroundWith is background with an injected clock.
func backgroundWith(t *testing.T, vars map[string]string, clk clock.Clock, args ...string) (addr string, stop func() result) {
	t.Helper()
	return backgroundEnv(t, vars, clk, nil, args...)
}

// backgroundEnv is backgroundWith with a run observer.
func backgroundEnv(t *testing.T, vars map[string]string, clk clock.Clock, onRun func(int), args ...string) (addr string, stop func() result) {
	t.Helper()
	return backgroundHooks(t, vars, &env{clock: clk, onRun: onRun}, args...)
}

// backgroundHooks is background with the clock and observers of hooks.
func backgroundHooks(t *testing.T, vars map[string]string, hooks *env, args ...string) (addr string, stop func() result) {
	t.Helper()
	var stdout, stderr syncBuffer
	addrs := make(chan string, 4)
	listen := func(ctx context.Context, network, _ string) (net.Listener, error) {
		ln, err := (&net.ListenConfig{}).Listen(ctx, network, "127.0.0.1:0")
		if err == nil {
			addrs <- ln.Addr().String()
		}
		return ln, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	lookupEnv := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	go func() {
		done <- run(ctx, args, &env{
			stdout: &stdout, stderr: &stderr, lookupEnv: lookupEnv, clock: hooks.clock, listen: listen,
			onRun: hooks.onRun, onInstances: hooks.onInstances, reload: hooks.reload, onReload: hooks.onReload,
		})
	}()
	select {
	case addr = <-addrs:
	case code := <-done:
		t.Fatalf("exited with %d before listening: %s", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the listener")
	}
	return addr, func() result {
		cancel()
		select {
		case code := <-done:
			res := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
			assertNoSecrets(t, vars, args, res)
			return res
		case <-time.After(15 * time.Second):
			t.Fatal("did not exit after cancellation")
			return result{}
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func httpGet(t *testing.T, url string, header ...string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSimCommand(t *testing.T) {
	addr, stop := background(t, nil, "sim", "--provider", "example-sim", "--log-format", "text")
	code, body := httpGet(t, "http://"+addr+"/_sim/status")
	if code != http.StatusOK || !strings.Contains(body, `"provider":"example-sim"`) {
		t.Fatalf("status endpoint: %d %s", code, body)
	}
	code, _ = httpGet(t, "http://"+addr+"/api/v1/devices", "Authorization", "Bearer sim-api-key")
	if code != http.StatusOK {
		t.Fatalf("devices: %d", code)
	}
	res := stop()
	if res.code != exitOK || !strings.Contains(res.stderr, "msg=\"simulator listening\"") {
		t.Fatalf("exit %d, stderr %s", res.code, res.stderr)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                              "0s",
		45 * time.Second:               "45s",
		2 * time.Minute:                "2m",
		10 * time.Minute:               "10m",
		30 * time.Minute:               "30m",
		time.Hour:                      "1h",
		90 * time.Minute:               "1h30m",
		10*time.Hour + 30*time.Second:  "10h0m30s",
		19*time.Minute + 2*time.Second: "19m2s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
