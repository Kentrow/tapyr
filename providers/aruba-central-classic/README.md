# aruba-central-classic

HPE Aruba Networking Central, **classic platform**, REST API: access points, switches, gateways and sites of a Central account. The newer GreenLake-based Central uses another API and will be a separate package.

**Validation status**: tested against a real Central account (November 2026, strictly read-only): every template, the refresh of the rotating token pair, a restart resuming from the stored pair, and the daily quota headers, which matched the documented formula. The package is built from the [Aruba developer hub](https://developer.arubanetworks.com/aruba-central/) and the official `pycentral` SDK, and tested in CI against a simulator that follows the shape of the real responses, rotates refresh tokens and enforces the limits below.

## Coverage

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `aruba-aps` | `GET /monitoring/v2/aps?calculate_client_count=true` | 5m | 1 per 1000 APs (288 a day) | `aruba_ap_up`, `aruba_ap_clients`, `aruba_ap_info` (name, model, firmware, group, site), `aruba_aps` |
| `aruba-switches` | `GET /monitoring/v1/switches` | 15m | 1 per 1000 switches (96 a day) | `aruba_switch_up`, `aruba_switch_info` (name, model, firmware, group, site, switch type) |
| `aruba-gateways` | `GET /monitoring/v1/gateways` | 15m | 1 per 1000 gateways (96 a day) | `aruba_gateway_up`, `aruba_gateway_info` (name, model, firmware, group, site, role) |
| `aruba-sites` | `GET /central/v2/sites` | 1h | 1 per 1000 sites (24 a day) | `aruba_site_info` |

- Lists are read 1000 items per page (the maximum), with `calculate_total=true` for the pagination.
- The client count of each access point comes with the access point list (`calculate_client_count=true`), at no extra call; per-site counts are sums by `site` of `aruba_ap_clients * on (serial) group_left (site) aruba_ap_info`.
- Measures carry the `serial` only; names, sites and groups are in the `_info` metrics. No client MAC address is collected.

## Auth setup

Aruba Central (classic) uses OAuth access tokens valid 2 hours and **rotating refresh tokens** valid 15 days: every refresh returns a new refresh token, and the previous one must not be used again ([access token management](https://developer.arubanetworks.com/central/docs/access-token-management)). The `aruba_central_classic` auth plugin handles it:

1. In Central, API Gateway > System Apps & Tokens (or My Apps & Tokens): add an application, note its client ID and secret, then generate and download a token pair.
2. Give Tapyr `clientId`, `clientSecret`, `refreshToken` and, optionally, `accessToken` as secrets (see `examples/aruba.yaml`), and the API gateway of your cluster as `baseURL` (a token only works on its cluster).
3. Set `TAPYR_STATE_KEY`: the rotated pair is stored in the state store, encrypted with it.

Tapyr refreshes the token 10 minutes before it expires with `POST /oauth2/token?client_id=...&client_secret=...&grant_type=refresh_token&refresh_token=...` (parameters in the query, as documented), writes the new pair to the state store **before** using it, and serializes refreshes per account. After a restart it resumes from the stored pair; changing the bootstrap `refreshToken` secret (after a reload or a restart) replaces the stored pair.

If the refresh token is rejected (400 or 401: expired after 15 days without use, revoked, or used by another tool), Tapyr stops calling the API for the account, logs an error, sets `tapyr_auth_reauth_required{provider="aruba-central-classic"}` to 1 and lists the account in the `conditions` of `/readyz`. Generate a new token pair and update the bootstrap secrets.

**Do not share a token pair between Tapyr and another tool**: the first one to refresh invalidates the refresh token of the other. A crash between a refresh answer and its storage loses the pair (the refresh is the only request in flight for the account): the account then needs a new pair.

`tapyr record` can run next to `tapyr run`: with the same configuration and `TAPYR_STATE_KEY`, it reads the current pair from the state store and never refreshes it. It uses the stored access token until its expiry; past it, it stops and asks to retry once `tapyr run` has renewed the token.

## Limits

| Limit | Value | Source |
|---|---|---|
| `daily` (quota) | max(5000, 30 per Foundation device + 90 per Advanced device) calls per account and day, reset at midnight GMT; `max: auto` synced from `X-RateLimit-Limit-day` and `X-RateLimit-Remaining-day` (5000 until learned) | [Usage and rate limits](https://developer.arubanetworks.com/hpe-aruba-networking-central/docs/usage-and-rate-limits) |
| `second` | 7 calls per account per second | Same page (429 when exceeded) |

The daily quota is the binding constraint: favor long intervals for inventory-like jobs and check `tapyr plan`. Token creation through the login and authorization-code APIs is limited to one per client ID every 30 minutes; the refresh API used by the plugin has no documented limit. No `Retry-After` is documented for 429: Tapyr applies a 2-second penalty, growing on consecutive 429.

If other integrations use the same account, give Tapyr a share of the quota (`budget.share`); their consumption shows in `tapyr_budget_external_usage` when the headers are synced.

## Simulator

```sh
tapyr sim --provider aruba-central-classic --listen 127.0.0.1:8081
```

8081 is the port of the simulator under `tapyr sim --all --listen-base 127.0.0.1:8081` (`portOffset: 0`). A synthetic account of 3 sites with 8 access points, 2 switches and a gateway each, in the shape of the responses of a real account. The simulator serves the refresh endpoint of the plugin (client `sim-client` / `sim-client-secret`, bootstrap refresh token `sim-refresh-token`, valid once, then rotated like Central; bootstrap access token `sim-access-token`), issues 2-hour access tokens, enforces 7 calls per second and the daily quota, and sends the `X-RateLimit-*-day` headers.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | The daily quota at its floor of 5000 calls, another integration spending 150 calls an hour |
| `bad-day` | Another integration spends 100 calls an hour; at 1h the access tokens are revoked (one refresh); from 2h, 30% of the monitoring requests fail with 503 for 30 minutes; from 3h, slow responses (p50 1 s, p99 8 s) for an hour |

## Dashboard

`dashboards/aruba-central-classic.json` (Grafana, Prometheus data source, site variable): devices up and down, wireless clients by site and access point, switches and gateways, re-bootstrap condition, daily quota (limit, used by Tapyr and by others, remaining).

## Known caveats

- The access point list carries no uptime, and the gateway list no access point count: the package has no metric for them.
- The `role` of a gateway can be empty.
- Whether a refresh token stays valid for a while after a refresh is not documented: the plugin treats it as spent at once.
- The CPU, memory and uptime of switches and gateways (in their lists) are not collected yet.
