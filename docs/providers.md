# Provider packages

A provider package holds everything Tapyr knows about an API: the `Provider` (base URL, auth, pagination, limits, errors), job templates, a simulator, scenarios, a contract test, a dashboard and examples. Packages are declarative: YAML and CEL, no Go, except for the rare auth flows written as plugins. The layout is described in [`03-configuration.md`](spec/03-configuration.md#provider-packages).

## Built-in packages

<!-- begin provider table: generated from the provider packages by TestProviderTables (go test ./cmd/tapyr -run TestProviderTables -update), do not edit -->

| Package | API | Tier | Validation |
|---|---|---|---|
| [`aruba-central-classic`](../providers/aruba-central-classic/README.md) | HPE Aruba Networking Central (classic platform) REST API | Official | Simulator only |
| [`example-sim`](../providers/example-sim/README.md) | Vendor-neutral example API, served by tapyr sim, for development, tests and demos | Official | Simulator only |
| [`meraki-dashboard`](../providers/meraki-dashboard/README.md) | Cisco Meraki Dashboard API v1 | Official | Simulator only |
| [`teltonika-rms`](../providers/teltonika-rms/README.md) | Teltonika Remote Management System (RMS) cloud API | Official | Real platform (RMS API v3-BETA, 2026-11) and simulator |
| [`teltonika-rutos`](../providers/teltonika-rutos/README.md) | Local REST API of Teltonika routers running RutOS 07.06 or later | Official | Real platform (RutOS 07.24.5, 2026-11) and simulator |

<!-- end provider table -->

`tapyr provider list` lists them with the packages of `providerPaths`, which override built-in packages of the same name.

Looking for an API to package? [Provider ideas](provider-ideas.md) lists candidates in other domains, with their rate-limit patterns and the engine features some of them still need.

## Writing a package

1. Read the vendor documentation and write `provider.yaml`. **Every limit cites its source** in its `source` (`vendor-docs` with the link, `measured` with the date, or `assumed`), and in its `description` and the README for humans. The metadata describes the package in the catalog: `displayName`, `categories` from [`providers/categories.yaml`](../providers/categories.yaml), `maintainers` (GitHub handles), `tier` (`official` or `community`) and `validation` (`simulator`, or `real-platform` with a date and the platform version), see [`03-configuration.md`](spec/03-configuration.md#provider). Prefer endpoints that return many objects per request (organization-wide lists, large pages): the budget engine spends quota deliberately, the templates should too.
2. Write the job templates (`templates/*.yaml`) with `estimate.pagesPerRun`. Metric names follow `<prefix>_<object>_<measure>_<unit>` in base units, descriptive attributes go to `_info` metrics, and no high-cardinality label (client MAC addresses, IPs) is used.
3. Build the simulator (`sim/sim.yaml`): a synthetic inventory with template fixtures (see [`08-simulator.md`](spec/08-simulator.md#synthetic-inventory-and-templated-fixtures)), or static fixtures from a recording. Ship the `default`, `quota-tight` and `bad-day` scenarios.
4. Write the contract test (`tests/contract.yaml`, `tests/contract.env`) and generate `tests/expected.prom` with `tapyr provider lint --update <dir>`. Review the samples: they are the contract of the package.
5. Add a Grafana dashboard (`dashboards/*.json`, with a `datasource` variable, an `API quota` row when the API has a quota, and a link to `Tapyr / Overview`), examples (`examples/*.yaml`) and the README. Every package README follows the same skeleton, as in [`example-sim`](../providers/example-sim/README.md):
   - an introduction (the API, what is collected) with the **Validation status** (real platform or simulator, versions, dates);
   - `Requirements`, when there are (firmware versions, permissions, network access);
   - `Coverage`: a table `Template | Endpoint | Default interval | Requests | Metrics` (requests per run), then the labels, joins and conversions;
   - `Auth setup`: how to get the credentials and give them to Tapyr;
   - `Limits`: a table `Limit | Value | Source`, then the quota maths with the default intervals;
   - `Simulator`: the command, with the port the simulator gets under `tapyr sim --all --listen-base 127.0.0.1:8081` (8081 plus its `portOffset`), the credentials it accepts and a `Scenario | Effect` table;
   - `Dashboard`, `Known caveats`, and `End-to-end test` when there is one.
6. `tapyr provider lint --catalog <dir>` must pass.

## CEL mapping recipes

Patterns taken from the built-in templates. The CEL environment and its functions are described in [`06-mapping-and-outputs.md`](spec/06-mapping-and-outputs.md#cel-environment).

**Enveloped lists.** `extract.items` points to the list inside the envelope (`teltonika-rms`, `aruba-central-classic`):

```yaml
extract:
  items: response.body.data          # {"success": true, "data": [...], "meta": {...}}
```

Paginate such a list in the simulator with `response.itemsField` (see [`08-simulator.md`](spec/08-simulator.md#synthetic-inventory-and-templated-fixtures)).

**Flattening nested lists.** One item per appliance and uplink (`meraki-appliance-uplink-statuses`):

```yaml
items: >-
  response.body.map(a, default(a.uplinks, []).map(u, {'serial': a.serial, 'interface': u.interface, 'status': default(u.status, '')})).flatten()
```

**Absent or null fields.** `default(value, fallback)` covers a missing or null field; `has()` tests a key before reading below it; `skipIf` drops a sample rather than inventing a value (`meraki-dashboard`, `aruba-central-classic`):

```yaml
labels:
  name: "default(item.name, '')"
  network_id: "has(item.network) ? default(item.network.id, '') : ''"
metrics:
  - name: aruba_ap_clients
    value: toFloat(item.client_count)
    skipIf: "!has(item.client_count) || item.client_count == null"
```

**Timestamps.** `parseTimestamp` reads RFC 3339 by default, or a Go layout; a timestamp metric is a number of seconds (`rms-devices`):

```yaml
value: "double(int(parseTimestamp(item.last_connection_at, '2006-01-02 15:04:05')))"
```

As the `timestamp` of a sample, it carries the source time of the value (`example-sim-devices`): `timestamp: parseTimestamp(item.updatedAt)`.

**`_info` metrics.** Descriptive attributes go to an `_info` metric (value 1) that shares an identifier label with the measures, so that queries join them (`meraki-devices`):

```yaml
info:
  - name: meraki_device_info
    labels:
      serial: item.serial
      name: "default(item.name, '')"
      model: "default(item.model, '')"
```

```promql
(meraki_device_up == 0) * on (serial) group_left (name, model) meraki_device_info
```

## Recording real responses

`tapyr record` runs jobs once against the real API, through the budget engine, and writes their responses anonymized:

```sh
tapyr record --config ./config --job devices --out ./recording [--anonymize ./anonymize.yaml]
```

- Serials, MAC and IP addresses, identifiers, names, emails, phone numbers, coordinates and addresses are replaced by consistent pseudonyms that keep their format (a MAC stays a MAC, an identifier keeps its length and its digits); the same value gets the same pseudonym in every page and in the request variables, so the recording stays consistent.
- `anonymize.yaml` adds rules and exceptions:

  ```yaml
  replace:
    - { path: "data[*].company_name", as: name }   # serial, digits, mac, ipv4, ipv6, email, hostname, name, id, coordinate, address, redact
  keep:
    - "data[*].model"
  ```

- `report.txt` lists every replaced path and rule, never a value. **Review the recording before committing anything**: free-text fields can still hold identifying data.
- Only the quota headers of the provider and `Retry-After` are recorded; request credentials never are.

The `teltonika-rms` fixtures follow the shape of such a recording.

## Linting and contract tests

```sh
tapyr provider lint providers/teltonika-rms        # one package
tapyr provider lint providers                      # every package of a directory
tapyr provider lint --update providers/my-api      # accept the new samples
tapyr provider lint --catalog providers/my-api     # also require the catalog metadata
```

The lint loads the package, compiles every template, checks the dashboards (valid JSON, a datasource variable, only metrics of the package or `tapyr_` self-metrics), the examples, the required scenarios, then runs the contract test: the jobs of `tests/contract.yaml` run once against the simulator, on a fake clock starting at `2026-01-01T00:00:00Z`, and their samples must equal `tests/expected.prom`. Every template must be exercised. With `--catalog`, the catalog metadata of the package and the `source` of every limit are required too, with existing categories and dates. `make lint` and the CI lint the built-in packages with `--catalog`.

## End-to-end tests

The packages tested against real platforms have e2e tests in `test/e2e/` (build tag `e2e`), skipped unless their credentials are set:

```sh
TAPYR_E2E_RMS_TOKEN=... make e2e
TAPYR_E2E_RUTOS_URL=https://192.0.2.10 TAPYR_E2E_RUTOS_USERNAME=... TAPYR_E2E_RUTOS_PASSWORD=... make e2e
```

They run `tapyr run` for one or two minutes and spend a few requests.

## Auth plugins

When an auth flow cannot be declared (Aruba Central rotates its refresh tokens), it is written in Go under `internal/auth/plugins/<name>` and registered in `internal/auth/plugins`. A plugin validates its `auth.plugin.config` (secret references as `{secret: name}`), creates an authenticator per account with a sender of auth requests through the budget engine and a sealed store for its state, and provides its simulator side. See [`05-collection.md`](spec/05-collection.md#implementation-notes).
