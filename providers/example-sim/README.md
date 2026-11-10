# example-sim

A vendor-neutral example provider, used for development, tests and demos, and the reference package that new packages copy: small, served by the built-in simulator, every feature exercised (four pagination styles, a three-level discovery chain, quota headers, scenarios).

**Validation status**: simulator only, by design. All fixture data is synthetic.

## Coverage

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `example-sim-sites` | `GET /sites` (offset pagination, 2 items per page) | 1h | 1 per 2 sites | `example_site_devices`, `example_site_info` |
| `example-sim-devices` | `GET /devices` (Link header pagination, 2 items per page) | 5m | 1 per 2 devices | `example_device_up`, `example_device_clients`, `example_device_received_bytes_total`, `example_device_info`, `example_devices` |
| `example-sim-networks` | `GET /sites/{siteId}/networks`, one instance per site (page pagination, 2 items per page) | 30m | 1 per 2 networks of a site | `example_network_info` |
| `example-sim-network-devices` | `GET /sites/{siteId}/networks/{networkId}/devices`, one instance per network (cursor pagination, 2 items per page, next cursor in `X-Next-Cursor`) | 5m | 1 per 2 devices of a network | `example_network_device_up` |

- `example-sim-sites` is also an inventory (`role: both`, one item per site), `example-sim-networks` fans out over it and is the inventory of `example-sim-network-devices`: a three-level discovery chain. A job using `example-sim-network-devices` alone is enough, the two parents are instantiated implicitly.
- `example_devices` is the number of devices returned by the last run.

## Auth setup

Bearer token from the `apiKey` secret. The simulator accepts `sim-api-key`.

## Limits

| Limit | Value | Source |
|---|---|---|
| `account-rate` | 10 requests per second per account | Assumed, example only |
| `concurrency` | 4 concurrent requests per Tapyr instance | Assumed, example only |
| `daily` (quota) | `max: auto`, synced from `X-RateLimit-Limit-Day` and `X-RateLimit-Remaining-Day` (5000 until learned), reset at 00:00 UTC | Assumed, example only |

The limits are illustrative and not sourced from any real API. The simulator enforces them (429 with `Retry-After`) and sends the `X-RateLimit-Limit-Day` and `X-RateLimit-Remaining-Day` headers.

## Simulator

```sh
tapyr sim --provider example-sim --listen 127.0.0.1:8081
```

Unlike the other packages, the example uses 8081 standalone, not the port it gets from `tapyr sim --all --listen-base 127.0.0.1:8081` (8082, `portOffset: 1`): the default `baseURL` of the provider, `examples/example-sim.yaml` and [`examples/config`](../../examples/config/) expect it there.

The simulator serves static fixtures under `/api/v1`: 3 sites, 5 devices, and the networks of each site with their devices.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | The daily quota drops to 600 calls |
| `bad-day` | Another client consumes 60 calls per hour; from 2h to 2h20, 30% of `/devices` requests fail with 503 |
| `quota-jump` | A daily quota of 2000 calls; another client spends 300 of them between 12h and 12h10, then 600 more between 18h and 18h10 |
| `inventory-change` | After 2h, `site-c` is closed, `site-d` opens, `site-a` loses a network and `site-b` gains one |

```sh
tapyr sim --provider example-sim --scenario quota-tight --time-scale 60
```

## Dashboard

`dashboards/example-sim.json` (Grafana, Prometheus data source, site variable): devices online and offline, clients, received throughput, inventory, devices per site, network devices online, daily quota (limit, used by Tapyr and by others, remaining).

## Known caveats

- Not a real API: the limits and payloads only exist in the simulator.
