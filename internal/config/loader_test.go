package config

import (
	"errors"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml/parser"
	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestLoadInvalid checks the diagnostics of every broken fixture against a
// golden file (testdata/invalid/<name>.golden). Run with -update to rewrite
// them after an intended change, then review the diff.
func TestLoadInvalid(t *testing.T) {
	cases, err := filepath.Glob(filepath.Join("testdata", "invalid", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases, filepath.Join("testdata", "invalid", "duplicates"))
	l := newTestLoader(t)
	for _, path := range cases {
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		t.Run(name, func(t *testing.T) {
			_, err := l.Load([]string{path})
			var diags Diagnostics
			if !errors.As(err, &diags) {
				t.Fatalf("Load() error = %v, want Diagnostics", err)
			}
			got := diags.Error() + "\n"
			golden := strings.TrimSuffix(path, ".yaml") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("reading golden file (run go test -update): %v", err)
			}
			if diff := cmp.Diff(string(want), got); diff != "" {
				t.Errorf("diagnostics mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLoadMissingPaths(t *testing.T) {
	l := newTestLoader(t)
	if _, err := l.Load(nil); err == nil || !strings.Contains(err.Error(), "no configuration path") {
		t.Fatalf("Load(nil) error = %v", err)
	}
	_, err := l.Load([]string{filepath.Join("testdata", "does-not-exist")})
	if err == nil || err.Error() != "testdata/does-not-exist: no such file or directory" {
		t.Fatalf("Load(missing) error = %v", err)
	}
}

func TestLoadTree(t *testing.T) {
	l := newTestLoader(t)
	tree := filepath.Join("testdata", "valid", "tree")
	// The same files through two paths are loaded once.
	cfg, err := l.Load([]string{tree, filepath.Join(tree, "provider.yaml")})
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(cfg.Providers) != 1 || len(cfg.JobTemplates) != 2 || len(cfg.Accounts) != 1 || len(cfg.Jobs) != 2 || len(cfg.Outputs) != 1 {
		t.Fatalf("unexpected document counts: %d providers, %d templates, %d accounts, %d jobs, %d outputs",
			len(cfg.Providers), len(cfg.JobTemplates), len(cfg.Accounts), len(cfg.Jobs), len(cfg.Outputs))
	}

	t.Run("merge keys", func(t *testing.T) {
		metrics := cfg.JobTemplates[1].Spec.Extract.Metrics
		if len(metrics) != 2 {
			t.Fatalf("metrics = %+v", metrics)
		}
		if metrics[1].Name != "example_device_clients" || metrics[1].Value != "double(item.clients)" ||
			metrics[1].Labels["site"] != "vars.siteId" {
			t.Errorf("merged metric = %+v", metrics[1])
		}
		if got := cfg.JobTemplates[0].Spec.Extract.Inventory.Fields["siteName"]; got != "item.name" {
			t.Errorf("anchored inventory field = %q", got)
		}
	})

	t.Run("str tag keeps leading zero", func(t *testing.T) {
		if got := cfg.Accounts[0].Spec.Labels["zip"]; got != "01234" {
			t.Errorf("zip label = %q, want 01234", got)
		}
	})

	t.Run("relative paths", func(t *testing.T) {
		nested := filepath.Join(tree, "nested")
		acc := cfg.Accounts[0].Spec
		if got, want := acc.Secrets["apiKey"].File, filepath.Join(nested, "secrets", "api-key"); got != want {
			t.Errorf("secret file = %q, want %q", got, want)
		}
		if got, want := acc.TLS.CAFile, filepath.Join(tree, "certs", "ca.pem"); got != want {
			t.Errorf("caFile = %q, want %q", got, want)
		}
		if got, want := cfg.Outputs[0].Spec.RemoteWrite.Headers["Authorization"].Secret.File, filepath.Join(nested, "secrets", "vm-token"); got != want {
			t.Errorf("header secret file = %q, want %q", got, want)
		}
		s := cfg.Settings.Spec
		if got, want := s.State.Path, filepath.Join(nested, "state", "tapyr.db"); got != want {
			t.Errorf("state path = %q, want %q", got, want)
		}
		if diff := cmp.Diff([]string{filepath.Join(tree, "providers"), "/opt/tapyr/providers"}, s.ProviderPaths); diff != "" {
			t.Errorf("providerPaths (-want +got):\n%s", diff)
		}
	})

	t.Run("values", func(t *testing.T) {
		q := cfg.Jobs[1].Spec.Request.Query
		if q["verbose"] != (Value{Literal: "true"}) || q["since"] != (Value{Expr: "string(now)"}) {
			t.Errorf("query = %+v", q)
		}
		auth := cfg.Providers[0].Spec.Auth.Header
		if auth.Name != "X-API-Key" || auth.Value != (Value{Secret: "apiKey"}) {
			t.Errorf("header auth = %+v", auth)
		}
		if got := cfg.Settings.Spec.Collection.Jitter.Fraction(); got != 0.05 {
			t.Errorf("jitter = %v, want 0.05", got)
		}
	})
}

func TestDefaults(t *testing.T) {
	cfg, err := newTestLoader(t).Load([]string{filepath.Join("testdata", "valid", "minimal.yaml")})
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	t.Run("settings", func(t *testing.T) {
		s := cfg.Settings
		if s == nil || s.Metadata.Name != defaultSettingsName {
			t.Fatalf("settings = %+v", s)
		}
		spec := s.Spec
		checks := []struct {
			name      string
			got, want any
		}{
			{"listen", spec.Server.Listen, "127.0.0.1:9790"},
			{"ui", *spec.Server.UI.Enabled, true},
			{"debug body", spec.Server.Debug.MaxBodyBytes, 1 << 20},
			{"state", spec.State.Path, "./data/tapyr.db"},
			{"concurrency", spec.Collection.MaxConcurrentRequests, 32},
			{"timeout", spec.Collection.DefaultTimeout.Std(), 30 * time.Second},
			{"jitter", spec.Collection.Jitter.Fraction(), 0.1},
			{"reserve", spec.Budget.RetryReserve.Fraction(), 0.1},
			{"replan", spec.Budget.ReplanInterval.Std(), 15 * time.Minute},
			{"replan on projection", *spec.Budget.ReplanOnProjection, true},
			{"stretch", spec.Budget.MaxStretch, 12.0},
			{"attempts", spec.Retry.MaxAttempts, 4},
			{"backoff", spec.Retry.InitialBackoff.Std(), time.Second},
			{"max backoff", spec.Retry.MaxBackoff.Std(), time.Minute},
			{"multiplier", spec.Retry.Multiplier, 2.0},
			{"threshold", spec.CircuitBreaker.FailureThreshold, 5},
			{"cooldown", spec.CircuitBreaker.Cooldown.Std(), 2 * time.Minute},
			{"max cooldown", spec.CircuitBreaker.MaxCooldown.Std(), 30 * time.Minute},
			{"cost", spec.Mapping.MaxCost, int64(1_000_000)},
			{"items", spec.Mapping.MaxItemsPerRun, 100_000},
			{"samples", spec.Mapping.MaxSamplesPerRun, 200_000},
			{"conflict", spec.Mapping.LabelConflict, "override"},
			{"log level", spec.Log.Level, "info"},
			{"log format", spec.Log.Format, "json"},
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
			}
		}
	})

	t.Run("provider", func(t *testing.T) {
		spec := cfg.Providers[0].Spec
		if spec.Auth.Type != AuthNone {
			t.Errorf("auth type = %q", spec.Auth.Type)
		}
		e := spec.Errors
		if diff := cmp.Diff([]int{429}, e.RateLimited.Statuses); diff != "" {
			t.Errorf("rate limited statuses: %s", diff)
		}
		if e.RateLimited.RetryAfterHeader != "Retry-After" || e.RateLimited.DefaultPenalty.Std() != 2*time.Second || e.RateLimited.Penalize != "narrowest" {
			t.Errorf("rate limited = %+v", e.RateLimited)
		}
		if diff := cmp.Diff([]int{401}, e.AuthFailed.Statuses); diff != "" {
			t.Errorf("auth failed statuses: %s", diff)
		}
		if diff := cmp.Diff([]int{500, 502, 503, 504}, e.Retryable.Statuses); diff != "" {
			t.Errorf("retryable statuses: %s", diff)
		}
		if spec.Pagination.MaxPages != 100 || spec.Pagination.LinkHeader != nil {
			t.Errorf("pagination = %+v", spec.Pagination)
		}
		rate, daily, weekly := spec.Limits[0], spec.Limits[1], spec.Limits[2]
		if rate.Burst != 10 {
			t.Errorf("burst = %d, want 10 (600/m is 10/s)", rate.Burst)
		}
		if diff := cmp.Diff([]string{"instance"}, rate.Scope.By); diff != "" {
			t.Errorf("default scope: %s", diff)
		}
		if rate.AppliesTo.AuthRequests != "include" {
			t.Errorf("authRequests = %q", rate.AppliesTo.AuthRequests)
		}
		if w := daily.Window; w.ResetAt != "00:00" || w.Timezone != "UTC" || w.WeekStart != "" || w.MonthDay != 0 {
			t.Errorf("daily window = %+v", w)
		}
		if w := weekly.Window; w.WeekStart != "monday" {
			t.Errorf("weekly window = %+v", w)
		}
	})

	t.Run("jobs", func(t *testing.T) {
		tmpl := cfg.JobTemplates[0].Spec
		if tmpl.Role != RoleMetrics || tmpl.Request.Method != http.MethodGet || tmpl.Extract.Metrics[0].Type != MetricGauge {
			t.Errorf("template defaults = %+v", tmpl)
		}
		fromTemplate, inline := cfg.Jobs[0].Spec, cfg.Jobs[1].Spec
		if fromTemplate.Interval.Std() != 10*time.Minute || fromTemplate.Priority != PriorityNormal {
			t.Errorf("job from template: interval %v priority %q", fromTemplate.Interval.Std(), fromTemplate.Priority)
		}
		if inline.Priority != PriorityNormal || inline.Request.Method != http.MethodGet || inline.Role != RoleMetrics {
			t.Errorf("inline job = %+v", inline)
		}
		if share := *cfg.Accounts[0].Spec.Budget.Share; share != 1 {
			t.Errorf("share = %v", share)
		}
	})

	t.Run("outputs", func(t *testing.T) {
		prom, otel := cfg.Outputs[0].Spec, cfg.Outputs[1].Spec
		if prom.Queue != nil || prom.Prometheus == nil {
			t.Errorf("prometheus output = %+v", prom)
		}
		if otel.OTLP.Protocol != "http/protobuf" || otel.OTLP.Timeout.Std() != 10*time.Second {
			t.Errorf("otlp = %+v", otel.OTLP)
		}
		if q := otel.Queue; q.Capacity != 50000 || q.MaxBatch != 5000 || q.FlushInterval.Std() != 5*time.Second {
			t.Errorf("queue = %+v", q)
		}
	})
}

func TestTemplateDefaultsInheritance(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "c.yaml"), `
apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: p, version: 0.1.0 }
spec: { baseURL: https://api.example.invalid }
---
apiVersion: tapyr/v1alpha1
kind: JobTemplate
metadata: { name: t }
spec: { provider: p, defaultInterval: 6h, defaultPriority: low, request: { path: /x } }
---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: a }
spec: { provider: p }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: inherits }
spec: { account: a, template: t }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: overrides }
spec: { account: a, template: t, interval: 1h, priority: critical }
`)
	cfg, err := newTestLoader(t).Load([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	inherits, overrides := cfg.Jobs[0].Spec, cfg.Jobs[1].Spec
	if inherits.Interval.Std() != 6*time.Hour || inherits.Priority != PriorityLow {
		t.Errorf("inherits: %v %q", inherits.Interval.Std(), inherits.Priority)
	}
	if overrides.Interval.Std() != time.Hour || overrides.Priority != PriorityCritical {
		t.Errorf("overrides: %v %q", overrides.Interval.Std(), overrides.Priority)
	}
}

func TestAliasExpansionLimit(t *testing.T) {
	// A "billion laughs" document must be rejected, not expanded. The
	// loader limit is exercised through a builder with a small limit to keep
	// the test fast; the end-to-end path is checked with the real limit on a
	// smaller bomb.
	var b strings.Builder
	b.WriteString("apiVersion: tapyr/v1alpha1\nkind: Output\nmetadata: { name: bomb }\nspec:\n  type: prometheus\n")
	b.WriteString("  externalLabels: {}\nx0: &x0 [a, a, a, a, a, a, a, a, a, a]\n")
	for i := 1; i <= 7; i++ {
		b.WriteString("x" + string(rune('0'+i)) + ": &x" + string(rune('0'+i)) + " [")
		for j := range 10 {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString("*x" + string(rune('0'+i-1)))
		}
		b.WriteString("]\n")
	}
	f, err := parser.ParseBytes([]byte(b.String()), 0)
	if err != nil {
		t.Fatal(err)
	}
	builder := newTreeBuilder("bomb.yaml")
	builder.maxNodes = 10_000
	if _, err := builder.convert(f.Docs[0].Body, nil, nil, false); err == nil || !strings.Contains(err.Error(), "document too large") {
		t.Fatalf("convert() error = %v, want document too large", err)
	}

	path := filepath.Join(t.TempDir(), "bomb.yaml")
	write(t, path, b.String())
	if _, err := newTestLoader(t).Load([]string{path}); err == nil || !strings.Contains(err.Error(), "document too large") {
		t.Fatalf("Load() error = %v, want document too large", err)
	}
}

func TestAliasCyclesAndDepth(t *testing.T) {
	// Found by FuzzParseDocuments: an alias inside the node of its own anchor
	// recursed until the stack overflowed (tapyr validate hung, then
	// crashed), and so did a document nested deeply enough.
	tests := []struct{ name, doc, want string }{
		{"self", "&a [*a, *a]\n", "alias *a is inside the node it refers to"},
		{"mapping", "a: &a\n  b: *a\n", "alias *a is inside the node it refers to"},
		{"merge", "a: &a\n  <<: *a\n", "alias *a is inside the node it refers to"},
		{"nested", "x: &x {y: &y [*x]}\n", "alias *x is inside the node it refers to"},
		{"depth", strings.Repeat("[", 5000) + strings.Repeat("]", 5000) + "\n", "nested more than"},
	}
	l := newTestLoader(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := l.parseDocuments("cycle.yaml", []byte(tt.doc), userKinds)
			if len(diags) != 1 || !strings.Contains(diags[0].Message, tt.want) {
				t.Fatalf("diagnostics %v, want %q", diags, tt.want)
			}
		})
	}
	// Aliases used side by side are no cycle.
	if _, diags := l.parseDocuments("ok.yaml", []byte("a: &a [1]\nb: [*a, *a]\nc: {<<: {x: *a}}\n"), userKinds); len(diags) != 1 ||
		strings.Contains(diags[0].Message, "inside the node") {
		t.Fatalf("diagnostics %v, want only the missing kind", diags)
	}
}

func TestPathFormatting(t *testing.T) {
	idx := &positionIndex{nodes: map[string]*nodeInfo{
		"/spec":                   {},
		"/spec/limits":            {},
		"/spec/limits/0":          {inSeq: true},
		"/spec/labels":            {},
		"/spec/labels/a.b":        {},
		"/spec/labels/with~1sl":   {},
		"/spec/labels/with space": {},
	}}
	tests := []struct {
		tokens []string
		want   string
	}{
		{[]string{"spec", "limits", "0", "rate"}, "spec.limits[0].rate"},
		{[]string{"spec", "labels", "a.b"}, `spec.labels["a.b"]`},
		{[]string{"spec", "labels", "with/sl"}, `spec.labels["with/sl"]`},
		{[]string{"spec", "labels", "with space"}, `spec.labels["with space"]`},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := idx.path(tt.tokens); got != tt.want {
			t.Errorf("path(%q) = %q, want %q", tt.tokens, got, tt.want)
		}
	}
	for _, tokens := range [][]string{{"a/b", "c~d"}, {"plain"}, nil} {
		if got := parsePointer(pointer(tokens)); !cmp.Equal(got, tokens) {
			t.Errorf("parsePointer(pointer(%q)) = %q", tokens, got)
		}
	}
}

func TestDiagnosticString(t *testing.T) {
	tests := []struct {
		d    Diagnostic
		want string
	}{
		{Diagnostic{Pos: Position{File: "a.yaml", Line: 3, Column: 5}, Path: "spec.x", Message: "bad"}, "a.yaml:3:5: spec.x: bad"},
		{Diagnostic{Pos: Position{File: "a.yaml", Line: 3}, Message: "bad"}, "a.yaml:3: bad"},
		{Diagnostic{Pos: Position{File: "a.yaml"}, Message: "missing"}, "a.yaml: missing"},
	}
	for _, tt := range tests {
		if got := tt.d.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
