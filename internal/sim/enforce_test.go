package sim

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/provider"
)

var midnight = time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC)

func exampleServer(t *testing.T, clk clock.Clock, scenario string) *Server {
	t.Helper()
	pkgs, err := provider.Builtin(loader(t))
	if err != nil {
		t.Fatal(err)
	}
	pkg := provider.Find(pkgs, "example-sim")
	var sc *config.Scenario
	if scenario != "" {
		if sc = pkg.Scenarios[scenario]; sc == nil {
			t.Fatalf("no scenario %s", scenario)
		}
	}
	s, err := New(pkg, Options{Clock: clk, Scenario: sc, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func call(s *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://sim"+path, http.NoBody)
	req.Header.Set("Authorization", "Bearer sim-api-key")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRateLimitEnforced(t *testing.T) {
	clk := clock.NewFake(midnight)
	s := exampleServer(t, clk, "")
	for i := range 10 {
		if rec := call(s, "/api/v1/sites"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	rec := call(s, "/api/v1/sites")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("11th request: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if got := rec.Header().Get("X-RateLimit-Remaining-Day"); got != "4990" {
		t.Errorf("remaining on a 429 = %q, want 4990", got)
	}
	clk.Advance(100 * time.Millisecond)
	if rec := call(s, "/api/v1/sites"); rec.Code != http.StatusOK {
		t.Fatalf("after a refill: %d", rec.Code)
	}
	st := s.Stats()
	if st.Refused["rate"] != 1 || st.Routes["GET /sites"] != 11 || st.Served != 11 {
		t.Errorf("stats = %+v", st)
	}
}

func TestQuotaTight(t *testing.T) {
	clk := clock.NewFake(midnight)
	s := exampleServer(t, clk, "quota-tight")
	for i := range 600 {
		clk.Advance(200 * time.Millisecond) // stay under 10/s
		rec := call(s, "/api/v1/devices")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
		if i == 0 {
			if rec.Header().Get("X-RateLimit-Limit-Day") != "600" || rec.Header().Get("X-RateLimit-Remaining-Day") != "599" {
				t.Fatalf("quota headers = %v", rec.Header())
			}
		}
	}
	clk.Advance(time.Second)
	rec := call(s, "/api/v1/devices")
	retry, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
	until := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC).Sub(clk.Now())
	if rec.Code != http.StatusTooManyRequests || time.Duration(retry)*time.Second < until || rec.Header().Get("X-RateLimit-Remaining-Day") != "0" {
		t.Fatalf("over quota: %d, Retry-After %d (window ends in %v), headers %v", rec.Code, retry, until, rec.Header())
	}
	if s.Stats().Refused["quota_exhausted"] != 1 {
		t.Errorf("stats = %+v", s.Stats())
	}
	clk.Set(time.Date(2026, 11, 3, 0, 0, 1, 0, time.UTC))
	if rec := call(s, "/api/v1/devices"); rec.Code != http.StatusOK {
		t.Fatalf("next day: %d", rec.Code)
	}
}

func TestBadDay(t *testing.T) {
	clk := clock.NewFake(midnight)
	s := exampleServer(t, clk, "bad-day")
	call(s, "/api/v1/sites")
	clk.Advance(time.Hour)
	// 60 calls per hour consumed by another client, plus the one above.
	rec := call(s, "/api/v1/sites")
	if got := rec.Header().Get("X-RateLimit-Remaining-Day"); got != strconv.Itoa(5000-62) {
		t.Errorf("remaining after one hour = %s, want %d", got, 5000-62)
	}
	failures := func() int {
		n := 0
		for range 100 {
			clk.Advance(200 * time.Millisecond)
			if rec := call(s, "/api/v1/devices"); rec.Code == http.StatusServiceUnavailable {
				n++
			}
		}
		return n
	}
	if n := failures(); n != 0 {
		t.Errorf("%d errors before the faulty period", n)
	}
	clk.Set(midnight.Add(2*time.Hour + time.Minute))
	if n := failures(); n < 15 || n > 45 {
		t.Errorf("%d errors out of 100 during the faulty period, want about 30", n)
	}
	if call(s, "/api/v1/sites").Code != http.StatusOK {
		t.Error("errors injected on a route outside the scenario")
	}
	clk.Set(midnight.Add(3 * time.Hour))
	if n := failures(); n != 0 {
		t.Errorf("%d errors after the faulty period", n)
	}
}

func TestScenarioStepEnds(t *testing.T) {
	clk := clock.NewFake(midnight)
	pkgs, err := provider.Builtin(loader(t))
	if err != nil {
		t.Fatal(err)
	}
	sc := &config.Scenario{Spec: config.ScenarioSpec{Steps: []config.ScenarioStep{
		{At: config.Duration(time.Hour), For: config.Duration(time.Hour), Limits: map[string]config.ScenarioLimit{"daily": {Max: 10}}},
	}}}
	s, err := New(provider.Find(pkgs, "example-sim"), Options{Clock: clk, Scenario: sc})
	if err != nil {
		t.Fatal(err)
	}
	limit := func() string { return call(s, "/api/v1/sites").Header().Get("X-RateLimit-Limit-Day") }
	if got := limit(); got != "5000" {
		t.Fatalf("before the step: %s", got)
	}
	clk.Advance(time.Hour)
	if got := limit(); got != "10" {
		t.Fatalf("during the step: %s", got)
	}
	clk.Advance(time.Hour)
	if got := limit(); got != "5000" {
		t.Fatalf("after the step: %s", got)
	}
}

// TestExternalConsumptionStepEnd is a regression test: the consumption of an
// externalConsumption step was charged only while the step was active at a
// request, so a step ending between two requests lost what it consumed since
// the last one.
func TestExternalConsumptionStepEnd(t *testing.T) {
	clk := clock.NewFake(midnight)
	pkgs, err := provider.Builtin(loader(t))
	if err != nil {
		t.Fatal(err)
	}
	// 300 calls between 1h and 1h10, 60 calls an hour from 2h on.
	sc := &config.Scenario{Spec: config.ScenarioSpec{Steps: []config.ScenarioStep{
		{
			At: config.Duration(time.Hour), For: config.Duration(10 * time.Minute),
			ExternalConsumption: &config.ExternalConsumption{Limit: "daily", RatePerHour: 1800},
		},
		{At: config.Duration(2 * time.Hour), ExternalConsumption: &config.ExternalConsumption{Limit: "daily", RatePerHour: 60}},
	}}}
	s, err := New(provider.Find(pkgs, "example-sim"), Options{Clock: clk, Scenario: sc})
	if err != nil {
		t.Fatal(err)
	}
	remaining := func() int {
		t.Helper()
		v, err := strconv.Atoi(call(s, "/api/v1/sites").Header().Get("X-RateLimit-Remaining-Day"))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		at   time.Duration
		want int // the requests of the test count too
	}{
		{0, 5000 - 1},
		{65 * time.Minute, 5000 - 2 - 150},  // halfway through the step
		{90 * time.Minute, 5000 - 3 - 300},  // the step ended between two requests
		{150 * time.Minute, 5000 - 4 - 330}, // 30 minutes of the open-ended step
		{4 * time.Hour, 5000 - 5 - 420},
	}
	for _, tt := range tests {
		clk.Set(midnight.Add(tt.at))
		if got := remaining(); got != tt.want {
			t.Errorf("remaining at %s = %d, want %d", tt.at, got, tt.want)
		}
	}
}

func TestInventoryChange(t *testing.T) {
	clk := clock.NewFake(midnight)
	s := exampleServer(t, clk, "inventory-change")
	list := func(path string) []string {
		t.Helper()
		rec := call(s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var items []struct {
			ID     string `json:"id"`
			Serial string `json:"serial"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.ID+it.Serial)
		}
		return out
	}
	// page pagination on the networks of a site, cursor on its devices.
	if got := append(list("/api/v1/sites/site-a/networks?page=1&size=2"), list("/api/v1/sites/site-a/networks?page=2&size=2")...); !slices.Equal(got, []string{"net-a1", "net-a2", "net-a3"}) {
		t.Errorf("site-a networks = %v", got)
	}
	rec := call(s, "/api/v1/sites/site-a/networks/net-a1/devices?limit=2")
	if next := rec.Header().Get(CursorHeader); next != "o2" {
		t.Errorf("next cursor = %q", next)
	}
	if got := list("/api/v1/sites/site-a/networks/net-a1/devices?limit=2&cursor=o2"); len(got) != 1 {
		t.Errorf("last device page = %v", got)
	}

	// From 2h, the scenario replaces the fixtures of the sites and networks.
	clk.Advance(2 * time.Hour)
	if got := list("/api/v1/sites?offset=0&limit=10"); !slices.Equal(got, []string{"site-a", "site-b", "site-d"}) {
		t.Errorf("sites after the change = %v", got)
	}
	if got := list("/api/v1/sites/site-b/networks?page=1&size=10"); !slices.Equal(got, []string{"net-b1", "net-b2"}) {
		t.Errorf("site-b networks after the change = %v", got)
	}
	if rec := call(s, "/api/v1/sites/site-c/networks"); rec.Code != http.StatusNotFound {
		t.Errorf("closed site: %d", rec.Code)
	}
}

func TestFaultActions(t *testing.T) {
	clk := clock.NewFake(midnight)
	pkgs, err := provider.Builtin(loader(t))
	if err != nil {
		t.Fatal(err)
	}
	sc := &config.Scenario{Metadata: config.ObjectMeta{Name: "faults"}, Spec: config.ScenarioSpec{Steps: []config.ScenarioStep{
		{At: config.Duration(time.Hour), For: config.Duration(time.Hour), Latency: &config.ScenarioLatency{
			P50: config.Duration(time.Second), P99: config.Duration(time.Second),
		}},
		{At: config.Duration(2 * time.Hour), For: config.Duration(time.Hour), Outage: true},
	}}}
	s, err := New(provider.Find(pkgs, "example-sim"), Options{Clock: clk, Scenario: sc})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	fetch := func() (int, error) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v1/sites", http.NoBody)
		req.Header.Set("Authorization", "Bearer sim-api-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	if status, err := fetch(); err != nil || status != http.StatusOK {
		t.Fatalf("before the steps: %d, %v", status, err)
	}

	// Latency: the response waits one simulated second (p50 = p99).
	clk.Advance(time.Hour)
	done := make(chan int)
	go func() {
		status, _ := fetch()
		done <- status
	}()
	// The handler waits on the fake clock: it cannot have answered yet.
	for clk.Waiters() == 0 {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("answered before the delay")
	default:
	}
	clk.Advance(time.Second)
	if status := <-done; status != http.StatusOK {
		t.Fatalf("after the delay: %d", status)
	}

	// Outage: the connection closes without a response.
	clk.Advance(time.Hour)
	if _, err := fetch(); err == nil {
		t.Fatal("no error during the outage")
	}
	if st := s.Stats(); st.Dropped == 0 { // the client may retry on a new connection
		t.Error("no connection dropped")
	}
	clk.Advance(time.Hour)
	if status, err := fetch(); err != nil || status != http.StatusOK {
		t.Fatalf("after the outage: %d, %v", status, err)
	}
}

// TestLatencyAfterLimits checks that a request is counted by the limits
// when it arrives, before the latency of the scenario (the processing time
// of the server). A client pacing its requests at the limit must not get a
// 429 because slow requests were counted late, together with the next
// ones: the RutOS simulator answered 429 to Tapyr under its quota-tight
// scenario (latency p50 1.5 s, p99 8 s) while Tapyr kept to 2 a second.
func TestLatencyAfterLimits(t *testing.T) {
	clk := clock.NewFake(midnight)
	pkgs, err := provider.Builtin(loader(t))
	if err != nil {
		t.Fatal(err)
	}
	// One second of latency for the requests that arrive within [1h, 1h+1s).
	sc := &config.Scenario{Metadata: config.ObjectMeta{Name: "slow"}, Spec: config.ScenarioSpec{Steps: []config.ScenarioStep{
		{At: config.Duration(time.Hour), For: config.Duration(time.Second), Latency: &config.ScenarioLatency{
			P50: config.Duration(time.Second), P99: config.Duration(time.Second),
		}},
	}}}
	s, err := New(provider.Find(pkgs, "example-sim"), Options{Clock: clk, Scenario: sc})
	if err != nil {
		t.Fatal(err)
	}

	// Within the limits of example-sim (10 a second, burst 10, 4 concurrent
	// requests), 4 requests sent during the latency step: their responses
	// wait one simulated second.
	clk.Advance(time.Hour + 900*time.Millisecond)
	slow := make(chan int, 4)
	for range 4 {
		go func() { slow <- call(s, "/api/v1/sites").Code }()
	}
	for clk.Waiters() < 4 {
		time.Sleep(time.Millisecond)
	}
	// One second later they are answered, and the bucket of a client that
	// spent 4 tokens a second ago is full again: 10 more requests, without
	// latency (the step is over).
	clk.Advance(time.Second)
	codes := map[int]int{}
	for range 4 {
		codes[<-slow]++
	}
	for range 10 {
		codes[call(s, "/api/v1/sites").Code]++
	}
	if codes[http.StatusOK] != 14 {
		t.Errorf("codes = %v, want 14 times 200: the slow requests were counted after their latency", codes)
	}
}

func TestTokenExpiry(t *testing.T) {
	clk := clock.NewFake(midnight)
	p := pkg(t, `{ type: session_login, sessionLogin: { request: { path: /login, body: { user: admin, pass: s3cr3t } },
	  token: response.body.token, apply: { header: Authorization, format: "Bearer {token}" } } }`,
		"  auth: { accept: { logins: [{ username: admin, password: s3cr3t }] }, tokenLifetime: 24h }\n  routes: [{ path: /orgs, response: { file: orgs.json } }]\n",
		map[string]string{"sim/orgs.json": `[]`})
	sc := &config.Scenario{Metadata: config.ObjectMeta{Name: "expiry"}, Spec: config.ScenarioSpec{Steps: []config.ScenarioStep{
		{At: config.Duration(time.Hour), TokenExpiry: true},
	}}}
	s, err := New(p, Options{Clock: clk, Scenario: sc})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	login := func() string {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/login",
			strings.NewReader(`{"user": "admin", "pass": "s3cr3t"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct{ Token string }
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Token
	}
	old := login()
	clk.Advance(time.Hour)
	if status, _, _ := get(t, srv.URL+"/orgs", old); status != http.StatusForbidden { // authFailed of the test provider
		t.Errorf("token issued before the step: %d, want 403", status)
	}
	if status, _, _ := get(t, srv.URL+"/orgs", login()); status != http.StatusOK {
		t.Errorf("token issued after the step: %d, want 200", status)
	}
}

func TestLatencyQuantiles(t *testing.T) {
	sc := &config.Scenario{Metadata: config.ObjectMeta{Name: "bad"}, Spec: config.ScenarioSpec{Steps: []config.ScenarioStep{
		{Latency: &config.ScenarioLatency{P50: config.Duration(time.Second), P99: config.Duration(time.Millisecond)}},
	}}}
	pkgs, err := provider.Builtin(loader(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(provider.Find(pkgs, "example-sim"), Options{Scenario: sc}); err == nil || !strings.Contains(err.Error(), "p99 is below p50") {
		t.Errorf("New() = %v", err)
	}
	s, err := New(provider.Find(pkgs, "example-sim"), Options{Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	var below int
	const n = 2000
	for range n {
		if s.latency(800*time.Millisecond, 5*time.Second) <= 5*time.Second {
			below++
		}
	}
	if ratio := float64(below) / n; ratio < 0.98 || ratio > 0.997 {
		t.Errorf("%.3f of the draws are below p99", ratio)
	}
}

// TestExternalConsumptionWindow is a regression test: the external
// consumption of a scenario was charged at the next request, all of it into
// the current window, so a minute window received everything consumed since
// the previous request. With the ThousandEyes quota-tight scenario (20
// requests a minute, 5 of them spent by another integration), a request every
// 5 minutes was answered 429: 25 calls landed in a window of 20.
func TestExternalConsumptionWindow(t *testing.T) {
	clk := clock.NewFake(midnight)
	pkgs, err := provider.Builtin(loader(t))
	if err != nil {
		t.Fatal(err)
	}
	pkg := provider.Find(pkgs, "cisco-thousandeyes")
	s, err := New(pkg, Options{Clock: clk, Scenario: pkg.Scenarios["quota-tight"]})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		// 30 seconds into a minute: 2.5 calls of the other integration in
		// this window, whatever the time since the previous request.
		clk.Set(midnight.Add(time.Duration(i)*5*time.Minute + 30*time.Second))
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://sim/v7/agents", http.NoBody)
		req.Header.Set("Authorization", "Bearer sim-thousandeyes-token")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d at %s: %d", i, clk.Now().Sub(midnight), rec.Code)
		}
		if got := rec.Header().Get("X-Organization-Rate-Limit-Remaining"); i > 0 && got != "17" {
			t.Errorf("request %d: remaining %s, want 17 (20, less this request and 2 external calls)", i, got)
		}
	}
}
