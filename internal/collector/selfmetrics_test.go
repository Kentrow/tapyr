package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/buildinfo"
	"github.com/kentrow/tapyr/internal/model"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/budget/planner"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/selfobs"
	"github.com/kentrow/tapyr/internal/state"
)

func TestScopeLabel(t *testing.T) {
	tests := []struct {
		key, want  string
		aggregated bool
	}{
		{key: "", want: ""},
		{key: "instance", want: "instance"},
		{key: "account=acme", want: "account=acme"},
		{key: "credential=0123456789abcdef,endpoint=/devices", want: "credential=0123456789abcdef,endpoint=/devices"},
		{key: "organizationId=4242", want: "var:organizationId", aggregated: true},
		{key: "account=acme,organizationId=4242", want: "account,var:organizationId", aggregated: true},
		{key: "organizationId=*", want: "var:organizationId", aggregated: true},
	}
	for _, tt := range tests {
		got, aggregated := scopeLabel(tt.key)
		if got != tt.want || aggregated != tt.aggregated {
			t.Errorf("scopeLabel(%q) = %q, %v; want %q, %v", tt.key, got, aggregated, tt.want, tt.aggregated)
		}
	}
}

func TestQuotaGauges(t *testing.T) {
	start := time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC)
	now := start.Add(6 * time.Hour)
	end := start.Add(24 * time.Hour)
	quotas := []budget.QuotaState{
		// 250 used in 6 hours: the 750 remaining last exactly until the window end.
		{
			Provider: "p", Limit: "daily", Scope: "account=acme", Max: 1000, Usable: 1000, Reserve: 100, Used: 250, Remaining: 750,
			ObservedRemaining: -1, WindowStart: start, WindowEnd: end,
		},
		// Two organizations: the most constrained one is exposed.
		{
			Provider: "p", Limit: "org", Scope: "organizationId=1", Max: 100, Usable: 100, Reserve: 10, Used: 10, Remaining: 90,
			ObservedRemaining: -1, WindowStart: start, WindowEnd: end, Mismatch: true,
		},
		{
			Provider: "p", Limit: "org", Scope: "organizationId=2", Max: 100, Usable: 100, Reserve: 10, Used: 95, Remaining: 5,
			ObservedRemaining: 4, WindowStart: start, WindowEnd: end,
		},
		{
			Provider: "p", Limit: "org", Scope: "organizationId=3", Max: 100, Usable: 100, Reserve: 10, Used: 50, Remaining: 50,
			ObservedRemaining: -1, WindowStart: start, WindowEnd: end,
		},
	}
	want := []selfobs.QuotaGauge{
		{
			Provider: "p", Limit: "daily", Scope: "account=acme", Max: 1000, Used: 250, Remaining: 750, ObservedRemaining: -1,
			ReserveRemaining: 100, WindowEnd: end,
		},
		{
			Provider: "p", Limit: "org", Scope: "var:organizationId", Max: 100, Used: 95, Remaining: 5, ObservedRemaining: 4,
			ReserveRemaining: 5, WindowEnd: end, ExhaustionETA: 6 * time.Hour * 5 / 95, Mismatch: true,
		},
	}
	if diff := cmp.Diff(want, quotaGauges(quotas, now)); diff != "" {
		t.Errorf("gauges (-want +got):\n%s", diff)
	}
}

func TestExhaustionETA(t *testing.T) {
	start := time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC)
	now := start.Add(12 * time.Hour)
	end := start.Add(24 * time.Hour)
	tests := []struct {
		name string
		q    budget.QuotaState
		want time.Duration
	}{
		{"nothing used", budget.QuotaState{WindowStart: start, WindowEnd: end, Remaining: 100}, 0},
		{"lasts the window", budget.QuotaState{WindowStart: start, WindowEnd: end, Used: 50, Remaining: 50}, 0},
		{"runs out", budget.QuotaState{WindowStart: start, WindowEnd: end, Used: 80, Remaining: 20}, 3 * time.Hour},
		{"exhausted", budget.QuotaState{WindowStart: start, WindowEnd: end, Used: 100}, time.Nanosecond},
		{"rolling", budget.QuotaState{WindowStart: start, WindowEnd: now, Used: 80, Remaining: 20, Rolling: true}, 0},
	}
	for _, tt := range tests {
		if got := exhaustionETA(tt.q, now); got != tt.want {
			t.Errorf("%s: ETA %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestPlanAndTokenGauges(t *testing.T) {
	if planGauges(nil) != nil {
		t.Error("gauges without a plan")
	}
	plan := &planner.Result{Scopes: []planner.ScopePlan{
		{Limit: "p/daily", Key: "account=acme", Feasible: true},
		{Limit: "p/org", Key: "organizationId=1", Feasible: true},
		{Limit: "p/org", Key: "organizationId=2", Feasible: false},
	}}
	wantPlan := []selfobs.PlanGauge{
		{Provider: "p", Limit: "daily", Scope: "account=acme", Feasible: true},
		{Provider: "p", Limit: "org", Scope: "var:organizationId", Feasible: false},
	}
	if diff := cmp.Diff(wantPlan, planGauges(plan)); diff != "" {
		t.Errorf("plan gauges (-want +got):\n%s", diff)
	}

	at := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	sessions := []auth.SessionInfo{
		{Provider: "p", Account: "a", Target: "t1", Expiry: at.Add(time.Hour)},
		{Provider: "p", Account: "a", Target: "t2", Expiry: at},
		{Provider: "p", Account: "b", Expiry: at},
		{Provider: "p", Account: "b", Target: "t2"},
	}
	wantTokens := []selfobs.TokenGauge{{Provider: "p", Account: "a", Expiry: at}, {Provider: "p", Account: "b"}}
	if diff := cmp.Diff(wantTokens, tokenGauges(sessions)); diff != "" {
		t.Errorf("token gauges (-want +got):\n%s", diff)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC))
	store, err := state.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "tapyr.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	c := &Collector{clock: clk, store: store, redact: func(s string) string { return s }}
	c.heartbeat.Store(clk.Now().UnixNano())

	get := func(h http.HandlerFunc) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return rec.Code, body
	}
	if code, _ := get(c.serveHealth); code != http.StatusOK {
		t.Errorf("healthz = %d", code)
	}
	clk.Advance(livenessTimeout + time.Second)
	if code, body := get(c.serveHealth); code != http.StatusServiceUnavailable || body["healthy"] != false {
		t.Errorf("healthz after a stalled flush loop = %d %v", code, body)
	}

	code, body := get(c.serveReady)
	want := []any{"configuration not loaded", "outputs not started"}
	if code != http.StatusServiceUnavailable || !cmp.Equal(body["reasons"], want) {
		t.Errorf("readyz = %d %v", code, body)
	}
	c.gen, _ = &generation{}, c.outputsStarted.Swap(true)
	if code, body := get(c.serveReady); code != http.StatusOK || body["ready"] != true {
		t.Errorf("readyz when ready = %d %v", code, body)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if code, body := get(c.serveReady); code != http.StatusServiceUnavailable || !cmp.Equal(body["reasons"], []any{"state store not answering"}) {
		t.Errorf("readyz with a closed store = %d %v", code, body)
	}
}

func TestSelfSamples(t *testing.T) {
	m := selfobs.NewMetrics(buildinfo.Info{Version: "1.0.0", Revision: "r", GoVersion: "go"}, nil)
	m.Request("p", "a", "j", "/e", 200, "", time.Second, 100)
	m.ConfigLoaded(time.Unix(0, 0), true)
	families, err := m.Gather()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	got := map[string]float64{}
	for _, s := range selfSamples(families, now) {
		if !s.Timestamp.Equal(now) {
			t.Errorf("%s stamped %v", s.Name, s.Timestamp)
		}
		if strings.HasPrefix(s.Name, "tapyr_") {
			got[s.Name+"{"+labelString(s.Labels)+"}"] = s.Value
		}
	}
	for key, want := range map[string]float64{
		`tapyr_build_info{goversion=go,revision=r,version=1.0.0}`:                                  1,
		`tapyr_config_last_reload_success{}`:                                                       1,
		`tapyr_http_requests_total{account=a,class=success,code=200,endpoint=/e,job=j,provider=p}`: 1,
		`tapyr_http_request_duration_seconds_bucket{account=a,endpoint=/e,le=1,provider=p}`:        1,
		`tapyr_http_request_duration_seconds_bucket{account=a,endpoint=/e,le=0.5,provider=p}`:      0,
		`tapyr_http_request_duration_seconds_bucket{account=a,endpoint=/e,le=+Inf,provider=p}`:     1,
		`tapyr_http_request_duration_seconds_sum{account=a,endpoint=/e,provider=p}`:                1,
		`tapyr_http_request_duration_seconds_count{account=a,endpoint=/e,provider=p}`:              1,
	} {
		if v, ok := got[key]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", key, v, ok, want)
		}
	}
}

func labelString(ls model.Labels) string {
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l.Name + "=" + l.Value
	}
	return strings.Join(parts, ",")
}
