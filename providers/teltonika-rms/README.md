# teltonika-rms

Teltonika [Remote Management System](https://rms.teltonika-networks.com) (RMS) cloud API: status, mobile signal and data usage of the devices managed in RMS.

**Validation status**: tested against the real platform (November 2026, RMS API v3-BETA), and against the simulator in CI. The RMS API is in beta: Teltonika announces additive changes that may still break clients.

## Coverage

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `rms-devices` | `GET /devices` (offset pagination, 100 per page, `fields` selected) | 5m | 1 per 100 devices | `rms_device_up`, `rms_device_last_connection_timestamp_seconds`, `rms_device_last_update_timestamp_seconds`, `rms_device_uptime_seconds`, `rms_device_temperature_celsius`, `rms_device_mobile_rssi_dbm`, `rms_device_mobile_rsrp_dbm`, `rms_device_mobile_rsrq_db`, `rms_device_mobile_sinr_db`, `rms_device_mobile_connected`, `rms_device_mobile_roaming`, `rms_device_info`, `rms_device_mobile_info`, `rms_devices` |
| `rms-device-data-usage` | `GET /devices/{id}/data-usage` for today (UTC), one instance per device | 6h | 1 per device | `rms_device_mobile_received_bytes_today`, `rms_device_mobile_transmitted_bytes_today` |

- The device list carries the mobile fields of the first modem (signal, RSRP, RSRQ, SINR, operator, connection type and state, home or roaming), so the radio metrics cost no extra request. The fields of a second modem (`*_2`) are not collected yet.
- `rms_device_up` is 1 for `status` 1 (online), 0 for offline (0, null) and pending (2).
- The mobile states are gauges: `rms_device_mobile_connected` is 1 when `connection_state` is `Connected`, `rms_device_mobile_roaming` is 1 when `network_state` is `Roaming` (0 for `Home` or `Searching`). `rms_device_mobile_info` keeps the operator and the connection type, and is absent while the device has no service (operator `N/A`).
- Data usage values are today's totals since 00:00 UTC per modem and SIM (window values exposed as gauges, see ADR 0009); a device without usage today has no sample.
- Device identifiers are the RMS `id`, in the `device_id` label of every device metric; the data usage metrics also carry `modem` and `sim`. `rms_device_info` carries the name, serial, model, firmware and company, to join with `* on (device_id) group_left (name) rms_device_info`.
- RMS shows what devices report, at their own monitoring interval (`dynamic_monitoring`, often 300 seconds). Polling the device list faster than that only spends quota: the default interval is 5 minutes.

## Auth setup

[Configure your accounts](../../docs/configure-accounts.md) walks through a configuration of your own: the account, its jobs, an output and the checks before the first request.

1. In RMS, enable two-factor authentication on the account (required for tokens).
2. Account settings > API > Access tokens: create a personal access token with the `devices:read` scope. It is shown once.
3. Give it to Tapyr as the `token` secret, for example from an environment variable (see `examples/rms.yaml`).

## Limits

| Limit | Value | Source |
|---|---|---|
| `monthly` (quota) | 100000 requests per month; `max: auto`, synced from `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` | Documented: [RMS web API](https://developers.rms.teltonika-networks.com/pages/web-api.html) ("100 000 requests per month", 429 with `Retry-After`). Measured on 2026-11-03: the headers are present on every response (not documented), and the reset was one month after the first request of the window, not the start of a calendar month; the reset header sets the window end |
| `rate` | 2 requests per second, burst 4, per token | Assumed: no rate is documented |
| `concurrency` | 2 concurrent requests per token | Assumed |

Quota maths: 100000 requests a month is about 3300 a day. With the defaults, `rms-devices` spends 288 requests a day per 100 devices; `rms-device-data-usage` adds 4 per device and day. `tapyr plan` shows the projection.

The quota is scoped by credential: accounts sharing a token share it. If other tools use the same token, give Tapyr a share of it (`budget.share`).

## Simulator

```sh
tapyr sim --provider teltonika-rms --listen 127.0.0.1:8084
```

8084 is the port of the simulator under `tapyr sim --all --listen-base 127.0.0.1:8081` (`portOffset: 3`). The fixtures follow the shape of anonymized recordings of the real API (`tapyr record`), over a synthetic fleet of 12 devices whose status, signal and temperature move over simulated time. The simulator accepts the token `sim-rms-token`, enforces the limits above and sends the three `X-RateLimit-*` headers.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | 3000 requests left in the month, another client spending 20 an hour |
| `bad-day` | From 1h, slow responses (p50 800 ms, p99 6 s) for an hour; from 2h, 30% of `/devices` requests fail with 503 for 30 minutes; at 4h, a 10-minute outage |

## Dashboard

`dashboards/teltonika-rms.json` (Grafana, Prometheus data source, model variable): fleet status, offline and stale devices, roaming, temperature, mobile signal, data usage, monthly quota (limit, used by Tapyr and by others, remaining, from Tapyr self-metrics).

## Known caveats

- The API is a beta: field names may change; `tapyr provider lint` and the e2e test catch it.
- Only the first modem is collected.
- `last_connection_at` and `last_update_at` are parsed as UTC, as documented.
- The monthly quota window is learned from the reset header; before the first response, Tapyr assumes a calendar month.

## End-to-end test

```sh
TAPYR_E2E_RMS_TOKEN=... go test -tags e2e -run TestTeltonikaRMS ./test/e2e/
```

It lists the devices once, checks that the samples have the expected shape and that the quota headers are learned, and spends one request per 100 devices plus one data-usage request per device.
