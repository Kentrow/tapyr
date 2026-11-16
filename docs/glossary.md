# Glossary

The words Tapyr uses in its configuration, its CLI, its web UI and its metrics, in alphabetical order. Where the web UI shows a term under another label, the entry says so. [Concepts](concepts.md) explains how the pieces fit together.

## Account

An instance of a provider: its secrets, its params (an organization, a base URL) and its labels, added to every sample it produces. You write one `Account` document per set of credentials. See [Accounts and targets](concepts.md#accounts-and-targets).

## Budget

What a quota leaves to Tapyr until its reset: its maximum, times the [budget share](#budget-share), minus what is already spent and the [retry reserve](#retry-reserve). The planner fits the jobs into it. In the web UI: **Available**, on the Plan page.

## Budget share

The part of every quota an account may spend, `budget.share` on the account (`0.5` leaves half for other tools using the same credentials). Default: the whole quota.

## Burst

The number of requests a [rate limit](#rate-limit) lets through at once after a quiet period, before it falls back to its rate: the size of its token bucket.

## Circuit breaker

Protects an account (or a target) that keeps failing: after 5 consecutive transient failures it **opens**, and Tapyr stops calling, so no quota is spent. After a cooldown (2 minutes, doubling up to 30) it is **half-open**: the next run is a probe, which closes it on success and opens it again on failure. See [Failures](concepts.md#failures).

## Concurrency limit

A limit on the requests in flight at the same time (`kind: concurrency`): a request waits for a free slot. In the web UI: "requests at once".

## Credential

A set of secret values. A limit scoped by credential is shared by the accounts that use the same secrets, even under different names. Tapyr keys it by a salted hash and never shows the secret; the status API and the web UI name the accounts that share it.

## Desired interval

How often you want a job to run: `interval` on the job, else the `defaultInterval` of its template. The planner may stretch it into a longer [effective interval](#effective-interval).

## Discovery

Listing the items of a hierarchical API (organizations, then their networks, then their devices) so that the jobs below run once per item. An [inventory](#inventory) job discovers, a [fan-out](#fan-out) job runs on what it found. See [Discovery](concepts.md#discovery).

## Effective interval

The interval a job actually runs at: its [desired interval](#desired-interval), or longer when the planner [stretches](#stretch) it to fit a quota. The Jobs page shows both when they differ.

## Endpoint

The path of a request with its variables left as placeholders, such as `/organizations/{organizationId}/networks`. Limits can be scoped by endpoint, and the self-metrics use it rather than the real URL, so that they never carry identifiers.

## External usage

The calls of other clients of the same quota (the vendor UI, scripts, other integrations): what the provider counts minus what Tapyr made. Known only when the API sends [quota headers](#quota-headers). The planner expects it to go on at the same pace until the reset. In the web UI: **Used by others**, on the Budgets page; self-metric `tapyr_budget_external_usage`.

## Fan-out

A job whose template has `forEach`: it runs once per item listed by its parent, as many [instances](#instance). "The fan-out of a job" is also the number of its instances.

## Implicit job

The parent of a fan-out that no job of the account declares: Tapyr runs it for you, named `<account>.<template>` (`campus.meraki-organizations`), at priority `low`. You only declare the jobs whose metrics you want. The Jobs page marks it "(implicit)".

## Infeasible plan

A plan in which the `critical` jobs alone spend more than a quota allows: they are never stretched, so nothing can make them fit. `tapyr plan` exits with code 3 and says what to change; `tapyr run` starts anyway, with a warning. In the web UI: **does not fit**.

## Instance

Two meanings, told apart by their context:

- **A job instance** is one copy of a [fan-out](#fan-out) job, for one item of its parent: the devices job of one network. The Instances column of the Jobs and Plan pages counts them, and the page of a job lists them.
- **A Tapyr instance** is one running Tapyr process. A limit scoped by `instance` is shared by every account of the process: it stands for the limits a provider counts per source IP.

## Inventory

A job whose template lists items for others (`role: inventory`, or `both` when it also produces metrics). Its last answer is kept in the [state store](#state-store), so that a restart does not wait for discovery.

## Job

What to collect for an account: a template of its provider (or an inline request), at an interval and a [priority](#priority), towards every output or some of them. See [Jobs](concepts.md#jobs).

## Job template

A request of a provider package and its mapping to metrics, which jobs name with `template`. `tapyr provider show <name>` lists the templates of a package, with their interval and metrics.

## Limit

A rule of a provider API that Tapyr keeps to: a [rate limit](#rate-limit), a [concurrency limit](#concurrency-limit) or a [quota](#quota), declared once in the provider package with its [scope](#scope) and the source of its value. The Budgets page shows every limit with its current state.

## Not collected

A job whose metrics no output keeps (every output drops them by name): it makes no API call and spends no quota, and its quota goes to the other jobs. `tapyr validate` and `tapyr plan` warn about it. See [Sending less to an output](outputs.md#sending-less-to-an-output).

## Output

Where the samples go: `prometheus` (served on `/metrics` for a scrape), `remote_write` or `otlp`. Each push output has its own queue, so a slow backend never holds back the others. See [Outputs](outputs.md).

## Pages per run

The requests a run makes when the answer comes in several pages: the median of the last runs, else the estimate of the template, else 1. The planner multiplies it by the [instances](#instance) and the runs per window.

## Penalty

After a `429 Too Many Requests`, the [scope](#scope) of the limit waits for the `Retry-After` of the response (2 seconds without one), longer on repeated ones, up to 10 minutes. The requests of that scope wait or are refused in the meantime.

## Plan

The intervals the [planner](#planner) gives the jobs so that they fit every quota until its reset, with the jobs it slowed down and why. `tapyr plan` computes it offline, before you deploy; the Plan page and `GET /api/v1/plan` show the plan applied. See [Planner](concepts.md#planner).

## Planner

The part of Tapyr that projects what the jobs will spend in every quota window and [stretches](#stretch) their intervals when it does not fit. It plans again every 15 minutes, after a reload or a discovery, and at once when a quota would run out before its reset, for example because of the [external usage](#external-usage).

## Priority

Who slows down first when a quota is tight: `low` first, then `normal` (the default), then `high`; `critical` jobs are never slowed down. Requests waiting on the same limit also go in priority order.

## Processing

Rules on an output (`spec.processing`) that drop metrics or labels before they reach it, written like Prometheus `metric_relabel_configs`. A job none of whose metrics reaches any output is [not collected](#not-collected).

## Projected

What the jobs will spend until the reset of a quota at their planned intervals, on the Plan page. **Use** is the projected spend against the [budget](#budget).

## Provider

A vendor API as Tapyr knows it: base URL, authentication, pagination, errors and [limits](#limit), in a `Provider` document.

## Provider package

A directory that describes one provider: the `Provider` document, its [job templates](#job-template), a [simulator](#simulator), a Grafana dashboard and a README, in YAML and CEL. The built-in packages come with the binary; `settings.providerPaths` adds your own. See [Providers](providers.md).

## Quota

A number of calls per window (`kind: quota`): per minute, hour, day, week or month, or per rolling period. Once it is spent, Tapyr makes no more calls in its scope until the [reset](#window). In the web UI: "calls per day", "calls per 1h, rolling".

## Quota headers

Response headers in which an API announces its quota: its maximum, what is left, when it resets. When a provider declares them, Tapyr follows them: it learns the real maximum, measures the [external usage](#external-usage) and never spends more than the API says is left. In the web UI: "reported by the API", next to **Remaining** on the Budgets page.

## Rate limit

A limit on requests per second or per minute (`kind: rate`), as a token bucket: a request waits for a token, and a [burst](#burst) can go at once.

## Replan

The [planner](#planner) computing the [plan](#plan) again from what is left in each window, so that a morning of retries or another client tightens the afternoon instead of exhausting the quota.

## Retry reserve

The part of every quota kept for retries and for `critical` jobs, 10% by default (`settings.budget.retryReserve`): the planner leaves it out, so that a retry never finds the quota empty. In the web UI: **For retries** on the Plan page, **Retry reserve** on the Budgets page.

## Role

What a template is for: `metrics` (the default) produces samples, `inventory` lists items for other templates, `both` does both.

## Run

One execution of a job instance: its requests, pages and retries, and the samples it produced. Its result is one of:

| Result | Meaning |
|---|---|
| `success` | every request succeeded and the samples were sent |
| `partial` | some pages failed and the template accepts partial data (`extract.partial`) |
| `failed` | a request or the mapping failed; the series stay until [stale](#stale) |
| `skipped_budget` | no quota left, or the scope is under [penalty](#penalty) |
| `skipped_circuit` | the [circuit breaker](#circuit-breaker) of the account is open |
| `skipped_overlap` | the previous run of the instance was still going |

A job with no run yet is **waiting**: for its first run, or for its parent to list its items.

## Sample

One value of one [series](#series) at one time. A sample carries the time the API gives when the mapping reads it, else the time of the collection.

## Scope

What a limit is counted per: the whole [Tapyr instance](#instance) (`instance`), every account of the provider together (`provider`), each `account`, each `target`, each [credential](#credential), each [endpoint](#endpoint), or each value of a request variable (`var:organizationId`). A limit scoped `[account, var:organizationId]` is one counter per account and organization. Each counter is a **scope key**. In the web UI: **Of**, on the Plan page.

## Self-metrics

Tapyr's metrics about itself, prefixed `tapyr_`, on `/metrics/self`: requests, retries, quotas, intervals, runs, circuits and outputs. They never carry identifiers such as serials or organization IDs. See [Self-observability](concepts.md#self-observability).

## Series

One metric with one set of label values, such as the clients of one access point. Backends count series, and hosted ones bill by them; `limits.maxSeries` caps the series of an output. See [Series and limits](outputs.md#series-and-limits).

## Simulator

An API served by Tapyr itself from the definition of a provider package, with its authentication, pagination and limits, and fault **scenarios** (429s, outages, slow responses): `tapyr sim`. It lets you try a configuration and its quota spend without an account.

## Stale

A series that is no longer produced: it leaves the Prometheus exposition, and push outputs receive a stale marker. A series goes stale when the API no longer returns it, or when its job keeps failing for longer than `staleAfter` (3 times the effective interval by default).

## State store

The SQLite database of `tapyr run` (`settings.state.path`): the spend of every quota window, the inventories, the run history, the tokens and the series of every job instance, so that a restart forgets nothing. See [State](concepts.md#state).

## Stretch

The factor by which the planner lengthens the interval of a job to fit a quota: `x2` runs it half as often. Jobs of a priority are stretched together, up to 12 times (`settings.budget.maxStretch`), before the next priority up. In the web UI: **Slowed down**, on the Plan page.

## Target

One device or endpoint of a **multi-target** account, such as one router of a fleet reached on its local API. Every job of the account runs once per target, and a target can override the params, secrets and labels of the account.

## Unexpected 429

A `429 Too Many Requests` that no declared limit predicted: the provider definition misses a limit, or another client shares it. Counted in `tapyr_budget_unexpected_429_total`; [Quota problems](troubleshooting.md#quota-problems), in the troubleshooting guide, says what to check.

## Window

The period a [quota](#quota) counts over. A calendar window resets at a fixed time (every day at 00:00 UTC, the first of every month); a rolling window counts the last hour or the last day at any moment. The state store keeps what was spent, so a restart does not start the window again.
