# Alerts

Tapyr exposes its own metrics on `/metrics/self` (see [`07-self-observability.md`](spec/07-self-observability.md)). The example rules in [`examples/alerts/tapyr-alerts.yaml`](../examples/alerts/tapyr-alerts.yaml) turn them into alerts for Prometheus or vmalert. They are starting points: copy them, then tune the thresholds and durations to your job intervals and quotas.

```yaml
# prometheus.yml
rule_files:
  - /etc/prometheus/rules/tapyr-alerts.yaml
scrape_configs:
  - job_name: tapyr
    metrics_path: /metrics/self
    honor_labels: true   # the self-metrics carry the job label of the Tapyr job
    static_configs:
      - targets: ["tapyr:9790"]
```

`honor_labels: true` matters: without it, the scrape renames the `job` label of the self-metrics (the name of the Tapyr job) to `exported_job`, and the alerts lose it. `TapyrDown` expects the scrape job to be named `tapyr`.

The rules are checked and unit tested with promtool (`make alerts-check`, in CI). A Go test checks that every metric they use is a documented self-metric and that every alert has its section below.

Severities: `critical` needs someone now (collection stopped or about to), `warning` needs someone today, `info` is worth a look.

## Collection

### TapyrDown

Nothing answered the scrape of `/metrics/self` for 5 minutes: the process is down, restarting in a loop, or unreachable. Check the logs of the container or service (`tapyr run` logs as JSON on stderr) and `GET /healthz`.

### TapyrReauthRequired

The credentials of an account can no longer be renewed (for example an Aruba Central refresh token that expired or was revoked), so its jobs stopped. Generate new credentials for the account (for Aruba, a new token pair under API Gateway > System Apps & Tokens, or My Apps & Tokens), update the secret, and reload the configuration (`SIGHUP`, or `docker kill -s HUP <container>`). The overview of the UI and `GET /api/v1/accounts` show the account concerned.

### TapyrJobStale

A job has not succeeded for three of its effective intervals. Open the job in the UI (Jobs) or `GET /api/v1/jobs/{name}/runs` for the errors of its last runs: authentication (`401`, `403`), a changed API (mapping errors), a provider outage (open circuit), or a quota that pauses it (see the quota alerts).

### TapyrJobLagging

The runs of a job start more than one interval after their schedule: requests wait for rate or concurrency limits, or the job takes longer than its interval. `tapyr_budget_wait_seconds` and the Provider calls row of the `tapyr-overview` dashboard show where the time goes. Raise the interval of the job or of the jobs that share the limit.

### TapyrCircuitOpen

Repeated failures opened the circuit of an account or a target: Tapyr stopped calling it and probes again on its own after the cooldown. The alert lasts while the provider (or the device, for RutOS) keeps failing. Check that the endpoint is reachable from Tapyr and that the provider status page shows no incident.

## Quotas

### TapyrQuotaAlmostExhausted

Less than 5% of a quota is left before its reset. The planner already pauses the low-priority jobs; critical jobs stop when the quota is spent. If Tapyr is the only client, the plan was too optimistic: check `tapyr plan`. Otherwise another client consumes the same quota (`tapyr_budget_external_usage`).

### TapyrQuotaExhaustionProjected

At the current pace, a quota runs out within 6 hours, before its reset. The usual cause is another client using the same credentials or organization: its consumption shows as external usage in the Budgets page of the UI. Lower the priority or raise the interval of jobs, or give Tapyr its own credentials.

### TapyrPlanInfeasible

The critical jobs do not fit a quota at their desired interval, even with every other job paused. `tapyr plan` (or the Plan page) lists the jobs and their consumption: raise intervals, reduce the fan-out (`forEach.maxInstances`, filters), or ask the provider for a larger quota.

### TapyrUnexpected429

A provider returned `429 Too Many Requests` that no declared limit predicted. Either the provider definition misses a limit (or declares it too high), or another client shares the credentials. Tapyr honors `Retry-After` and backs off, but the definition should be fixed: open an issue with the provider, the endpoint and the headers of the response (the debug endpoint shows them, secrets redacted).

### TapyrLimitMismatch

The quota headers of a provider announce a maximum different from the one declared in its definition. Tapyr follows the headers; update the limit in the provider definition (`max: auto` with `initialMax` when the maximum depends on the account) so that `tapyr plan` is accurate.

## Outputs and state

### TapyrOutputDropping

A push output (remote write or OTLP) drops samples: `queue_full` when the backend is slower than the collection, `rejected` or `out_of_order` when it refuses them. `tapyr_output_send_failures_total` gives the class of the errors; for `out_of_order`, check that no other writer sends the same series. `duplicate` means that processing rules made two series identical. `series_limit` means that the output reached its `limits.maxSeries`: the series already sent keep flowing, new ones are not sent; the Outputs page of the web UI says how many are held back. Raise the limit, or drop metrics with processing rules ([Series and limits](outputs.md#series-and-limits)). With a disk buffer, `buffer_full` means that it reached its `maxSize` during a long outage, and `too_old` that samples waited longer than its `maxAge` ([Keeping the queue on disk](outputs.md#keeping-the-queue-on-disk)).

### TapyrOutputFailing

A push output has not delivered anything for 10 minutes, or keeps failing without having ever delivered (a wrong URL or credentials from the start). The samples wait in the bounded queue, and the oldest are dropped when it is full. Check that the backend is up and reachable, and its authentication.

### TapyrOutputBehind

The oldest sample in the disk buffer of an output (`queue.disk`) has waited three quarters of its `maxAge`: the backend has not accepted samples for that long, and the buffered samples older than `maxAge` will be dropped (`too_old`). `TapyrOutputFailing` usually fires too: fix the backend first. If the backend accepts old samples (VictoriaMetrics, Mimir) and the outage is planned, raise `maxAge`; Prometheus refuses samples older than its head block unless it has an out-of-order window.

### TapyrOutputBufferNotWritable

The disk buffer of an output cannot be written (a full disk, permissions, a directory removed): the queue keeps its samples in memory only, as without a buffer, and loses them on a restart. `tapyr_output_buffer_errors_total{kind="write"}` counts the failures and the log says why. Free space or fix the permissions of the directory (`buffer/<output name>` next to the state store by default): the queue writes to the buffer again on its own.

### TapyrConfigReloadFailed

The last reload of the configuration was rejected; the previous configuration keeps running. `tapyr validate --config <paths>` prints every error with its file and line.

### TapyrStateFlushFailing

Tapyr cannot write its state store (SQLite): quota consumption, rotated tokens and counters since the last flush would be lost on restart, and rotated Aruba tokens could be lost for good. Check the free space and the permissions of the state path (`settings.state.path`).
