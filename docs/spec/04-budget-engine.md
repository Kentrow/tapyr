# 04 - Budget engine

The budget engine is the heart of Tapyr. Its contract: **no request leaves Tapyr unless every applicable limit allows it**, and quota is spent according to priorities, with a reserve for retries.

## Concepts

- **Limit**: a constraint declared in a provider definition (`spec.limits`).
- **Scope**: the key that partitions a limit. A limit with scope `[account, var:organizationId]` is a separate counter for every (account, organization) pair.
- **Scope key**: the concrete value of a scope for a request, e.g. `acme-meraki/org=549236`.
- **Request descriptor**: what the executor passes to the engine for each request (provider, account, target, credential, templated endpoint, variables, job, priority, retry flag, auth-request flag).
- **Reservation**: a grant to send one request, possibly obtained after waiting. Reservations are released (concurrency) and settled (quota sync) after the response.

## Limit kinds

### `rate` - token bucket

```yaml
- name: org-rate
  kind: rate
  rate: 10/s          # refill rate
  burst: 10           # bucket capacity (default: rate per second, minimum 1)
  scope: { by: [account, "var:organizationId"] }
```

In-memory. Requests wait for a token (bounded by the request's deadline). When the burst is 2 or more, a request also leaves one token in the bucket: requests leave at the pace of the limit but reach the provider with a varying latency, and a provider bucket of the same size would refuse one that arrives slightly early. The token in hand keeps the provider's bucket one token ahead, which absorbs up to one interval of jitter; the sustained rate is unchanged and a burst is one request shorter. The simulator enforces the full burst.

### `concurrency`

```yaml
- name: ip-concurrency
  kind: concurrency
  max: 10
  scope: { by: [instance] }
```

In-memory semaphore. Released when the response is fully read or the request fails.

### `quota` - fixed window

```yaml
- name: daily
  kind: quota
  max: 5000                 # or "auto" (learned from headers, see Header sync)
  # initialMax: 5000        # required with max: auto, used until a header is seen
  window:
    period: day             # minute | hour | day | week | month
    resetAt: "00:00"        # for day/week/month
    timezone: UTC           # IANA name
    weekStart: monday       # for week
    monthDay: 1             # for month
  scope: { by: [account] }
  sync:
    limitHeader: X-RateLimit-Limit-day
    remainingHeader: X-RateLimit-Remaining-day
    resetHeader: ""         # optional: header carrying reset time (epoch seconds or delta)
```

Persistent. Window boundaries are computed from the calendar in the declared timezone (DST-safe).

- Defaults: `resetAt: "00:00"`, `timezone: UTC`, `weekStart: monday`, `monthDay: 1`.
- `resetAt` is `HH:MM` (24h). A reset time that does not exist on a given day (DST gap) is normalized forward, following Go's `time.Date` semantics.
- `monthDay` is 1 to 31; in shorter months the window resets on the last day of the month.
- `minute` and `hour` windows are aligned on the clock in the declared timezone.
- `max: auto` requires `initialMax` (a conservative value used until the first limit header is observed) and `sync.limitHeader`.

### `quota` - rolling window

```yaml
- name: rolling-24h
  kind: quota
  max: 10000
  window: { rolling: 24h }
  scope: { by: [credential] }
```

Persistent. Implemented with per-minute buckets (bounded memory, 1-minute precision).

### Applicability filters

A limit applies to a request only if its scope can be resolved and its filters match:

```yaml
appliesTo:
  endpoints: ["/organizations/**"]      # glob on templated path
  methods: [GET]
  requestsWithVar: organizationId       # only if this variable is set
  authRequests: include                 # include (default) | only | exclude, see Auth limits
```

Endpoint globs match the templated path segment by segment: `*` matches within one segment, `**` matches zero or more whole segments. `/organizations/*` matches `/organizations/{organizationId}` but not `/organizations/{organizationId}/networks`; `/organizations/**` matches both.

### Auth limits

Token requests (OAuth, login) are budgeted too. `appliesTo.authRequests` selects which requests a limit counts:

- `include` (default): both regular and auth requests,
- `only`: auth requests only (for example a dedicated limit on token creations per hour),
- `exclude`: regular requests only.

Without a dedicated limit, auth requests therefore count against every generic limit that does not exclude them.

## Scope dimensions

| Dimension | Meaning |
|---|---|
| `instance` | The whole Tapyr process (approximates "per source IP") |
| `provider` | All accounts of the provider in this process |
| `account` | One Account |
| `target` | One target of a multi-target Account |
| `credential` | One credential set (accounts sharing the same secret values share this key; computed as a salted hash, never logged; the salt is generated randomly once and stored in the state store so ledger keys stay stable across restarts) |
| `endpoint` | The templated path |
| `var:<name>` | Value of a request variable (from discovery or params) |

An empty `by` is equivalent to `[instance]`.

## Reservation algorithm

For each request:

1. Collect applicable limits and compute scope keys.
2. Reject immediately if:
   - a scope is **penalized** (after a 429 with `Retry-After`) and the penalty outlasts the request deadline,
   - a quota scope is **exhausted** for the current window (taking budget share and reserves into account),
   - the account's circuit breaker is open.
   The refusal carries a reason (`quota_exhausted`, `penalized`, `circuit_open`, `deadline`).
3. Otherwise, acquire every applicable limit atomically under the engine lock: quota units (check and increment), concurrency slots and rate tokens. When a concurrency slot or a rate token is not available, the request acquires nothing and waits; if the wait would exceed the deadline, it is refused with `deadline`.
4. Return the reservation.

Waiting is priority-aware: when several requests wait on the same scope, higher priority goes first (`critical` > `high` > `normal` > `low`), FIFO within a priority.

### Quota accounting with share and reserve

For a quota limit with effective maximum `M` (after header learning) and account `budget.share` `s`:

- Usable budget: `U = floor(M * s)`.
- Retry reserve: `R = floor(U * retryReserve)` (global setting, default 10%).
- Normal requests may consume up to `U - R`.
- Retries may consume up to `U`.
- `critical` jobs may dip into the reserve for normal requests once the normal budget is exhausted (they keep running, retries for others stop first).
- When a scope key is shared by several accounts with different shares (scopes without `account` or `target`, such as `instance`, `provider` or `credential`), `s` is the smallest share among the accounts contributing to that key.

## Header sync

After each response, the engine reads the `sync` headers of every applicable quota limit:

- **Learn the limit**: if `max: auto` (or if the header disagrees with the declared max), the effective max becomes the header value. Disagreement is logged once per window and exposed as a metric.
- **Learn the remaining**: with `M` the effective max (whole quota, as reported by the provider), `U = floor(M * s)` the usable budget, `c` Tapyr's own consumption in the window and `r_obs` the observed remaining (whole quota):
  - observed total consumption: `C_obs = M - r_obs`,
  - **external consumption** (other tools using the same quota): `external = max(0, C_obs - c)`, exposed as `tapyr_budget_external_usage`,
  - Tapyr's remaining budget: `remaining = min(U - c, r_obs)`. Tapyr never exceeds its share and never exceeds what is actually left.
- **Reset**: if a reset header is declared, it overrides the computed window end.
- **Windows that start at the first request** (ThousandEyes: one minute "from the first request in a batch") need no window option: with a reset header, every response sets the end of the window to the provider's, so the engine rolls when the provider does and its next window, opened by the next request, matches the provider's; the remaining header keeps the count exact in between. Without a reset header, the engine rolls on the clock while the window of the provider still runs, and a burst sent right after, before any response, can exceed it: declare the reset header for such providers (`TestFirstRequestWindows` covers both).

Headers are matched case-insensitively. Missing headers are ignored silently.

## 429 handling

When a response matches `errors.rateLimited`:

- Determine the penalty duration from `Retry-After` (seconds or HTTP date), else from the provider default (`errors.rateLimited.defaultPenalty`, default 2s), with exponential growth on consecutive 429s for the same scope (cap 10 minutes).
- Apply the penalty to the **narrowest scope** among applicable limits by default, or to all applicable scopes if `errors.rateLimited.penalize: all`.
- If the response carries quota headers showing remaining = 0 for a quota limit, mark that quota scope exhausted until window end.
- Count `tapyr_budget_rate_limited_total` with the limit name. An unexpected 429 (no internal limit predicted exhaustion) is also counted as `tapyr_budget_unexpected_429_total`: it means the provider definition is wrong or the quota is shared.

## Ledger persistence

- Quota counters are persisted: `(limit, scope_key, window_start) -> used, observed_remaining, observed_max, updated_at`.
- Rolling windows persist per-minute buckets for their duration.
- On startup, current-window counters are loaded. Expired windows are discarded.
- Flush cadence: every 5 seconds and on shutdown. The engine tolerates losing the last seconds (header sync corrects).
- A flush writes only what changed since the previous successful one: the counters that moved and the minute buckets added, changed or gone (a daily window holds up to 1440 of them, and one or two change between flushes). The first flush of a process writes every entry in full, replacing the buckets a previous process left; a failed flush is written again in full by the next.

## Planner

The planner turns desired intervals into **effective intervals** that fit every quota.

### Inputs

- Jobs with desired interval, priority, fan-out (number of child instances, from inventory cache or estimate), estimated requests per run (pages, from run history: rolling median of the last 20 runs; else `estimate.pagesPerRun`; else 1), retry overhead estimate (rolling ratio, default 5%).
- Quota limits with usable budget for the current window and time remaining in the window.
- Rate limits (to check feasibility of bursts: a run's requests must fit in the interval at the limit's rate).

### Algorithm

1. For each quota scope, compute the projected consumption per window of every job contributing to it: `calls_per_run * fanout * runs_per_window`.
2. If the sum fits `U - R`, effective intervals equal desired intervals.
3. Otherwise, stretch intervals starting from the lowest priority class: within a class, apply a common multiplier `k >= 1` to all jobs of that class contributing to the scope, choose the smallest `k` that makes the sum fit (or stretch up to `settings.budget.maxStretch`, default 12x, then move to the next class).
4. `critical` jobs are never stretched. If critical jobs alone do not fit, the plan is **infeasible**: `validate` and `plan` report an error, `run` starts anyway with a loud warning and the status API flags it.
5. A job contributing to several quota scopes takes the largest stretch required by any of them.
6. Mid-window replanning (every `replanInterval`, on reload, on inventory change, on header-learned max change, and when the current plan is projected to exhaust a quota before its reset, see Projections): recompute using the **remaining** budget and the **remaining** time in the window, so a burst of failures in the morning tightens the afternoon instead of exhausting the quota. The remaining budget is `min(U - c, r_obs)` (see Header sync), less the reserve, less the consumption of other clients projected to the end of a calendar window at their pace so far (`external * time_left / time_elapsed`, the elapsed time counting as at least 1/24 of the window): another integration sharing the quota keeps spending until the reset. A fixed window shorter than `replanInterval` (a one-minute window) is planned over a full window instead (`U - R`, as offline): a plan paces several of them, what is left of the current one says nothing of the next, and a minute window exhausted by a 429 or by another client would otherwise stretch every job for a whole replan interval. The engine still refuses what the current window cannot take until its reset.
7. Rate-limit feasibility: if a run's estimated calls cannot complete within its interval given applicable rate limits, the interval is stretched to the minimum feasible value.

### Outputs

For each job: desired interval, effective interval, stretch factor, contributing scopes, projected calls per window. For each quota scope: budget, projected consumption, projected exhaustion time (if any), feasibility.

The same planner code runs offline in `tapyr plan`, which can take inventory size hints (`--fanout meraki-organizations=3`) when no inventory cache exists.

## Projections

For each quota scope of a fixed window, the projected exhaustion time (`tapyr_budget_exhaustion_eta_seconds`, absent if not projected to exhaust) extrapolates the consumption rate since the window start.

### Replan on projection

Between two `replanInterval`, the collector checks that the current plan still fits (`settings.budget.replanOnProjection`, on by default):

- The check follows every completed run: its requests counted against the quotas, and its responses reported their remaining. It runs at most once a minute; a run ending within the minute is checked with the next one, so no timer is added.
- The check projects the consumption of the **current** effective intervals until the end of every quota window (`planner.Evaluate`, same inputs as the online plan) and compares it with the remaining budget of the online plan (step 6 of the algorithm, external consumption projected to the reset included). Without quota headers, it only sees Tapyr's own consumption: more pages or retries than planned.
- A scope whose projection exceeds its budget by more than 10% triggers a replan at once, at most once every 5 minutes per scope. The new plan fits the budget exactly; a later jump of external consumption crosses the 10% again.
- A scope still infeasible after its replan (maximum stretch reached, critical jobs alone over budget) is disarmed until its projection fits its budget again, so that a plan that cannot fit never replans in a loop.
- Only a tightening triggers it: a relaxation waits for the next `replanInterval`.

Every plan computation counts in `tapyr_plan_recomputations_total{reason}`: `start`, `reload`, `interval`, `limit_learned`, `inventory` or `projection`.

## Implementation notes

Decisions taken when implementing the engine:

- **Single engine lock.** All limit state lives behind one mutex. API polling volumes are low enough, and a single lock makes multi-limit acquisition trivially atomic (no partial acquisition, no lock ordering).
- **Waiting.** A request that cannot acquire every applicable limit registers as a waiter and sleeps until a limit is released, a token refills or a penalty ends (timers on the injected clock), or its deadline. A waiter may acquire only when no waiter of higher priority (or of the same priority and older) is waiting on one of its scope keys. Waiters hold nothing while they wait, so there is no deadlock.
- **Deadline.** When the next token of a rate limit or the end of a penalty comes after the request deadline, the request is refused at once with `deadline` instead of waiting in vain.
- **Quota counting.** Quota units are counted when the reservation is granted, not when the response arrives: a request that fails still consumed provider quota.
- **Remaining after an observation.** `r_obs` is valid at the time of the response that carried it. Until the next observation, the engine uses `r_obs - (c_now - c_obs)`, where `c_obs` is Tapyr's consumption when `r_obs` was observed.
- **Reset header.** A value greater than `1e9` is an epoch in seconds; otherwise it is a delay in seconds.
- **Circuit breaker.** The breakers live in the engine (it refuses with `circuit_open`); the executor reports successes and transient failures.
- **Credential key.** `HMAC-SHA256(salt, secret values)`, hex-truncated to 16 characters; the salt is stored in the state store.
- **Enforcer mode** (simulator). The same limit evaluation answers, without waiting, whether a request is allowed now; otherwise the simulator answers `429` with `Retry-After` (time to the next token, end of the penalty or end of the quota window). Server-side scope keys: `account`, `target` and `credential` map to the presented credential, `instance` and `provider` to the whole simulator, `endpoint` to the route path, `var:<name>` to the route variable.
- **Timeline.** Every quota scope keeps its consumption per bucket (1 minute for windows up to one day, 1 hour beyond) for the current and the previous window, for the status API. Fixed windows persist the buckets of the current window in `ledger_minutes`, as rolling windows already do.
- **Observer.** The engine reports grants (with the wait), refusals and rate-limited responses to an optional observer, the source of the budget self-metrics.

## Testing requirements

- Fake clock for everything. Daily windows across DST changes, month boundaries, leap years, timezone offsets.
- Property tests: under random request streams, no limit is ever exceeded.
- Concurrency tests with the race detector: no partial acquisition, no deadlock with overlapping scopes.
- Planner golden tests: given inputs, expected effective intervals.
- Simulation test (with the simulator, accelerated fake clock): 24 simulated hours, undersized quota, verify priorities and zero quota-related 429s.
