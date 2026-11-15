package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/clock"
)

var at = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// fake is a Source with one of everything.
type fake struct{ noPlan bool }

func (fake) Status(context.Context) Status {
	return Status{
		Version: "1.2.3", Revision: "abc", GoVersion: "go1.27", StartedAt: at, UptimeSeconds: 60, ConfigHash: "f00",
		LastReload: Reload{At: at, Success: false, Error: "invalid YAML"}, ReadinessReasons: []string{}, Instances: 3,
	}
}

func (fake) Debug(_ context.Context, job, _ string) (Debug, error) {
	switch job {
	case "devices":
		return Debug{
			Job: "devices", Instance: "devices", RunID: "000001", Time: at, Result: "success",
			Pages:     []DebugPage{{Status: 200, Headers: map[string]string{"Content-Type": "application/json"}, Body: `[{"id": 1}]`}},
			Samples:   []DebugSample{{Name: "device_up", Type: "gauge", Labels: map[string]string{"id": "1"}, Value: 1}},
			Instances: []string{"devices"},
		}, nil
	case "off":
		return Debug{}, ErrDebugDisabled
	}
	return Debug{}, ErrNotFound
}

func (fake) Providers(context.Context) []Provider {
	return []Provider{{Name: "meraki", Version: "0.1.0", Accounts: 1, Jobs: 2, Health: "ok"}}
}

func (fake) Accounts(context.Context) []Account {
	return []Account{
		{Name: "acme", Provider: "meraki", Labels: map[string]string{"tenant": "acme"}, Targets: []Target{}, Circuit: "closed", Auth: AuthStatus{Type: "header"}, Health: "ok", HealthReasons: []string{}},
		{
			Name: "fleet", Provider: "rutos", Labels: map[string]string{},
			Targets: []Target{{Name: "south", Labels: map[string]string{"site": "south"}, Circuit: "open"}},
			Auth:    AuthStatus{Type: "session_login", TokenExpiry: ptr(at)},
			Health:  "failing", HealthReasons: []string{"circuit open for target south"},
		},
	}
}

var job = Job{
	Name: "devices", Provider: "meraki", Account: "acme", Template: "meraki-devices", Role: "metrics", Priority: "normal",
	DesiredIntervalSeconds: 300, EffectiveIntervalSeconds: 600, StretchReason: "quota daily", Instances: 2, Collected: true,
	Outputs: []string{}, Labels: map[string]string{},
	LastRun: &RunSummary{RunID: "000001", Instance: "devices", Result: "success", StartedAt: at, EndedAt: at.Add(time.Second)},
	NextRun: ptr(at.Add(time.Minute)),
}

func (fake) Jobs(context.Context) []Job { return []Job{job} }

func (fake) Job(_ context.Context, name string) (Job, error) {
	if name != "devices" {
		return Job{}, ErrNotFound
	}
	j := job
	j.Definition = json.RawMessage(`{"request": {"path": "/devices"}}`)
	return j, nil
}

func (fake) Instances(_ context.Context, name string) ([]Instance, error) {
	if name != "devices" {
		return nil, ErrNotFound
	}
	return []Instance{{Key: "devices[orgId=1]", Vars: map[string]string{"orgId": "1"}, LastSuccess: ptr(at), Series: 4}}, nil
}

func (fake) Runs(_ context.Context, name, instance string, limit int) ([]Run, error) {
	switch {
	case name == "broken":
		return nil, errors.New("database is locked")
	case name != "devices":
		return nil, ErrNotFound
	case instance == "none":
		return nil, nil
	}
	runs := []Run{
		{RunSummary: RunSummary{RunID: "000002", Instance: "devices", Result: "failed", StartedAt: at, EndedAt: at, Error: "transient /devices: HTTP 503"}},
		{RunSummary: RunSummary{RunID: "000001", Instance: "devices", Result: "success", StartedAt: at, EndedAt: at}, Requests: 3, Pages: 3},
	}
	return runs[:min(limit, len(runs))], nil
}

func (fake) Budgets(context.Context) []Budget {
	return []Budget{
		{
			Provider: "meraki", Limit: "daily", Kind: "quota", Scope: "account=acme", Accounts: []string{"acme"}, Max: ptr[int64](1000), Usable: ptr[int64](1000),
			Reserve: ptr[int64](100), Used: ptr[int64](10), Remaining: ptr[int64](990), ExternalUsage: ptr[int64](0),
			ReserveRemaining: ptr[int64](100), WindowStart: ptr(at), WindowEnd: ptr(at.Add(time.Hour)), ExhaustionETASeconds: ptr(30.0),
		},
		{Provider: "meraki", Limit: "rate", Kind: "rate", Scope: "account=acme", Accounts: []string{"acme"}, RatePerSecond: ptr(10.0), Burst: ptr(10.0), Tokens: ptr(9.5)},
		{Provider: "meraki", Limit: "conc", Kind: "concurrency", Scope: "instance", Accounts: []string{"acme"}, Max: ptr[int64](4), InUse: ptr[int64](1), PenaltyUntil: ptr(at)},
	}
}

func (fake) Timeline(_ context.Context, limit, scope string) (Timeline, error) {
	if limit != "meraki/daily" || scope != "account=acme" {
		return Timeline{}, ErrNotFound
	}
	w := TimelineWindow{Start: at, End: at.Add(time.Hour), Buckets: []Bucket{{Start: at, Count: 3}}}
	return Timeline{Provider: "meraki", Limit: "daily", Scope: scope, BucketSeconds: 60, Current: w, Previous: &w}, nil
}

func (f fake) Plan(context.Context) (Plan, error) {
	if f.noPlan {
		return Plan{}, ErrNotFound
	}
	return Plan{
		At: at, Feasible: true,
		Jobs: []PlanJob{{
			Name: "devices", Provider: "meraki", Account: "acme", Priority: "normal", DesiredIntervalSeconds: 300,
			EffectiveIntervalSeconds: 300, Stretch: 1, Instances: 2, PagesPerRun: 1,
		}},
		Scopes: []PlanScope{{
			Limit: "meraki/daily", Scope: "account=acme", PeriodSeconds: 86400, Budget: 900, Reserve: 100,
			Projected: 576, Feasible: true, Jobs: []string{"devices"},
		}},
	}, nil
}

func (fake) Graph(context.Context) Graph {
	return Graph{
		Nodes: []Node{{ID: "provider/meraki", Kind: "provider", Label: "meraki", Health: "ok"}, {ID: "job/devices", Kind: "job", Label: "devices", Health: "ok", Instances: 2}},
		Edges: []Edge{{From: "provider/meraki", To: "job/devices"}},
	}
}

func (fake) Outputs(context.Context) []Output {
	return []Output{
		{Name: "prom", Type: "prometheus"},
		{Name: "vm", Type: "remote_write", Queue: 3, Capacity: 50000, Sent: 100, Dropped: 2, LastSuccess: ptr(at), LastError: "transient: down", LastErrorAt: ptr(at)},
	}
}

func get(t *testing.T, h http.Handler, url string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, url, nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, body
}

func TestRoutesAreDocumented(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	documented := v.Paths()
	slices.Sort(documented)
	routes := slices.Clone(routes)
	slices.Sort(routes)
	if diff := cmp.Diff(routes, documented); diff != "" {
		t.Errorf("routes and OpenAPI paths differ (-routes +documented):\n%s", diff)
	}
}

func TestResponses(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(fake{}, NewBroker(nil))
	tests := []struct {
		operation, url string
		status         int
	}{
		{"GET /api/v1/status", "/api/v1/status", 200},
		{"GET /api/v1/providers", "/api/v1/providers", 200},
		{"GET /api/v1/accounts", "/api/v1/accounts", 200},
		{"GET /api/v1/accounts/{name}", "/api/v1/accounts/fleet", 200},
		{"GET /api/v1/accounts/{name}", "/api/v1/accounts/nope", 404},
		{"GET /api/v1/jobs", "/api/v1/jobs", 200},
		{"GET /api/v1/jobs/{name}", "/api/v1/jobs/devices", 200},
		{"GET /api/v1/jobs/{name}", "/api/v1/jobs/nope", 404},
		{"GET /api/v1/jobs/{name}/instances", "/api/v1/jobs/devices/instances", 200},
		{"GET /api/v1/jobs/{name}/instances", "/api/v1/jobs/nope/instances", 404},
		{"GET /api/v1/jobs/{name}/runs", "/api/v1/jobs/devices/runs?limit=1", 200},
		{"GET /api/v1/jobs/{name}/runs", "/api/v1/jobs/devices/runs?instance=none", 200},
		{"GET /api/v1/jobs/{name}/runs", "/api/v1/jobs/devices/runs?limit=0", 400},
		{"GET /api/v1/jobs/{name}/runs", "/api/v1/jobs/nope/runs", 404},
		{"GET /api/v1/budgets", "/api/v1/budgets", 200},
		{"GET /api/v1/budgets/{limit}/timeline", "/api/v1/budgets/meraki%2Fdaily/timeline?scope=account%3Dacme", 200},
		{"GET /api/v1/budgets/{limit}/timeline", "/api/v1/budgets/meraki%2Fdaily/timeline?scope=nope", 404},
		{"GET /api/v1/plan", "/api/v1/plan", 200},
		{"GET /api/v1/graph", "/api/v1/graph", 200},
		{"GET /api/v1/outputs", "/api/v1/outputs", 200},
		{"GET /api/v1/outputs", "/api/v1/outputs?cursor=!", 400},
		{"GET /api/v1/debug/jobs/{name}/last", "/api/v1/debug/jobs/devices/last", 200},
		{"GET /api/v1/debug/jobs/{name}/last", "/api/v1/debug/jobs/nope/last", 404},
		{"GET /api/v1/debug/jobs/{name}/last", "/api/v1/debug/jobs/off/last", 404},
	}
	for _, tt := range tests {
		status, body := get(t, h, tt.url)
		if status != tt.status {
			t.Errorf("%s: %d, want %d: %s", tt.url, status, tt.status, body)
			continue
		}
		if err := v.Validate(tt.operation, status, body); err != nil {
			t.Errorf("%s: %v\n%s", tt.url, err, body)
		}
	}
	if status, body := get(t, Handler(fake{noPlan: true}, NewBroker(nil)), "/api/v1/plan"); status != 404 || v.Validate("GET /api/v1/plan", 404, body) != nil {
		t.Errorf("no plan: %d %s", status, body)
	}
	if status, body := get(t, h, "/api/v1/openapi.yaml"); status != 200 || !strings.HasPrefix(string(body), "openapi: 3.1.0") {
		t.Errorf("openapi.yaml: %d", status)
	}
	if status, body := get(t, h, "/api/v1/nope"); status != 404 || !strings.Contains(string(body), `"not_found"`) {
		t.Errorf("unknown endpoint: %d %s", status, body)
	}
	if status, _ := get(t, h, "/api/v1/jobs/broken/runs"); status != 500 {
		t.Errorf("source error: %d", status)
	}
	if err := v.Validate("GET /api/v1/status", 418, []byte(`{}`)); err == nil {
		t.Error("an undocumented status validated")
	}
	if err := v.Validate("GET /api/v1/status", 200, []byte(`{"version": 1}`)); err == nil {
		t.Error("an invalid body validated")
	}
}

func TestPagination(t *testing.T) {
	h := Handler(fake{}, NewBroker(nil))
	var page List[Budget]
	var names []string
	url := "/api/v1/budgets?limit=2"
	for range 5 {
		status, body := get(t, h, url)
		if status != 200 {
			t.Fatalf("%s: %d %s", url, status, body)
		}
		page = List[Budget]{}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		for _, b := range page.Items {
			names = append(names, b.Limit)
		}
		if page.NextCursor == "" {
			break
		}
		url = "/api/v1/budgets?limit=2&cursor=" + page.NextCursor
	}
	if !cmp.Equal(names, []string{"daily", "rate", "conc"}) {
		t.Errorf("pages = %v", names)
	}
	// A cursor past the end gives an empty page.
	if status, body := get(t, h, "/api/v1/budgets?cursor=OTk"); status != 200 || !strings.Contains(string(body), `"items": []`) {
		t.Errorf("past the end: %d %s", status, body)
	}
	for _, q := range []string{"limit=1001", "limit=x", "cursor=LTE"} {
		if status, _ := get(t, h, "/api/v1/jobs?"+q); status != 400 {
			t.Errorf("%s: %d", q, status)
		}
	}
}

func TestEvents(t *testing.T) {
	clk := clock.NewFake(at)
	b := NewBroker(clk)
	srv := httptest.NewServer(Handler(fake{}, b))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %s", ct)
	}
	r := bufio.NewReader(resp.Body)
	readBlock := func() string {
		t.Helper()
		var lines []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line == "\n" {
				return strings.Join(lines, "")
			}
			lines = append(lines, line)
		}
	}
	if got := readBlock(); got != ": connected\n" {
		t.Fatalf("first block = %q", got)
	}
	for b.Subscribers() == 0 {
		time.Sleep(time.Millisecond)
	}
	b.Publish(EventRunFinished, map[string]any{"job": "devices", "result": "success"})
	if got := readBlock(); got != "event: run_finished\ndata: {\"job\":\"devices\",\"result\":\"success\",\"time\":\"2026-11-03T12:00:00Z\"}\n" {
		t.Errorf("event = %q", got)
	}
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(15 * time.Second)
	if got := readBlock(); got != ": heartbeat\n" {
		t.Errorf("heartbeat = %q", got)
	}
	// A client that does not read loses events, the others are not blocked.
	for range 2 * subscriberBuffer {
		b.Publish(EventPlanChanged, nil)
	}
	cancel()
	for b.Subscribers() != 0 {
		time.Sleep(time.Millisecond)
	}
	var nilBroker *Broker
	nilBroker.Publish(EventPlanChanged, nil) // a collector without UI publishes to nothing
}

// Subscribers returns the number of connected clients.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
