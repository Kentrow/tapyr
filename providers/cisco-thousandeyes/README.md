# cisco-thousandeyes

Cisco ThousandEyes API v7: the network results of the tests (latency, loss, jitter and health score, per test and agent), the tests and the agents of an account group.

**Validation status**: simulator only. The package is built from the [rate limit documentation](https://developer.cisco.com/docs/thousandeyes/rate-limits/) of API v7 and the official SDKs ([thousandeyes-sdk-python](https://github.com/thousandeyes/thousandeyes-sdk-python) 7.0.110: paths, parameters and fields of its generated docs), and tested against a simulator in CI; it has not been run against a real ThousandEyes account yet. A real validation needs an account group with network tests, an API token of a user with read access, one run of `examples/thousandeyes.yaml`, and anonymized recordings (`tapyr record`) compared with the fixtures. Reports are welcome.

## Coverage

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `thousandeyes-tests` | `GET /tests` | 1h | 1 | `thousandeyes_test_info` (name, type), `thousandeyes_test_enabled`, `thousandeyes_test_interval_seconds` |
| `thousandeyes-agents` | `GET /agents` | 15m | 1 | `thousandeyes_agent_info` (name, type, location, country), `thousandeyes_agent_up` (enterprise agents) |
| `thousandeyes-network-results` | `GET /test-results/{testId}/network`, one instance per enabled `agent-to-server` or `agent-to-agent` test | 5m | 1 per test | `thousandeyes_network_latency_seconds` (average), `thousandeyes_network_latency_min_seconds`, `thousandeyes_network_latency_max_seconds`, `thousandeyes_network_jitter_seconds`, `thousandeyes_network_loss_ratio`, `thousandeyes_network_health_score`, `thousandeyes_network_results_truncated` |

- `thousandeyes-tests` is also the inventory of the network results: a job using `thousandeyes-network-results` alone is enough, the tests are listed implicitly.
- Without a time window, the API returns the latest round of every agent of a test. The network samples carry the time of their round (`roundId`) as source timestamp: a round already sent is not sent again (deduplication), so a 5-minute job does not repeat a 10-minute round.
- Network measures carry `test_id` and `agent_id`; names, types and locations are in the `_info` metrics. No IP address, hostname or serial of an agent, no target address, and no user name (`createdBy`, `modifiedBy`) is collected.
- `thousandeyes_network_health_score` is from 0 to 1; the negative values of the API (error codes) are skipped.
- One page is read per request: without a window, the latest round of every agent fits it. `thousandeyes_network_results_truncated` is 1 when the API reported a next page, which would mean results were not read.
- Other test types (HTTP server, page load, web transactions, DNS, BGP, voice) have their own results endpoints: not collected yet. Endpoint agents (the endpoint experience APIs) neither.

## Auth setup

API v7 authenticates with a bearer token (`Authorization: Bearer <token>`).

1. In ThousandEyes, create an API token for a user whose role can read the tests and the agents of the account group (the user API tokens of the user settings).
2. Give Tapyr the token as the `apiToken` secret (see `examples/thousandeyes.yaml`).
3. Optional: the `accountGroupId` param selects the account group (the `aid` parameter of the API); without it, the API uses the default account group of the token's user. The account group IDs are listed by `GET /account-groups`.

## Limits

| Limit | Value | Source |
|---|---|---|
| `organization` (quota) | 240 requests a minute per organization, the minute starting at the first request of a batch; `max: auto` synced from `X-Organization-Rate-Limit-Limit`, `-Remaining` and `-Reset` (240 until learned) | [Rate limits](https://developer.cisco.com/docs/thousandeyes/rate-limits/) |

- The documented example shows `X-Organization-Rate-Limit-Limit: 20` where the text says 240: the max is learned from the header.
- The window starts at the first request, not on the clock: the reset header (the epoch of the next rollover) keeps the window of Tapyr on the one of the API, and the remaining header keeps the count exact in between. Tested in the engine against a model of this API: no 429, even for bursts of the whole budget (spec 04, Header sync). Do not remove the reset header from the `sync` block.
- Exceeded, the API answers 429, without `Retry-After`; the documentation says to wait for the reset, which the quota does.
- The limit is per organization, shared by every user, token and integration of it: other integrations show in `tapyr_budget_external_usage`; give Tapyr a share (`budget.share`) when they need room. Instant tests (24 calls a minute) and template deployments have limits of their own; the package uses neither.

With the defaults, an account group with 50 network tests spends about 10 requests a minute (one per test every 5 minutes, plus one for the tests every hour and one for the agents every 15 minutes), far from the 240 of the limit.

## Simulator

```sh
tapyr sim --provider cisco-thousandeyes --listen 127.0.0.1:8087
```

8087 is the port of the simulator under `tapyr sim --all --listen-base 127.0.0.1:8081` (`portOffset: 6`). A synthetic account group of 5 tests (3 `agent-to-server`, one of them disabled, 1 `agent-to-agent`, 1 `http-server`) and 5 agents (3 cloud, 2 enterprise), under `/v7`, in the response shape of the SDK docs. It accepts the token `sim-thousandeyes-token`, enforces the organization limit and sends the three headers.

Known gaps of the simulator: its windows are aligned on the clock (not on the first request), and it sends the reset header as seconds until the reset (the API sends an epoch). Tapyr reads both forms.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | The limit at 20 requests a minute (the documented example), another integration spending 5 of them |
| `bad-day` | From 1h, slow responses (p50 700 ms, p99 4 s) for an hour; from 2h, 30% of the test results requests fail with 502 for 30 minutes; at 4h, a 10-minute outage |

## Dashboard

`dashboards/cisco-thousandeyes.json` (Grafana, Prometheus data source, test variable): tests, enterprise agents offline, highest loss and latency, lowest health score, average latency, loss and jitter by test, the worst paths by loss, the agents, and the organization limit of the current minute (limit, used by Tapyr and by others).

## Known caveats

- Simulator-validated only (see above). The fields come from the generated docs of the official SDKs.
- Only the network results of `agent-to-server` and `agent-to-agent` tests; bidirectional agent-to-agent tests are read with the default direction of the API.
