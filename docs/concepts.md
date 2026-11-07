# Concepts

Tapyr polls the APIs of vendor platforms and turns their answers into metrics. What makes it different from a script with a loop is that it treats the API quota as a budget: the limits of every API are declared once, every request is accounted against them, and the polling slows down on purpose before a quota runs out. This page explains the pieces and how they fit; the [specification](spec/01-vision-and-scope.md) has the full detail.

## Configuration documents

A configuration is a set of YAML documents, in one file or many, in a directory or several. Every document has the same envelope:

```yaml
apiVersion: tapyr/v1alpha1
kind: Account          # the kind of the document
metadata:
  name: campus         # unique per kind
spec: {}               # depends on the kind
```

| Kind | Written by | What it is |
|---|---|---|
| `Provider` | provider packages | An API: its base URL, authentication, pagination, error semantics and rate limits |
| `JobTemplate` | provider packages | A reusable request and its mapping to metrics |
| `Account` | you | An instance of a provider, with its credentials, parameters, targets and labels |
| `Job` | you | What to collect for an account, from a template (or inline), at which interval and priority |
| `Output` | you | Where the samples go: Prometheus scrape, remote write or OTLP |
| `Settings` | you, optionally | Server, state store, retries, logs and other global settings |

You write the last four. `tapyr validate` checks every document against its JSON Schema and reports every error with its file, line and column. The [configuration reference](reference/README.md) lists every field.

## Provider packages

A provider package describes one vendor API: the `Provider` document, its `JobTemplate`s, a simulator with fault scenarios, a contract test, a Grafana dashboard and a README. Packages are YAML and CEL, never Go code (authentication flows that cannot be declared are the exception, as Go auth plugins). The [built-in packages](providers.md#built-in-packages) are compiled into the binary; `tapyr provider list` shows them, and `settings.providerPaths` adds packages of your own.

The same definition drives the collector and the simulator: `tapyr sim` serves an API that behaves like the real one, with its authentication, pagination and limits, so you can try a configuration and its quota consumption without an account.

## Accounts and targets

An `Account` instantiates a provider with what that provider asks for: `params` (an organization, a base URL) and `secrets`. Secrets are never written in the configuration: they are references to environment variables or files.

```yaml
spec:
  provider: meraki-dashboard
  secrets:
    apiKey: { env: TAPYR_MERAKI_API_KEY }    # or { file: /run/secrets/meraki }
  labels:
    tenant: campus                           # added to every sample of the account
```

Some APIs live on each device rather than in a cloud, like the local API of Teltonika routers. A **multi-target** account lists the devices as `targets`; every job of the account then runs once per target, and each target can override the params, secrets and labels of the account.

When other tools share the same credentials, `budget.share` limits Tapyr to a part of every quota (`share: 0.5` leaves half for the others).

## Jobs

A `Job` collects one thing for one account, usually by naming a template of the provider package:

```yaml
spec:
  account: campus
  template: meraki-device-availabilities
  interval: 5m           # the desired interval, defaults to the template's
  priority: high         # critical, high, normal (default) or low
```

The **interval** is a wish: the planner may stretch it to fit the quotas (see below). The **priority** decides who slows down first. A job sends its samples to every output, or to the subset in `outputs`; `labels` are added to its samples, and win over target and account labels.

### Discovery

Many APIs are hierarchical: organizations contain networks, networks contain devices, and the URL of a request needs the identifier of its parent. An **inventory** template (`role: inventory`, or `both` when it also produces metrics) lists items; a template with `forEach` runs **once per item** of its parent, with the variables of the item in its request:

```text
organizations (inventory) -> networks (inventory, one instance per organization) -> devices (one instance per network)
```

These per-item runs are **instances** of the job. You only declare the job you want: parents that no job of the account instantiates run implicitly, named `<account>.<template>`, at priority `low`. The inventory is cached in the state store, so a restart does not wait for discovery; the instances of a fan-out are spread over their interval instead of firing together (a newly discovered one runs first within the jitter, then at its point of the interval), and `forEach.maxInstances` (default 10000) protects against a runaway discovery.

## Mapping

A template maps the JSON of a response to samples with [CEL](https://cel.dev) expressions: which list holds the items, and for every metric its value and labels.

```yaml
extract:
  items: response.body
  filter: "item.status != 'decommissioned'"
  metrics:
    - name: example_device_up
      help: Device status (1 online, 0 otherwise)
      value: "item.status == 'online'"
      labels:
        serial: item.serial
        site_id: item.siteId
  info:
    - name: example_device_info
      labels:
        serial: item.serial
        model: item.model
```

Metrics are gauges or counters, and descriptive attributes go to `_info` metrics (value 1) rather than to every series. A sample carries the time the API gives when the mapping reads it, else the collection time. The [mapping recipes](providers.md#cel-mapping-recipes) show the common patterns.

## Budget engine

Every provider declares its limits:

| Kind | Example | Enforced as |
|---|---|---|
| `rate` | 10 requests per second | a token bucket: requests wait for a token |
| `concurrency` | 4 requests in flight | requests wait for a free slot |
| `quota` | 10,000 calls per day, per month, or per rolling hour | a counter per window: requests are refused once it is spent |

A limit has a **scope**: one counter for the whole Tapyr instance, per account, per credential, per endpoint, or per value of a request variable such as an organization. **Every request** reserves its place in every limit that applies to it before leaving, retries, pages, token requests and discovery included: this is how Tapyr guarantees that it never exceeds a declared limit.

The engine also learns from the provider:

- **Quota headers.** When the API announces its quota in response headers, Tapyr follows them: the real maximum (`max: auto`), what is left, and when the window resets. What the provider counts beyond Tapyr's own requests is the **external usage**, from another tool or a script using the same quota.
- **429 responses.** A `429 Too Many Requests` puts its scope in penalty for its `Retry-After`, growing on repeated ones. An **unexpected** 429, one that no declared limit predicted, is counted apart: it means the definition misses a limit, or someone else shares it.
- **Retry reserve.** 10% of every quota (`settings.budget.retryReserve`) is kept for retries, so that a burst of errors does not starve the rest of the day.

## Planner

Before running, and again while running, the planner checks that the jobs fit every quota. For each quota scope, it projects the calls of every job over the window: calls per run (from the run history, or the estimate of the template), times the instances of the fan-out, times the runs per window.

- When the projection fits, every job runs at its desired interval.
- When it does not, the planner **stretches** the intervals, lowest priority first: all the `low` jobs of the scope by a common factor, up to 12 times (`settings.budget.maxStretch`), then the `normal` ones, then `high`. The interval actually used is the **effective interval**.
- `critical` jobs are never stretched. When they alone exceed a quota, the plan is **infeasible**: `tapyr plan` exits with code 3, and `tapyr run` starts anyway with a warning.
- Every 15 minutes (`settings.budget.replanInterval`), and after a reload or an inventory change, the planner starts again from what is left in the window and from the pace of the external usage, so that a morning of retries tightens the afternoon instead of exhausting the quota.

`tapyr plan` runs the same planner offline, before you deploy a configuration: see the [getting started](getting-started.md#3-check-what-it-will-spend) guide.

## Failures

Tapyr classifies every failed request:

- **Transient** errors (network errors, timeouts, and the statuses the provider declares retryable, usually 5xx) are retried with exponential backoff and jitter, up to 4 attempts, within the deadline of the run. Only idempotent requests are retried.
- **Rate limited** requests are retried after their penalty.
- **Authentication** failures with a token (OAuth2, session login) drop the token and retry once with a new one. A rejected token or login request backs off from 1 minute up to 1 hour, so that a wrong password never locks an account.
- Other client errors and responses that cannot be decoded fail the run without a retry.

After 5 consecutive transient failures, the **circuit breaker** of the account (or of the target) opens: Tapyr stops calling it and spends no quota, then probes again after a cooldown of 2 minutes, doubling up to 30 minutes. A run ends as `success`, `partial`, `failed`, or skipped (`skipped_budget`, `skipped_circuit`, `skipped_overlap` when the previous run is still going).

## Series and outputs

Tapyr keeps track of the series each job instance produced at its last successful run:

- A series that disappears from the answer (a device removed) is **stale**: it leaves the Prometheus exposition, and push outputs receive a stale marker.
- A **failed** run changes nothing: the series stay until `staleAfter` (3 times the effective interval by default), then go stale.
- A sample whose source timestamp was already sent is dropped, since APIs often return overlapping windows.

Three output types deliver the samples: `prometheus` (served on `/metrics` for a scrape), `remote_write` (Prometheus, VictoriaMetrics, Mimir, Thanos) and `otlp` (an OpenTelemetry Collector or any OTLP backend). Each push output has its own bounded queue: a slow backend never blocks the collection or the other outputs. [Outputs](outputs.md) has the details.

## Self-observability

Tapyr exposes its own metrics on `/metrics/self`: requests by status, retries and waits, the quotas used, left and projected, the effective intervals, the freshness of every job, circuits and outputs. They never carry identifiers such as serials or organization IDs. The other ways to see what it does:

- `/healthz` and `/readyz`, for probes;
- a read-only **status API** under `/api/v1`, described in `/api/v1/openapi.yaml`;
- the read-only **web UI** on `/ui/`: overview, graph of providers, accounts, jobs and outputs, budgets with their timelines, the plan, and a live view of the raw responses when the debug endpoint is enabled;
- the `tapyr-overview` Grafana dashboard and example [alert rules](alerts.md).

## State

`tapyr run` keeps a SQLite database (`settings.state.path`, default `./data/tapyr.db`), written every 5 seconds and on shutdown:

- the consumption of every quota window, so that a restart does not forget what was already spent;
- the cached inventories and the run history used by the planner;
- the tokens of the providers, encrypted when `TAPYR_STATE_KEY` is set;
- the series of every job instance and the last timestamps sent.

Losing it is not fatal, since the quota headers correct the counters, but keep it on persistent storage: see [Operations](operations.md#state-store).
