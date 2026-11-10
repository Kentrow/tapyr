package selfobs

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"

	"github.com/kentrow/tapyr/internal/buildinfo"
)

// Metrics holds Tapyr's own metrics (see "Metrics" in
// 07-self-observability.md). Counters and histograms belong to the process
// and survive reloads; the gauges describing the running configuration are
// computed at scrape time by a State function.
type Metrics struct {
	reg *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpSize     *prometheus.HistogramVec
	httpRetries  *prometheus.CounterVec
	httpInFlight *prometheus.GaugeVec

	budgetWait          *prometheus.HistogramVec
	budgetRefusals      *prometheus.CounterVec
	budgetRateLimited   *prometheus.CounterVec
	budgetUnexpected429 *prometheus.CounterVec
	planRecomputations  *prometheus.CounterVec

	jobRuns       *prometheus.CounterVec
	jobDuration   *prometheus.HistogramVec
	jobPages      *prometheus.HistogramVec
	jobPartial    *prometheus.CounterVec
	mappingErrors *prometheus.CounterVec
	deduplicated  *prometheus.CounterVec

	tokenRequests *prometheus.CounterVec

	outputSent     *prometheus.CounterVec
	outputDropped  *prometheus.CounterVec
	outputFailures *prometheus.CounterVec
	outputDuration *prometheus.HistogramVec

	reloadSuccess   prometheus.Gauge
	reloadTimestamp prometheus.Gauge
	flushDuration   prometheus.Histogram
	flushFailures   prometheus.Counter
}

// Histogram buckets.
var (
	sizeBuckets  = prometheus.ExponentialBuckets(256, 4, 10) // 256 B to 64 MiB
	pagesBuckets = prometheus.ExponentialBuckets(1, 2, 11)   // 1 to 1024
)

// NewMetrics returns the self-metrics with the Go and process collectors and
// tapyr_build_info. state, when not nil, provides the scrape-time gauges.
func NewMetrics(info buildinfo.Info, state func() State) *Metrics {
	reg := prometheus.NewRegistry()
	f := factory{reg}
	m := &Metrics{
		reg: reg,
		httpRequests: f.counter("tapyr_http_requests_total", "Requests sent to providers, by response code and error class.",
			"provider", "account", "job", "endpoint", "code", "class"),
		httpDuration: f.histogram("tapyr_http_request_duration_seconds", "Duration of the requests sent to providers.",
			prometheus.DefBuckets, "provider", "account", "endpoint"),
		httpSize: f.histogram("tapyr_http_response_size_bytes", "Size of the response bodies received from providers.",
			sizeBuckets, "provider", "endpoint"),
		httpRetries:  f.counter("tapyr_http_retries_total", "Requests retried, by reason.", "provider", "account", "job", "reason"),
		httpInFlight: f.gauge("tapyr_http_in_flight", "Requests to providers in flight.", "provider", "account"),

		budgetWait: f.histogram("tapyr_budget_wait_seconds", "Time granted reservations waited for rate, concurrency or penalties.",
			prometheus.DefBuckets, "provider", "limit"),
		budgetRefusals: f.counter("tapyr_budget_refusals_total", "Reservations refused by the budget engine, by reason.",
			"provider", "account", "job", "reason"),
		budgetRateLimited: f.counter("tapyr_budget_rate_limited_total", "Rate-limited responses (429) received.",
			"provider", "account", "limit"),
		budgetUnexpected429: f.counter("tapyr_budget_unexpected_429_total", "Rate-limited responses not predicted by any internal limit.",
			"provider", "account"),
		planRecomputations: f.counter("tapyr_plan_recomputations_total", "Plan recomputations, by reason.", "reason"),

		jobRuns:       f.counter("tapyr_job_runs_total", "Job runs, by result.", "provider", "account", "job", "result"),
		jobDuration:   f.histogram("tapyr_job_run_duration_seconds", "Duration of job runs.", prometheus.DefBuckets, "provider", "job"),
		jobPages:      f.histogram("tapyr_job_pages_per_run", "Pages fetched by successful job runs.", pagesBuckets, "provider", "job"),
		jobPartial:    f.counter("tapyr_job_partial_runs_total", "Job runs stopped by maxPages.", "provider", "account", "job"),
		mappingErrors: f.counter("tapyr_mapping_errors_total", "Samples skipped because of a mapping error.", "provider", "job", "metric"),
		deduplicated: f.counter("tapyr_samples_deduplicated_total", "Samples dropped because their source timestamp was already sent.",
			"provider", "job"),

		tokenRequests: f.counter("tapyr_auth_token_requests_total", "Token and login requests, by result.", "provider", "account", "result"),

		outputSent:     f.counter("tapyr_output_samples_sent_total", "Samples accepted by the output backend.", "output"),
		outputDropped:  f.counter("tapyr_output_samples_dropped_total", "Samples dropped by an output, by reason.", "output", "reason"),
		outputFailures: f.counter("tapyr_output_send_failures_total", "Failed send requests of an output, by class.", "output", "class"),
		outputDuration: f.histogram("tapyr_output_send_duration_seconds", "Duration of the send requests of an output.",
			prometheus.DefBuckets, "output"),

		reloadSuccess: f.plainGauge("tapyr_config_last_reload_success",
			"1 when the last configuration load or reload succeeded."),
		reloadTimestamp: f.plainGauge("tapyr_config_last_reload_timestamp_seconds",
			"Time of the last configuration load or reload attempt."),
	}
	m.flushDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "tapyr_state_flush_duration_seconds", Help: "Duration of the state store flushes.", Buckets: prometheus.DefBuckets,
	})
	m.flushFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tapyr_state_flush_failures_total", Help: "State store flushes that failed.",
	})
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tapyr_build_info", Help: "Build information of the running binary.",
		ConstLabels: prometheus.Labels{"version": info.Version, "revision": info.Revision, "goversion": info.GoVersion},
	})
	build.Set(1)
	reg.MustRegister(m.flushDuration, m.flushFailures, build,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if state != nil {
		reg.MustRegister(&stateCollector{state: state})
	}
	return m
}

type factory struct{ reg *prometheus.Registry }

func (f factory) counter(name, help string, labels ...string) *prometheus.CounterVec {
	v := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	f.reg.MustRegister(v)
	return v
}

func (f factory) gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	v := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	f.reg.MustRegister(v)
	return v
}

func (f factory) plainGauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	f.reg.MustRegister(g)
	return g
}

func (f factory) histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	v := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	f.reg.MustRegister(v)
	return v
}

// Handler serves the self-metrics (/metrics/self), in the Prometheus text
// format or OpenMetrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// Gather returns the current self-metrics, for the push to outputs.
func (m *Metrics) Gather() ([]*dto.MetricFamily, error) { return m.reg.Gather() }

// Request records a request sent to a provider. status is 0 when no
// response was received; class is empty for a successful response; size is
// -1 when unknown.
func (m *Metrics) Request(provider, account, job, endpoint string, status int, class string, d time.Duration, size int) {
	code := "none"
	if status != 0 {
		code = strconv.Itoa(status)
	}
	if class == "" {
		class = "success"
	}
	m.httpRequests.WithLabelValues(provider, account, job, endpoint, code, class).Inc()
	m.httpDuration.WithLabelValues(provider, account, endpoint).Observe(d.Seconds())
	if status != 0 && size >= 0 {
		m.httpSize.WithLabelValues(provider, endpoint).Observe(float64(size))
	}
}

// Retry records a retried request.
func (m *Metrics) Retry(provider, account, job, reason string) {
	m.httpRetries.WithLabelValues(provider, account, job, reason).Inc()
}

// InFlight adds delta to the requests in flight.
func (m *Metrics) InFlight(provider, account string, delta int) {
	m.httpInFlight.WithLabelValues(provider, account).Add(float64(delta))
}

// BudgetGranted records the wait of a granted reservation.
func (m *Metrics) BudgetGranted(provider, limit string, wait time.Duration) {
	m.budgetWait.WithLabelValues(provider, limit).Observe(wait.Seconds())
}

// BudgetRefused records a refused reservation.
func (m *Metrics) BudgetRefused(provider, account, job, reason string) {
	m.budgetRefusals.WithLabelValues(provider, account, job, reason).Inc()
}

// PlanRecomputed records a recomputation of the plan: reason is start,
// reload, interval, limit_learned, inventory or projection.
func (m *Metrics) PlanRecomputed(reason string) {
	m.planRecomputations.WithLabelValues(reason).Inc()
}

// RateLimited records a rate-limited response, penalizing limit.
func (m *Metrics) RateLimited(provider, account, limit string, unexpected bool) {
	m.budgetRateLimited.WithLabelValues(provider, account, limit).Inc()
	if unexpected {
		m.budgetUnexpected429.WithLabelValues(provider, account).Inc()
	}
}

// TokenRequest records a token or login request.
func (m *Metrics) TokenRequest(provider, account, result string) {
	m.tokenRequests.WithLabelValues(provider, account, result).Inc()
}

// Run describes a finished job run.
type Run struct {
	Provider, Account, Job string
	// Result is success, failed, skipped_budget or skipped_circuit.
	Result   string
	Duration time.Duration
	// Pages is observed for successful runs.
	Pages     int
	Truncated bool
	// MappingErrors counts the skipped samples per metric name.
	MappingErrors map[string]int
	Deduplicated  int
}

// RunFinished records a job run.
func (m *Metrics) RunFinished(r Run) {
	m.jobRuns.WithLabelValues(r.Provider, r.Account, r.Job, r.Result).Inc()
	m.jobDuration.WithLabelValues(r.Provider, r.Job).Observe(r.Duration.Seconds())
	if r.Result != "success" {
		return
	}
	m.jobPages.WithLabelValues(r.Provider, r.Job).Observe(float64(r.Pages))
	if r.Truncated {
		m.jobPartial.WithLabelValues(r.Provider, r.Account, r.Job).Inc()
	}
	for metric, n := range r.MappingErrors {
		m.mappingErrors.WithLabelValues(r.Provider, r.Job, metric).Add(float64(n))
	}
	if r.Deduplicated > 0 {
		m.deduplicated.WithLabelValues(r.Provider, r.Job).Add(float64(r.Deduplicated))
	}
}

// OutputSent records samples accepted by an output backend.
func (m *Metrics) OutputSent(output string, n int) {
	m.outputSent.WithLabelValues(output).Add(float64(n))
}

// OutputDropped records samples dropped by an output.
func (m *Metrics) OutputDropped(output, reason string, n int) {
	m.outputDropped.WithLabelValues(output, reason).Add(float64(n))
}

// OutputSendFailed records a failed send request.
func (m *Metrics) OutputSendFailed(output, class string) {
	m.outputFailures.WithLabelValues(output, class).Inc()
}

// OutputSendDuration records the duration of a send request.
func (m *Metrics) OutputSendDuration(output string, d time.Duration) {
	m.outputDuration.WithLabelValues(output).Observe(d.Seconds())
}

// ConfigLoaded records a configuration load or reload attempt.
func (m *Metrics) ConfigLoaded(at time.Time, ok bool) {
	m.reloadTimestamp.Set(float64(at.UnixMilli()) / 1e3)
	if ok {
		m.reloadSuccess.Set(1)
	} else {
		m.reloadSuccess.Set(0)
	}
}

// StateFlushed records a state store flush.
func (m *Metrics) StateFlushed(d time.Duration, err error) {
	m.flushDuration.Observe(d.Seconds())
	if err != nil {
		m.flushFailures.Inc()
	}
}
