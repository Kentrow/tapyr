# 10 - Built-in providers

The built-in provider packages. Each section was checked against the vendor documentation (and, where its section says so, the real platform) in November 2026; each package README cites the source of every declared limit.

| Package | Kind of API | Testing | What it validates |
|---|---|---|---|
| `teltonika-rms` | Cloud, central | Real platform (maintainer access) + simulator | The whole pipeline against a real platform |
| `teltonika-rutos` | Local device API, multi-target, session auth | Real devices + simulator | Validates multi-target accounts and `session_login` |
| `meraki-dashboard` | Cloud, per-org rate limits, Link pagination | DevNet sandbox (e2e) + simulator (docs + public OpenAPI spec) | Validates scoped rate limits and discovery chains |
| `aruba-central-classic` | Cloud, daily quota, rotating refresh tokens | Real account, read-only (November 2026) + simulator | Validates quota windows, header sync, auth plugin |
| `juniper-mist` | Cloud, hourly quota per token, regional clouds | Simulator only (docs + public OpenAPI spec) | Validates header auth with a format, page pagination, hourly windows |

---

## teltonika-rms

Teltonika Remote Management System, cloud API. Checked in November 2026 against the [API documentation](https://developers.rms.teltonika-networks.com/pages/web-api.html), its [OpenAPI description](https://api.rms.teltonika-networks.com/openapi/compiled.yaml) (version 3.0.0, "v3-BETA": additive changes that may still break clients) and the real platform.

- **Auth**: personal access token (created under Account settings > API, requires 2FA, shown once) as `Authorization: Bearer <token>`, scope `devices:read` ([authentication](https://developers.rms.teltonika-networks.com/pages/authentication.html), [scopes](https://developers.rms.teltonika-networks.com/pages/scopes.html)). OAuth (authorization code with PKCE) exists for multi-user apps; not needed.
- **Base URL**: `https://api.rms.teltonika-networks.com` (the OpenAPI servers entry `https://rms.teltonika-networks.com/api` serves the same API). No version segment.
- **Rate limits**: documented as 100 000 requests per month per application, `429` with `Retry-After`. Measured on the platform (2026-11-03): every response carries `X-RateLimit-Limit` (100000), `X-RateLimit-Remaining` and `X-RateLimit-Reset` (epoch seconds), and the reset was one month after the first request of the window, not a calendar month. Declared as a `month` quota with `max: auto` synced from the three headers (the reset header sets the window end), plus a conservative `rate: 2/s` (assumed, none documented).
- **Pagination**: `offset` / `limit` (default 10, no documented maximum: the templates use 100), total in `meta.total`. Lists accept `fields=` to select fields and filters such as `status=online`.
- **Envelope**: `{"success": true, "data": [...], "meta": {"total": N}}`; errors `{"success": false, "errors": [{"code": "UNAUTHORIZED", "message": "..."}]}`. Timestamps are `YYYY-MM-DD HH:MM:SS` strings in UTC.
- **Templates** (metrics in the [package README](../../providers/teltonika-rms/README.md)):
  - `rms-devices` (`GET /devices`, inventory and metrics in one paginated call, the mobile signal of the first modem included, so no per-device call is needed);
  - `rms-device-data-usage` (`GET /devices/{id}/data-usage`, one call per device, 6h interval; the README gives the monthly cost per device).
- **Freshness**: RMS data is as fresh as each device reports it (`dynamic_monitoring`, often 300 s); polling faster than that only spends quota.
- **Deliverables**: provider package, simulator fixtures built from anonymized recordings (`tapyr record`), dashboard, e2e test behind the `e2e` build tag.

## teltonika-rutos

Local REST API of Teltonika routers running RutOS. Checked in November 2026 against the [RutOS API documentation](https://developers.teltonika-networks.com) (OpenAPI descriptions per firmware, from 7.6 to 7.26.1).

- **Firmware**: the REST API exists from RutOS 07.06 ([Teltonika community](https://community.teltonika.lt/t/code-122-error-endpoint-not-implemented/7023)); the package targets 07.06 and later. Base path `/api` (the latest API version; `/api/v1` pins it). The legacy ubus JSON-RPC API is not supported.
- **Auth**: `POST /api/login` with `{"username": ..., "password": ...}` answers `{"success": true, "data": {"username": ..., "token": ..., "expires": 299}}`; the token is a bearer token valid 5 minutes by default (`api_session_timeout`). `POST /api/logout` closes the session: RutOS keeps expired sessions open and its memory grows when a client logs in repeatedly ([Teltonika community](https://community.teltonika.lt/t/memory-leak-in-rutm52-with-rutm-r-00-07-24-1/20400)), so the package declares `logout`. Implemented with `session_login`, `perTarget: true`. A dedicated user in a group with read access only is recommended.
- **TLS**: HTTPS with a self-signed certificate is the default from 07.12 (RUTX) and 07.15 (other models); accounts use `tls.caFile` or, explicitly, `tls.insecureSkipVerify`.
- **Rate limits**: none documented; devices are resource-constrained. Declared: `concurrency: 2` and `rate: 2/s` per target (assumed).
- **Envelope**: `{"success": true, "data": ...}`; errors `{"success": false, "errors": [{"code": 120, "error": "...", "source": "...", "section": "..."}]}` (120 unauthorized, 121 login failed). Payloads may change between firmware versions; templates read current field names with fallbacks to the 7.6 names (`signal`, `oper`, `state`).
- **Multi-target**: one Account lists the routers as targets.
- **Templates** (metrics in the [package README](../../providers/teltonika-rutos/README.md)):
  - `rutos-device` (`GET /api/system/device/status`, 30m): model, firmware and names as `rutos_device_info`;
  - `rutos-system` (`GET /api/system/device/usage/status`);
  - `rutos-modems` (`GET /api/modems/status`);
  - `rutos-interfaces` (`GET /api/interfaces/status`);
  - `rutos-failover` (`GET /api/failover/status`).
- **Deliverables**: provider package, fixtures from anonymized recordings, dashboard, e2e test.

## meraki-dashboard

Cisco Meraki Dashboard API v1. Checked in November 2026 against the [developer documentation](https://developer.cisco.com/meraki/api-v1/) and the [official OpenAPI description](https://github.com/meraki/openapi) (v1.75.1).

- **Auth**: API key as `Authorization: Bearer <key>` ([authorization](https://developer.cisco.com/meraki/api-v1/authorization/)).
- **Base URL**: `https://api.meraki.com/api/v1`; regional variants `api.meraki.ca`, `api.meraki.cn`, `api.meraki.in`, `api.gov-meraki.com` (account param `baseURL`).
- **Rate limits** ([rate limit](https://developer.cisco.com/meraki/api-v1/rate-limit/)):
  - per organization: 10 requests per second, token bucket, with 10 more allowed in the first second (30 in two seconds), shared by every application of the organization: `rate: 10/s`, `burst: 20`, scoped by `var:organizationId` on the organization endpoints;
  - per source IP: 100 requests per second, shared by every client on the address: `rate: 100/s` per instance;
  - concurrency: not documented officially (10 per IP is community knowledge): `concurrency: 10` per instance, marked assumed;
  - `429` with `Retry-After`, body `{"errors": ["API rate limit exceeded for organization"]}`; no quota headers.
- **Pagination**: `Link` header (RFC 8288) with `perPage`; the `startingAfter` and `endingBefore` tokens are opaque, the `rel=next` link is followed ([pagination](https://developer.cisco.com/meraki/api-v1/pagination/)).
- **Discovery chain**: organizations -> organization-wide endpoints (one call for every device of an organization), networks only for `_info` labels.
- **Templates** (metrics in the [package README](../../providers/meraki-dashboard/README.md)):
  - `meraki-organizations` (inventory, `GET /organizations`);
  - `meraki-networks` (`forEach` organizations, `GET /organizations/{organizationId}/networks`);
  - `meraki-devices` (`GET /organizations/{organizationId}/devices`);
  - `meraki-device-availabilities` (`GET /organizations/{organizationId}/devices/availabilities`). It replaces `devices/statuses`, deprecated in October 2024 ([deprecated operations](https://developer.cisco.com/meraki/api-v1/deprecated-operations/));
  - `meraki-appliance-uplink-statuses` (`GET /organizations/{organizationId}/appliance/uplink/statuses`);
  - `meraki-wireless-client-counts` (`GET /organizations/{organizationId}/wireless/clients/overview/byDevice`, an enveloped list under `items`).
- **Deliverables**: provider package, synthetic simulator (inventory generator sized by config), fixtures derived from the OpenAPI examples, dashboard, contract tests, and an e2e test run against a DevNet sandbox organization (November 2026, every template; the sandbox appliance reports no uplink, so the uplink metrics are validated against the simulator only).

## aruba-central-classic

HPE Aruba Networking Central (classic platform) REST API. Checked in November 2026 against the [Aruba developer hub](https://developer.arubanetworks.com/aruba-central/) and the official `pycentral` SDK, then against a real Central account, read-only: every template, the token rotation and the quota headers.

- **Auth**: OAuth access tokens valid 2 hours and **rotating refresh tokens** valid 15 days, implemented by the `aruba_central_classic` auth plugin (see `05-collection.md`). Bootstrap: client ID and secret of an API Gateway application and the token pair downloaded from API Gateway > System Apps & Tokens (or My Apps & Tokens).
- **Base URL**: regional API gateway (account param `baseURL`, required), for example `https://apigw-eucentral3.central.arubanetworks.com`; the README lists the clusters. A token only works on the gateway of its cluster.
- **Rate limits** ([usage and rate limits](https://developer.arubanetworks.com/hpe-aruba-networking-central/docs/usage-and-rate-limits)):
  - per account, per second: 7 calls (429 when exceeded);
  - per account, per day: the maximum of 5000 and the sum of 30 calls per Foundation device and 90 per Advanced device; resets at midnight GMT;
  - response headers `X-RateLimit-Limit-day`, `X-RateLimit-Remaining-day`, `X-RateLimit-Limit-second`, `X-RateLimit-Remaining-second`; no `Retry-After` documented;
  - token creation is limited to one per client ID every 30 minutes for the login and authorization-code APIs; the refresh API has no documented limit;
  - declared as `kind: quota`, `period: day`, `resetAt: "00:00"`, `timezone: UTC`, `max: auto` (`initialMax: 5000`) with `sync` headers, plus `kind: rate` `7/s` per account.
- **Pagination**: `offset` / `limit`, maximum 1000 on the monitoring lists, `total` only with `calculate_total=true`.
- **Budget strategy**: the daily quota is the binding constraint. List endpoints with 1000 items per page; no per-device endpoint; inventory jobs with long intervals. `tapyr plan` makes the cost visible.
- **Templates** (metrics in the [package README](../../providers/aruba-central-classic/README.md)):
  - `aruba-aps` (`GET /monitoring/v2/aps?calculate_client_count=true`: the client count comes with the list, at no extra cost);
  - `aruba-switches` (`GET /monitoring/v1/switches`);
  - `aruba-gateways` (`GET /monitoring/v1/gateways`);
  - `aruba-sites` (inventory, `GET /central/v2/sites`).
- **Deliverables**: provider package, synthetic simulator enforcing 7/s and the daily quota with headers, `quota-tight` scenario, dashboard, contract tests. No e2e test (no access).

### Note on Aruba Central "new" (GreenLake-based)

The newer HPE Aruba Networking Central platform uses a different API surface and OAuth client credentials through HPE GreenLake. It will be a separate provider package (`aruba-central`), after v0.1.0. Its client-credentials auth should work with the declarative `oauth2_client_credentials` type, which is a good sign for the abstraction.

---

## juniper-mist

Juniper Mist cloud REST API. Checked in November 2026 against the Mist documentation and the [Mist OpenAPI description](https://github.com/mistsys/mist_openapi) (2610.1.0).

- **Auth**: API token as `Authorization: Token <key>` (`header` auth with `format: "Token {value}"`). An organization token with a read-only role is recommended; a user token works too ([Org API token](https://www.mist.com/documentation/org-api-token/)).
- **Base URL**: the API host of the cloud of the organization (account param `baseURL`, default `https://api.mist.com`), 12 regional hosts ([endpoints by global region](https://networkingdocs.hpe.com/documentation/us/en/software/mist/automation-integration/topics/topic-map/api-endpoint-url-global-regions.html)); the organization ID is the account param `orgId`.
- **Rate limits**:
  - 5000 calls per hour, reset at the hourly boundary ([API rate limiting](https://www.mist.com/documentation/api-rate-limiting/), [RESTful API overview](https://networkingdocs.hpe.com/documentation/us/en/software/mist/automation-integration/topics/concept/restful-api-overview.html));
  - counted per organization token, or for the whole user account with a user token, portal logins included;
  - `429` when exceeded; no `Retry-After` and no rate-limit headers documented; no per-second or concurrency limit documented;
  - declared as `kind: quota`, `period: hour`, `timezone: UTC`, `max: 5000`, `scope: { by: [credential] }`.
- **Pagination**: `limit` (maximum 1000) and `page`, with the total in `X-Page-Total` ([pagination](https://networkingdocs.hpe.com/documentation/us/en/software/mist/automation-integration/topics/concept/rest-api-pagination.html)); the package stops on a short page.
- **Budget strategy**: organization-level statistics only, about 20 calls an hour for an organization of up to 1000 devices.
- **Templates** (metrics in the [package README](../../providers/juniper-mist/README.md)):
  - `mist-devices` (`GET /api/v1/orgs/{orgId}/stats/devices?type=all`: access points, switches and gateways in one list);
  - `mist-sites` (`GET /api/v1/orgs/{orgId}/stats/sites`: device and client counts per site);
  - `mist-alarms` (`GET /api/v1/orgs/{orgId}/alarms/count?distinct=severity&duration=1d`).
- **Deliverables**: provider package, synthetic simulator enforcing the hourly quota, scenarios, dashboard, contract test. No e2e test (no access).

## Provider package checklist (definition of done)

- [ ] `provider.yaml` with every limit sourced (link in README) or marked as measured/assumed.
- [ ] Job templates with `estimate.pagesPerRun` hints.
- [ ] Simulator: routes, fixtures (synthetic or anonymized recordings), `default`, `quota-tight` and `bad-day` scenarios.
- [ ] Contract tests passing (`tapyr provider lint`).
- [ ] Dashboard in `dashboards/`.
- [ ] Example Account and Job documents in `examples/`.
- [ ] README: coverage, auth setup steps, limits with sources, validation status (real platform or simulator only), known caveats.
- [ ] Metric naming reviewed: `<provider_prefix>_<object>_<measure>_<unit>`, base units, `_info` for descriptive attributes, no high-cardinality labels (no client MACs).
