# juniper-mist

Juniper Mist cloud REST API: access points, switches and gateways (WAN edge) of a Mist organization, their sites and alarms.

**Validation status**: simulator only. The package is built from the Mist documentation (rate limits, pagination, regional clouds) and the [Mist OpenAPI description](https://github.com/mistsys/mist_openapi) (2610.1.0, November 2026), and tested against a simulator in CI; it has not been run against a real Mist organization yet. A real validation needs an organization, an organization API token with a read-only role, one run of `examples/mist.yaml`, and anonymized recordings (`tapyr record`) compared with the fixtures. Reports are welcome.

## Coverage

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `mist-devices` | `GET /api/v1/orgs/{orgId}/stats/devices?type=all` | 5m | 1 per 1000 devices (12 an hour) | `mist_device_up`, `mist_device_uptime_seconds`, `mist_device_clients`, `mist_device_cpu_usage_ratio`, `mist_device_memory_usage_ratio`, `mist_device_info` (name, type, model, serial, version, site) |
| `mist-sites` | `GET /api/v1/orgs/{orgId}/stats/sites` | 15m | 1 per 1000 sites (4 an hour) | `mist_site_devices` and `mist_site_devices_connected` (by `type`: ap, switch, gateway), `mist_site_clients`, `mist_site_info` (name, country code, time zone) |
| `mist-alarms` | `GET /api/v1/orgs/{orgId}/alarms/count?distinct=severity&duration=1d` | 15m | 1 (4 an hour) | `mist_alarms_last_day` (by `severity`: critical, warn, info) |

- Organization-level endpoints only: the cost does not grow with the number of sites, as the Mist documentation recommends for large deployments.
- Device measures carry the device `mac` (the identifier of a Mist device), sites the `site_id`; names are in the `_info` metrics. No client MAC or IP address is collected.
- `mist_device_clients` exists for access points only. `mist_device_cpu_usage_ratio` comes from `cpu_util` on access points and from the idle share of `cpu_stat` on switches and gateways; `mist_device_memory_usage_ratio` from `mem_used_kb / mem_total_kb` on access points and from `memory_stat.usage` on switches and gateways.
- `mist_alarms_last_day` is a rolling count of the alarms raised over the last 24 hours, acknowledged ones included.
- Lists are read 1000 items per page (`limit` and `page`, the documented maximum). The pagination stops on the first page shorter than 1000 items: when a list holds an exact multiple of 1000, one more (empty) page is requested.

## Auth setup

Mist API tokens are sent as `Authorization: Token <key>` (the `header` auth type with `format: "Token {value}"`).

1. In the Mist portal, Organization > Settings > API Token: create an **organization token** with a read-only role (Observer). It is not bound to a user and only works for its organization.
2. Give Tapyr the token as the `apiToken` secret, the organization ID (Organization > Settings) as the `orgId` param, and the API host of your cloud as `baseURL` (see `examples/mist.yaml`).

A user token works too, but its rate limit is shared with every token of the user and with their portal sessions (see below).

### Regional clouds

The API host depends on the cloud of the organization: replace `manage` by `api` in the address of the portal ([endpoints by global region](https://networkingdocs.hpe.com/documentation/us/en/software/mist/automation-integration/topics/topic-map/api-endpoint-url-global-regions.html)).

| Cloud | `baseURL` |
|---|---|
| Global 01 (default) | `https://api.mist.com` |
| Global 02 | `https://api.gc1.mist.com` |
| Global 03 | `https://api.ac2.mist.com` |
| Global 04 | `https://api.gc2.mist.com` |
| Global 05 | `https://api.gc4.mist.com` |
| EMEA 01 | `https://api.eu.mist.com` |
| EMEA 02 | `https://api.gc3.mist.com` |
| EMEA 03 | `https://api.ac6.mist.com` |
| EMEA 04 | `https://api.gc6.mist.com` |
| APAC 01 | `https://api.ac5.mist.com` |
| APAC 02 | `https://api.gc5.mist.com` |
| APAC 03 | `https://api.gc7.mist.com` |

## Limits

| Limit | Value | Source |
|---|---|---|
| `hourly` (quota) | 5000 calls per hour, reset at the hourly boundary (aligned on the clock, UTC), per API token (`scope: credential`) | [API rate limiting](https://www.mist.com/documentation/api-rate-limiting/), [Org API token](https://www.mist.com/documentation/org-api-token/), [RESTful API overview](https://networkingdocs.hpe.com/documentation/us/en/software/mist/automation-integration/topics/concept/restful-api-overview.html) |

- An organization token is limited on its own: another token reaching its limit does not affect it. A user token counts for the whole user account, other tokens and portal logins included: Tapyr only sees its own calls, so give it a dedicated organization token, or a share of the quota (`budget.share`) when the token is shared.
- Exceeded, the API answers 429 ([REST API HTTP response codes](https://networkingdocs.hpe.com/documentation/us/en/software/mist/automation-integration/topics/concept/rest-api-http-response-codes.html)). No `Retry-After` and no rate-limit headers are documented: Tapyr applies a 60-second penalty, growing on consecutive 429.
- No per-second or concurrency limit is documented, and none is declared.
- The login API is rate-limited after three failures; the package does not use it.

With the defaults, an organization of up to 1000 devices and 1000 sites spends about 20 calls an hour (12 for the devices, 4 for the sites, 4 for the alarms), far from the 5000 of the quota; each further 1000 devices adds 12 calls an hour.

## Simulator

```sh
tapyr sim --provider juniper-mist --listen 127.0.0.1:8086
```

8086 is the port of the simulator under `tapyr sim --all --listen-base 127.0.0.1:8081` (`portOffset: 5`). A synthetic organization (`orgId` free) of 3 sites with 6 access points, 2 switches and a gateway each, in the response shape of the Mist OpenAPI description. It accepts the token `sim-mist-token` (as `Authorization: Token sim-mist-token`) and enforces the hourly quota. It does not send `X-Page-Total`.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | The token is shared with a script spending 4990 calls an hour: about 10 calls are left, fewer than the defaults need |
| `bad-day` | From 1h, slow responses (p50 800 ms, p99 5 s) for an hour; from 2h, 30% of the statistics requests fail with 503 for 30 minutes; at 4h, a 10-minute outage |

## Dashboard

`dashboards/juniper-mist.json` (Grafana, Prometheus data source, site variable): devices connected and disconnected, wireless clients, alarms of the last 24 hours by severity, clients and disconnected devices by site, busiest access points, CPU and memory of the busiest devices, hourly quota (limit, used by Tapyr and by others, remaining).

## Known caveats

- Simulator-validated only (see above). Paths and fields come from the Mist OpenAPI description, a secondary source: the official API reference cannot be read without a browser.
- The device and site statistics carry more than the package collects (radio, port and interface statistics, WAN uplinks): not collected yet.
- The alarm counts are organization-wide: the site variable of the dashboard does not filter them.
