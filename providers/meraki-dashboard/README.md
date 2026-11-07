# meraki-dashboard

Cisco Meraki [Dashboard API v1](https://developer.cisco.com/meraki/api-v1/): device availability, appliance uplinks and wireless clients of every organization the API key can read.

**Validation status**: tested against a real Dashboard (a DevNet sandbox organization, November 2026: every template, with 1 network and 4 devices: appliance, switch, access point and camera), and against a synthetic simulator in CI. The package is built from the official documentation and [OpenAPI description](https://github.com/meraki/openapi) (v1.75.1). The appliance of the sandbox reports no uplink, so `meraki-appliance-uplink-statuses` was checked on the shape of the response only: reports from organizations with active uplinks are welcome.

## Coverage

All metric jobs use **organization-wide endpoints**: one request per organization and job (one page per 1000 items), whatever the number of networks and devices.

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `meraki-organizations` | `GET /organizations` | 1h | 1 per account | `meraki_organization_api_enabled`, `meraki_organization_info`; inventory of the other jobs |
| `meraki-networks` | `GET /organizations/{organizationId}/networks` | 1h | 1 per organization (per 1000 items) | `meraki_network_info` (name, product types, time zone) |
| `meraki-devices` | `GET /organizations/{organizationId}/devices` | 1h | 1 per organization (per 1000 items) | `meraki_device_info` (name, model, firmware, product type, network) |
| `meraki-device-availabilities` | `GET /organizations/{organizationId}/devices/availabilities` | 5m | 1 per organization (per 1000 items) | `meraki_device_up` (online or alerting), `meraki_device_alerting` |
| `meraki-appliance-uplink-statuses` | `GET /organizations/{organizationId}/appliance/uplink/statuses` | 5m | 1 per organization (per 1000 items) | `meraki_appliance_uplink_active`, `meraki_appliance_uplink_up` (active or ready), per appliance and interface |
| `meraki-wireless-client-counts` | `GET /organizations/{organizationId}/wireless/clients/overview/byDevice` | 5m | 1 per organization (per 1000 items) | `meraki_wireless_device_clients` (clients connected now, per access point) |

- `meraki-device-availabilities` replaces `devices/statuses`, deprecated in October 2024 ([deprecated operations](https://developer.cisco.com/meraki/api-v1/deprecated-operations/)); Meraki refreshes availabilities every 5 minutes, hence the default interval.
- Measures carry `organization_id`, `network_id` and `serial`, and `meraki_device_up` and `meraki_device_alerting` also carry `product_type` (`appliance`, `switch`, `wireless`...); names, models and firmware are in the `_info` metrics. No client MAC address is collected.
- Uplink IP addresses are not exposed.

## Auth setup

Create an API key in the Dashboard (My profile > API access), ideally for a read-only administrator of the organizations to monitor, and give it to Tapyr as the `apiKey` secret. It is sent as `Authorization: Bearer` ([authorization](https://developer.cisco.com/meraki/api-v1/authorization/)). For regional Dashboards, set the `baseURL` param (`https://api.meraki.ca/api/v1`, `https://api.meraki.cn/api/v1`, `https://api.meraki.in/api/v1`, `https://api.gov-meraki.com/api/v1`). The Dashboard may redirect a request to the shard of the organization (`n123.meraki.com`): the package allows redirects to the hosts of the Meraki domains (`redirects.hosts`), and Tapyr sends the key there too, each redirect being a request reserved in the budget.

## Limits

| Limit | Value | Source |
|---|---|---|
| `org-rate` | 10 requests per second per organization, burst 20 (10 extra in the first second), on the requests carrying `organizationId` | [Rate limit](https://developer.cisco.com/meraki/api-v1/rate-limit/): "up to 10 requests per second per organization", "an extra 10 requests in the first second", shared across all API applications of the organization |
| `ip-rate` | 100 requests per second per Tapyr instance | Same page: "up to 100 requests per second from each source IP address" |
| `ip-concurrency` | 10 concurrent requests per Tapyr instance | **Assumed**: not documented officially, commonly reported by the community |

429 responses carry `Retry-After`, honored by the budget engine. There is no daily quota: the per-organization rate is the constraint, and it is shared with the other applications using the API on the same organizations (the official SDK keeps 1 request per second for them). The example gives Tapyr half of it (`budget: { share: 0.5, scaleRates: true }`).

Quota maths with the defaults, one page per 1000 items: each organization costs 3 requests per 5 minutes (`meraki-device-availabilities`, `meraki-appliance-uplink-statuses`, `meraki-wireless-client-counts`), 36 an hour, plus 2 an hour (`meraki-networks`, `meraki-devices`); the account adds 1 an hour (`meraki-organizations`). An account with 2 organizations sends 77 requests an hour, far below the per-organization rate: the rate matters for bursts and for the share left to other applications. `tapyr plan` shows the projection.

## Simulator

```sh
tapyr sim --provider meraki-dashboard --listen 127.0.0.1:8083
```

8083 is the port of the simulator under `tapyr sim --all --listen-base 127.0.0.1:8081` (`portOffset: 2`). A synthetic Dashboard of 2 organizations, each with 4 networks of 10 devices (an MX appliance, three MS switches, six MR access points), generated from a seed. Statuses flap every 5 minutes, client counts and appliance uplinks move over simulated time. The simulator accepts the key `sim-meraki-key`, paginates with `Link` headers and enforces the limits above.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | No daily quota on Meraki: other applications spend most of the per-organization rate, a third of the organization requests get 429 |
| `bad-day` | From 1h, slow responses (p50 600 ms, p99 4 s) for an hour; from 2h, 25% of the organization requests fail with 502 for 30 minutes; at 4h, a 10-minute outage |

## Dashboard

`dashboards/meraki-dashboard.json` (Grafana, Prometheus data source, organization and network variables): devices up and down, alerting, appliance uplinks and networks on a backup uplink, wireless clients, API requests and 429 (Tapyr self-metrics). The network names come from `meraki_network_info`: run the `meraki-networks` job for the network variable and the joins.

## Known caveats

- The uplink metrics are validated against the simulator only (see above).
- Devices without a name have no `name` label on `meraki_device_info`: Tapyr leaves out empty label values, as Prometheus does.
- `meraki-wireless-client-counts` needs access points; organizations without wireless return no item.
- Networks bound to configuration templates are reported as returned by the API.

## End-to-end test

```sh
TAPYR_E2E_MERAKI_API_KEY=... go test -tags e2e -run TestMerakiDashboard ./test/e2e/
```

It runs every template once against the organizations of the key (a read-only key is enough, for example the one of a DevNet sandbox), checks that the organization, network and device samples have the expected shape and that the device jobs succeed, and spends one request per organization and template, plus one for the organizations. A key generated a minute before can still be refused (`401`): wait a little and run it again.
