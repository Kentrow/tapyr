# 07 - Self-observability ("monitoring of monitoring")

Tapyr must answer, at any time: are my API calls healthy, how much quota is left, when will it run out, are my jobs fresh, are my outputs keeping up?

## Endpoints

| Path | Content |
|---|---|
| `/metrics` | Collected data (when a `prometheus` output is configured) |
| `/metrics/self` | Tapyr's own metrics (always available) |
| `/healthz` | Liveness: process alive, main loops running. 200 or 503 |
| `/readyz` | Readiness: config loaded, state store open, outputs initialized. 200 or 503 with JSON reasons |

Self-metrics can also be pushed: setting `selfMetrics.outputs: [name]` in `Settings` sends them through the given outputs (with `job="tapyr_self"` semantics), so Tapyr can be monitored even where nothing scrapes it.

## Label conventions

- `provider`, `account`, `target` (empty when not multi-target), `job` (job name, not instance), `endpoint` (templated path), `limit` (limit name), `scope_kind` (dimension names joined, e.g. `account,var:organizationId`), `output`.
- **Never** concrete IDs, serials, IPs or resolved URLs in self-metric labels. Per-scope-key detail (e.g. per organization) is available in the status API, not in metrics, except for quota scopes whose cardinality is bounded by configuration (account, target, credential): those expose a `scope` label with a non-sensitive key (`account=acme-meraki`).

## Metrics

### HTTP calls

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `tapyr_http_requests_total` | counter | provider, account, job, endpoint, code, class | |
| `tapyr_http_request_duration_seconds` | histogram | provider, account, endpoint | |
| `tapyr_http_response_size_bytes` | histogram | provider, endpoint | |
| `tapyr_http_retries_total` | counter | provider, account, job, reason | |
| `tapyr_http_in_flight` | gauge | provider, account | |

### Budget

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `tapyr_budget_limit` | gauge | provider, limit, scope | effective max for the window (after learning and share) |
| `tapyr_budget_used` | gauge | provider, limit, scope | consumed in the current window |
| `tapyr_budget_remaining` | gauge | provider, limit, scope | internal estimate |
| `tapyr_budget_remaining_observed` | gauge | provider, limit, scope | from headers, when available |
| `tapyr_budget_external_usage` | gauge | provider, limit, scope | estimated consumption by other clients in the window |
| `tapyr_budget_reserve_remaining` | gauge | provider, limit, scope | retry reserve left |
| `tapyr_budget_window_end_timestamp_seconds` | gauge | provider, limit, scope | |
| `tapyr_budget_exhaustion_eta_seconds` | gauge | provider, limit, scope | absent when no exhaustion is projected |
| `tapyr_budget_wait_seconds` | histogram | provider, limit | time spent waiting for rate or concurrency |
| `tapyr_budget_refusals_total` | counter | provider, account, job, reason | quota_exhausted, penalized, circuit_open, deadline |
| `tapyr_budget_rate_limited_total` | counter | provider, account, limit | 429 received |
| `tapyr_budget_unexpected_429_total` | counter | provider, account | 429 not predicted by any internal limit |
| `tapyr_budget_limit_mismatch` | gauge | provider, limit | 1 when headers disagree with the declared max |
| `tapyr_plan_feasible` | gauge | provider, limit, scope | 1 feasible, 0 infeasible |
| `tapyr_plan_recomputations_total` | counter | reason | start, reload, interval, limit_learned, inventory, projection (`04-budget-engine.md`, Replan on projection) |

### Jobs

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `tapyr_job_runs_total` | counter | provider, account, job, result | |
| `tapyr_job_run_duration_seconds` | histogram | provider, job | |
| `tapyr_job_last_success_timestamp_seconds` | gauge | provider, account, job | min across instances |
| `tapyr_job_desired_interval_seconds` | gauge | provider, account, job | |
| `tapyr_job_effective_interval_seconds` | gauge | provider, account, job | |
| `tapyr_job_lag_seconds` | gauge | provider, account, job | max delay vs schedule across instances |
| `tapyr_job_instances` | gauge | provider, account, job | fan-out size |
| `tapyr_job_series` | gauge | provider, account, job | series produced by the last runs |
| `tapyr_job_pages_per_run` | histogram | provider, job | |
| `tapyr_job_partial_runs_total` | counter | provider, account, job | |
| `tapyr_mapping_errors_total` | counter | provider, job, metric | |
| `tapyr_samples_deduplicated_total` | counter | provider, job | |

### Auth and resilience

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `tapyr_auth_token_requests_total` | counter | provider, account, result | |
| `tapyr_auth_token_expiry_timestamp_seconds` | gauge | provider, account | |
| `tapyr_auth_reauth_required` | gauge | provider, account | 1 when manual re-bootstrap is needed |
| `tapyr_circuit_state` | gauge | provider, account, target | 0 closed, 1 half-open, 2 open |

### Outputs

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `tapyr_output_samples_sent_total` | counter | output | |
| `tapyr_output_samples_dropped_total` | counter | output, reason | queue_full, rejected, out_of_order |
| `tapyr_output_send_failures_total` | counter | output, class | |
| `tapyr_output_queue_samples` | gauge | output | |
| `tapyr_output_send_duration_seconds` | histogram | output | |
| `tapyr_output_last_success_timestamp_seconds` | gauge | output | |

### Process and config

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `tapyr_build_info` | gauge | version, revision, goversion | |
| `tapyr_config_last_reload_success` | gauge | | |
| `tapyr_config_last_reload_timestamp_seconds` | gauge | | |
| `tapyr_state_flush_duration_seconds` | histogram | | |
| `tapyr_state_flush_failures_total` | counter | | |

Plus standard Go and process collectors.

## Logs

- `log/slog`, JSON by default (`text` for local development).
- Standard fields: `ts` (from the injected clock), `level`, `msg`, `component`, and when relevant `provider`, `account`, `target`, `job`, `instance`, `run_id`, `endpoint`, `status`, `duration_ms`, `reason`.
- Per-request logs at `debug` level only. `info` logs: lifecycle, config reloads, plan changes, circuit transitions, quota exhaustion, auth events.
- Repetitive warnings are rate-limited (same message and key at most once per minute, with a suppressed count).
- Redaction: headers `Authorization`, `Cookie`, `Set-Cookie`, `X-*-Key`, `X-*-Token` and any configured secret value are replaced by `[REDACTED]`. A test asserts that no configured secret value appears in any log line produced by the test suite.

## Implementation notes

- **Registry.** `client_golang` with a registry owned by `tapyr run`, never the global one. Counters and histograms belong to the process and survive reloads; gauges describing the running configuration (budgets, jobs, circuits, tokens, outputs) are computed at scrape time from the current generation, so a reload never leaves gauges of removed jobs behind.
- **Hooks.** The budget engine, the executor, the token manager and the outputs report events through small observer interfaces of their own packages; `internal/selfobs` implements them. Packages never import `client_golang` except `internal/selfobs`.
- **Budget scopes.** Quota gauges carry a `scope` label only for scopes whose dimensions are bounded by configuration (`instance`, `provider`, `account`, `target`, `credential`, `endpoint`; the credential appears as its keyed hash). For scopes partitioned by a variable, one series per limit carries the most constrained key (lowest remaining fraction) with `scope` set to the dimension names (`var:organizationId`), never the value; every key is in the status API.
- **Projection.** `tapyr_budget_exhaustion_eta_seconds` extrapolates the consumption rate since the start of a fixed window and is exposed only when the remaining budget runs out before the window ends. Rolling windows never end: at a steady rate their consumption stays level, so no exhaustion is projected for them.
- **Waits.** `tapyr_budget_wait_seconds` observes the time a granted reservation waited, labelled with the limit that held it last (`none` for penalties and priority waits).
- **Durations** are measured on the injected clock; histograms use the default buckets for seconds, powers of 4 from 256 bytes to 64 MiB for sizes and powers of 2 up to 1024 for pages.
- **Push.** `selfMetrics.outputs` gathers the registry every `selfMetrics.interval` (default 30s) and enqueues the result in the given outputs as the pseudo job instance `tapyr_self` (histograms as `_bucket`, `_sum` and `_count` counters). Self-metrics keep their own `job` label; no label is added. They bypass the series registry: no deduplication and no stale markers.
- **Health.** `/healthz` answers 503 when the flush loop (every 5 seconds) has not ticked for 30 seconds on the injected clock. `/readyz` answers 503 with `{"ready": false, "reasons": [...]}` until the state store answers a ping, the configuration is loaded and every output has started its sender loop. `conditions` lists the accounts whose credentials need a manual re-bootstrap (auth plugins), without failing the readiness: the other accounts keep collecting.
- **Redaction.** A `slog` handler wraps every logger of `tapyr run`: values of configured secrets and of tokens obtained by authenticators (4 characters or more; a shorter secret is reported at startup as too short to be redacted) are replaced by `[REDACTED]` in messages and string attributes, and attributes named after a redacted header are replaced whole. The CLI test helpers fail any test whose captured logs contain a secret of its environment.

## Shipped dashboards and alerts

- `dashboards/tapyr-overview.json`: providers health, budgets (used vs limit, projections, external usage), job freshness, errors, outputs.
- `docs/alerts.md` (and an example rules file in `examples/`): quota exhaustion projected before window end, unexpected 429s, job stale for 3x interval, circuit open, re-auth required, output dropping samples, plan infeasible.

- `dashboards/tapyr-overview.json` has rows Health (plan, failed runs, unexpected 429, open circuits, re-bootstrap conditions, version), Quotas (left, projected exhaustion, consumption against the limit with external usage), Jobs (runs by result, lag, desired and effective intervals), Provider calls (requests by status, retries, p95 durations and waits) and Outputs, with `datasource` and `provider` variables.
- The rules are in `examples/alerts/tapyr-alerts.yaml` (14 alerts, `severity` critical, warning or info), with promtool unit tests (`make alerts-check`). A Go test checks that every metric used by the dashboard and the rules is documented in the tables above, and that every alert has its section in `docs/alerts.md`.
- The self-metrics carry the `job` label of the Tapyr job: a scrape of `/metrics/self` needs `honor_labels: true`, or the label becomes `exported_job`.
