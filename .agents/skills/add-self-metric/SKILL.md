---
name: add-self-metric
description: Add, rename or change a Tapyr self-metric (tapyr_* on /metrics/self) together with everything that depends on it - the spec table, the implementation in internal/selfobs, tests, the tapyr-overview dashboard, the example alert rules and docs/alerts.md. Use when a change needs Tapyr to expose something new about itself. Not for metrics collected from provider APIs (those are provider package mappings).
---

# Add a self-metric

Self-metrics are Tapyr's view of itself: calls, budgets, jobs, auth, circuits, outputs, state. Dashboards and alerts of users depend on their names and labels, so they are designed in the spec first and changed with care.

## 1. Design it in the spec

Add a row to the right table of [`07-self-observability.md`](../../../docs/spec/07-self-observability.md) (name, type, labels, notes). Rules:

- Name `tapyr_<area>_<what>_<unit>`: base units (`_seconds`, `_bytes`), counters end in `_total`, timestamps in `_timestamp_seconds`.
- **Bounded labels only**: provider, account, target, job, output, limit, scope, result, reason, class, templated endpoint (`/organizations/{organizationId}/networks`). Never a resolved URL, an ID, a serial, an IP, an error message or a token.
- A gauge that describes the running configuration is computed at scrape time from the state (`internal/selfobs/state.go`); an event (a request, a run, a refusal) is a counter or a histogram updated when it happens (`internal/selfobs/metrics.go`).
- Renaming or removing a metric, or changing its labels, breaks dashboards and alerts of users: it is a breaking change (`!` in the commit, `Changed` or `Removed` in the changelog), avoided before a good reason.

## 2. Implement it

- **Event metrics**: a field and its registration in `NewMetrics` (`internal/selfobs/metrics.go`), and a method called by the component through the observer interface it already receives (budget, collect, output...). Components never import Prometheus types.
- **State gauges**: a `prometheus.Desc` in `internal/selfobs/state.go`, emitted in `stateCollector.Collect` from the `State` returned by `(*Collector).selfState` (`internal/collector/selfmetrics.go`); add the field to `State` if needed.
- Time comes from the injected clock; labels from the configuration and the descriptor, never from a response body.

## 3. Test it

- `internal/selfobs/metrics_test.go`: `TestMetricsEvents` (events) or `TestStateGauges` (gauges) asserts the exposition with its labels.
- The conversion of the state into gauges: `internal/collector/selfmetrics_test.go`. When the value depends on a real run, an end-to-end check in `cmd/tapyr/selfmetrics_acceptance_test.go` against the simulator.
- Redaction: if a label value could come from user input, prove it cannot hold a secret.

## 4. Dashboard and alerts

- [`dashboards/tapyr-overview.json`](../../../dashboards/tapyr-overview.json): add or update the panel that shows it, keeping the `datasource` and `provider` variables.
- [`examples/alerts/tapyr-alerts.yaml`](../../../examples/alerts/tapyr-alerts.yaml): when the metric signals something to act on, an alert with `severity`, a `summary` and a `description`; a unit test in `tapyr-alerts.test.yaml`; a section `### <AlertName>` in [`docs/alerts.md`](../../../docs/alerts.md) saying what it means and what to do.
- A renamed metric is renamed everywhere: `grep -rn <old_name> dashboards examples docs providers`.

## 5. Check

```sh
go test ./internal/selfobs/ ./internal/collector/ ./cmd/tapyr -run 'Metrics|Gauges|SelfMetrics|OverviewDashboard|ExampleAlerts'
make alerts-check    # promtool (Docker)
```

`TestOverviewDashboard` and `TestExampleAlerts` fail when the dashboard or the rules use a metric missing from the spec tables, or when an alert has no section in `docs/alerts.md`. Then run the [`check-change`](../check-change/SKILL.md) skill.
