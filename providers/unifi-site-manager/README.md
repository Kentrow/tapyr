# unifi-site-manager

UniFi Site Manager API v1, the cloud API of Ubiquiti: the consoles, sites, devices and ISP metrics of every console of a UI account, with one API key. It suits a fleet of consoles (an MSP, several offices): one key reads them all through `api.ui.com`, without access to each console.

**Validation status**: real platform. Run against a real UI account on 2026-11-13, strictly read-only, with an API key limited to the Site Manager scope: one console (UniFi OS 5.1, UniFi Network 10.6) with two WAN interfaces. The hosts, sites, devices and 5-minute ISP metrics templates ran with `tapyr run`, and the units were checked against the UniFi interface. The hourly ISP metrics endpoint was called but not run as a job, and the pagination over several pages is simulator-only (the account has one console). The package is built from the [Getting Started page](https://developer.ui.com/site-manager/v1.0.0/gettingstarted) and the [OpenAPI description](https://developer.ui.com/site-manager/v1.0.0/openapi.json) (Site Manager API 1.0.0).

Not covered: the local API of the consoles, and the old session API of the UniFi Network application (which [UniFi Poller](https://github.com/unpoller/unpoller) reads, with the detail of each client and port). This package gives the cloud view of the fleet.

## Coverage

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `unifi-hosts` | `GET /v1/hosts` | 15m | 1 per 100 consoles | `unifi_host_info` (type, name, model, version, release channel), `unifi_host_connected`, `unifi_host_blocked`, `unifi_host_state_change_timestamp_seconds`, `unifi_host_last_backup_timestamp_seconds` |
| `unifi-sites` | `GET /v1/sites` | 5m | 1 per 100 sites | `unifi_site_info` (name, timezone, gateway model), `unifi_site_devices` and `unifi_site_devices_offline` (by kind: gateway, wifi, wired), `unifi_site_devices_pending_update`, `unifi_site_clients` (wifi, wired), `unifi_site_guest_clients`, `unifi_site_critical_notifications`, `unifi_site_internet_issues`, `unifi_site_internet_uptime_ratio`, `unifi_site_wifi_tx_retry_ratio`; per WAN interface: `unifi_site_wan_up`, `unifi_site_wan_uptime_ratio`, `unifi_site_wan_info` (ISP name and ASN) |
| `unifi-devices` | `GET /v1/devices` | 5m | 1 per 100 consoles | `unifi_device_info` (name, model, product line, version, firmware status, console), `unifi_device_up`, `unifi_device_update_available`, `unifi_device_boot_timestamp_seconds` |
| `unifi-isp-metrics` | `GET /v1/isp-metrics/5m?beginTimestamp=<now - 1h>` | 5m | 1 | Per site and WAN interface, in 5-minute windows: `unifi_isp_latency_avg_seconds`, `unifi_isp_latency_max_seconds`, `unifi_isp_packet_loss_ratio`, `unifi_isp_uptime_ratio`, `unifi_isp_expected_download_bits_per_second`, `unifi_isp_expected_upload_bits_per_second` (label `period="5m"`) |
| `unifi-isp-metrics-hourly` | `GET /v1/isp-metrics/1h?beginTimestamp=<now - 4h>` | 1h | 1 | The same metrics in hourly windows (label `period="1h"`), optional |

- **ISP metrics are published late.** The 5-minute windows reach the API about 30 minutes after they end (2 to 3 hours for the hourly ones). Each run reads the last hour (4 hours for the hourly template), and every sample carries the start of its window (`metricTime`) as its source timestamp. The windows already sent are deduplicated: a 5-minute job sends about one new window per run, and a missed run leaves no gap.
- **The expected speeds are configured, not measured.** `download_kbps` and `upload_kbps` of the ISP metrics are the expected ISP speeds set on the WAN interface in UniFi Network (Settings > Internet, the expected ISP speeds of the WAN). They are neither the traffic nor the result of a speed test, and they stay constant until someone changes them (checked on a real account).
- **Units.** The latency is in milliseconds in the API (seconds here), the uptime and the packet loss are integer percentages (ratios here). `downtime` is not mapped: its unit is not documented, and the real account only showed zeros.
- **WAN interfaces** are named after their interface (`wan`, `wan2`), as the API gives them, never after the name or the ISP set by the user; the ISP of each is in `unifi_site_wan_info`.
- **Devices** are those of the consoles where the account is owner or super admin (as documented). Their identifier (`device_id`) is the device MAC address, as the API names it. Descriptive attributes are in `unifi_device_info`; no IP address and no note (free text) are collected.
- **Not collected**: the public address of the consoles and of the WAN interfaces, MAC addresses of consoles and gateways, serial numbers, the location of the consoles, the owner (email, name) and the free-text description of the sites, all present in the responses. No client is collected (the API gives counts only).
- **Not covered yet**: the SD-WAN configurations and their status (`/v1/sd-wan-configs`, one request per configuration), and the connector (`/v1/connector/consoles/{id}/...`), which reaches the local APIs of a console and also accepts writes.

## Auth setup

The API authenticates with an API key in the `X-API-Key` header.

1. Sign in to [unifi.ui.com](https://unifi.ui.com) with an account that owns or administers the consoles.
2. Open Settings > API Keys and create a key. Keep only the **Site Manager** scope: the UniFi Applications scope also manages the settings of Network and Protect, which Tapyr never needs. Pick an expiration that suits your rotation.
3. Copy the key (it is shown once) and give it to Tapyr as the `apiKey` secret (see `examples/unifi.yaml`).

The key sees every console of the account. To collect a subset, use another account with access to that subset only.

## Limits

| Limit | Value | Source |
|---|---|---|
| `rate` | 10,000 requests a minute, per API key (assumed scope) | [Getting Started, Rate Limiting](https://developer.ui.com/site-manager/v1.0.0/gettingstarted) |

- The documentation gives the rate per API version (10,000 a minute for the v1 stable release, 100 for Early Access endpoints, which no template uses) without its scope: the package assumes it is per key (`credential`). Accounts sharing a key share it.
- Exceeded, the API answers 429 with `Retry-After` in seconds, which Tapyr follows. No quota header is sent (checked on a real account), so there is nothing to sync.
- The connector has its own limit (100 requests a minute per console), unused by the package.

With the defaults, an account with up to 100 consoles spends 4 requests every 5 minutes and one every 15 minutes, about one request a minute: the limit is never in sight.

## Simulator

```sh
tapyr sim --provider unifi-site-manager --listen 127.0.0.1:8088
```

8088 is the port of the simulator under `tapyr sim --all --listen-base 127.0.0.1:8081` (`portOffset: 7`). A synthetic account of 3 consoles, each with one site and 6 devices (a gateway, 2 switches, 3 access points), one site with two WAN interfaces, in the response shapes of the OpenAPI description and of the real responses. It accepts the key `sim-unifi-key`, enforces the rate limit, and serves ISP windows published late (30 minutes for 5-minute windows, 3 hours for hourly ones).

Known gaps of the simulator: the cursor of the API is in the body (`nextToken`), which the cursor pagination of the simulator does not serve, so the fixtures of the lists paginate themselves, 2 consoles a page, whatever `pageSize` asks; the ISP metrics fixture returns the windows of the range the templates request, whatever `beginTimestamp` says.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | Other integrations of the account spend most of the rate limit: a quarter of the requests get 429 |
| `bad-day` | From 1h, slow responses (p50 800 ms, p99 5 s) for an hour; from 2h, 30% of the ISP metrics requests fail with 502 for 30 minutes; at 4h, a 10-minute outage |

## Dashboard

`dashboards/unifi-site-manager.json` (Grafana, Prometheus data source, site variable): consoles and disconnected consoles, devices offline, firmware updates, clients, WAN ports down; average latency and packet loss by WAN interface, WAN port states, the internet connections with their ISP and expected speeds; clients and Wi-Fi retries by site, the devices offline, the consoles; the requests of Tapyr and the 429 responses of the API.

## Known caveats

- The statistics of the sites and the `reportedState` of the consoles vary with the UniFi Network and UniFi OS versions (as documented): every field is optional, and a missing one skips its sample.
- `pageSize` counts consoles on `/v1/devices` (each with all its devices), not devices. No maximum page size is documented: the package asks for 100.
- The ISP metrics of the dashboards read back 2 hours (`last_over_time`), since a window arrives 30 minutes after it ends.
