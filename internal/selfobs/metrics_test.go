package selfobs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kentrow/tapyr/internal/buildinfo"
)

var at = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

func TestMetricsEvents(t *testing.T) {
	m := NewMetrics(buildinfo.Info{Version: "1.2.3", Revision: "abc", GoVersion: "go1.27"}, nil)
	m.Request("meraki", "acme", "devices", "/organizations/{organizationId}/devices", 200, "", 120*time.Millisecond, 2048)
	m.Request("meraki", "acme", "devices", "/organizations/{organizationId}/devices", 0, "transient", time.Second, 0)
	m.Request("meraki", "acme", "", "/oauth/token", 200, "", 10*time.Millisecond, -1)
	m.Retry("meraki", "acme", "devices", "transient")
	m.InFlight("meraki", "acme", 1)
	m.InFlight("meraki", "acme", 1)
	m.InFlight("meraki", "acme", -1)
	m.BudgetGranted("meraki", "rate", 2*time.Second)
	m.BudgetRefused("meraki", "acme", "devices", "quota_exhausted")
	m.RateLimited("meraki", "acme", "rate", true)
	m.RateLimited("meraki", "acme", "rate", false)
	m.TokenRequest("meraki", "acme", "success")
	m.RunFinished(Run{
		Provider: "meraki", Account: "acme", Job: "devices", Result: "success", Duration: 3 * time.Second, Pages: 3,
		Truncated: true, MappingErrors: map[string]int{"meraki_device_up": 2}, Deduplicated: 4,
	})
	m.RunFinished(Run{Provider: "meraki", Account: "acme", Job: "devices", Result: "failed", Duration: time.Second, Pages: 9})
	m.OutputSent("vm", 10)
	m.OutputDropped("vm", "queue_full", 3)
	m.OutputFiltered("vm", 7)
	m.OutputSendFailed("vm", "transient")
	m.OutputSendDuration("vm", 50*time.Millisecond)
	m.ConfigLoaded(at, false)
	m.PlanRecomputed("start")
	m.PlanRecomputed("projection")
	m.PlanRecomputed("projection")
	m.StateFlushed(time.Millisecond, nil)
	m.StateFlushed(time.Millisecond, errors.New("disk full"))

	want := `
# HELP tapyr_auth_token_requests_total Token and login requests, by result.
# TYPE tapyr_auth_token_requests_total counter
tapyr_auth_token_requests_total{account="acme",provider="meraki",result="success"} 1
# HELP tapyr_budget_rate_limited_total Rate-limited responses (429) received.
# TYPE tapyr_budget_rate_limited_total counter
tapyr_budget_rate_limited_total{account="acme",limit="rate",provider="meraki"} 2
# HELP tapyr_budget_refusals_total Reservations refused by the budget engine, by reason.
# TYPE tapyr_budget_refusals_total counter
tapyr_budget_refusals_total{account="acme",job="devices",provider="meraki",reason="quota_exhausted"} 1
# HELP tapyr_budget_unexpected_429_total Rate-limited responses not predicted by any internal limit.
# TYPE tapyr_budget_unexpected_429_total counter
tapyr_budget_unexpected_429_total{account="acme",provider="meraki"} 1
# HELP tapyr_build_info Build information of the running binary.
# TYPE tapyr_build_info gauge
tapyr_build_info{goversion="go1.27",revision="abc",version="1.2.3"} 1
# HELP tapyr_config_last_reload_success 1 when the last configuration load or reload succeeded.
# TYPE tapyr_config_last_reload_success gauge
tapyr_config_last_reload_success 0
# HELP tapyr_config_last_reload_timestamp_seconds Time of the last configuration load or reload attempt.
# TYPE tapyr_config_last_reload_timestamp_seconds gauge
tapyr_config_last_reload_timestamp_seconds 1.7937072e+09
# HELP tapyr_http_in_flight Requests to providers in flight.
# TYPE tapyr_http_in_flight gauge
tapyr_http_in_flight{account="acme",provider="meraki"} 1
# HELP tapyr_http_requests_total Requests sent to providers, by response code and error class.
# TYPE tapyr_http_requests_total counter
tapyr_http_requests_total{account="acme",class="success",code="200",endpoint="/oauth/token",job="",provider="meraki"} 1
tapyr_http_requests_total{account="acme",class="success",code="200",endpoint="/organizations/{organizationId}/devices",job="devices",provider="meraki"} 1
tapyr_http_requests_total{account="acme",class="transient",code="none",endpoint="/organizations/{organizationId}/devices",job="devices",provider="meraki"} 1
# HELP tapyr_http_retries_total Requests retried, by reason.
# TYPE tapyr_http_retries_total counter
tapyr_http_retries_total{account="acme",job="devices",provider="meraki",reason="transient"} 1
# HELP tapyr_job_partial_runs_total Job runs stopped by maxPages.
# TYPE tapyr_job_partial_runs_total counter
tapyr_job_partial_runs_total{account="acme",job="devices",provider="meraki"} 1
# HELP tapyr_job_runs_total Job runs, by result.
# TYPE tapyr_job_runs_total counter
tapyr_job_runs_total{account="acme",job="devices",provider="meraki",result="failed"} 1
tapyr_job_runs_total{account="acme",job="devices",provider="meraki",result="success"} 1
# HELP tapyr_mapping_errors_total Samples skipped because of a mapping error.
# TYPE tapyr_mapping_errors_total counter
tapyr_mapping_errors_total{job="devices",metric="meraki_device_up",provider="meraki"} 2
# HELP tapyr_output_samples_dropped_total Samples dropped by an output, by reason.
# TYPE tapyr_output_samples_dropped_total counter
tapyr_output_samples_dropped_total{output="vm",reason="queue_full"} 3
# HELP tapyr_output_samples_sent_total Samples accepted by the output backend.
# TYPE tapyr_output_samples_sent_total counter
tapyr_output_samples_sent_total{output="vm"} 10
# HELP tapyr_output_samples_filtered_total Samples removed by the keep and drop processing rules of an output.
# TYPE tapyr_output_samples_filtered_total counter
tapyr_output_samples_filtered_total{output="vm"} 7
# HELP tapyr_output_send_failures_total Failed send requests of an output, by class.
# TYPE tapyr_output_send_failures_total counter
tapyr_output_send_failures_total{class="transient",output="vm"} 1
# HELP tapyr_plan_recomputations_total Plan recomputations, by reason.
# TYPE tapyr_plan_recomputations_total counter
tapyr_plan_recomputations_total{reason="projection"} 2
tapyr_plan_recomputations_total{reason="start"} 1
# HELP tapyr_samples_deduplicated_total Samples dropped because their source timestamp was already sent.
# TYPE tapyr_samples_deduplicated_total counter
tapyr_samples_deduplicated_total{job="devices",provider="meraki"} 4
# HELP tapyr_state_flush_failures_total State store flushes that failed.
# TYPE tapyr_state_flush_failures_total counter
tapyr_state_flush_failures_total 1
`
	names := []string{
		"tapyr_auth_token_requests_total", "tapyr_budget_rate_limited_total", "tapyr_budget_refusals_total",
		"tapyr_budget_unexpected_429_total", "tapyr_build_info", "tapyr_config_last_reload_success",
		"tapyr_config_last_reload_timestamp_seconds", "tapyr_http_in_flight", "tapyr_http_requests_total",
		"tapyr_http_retries_total", "tapyr_job_partial_runs_total", "tapyr_job_runs_total", "tapyr_mapping_errors_total",
		"tapyr_output_samples_dropped_total", "tapyr_output_samples_filtered_total", "tapyr_output_samples_sent_total",
		"tapyr_output_send_failures_total",
		"tapyr_plan_recomputations_total", "tapyr_samples_deduplicated_total", "tapyr_state_flush_failures_total",
	}
	if err := testutil.GatherAndCompare(m.reg, strings.NewReader(want), names...); err != nil {
		t.Error(err)
	}
	// Histograms: one series per endpoint (sizes only with a known size), per
	// successful run for pages.
	for name, want := range map[string]int{
		"tapyr_http_request_duration_seconds": 2, "tapyr_http_response_size_bytes": 1, "tapyr_budget_wait_seconds": 1,
		"tapyr_job_run_duration_seconds": 1, "tapyr_job_pages_per_run": 1, "tapyr_output_send_duration_seconds": 1,
		"tapyr_state_flush_duration_seconds": 1,
	} {
		if got, err := testutil.GatherAndCount(m.reg, name); err != nil || got != want {
			t.Errorf("%s: %d series (%v), want %d", name, got, err, want)
		}
	}
	m.ConfigLoaded(at, true)
	if v := testutil.ToFloat64(m.reloadSuccess); v != 1 {
		t.Errorf("reload success = %v after a successful reload", v)
	}
}

func TestMetricsHandler(t *testing.T) {
	m := NewMetrics(buildinfo.Get(), nil)
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	for accept, wantType := range map[string]string{
		"": "text/plain",
		"application/openmetrics-text;version=1.0.0": "application/openmetrics-text",
	} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), wantType) {
			t.Errorf("Accept %q: Content-Type %q", accept, resp.Header.Get("Content-Type"))
		}
		for _, s := range []string{"tapyr_build_info", "go_goroutines", "process_"} {
			if !strings.Contains(string(body), s) {
				t.Errorf("Accept %q: %s missing", accept, s)
			}
		}
	}
	if families, err := m.Gather(); err != nil || len(families) == 0 {
		t.Errorf("Gather: %d families, %v", len(families), err)
	}
}

func TestStateGauges(t *testing.T) {
	state := State{
		Quotas: []QuotaGauge{
			{
				Provider: "meraki", Limit: "daily", Scope: "account=acme", Max: 1000, Used: 400, Remaining: 600,
				ObservedRemaining: 590, ExternalUsage: 10, ReserveRemaining: 100, WindowEnd: at, ExhaustionETA: time.Hour,
			},
			{Provider: "meraki", Limit: "daily", Scope: "account=beta", Max: 1000, ObservedRemaining: -1, WindowEnd: at, Mismatch: true},
		},
		Plan: []PlanGauge{{Provider: "meraki", Limit: "daily", Scope: "account=acme", Feasible: true}},
		Jobs: []JobGauge{
			{
				Provider: "meraki", Account: "acme", Job: "devices", LastSuccess: at, Desired: time.Minute,
				Effective: 2 * time.Minute, Lag: time.Second, Instances: 3, Series: 12,
			},
			{Provider: "meraki", Account: "acme", Job: "clients"},
		},
		Tokens:   []TokenGauge{{Provider: "aruba", Account: "lab", Expiry: at}, {Provider: "aruba", Account: "lab2", ReauthRequired: true}},
		Circuits: []CircuitGauge{{Provider: "meraki", Account: "acme", State: 2}},
		Outputs:  []OutputGauge{{Output: "vm", Queue: 5, LastSuccess: at}, {Output: "prom"}},
	}
	m := NewMetrics(buildinfo.Get(), func() State { return state })
	want := `
# HELP tapyr_auth_reauth_required 1 when the account needs a manual re-bootstrap.
# TYPE tapyr_auth_reauth_required gauge
tapyr_auth_reauth_required{account="lab",provider="aruba"} 0
tapyr_auth_reauth_required{account="lab2",provider="aruba"} 1
# HELP tapyr_auth_token_expiry_timestamp_seconds Expiry of the token held for the account.
# TYPE tapyr_auth_token_expiry_timestamp_seconds gauge
tapyr_auth_token_expiry_timestamp_seconds{account="lab",provider="aruba"} 1.7937072e+09
# HELP tapyr_budget_exhaustion_eta_seconds Projected time to quota exhaustion, when it comes before the window ends.
# TYPE tapyr_budget_exhaustion_eta_seconds gauge
tapyr_budget_exhaustion_eta_seconds{limit="daily",provider="meraki",scope="account=acme"} 3600
# HELP tapyr_budget_limit Effective max of the quota window, after learning and share.
# TYPE tapyr_budget_limit gauge
tapyr_budget_limit{limit="daily",provider="meraki",scope="account=acme"} 1000
tapyr_budget_limit{limit="daily",provider="meraki",scope="account=beta"} 1000
# HELP tapyr_budget_limit_mismatch 1 when the quota headers disagree with the declared max.
# TYPE tapyr_budget_limit_mismatch gauge
tapyr_budget_limit_mismatch{limit="daily",provider="meraki"} 1
# HELP tapyr_budget_remaining_observed Remaining quota reported by the provider headers.
# TYPE tapyr_budget_remaining_observed gauge
tapyr_budget_remaining_observed{limit="daily",provider="meraki",scope="account=acme"} 590
# HELP tapyr_circuit_state Circuit breaker state: 0 closed, 1 half-open, 2 open.
# TYPE tapyr_circuit_state gauge
tapyr_circuit_state{account="acme",provider="meraki",target=""} 2
# HELP tapyr_job_effective_interval_seconds Interval applied by the planner.
# TYPE tapyr_job_effective_interval_seconds gauge
tapyr_job_effective_interval_seconds{account="acme",job="clients",provider="meraki"} 0
tapyr_job_effective_interval_seconds{account="acme",job="devices",provider="meraki"} 120
# HELP tapyr_job_last_success_timestamp_seconds Oldest last successful run across the instances.
# TYPE tapyr_job_last_success_timestamp_seconds gauge
tapyr_job_last_success_timestamp_seconds{account="acme",job="devices",provider="meraki"} 1.7937072e+09
# HELP tapyr_job_instances Scheduled instances of the job (fan-out size).
# TYPE tapyr_job_instances gauge
tapyr_job_instances{account="acme",job="clients",provider="meraki"} 0
tapyr_job_instances{account="acme",job="devices",provider="meraki"} 3
# HELP tapyr_output_last_success_timestamp_seconds Time of the last successful send.
# TYPE tapyr_output_last_success_timestamp_seconds gauge
tapyr_output_last_success_timestamp_seconds{output="vm"} 1.7937072e+09
# HELP tapyr_output_queue_samples Samples waiting in the output queue.
# TYPE tapyr_output_queue_samples gauge
tapyr_output_queue_samples{output="prom"} 0
tapyr_output_queue_samples{output="vm"} 5
# HELP tapyr_plan_feasible 1 when the critical jobs fit in the remaining quota.
# TYPE tapyr_plan_feasible gauge
tapyr_plan_feasible{limit="daily",provider="meraki",scope="account=acme"} 1
`
	names := []string{
		"tapyr_auth_reauth_required", "tapyr_auth_token_expiry_timestamp_seconds", "tapyr_budget_exhaustion_eta_seconds",
		"tapyr_budget_limit", "tapyr_budget_limit_mismatch", "tapyr_budget_remaining_observed", "tapyr_circuit_state",
		"tapyr_job_effective_interval_seconds", "tapyr_job_last_success_timestamp_seconds", "tapyr_job_instances",
		"tapyr_output_last_success_timestamp_seconds", "tapyr_output_queue_samples", "tapyr_plan_feasible",
	}
	if err := testutil.GatherAndCompare(m.reg, strings.NewReader(want), names...); err != nil {
		t.Error(err)
	}
	for name, want := range map[string]int{
		"tapyr_budget_used": 2, "tapyr_budget_remaining": 2, "tapyr_budget_external_usage": 2, "tapyr_budget_reserve_remaining": 2,
		"tapyr_budget_window_end_timestamp_seconds": 2, "tapyr_job_desired_interval_seconds": 2, "tapyr_job_lag_seconds": 2,
		"tapyr_job_series": 2,
	} {
		if got, err := testutil.GatherAndCount(m.reg, name); err != nil || got != want {
			t.Errorf("%s: %d series (%v), want %d", name, got, err, want)
		}
	}
	if problems, err := testutil.GatherAndLint(m.reg); err != nil || len(problems) > 0 {
		t.Errorf("lint: %v %+v", err, problems)
	}
}
