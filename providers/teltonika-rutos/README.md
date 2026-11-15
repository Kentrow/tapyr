# teltonika-rutos

Local REST API of Teltonika routers running RutOS: system, mobile modems, interfaces and failover, polled on each router.

**Validation status**: built from the official OpenAPI descriptions of the RutOS API (7.6 to 7.26.1), tested against a real router (Teltonika CAP7xx, RutOS 07.24.5, November 2026: every template, 30 metric families, login and logout) and against the simulator in CI. The documentation describes `load` as percentages; the router reports load averages (0.42), which the metrics expose as such.

## Requirements

- RutOS **07.06 or later** (the REST API does not exist before; [Teltonika community](https://community.teltonika.lt/t/code-122-error-endpoint-not-implemented/7023)). The legacy ubus JSON-RPC API is not supported.
- A RutOS user, ideally in a group whose ACL grants read access only (GET requests map to the read permission).
- HTTPS access from Tapyr to the router (the default from 07.12 on RUTX and 07.15 on other models). Routers use self-signed certificates: give Tapyr their certificates with `tls.caFile`, or set `tls.insecureSkipVerify: true` explicitly (logged as a warning).

## Coverage

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `rutos-system` | `GET /api/system/device/usage/status` | 1m | 1 per router | `rutos_system_uptime_seconds`, `rutos_system_load1`, `rutos_system_load5`, `rutos_system_load15` (load averages), `rutos_memory_total_bytes`, `rutos_memory_free_bytes`, `rutos_flash_total_bytes`, `rutos_flash_free_bytes` |
| `rutos-device` | `GET /api/system/device/status` | 30m | 1 per router | `rutos_device_info` (model, product code, firmware, hostname, serial, hardware revision) |
| `rutos-modems` | `GET /api/modems/status` | 1m | 1 per router | `rutos_modem_connected`, `rutos_modem_rssi_dbm`, `rutos_modem_rsrp_dbm`, `rutos_modem_rsrq_db`, `rutos_modem_sinr_db`, `rutos_modem_signal_quality_ratio`, `rutos_modem_temperature_celsius`, `rutos_modem_received_bytes_total`, `rutos_modem_transmitted_bytes_total`, `rutos_modem_registered`, `rutos_modem_roaming`, `rutos_modem_sim_inserted`, `rutos_modem_active_sim`, `rutos_modem_info` (model, operator, network type, band) |
| `rutos-interfaces` | `GET /api/interfaces/status` | 1m | 1 per router | `rutos_interface_up`, `rutos_interface_uptime_seconds`, `rutos_interface_received_bytes_total`, `rutos_interface_transmitted_bytes_total`, `rutos_interface_received_packets_total`, `rutos_interface_transmitted_packets_total`, `rutos_interface_info` |
| `rutos-failover` | `GET /api/failover/status` | 1m | 1 per router | `rutos_failover_interface_online`, `rutos_failover_interface_enabled`, `rutos_failover_interface_score`, `rutos_failover_interface_lost_probes` |

- Every sample has a `router` label: the name of the target.
- Memory and flash sizes are reported by RutOS in MB and converted to bytes.
- Offline modems (no SIM, disabled) are skipped.
- The signal quality computed by RutOS (a percentage) is exposed as a ratio from 0 to 1.
- States that change go to gauges, not to `rutos_modem_info`: `rutos_modem_registered` (1 registered on a network, home or roaming), `rutos_modem_roaming` (1 roaming), `rutos_modem_sim_inserted` (1 when RutOS reports the SIM `Inserted`) and `rutos_modem_active_sim` (the SIM slot in use, 1 or 2).
- RutOS renamed some modem fields in 7.12 (`signal` to `rssi`, `oper` to `operator`, `state` to `data_conn_state`, `netstate` to `operator_state`); the templates read the new names and fall back to the old ones.
- Interface and modem byte counters are native counters (`type: counter`): they reset when the router reboots, which `rate()` handles.

## Auth setup

[Configure your accounts](../../docs/configure-accounts.md) walks through a configuration of your own: the account, its jobs, an output and the checks before the first request.

Give Tapyr the `username` and `password` of the RutOS user as secrets, and the address of each router as the `baseURL` param of its target (see `examples/rutos.yaml`).

`session_login`: `POST /api/login` with the username and password answers a bearer token valid 5 minutes (`api_session_timeout`, 300 seconds by default). Tapyr keeps one session per router (`perTarget`), renews it 30 seconds before it expires and **logs the replaced session out** (`POST /api/logout`), and logs out every session when it stops: RutOS keeps sessions open after their token expires and its memory grows when a client logs in repeatedly ([Teltonika community](https://community.teltonika.lt/t/memory-leak-in-rutm52-with-rutm-r-00-07-24-1/20400)).

## Limits

| Limit | Value | Source |
|---|---|---|
| `rate` | 2 requests per second per router, burst 2 | Assumed: no limit is documented, routers are resource-constrained |
| `concurrency` | 2 concurrent requests per router | Assumed |

With the default intervals, a router answers about 4 requests per minute plus a login every 4.5 minutes.

## Simulator

```sh
tapyr sim --provider teltonika-rutos --listen 127.0.0.1:8085
```

8085 is the port of the simulator under `tapyr sim --all --listen-base 127.0.0.1:8081` (`portOffset: 4`). The simulator serves one router (a RUTX50 with a 5G modem, a second modem without SIM, LAN, wired WAN and a mobile backup in the failover) under `/api`, with values moving over simulated time. It accepts the login `sim-user` / `sim-password`, issues 5-minute tokens and serves the logout. Every target of an account reaches the same simulated router.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | RutOS has no quota: a router at the limit of its resources, slow (p50 1.5 s, p99 8 s) and answering 20% of the requests with 503 |
| `bad-day` | At 1h, the router reboots: 5 minutes without answer, then every session opened before is rejected; from 2h, half of the modem requests fail with 500 for 30 minutes |

## Dashboard

`dashboards/teltonika-rutos.json` (Grafana, Prometheus data source, router variable): uptime, load average, memory and flash, router inventory, mobile signal and throughput, interface throughput, failover state. RutOS has no quota: the dashboard has no API quota row.

## Known caveats

- Payloads may change between firmware versions; the e2e test and `tapyr provider lint` catch it for the versions tested.
- `tls.caFile` applies to the account: one bundle holding the certificates of every router of the account.

## End-to-end test

```sh
TAPYR_E2E_RUTOS_URL=https://192.0.2.10 TAPYR_E2E_RUTOS_USERNAME=... TAPYR_E2E_RUTOS_PASSWORD=... \
  go test -tags e2e -run TestTeltonikaRutOS ./test/e2e/
```

It runs every template once against the router (certificate verification skipped unless `TAPYR_E2E_RUTOS_CA_FILE` is set) and checks that the samples have the expected shape and that the login succeeds.
