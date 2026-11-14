# 06 - Mapping and outputs

## Mapping language: CEL

Mappings use CEL (Common Expression Language, `cel.dev/cel-go`). Rationale in ADR 0002. Key properties: non Turing-complete (termination guaranteed), type-checked at compile time (errors surface in `tapyr validate`), cost-estimable, fast once compiled, extensible with custom functions.

### CEL environment

Variables available in `extract` expressions:

| Variable | Type | Content |
|---|---|---|
| `response` | map | Current page: `status` (int), `headers` (map of lowercase name to list of strings), `body` (dyn, decoded JSON: integral numbers that fit in int64 decode as `int`, other numbers as `double`, so IDs keep their precision and `string(item.id)` yields `"123"`) |
| `responses` | list | All pages (same shape), available in `extract.post` |
| `item` | dyn | Current element of `extract.items` |
| `vars` | map(string, string) | Request variables (discovery and params) |
| `parent` | map | Parent inventory item fields (in `forEach`) |
| `account` | map | `name`, `labels`, `params` (never secrets) |
| `target` | map | `name`, `labels`, `params` (multi-target accounts) |
| `now` | timestamp | Run start time (from the injected clock) |

Variables are declared only where they are meaningful, so `validate` rejects, for example, `item` in `extract.items`:

| Fields | Variables |
|---|---|
| `extract.items`, pagination `stop`, `total`, `cursor`, `next` | `response`, `vars`, `account`, `target`, `now` |
| `extract.filter`, `metrics.*`, `info.*`, `inventory.*` | the above plus `item` |
| `extract.post.*` | `responses`, `vars`, `account`, `target`, `now` |
| `forEach.vars`, `forEach.filter` | `parent`, `account`, `target`, `now` |
| `request.query` and `request.body` values `{ expr }` | `vars`, `account`, `target`, `now` |
| `sessionLogin.token`, `sessionLogin.expiresIn` | `response`, `now` |

Custom functions:

| Function | Purpose |
|---|---|
| `default(value, fallback)` | Fallback when a field is absent or null |
| `toFloat(dyn)` | Number or numeric string to double, error otherwise |
| `parseTimestamp(string)` / `parseTimestamp(string, layout)` | RFC 3339 by default, or a Go layout |
| `fromUnix(number)` / `fromUnixMillis(number)` | Epoch to timestamp |
| `parseDuration(string)` | Human durations (`"3d 4h"`, `"01:02:03"`) to seconds |
| `parseBytes(string)` | Human sizes (`"1.5 GB"`, `"512 KiB"`) to bytes as a double (`bytes()` is a built-in CEL conversion and cannot be overloaded) |
| `lower()`, `upper()` | Unicode case conversion (`item.name.lower()`) |
| `trim`, `replace`, `split`, ... | cel-go string extension |
| `macToString(string)` | Normalize MAC address formats |

The `has()` macro and optional types (`?.` syntax) are enabled to handle missing fields.

Compilation limits, configured in `settings.mapping`: max expression cost (`maxCost`, default 1000000), max items per run (`maxItemsPerRun`, default 100000), max samples per run (`maxSamplesPerRun`, default 200000).

A metric `value` expression may return `int`, `uint`, `double` or `bool` (`true` is 1); the result is converted to float64.

## Extract block

```yaml
extract:
  items: "response.body.items"          # CEL -> list; default: response.body if it is a list
  filter: "item.status != 'unknown'"    # optional CEL -> bool
  metrics:
    - name: aruba_ap_clients
      type: gauge                       # gauge | counter
      help: Number of clients connected to the AP
      unit: ""                          # optional, Prometheus base units preferred
      value: "toFloat(item.client_count)"
      labels:
        serial: "item.serial"
        site: "default(item.site, '')"
      timestamp: "parseTimestamp(item.last_modified)"   # optional
      skipIf: "!has(item.client_count)"                 # optional CEL -> bool
  info:
    - name: aruba_ap_info
      labels:
        serial: "item.serial"
        model: "item.model"
        firmware: "item.firmware_version"
  inventory:                            # for role: inventory | both
    key: "item.serial"
    fields:
      serial: "item.serial"
      site: "item.site"
  post: []                              # optional metrics computed over all items
```

- `info` metrics are gauges with value 1 and descriptive labels (standard `_info` pattern). Descriptive attributes belong there, not on every metric.
- `post` entries can aggregate across items, e.g. `value: "double(size(responses[0].body))"` for a count.
- Account labels, target labels and job labels are added to every sample. Conflicts: job > target > account > mapping (`settings.mapping.labelConflict: error` turns conflicts into run errors).
- Empty-string label values are dropped (Prometheus semantics).
- Label expressions return strings; ints, doubles and bools are converted to their string form, `null` gives an empty (dropped) label. Other types are mapping errors.
- A mapping error on one item or one metric skips that sample and is counted (`tapyr_mapping_errors_total`); an error in `extract.items` fails the run.

## Window values and counters

Many APIs return values aggregated over a window ("bytes over the last 5 minutes") rather than cumulative counters.

- Default: expose them as **gauges** named explicitly (`meraki_uplink_sent_bytes_5m`). This is honest and simple.
- Optional reconstruction: `type: counter` with `accumulate: true` sums successive window values into a monotonic counter persisted in the state store. Requires non-overlapping windows (`windowField` or job interval aligned). Counter resets on restart are avoided by persistence; gaps are documented.
- Native cumulative counters from APIs map directly to `type: counter`.

## Internal metric model

Each sample: metric name, type, unit, help, label set (sorted), value (float64), timestamp (source timestamp or collection time), plus provenance (job instance, run id). Converted to each output format at the edge.

## Series registry: staleness and deduplication

Per job instance, the registry keeps the set of series emitted by the last successful run and the last timestamp sent per series and output.

- **Staleness**: a series present in the previous successful run but absent from the current one is stale.
  - Prometheus pull: removed from the exposition.
  - Remote write: a stale marker sample (the Prometheus special NaN value `0x7ff0000000000002`) is sent once.
  - OTLP: one data point with the `FLAG_NO_RECORDED_VALUE` flag is sent once.
  - When a job instance is removed (inventory change, config reload), all its series become stale.
  - A failed run does not cause staleness (series stay as-is until `staleAfter`, default 3x effective interval, after which they are removed from pull exposition and marked stale in push outputs).
- **Deduplication**: when a sample carries a source timestamp lower than or equal to the last timestamp sent for that series and output, it is dropped (overlapping API windows). Counted in `tapyr_samples_deduplicated_total`.

### Implementation notes

- The registry is keyed by job instance. For an output with processing rules (see Processing), it also keeps the processed series last sent to that output, and staleness and deduplication use them; they are not persisted, but derived from the series of the snapshot with the current rules after a restart.
- Pull exposition follows the registry directly: a successful run replaces the series of its instance; an instance whose last success is older than `staleAfter` is withdrawn; a removed instance is withdrawn at once.
- Push outputs receive, per output, the samples that pass deduplication and the series to mark stale; the registry computes both.
- Deduplication only concerns samples with a source timestamp; samples stamped with the collection time are never deduplicated.
- The registry is snapshotted with the ledger (every 5 seconds and on shutdown): the series of every instance, and the last source timestamp sent per series and output.
- The source timestamps of a series are forgotten when it goes stale, which bounds the registry to the live series. After a restart, instances of the snapshot that are no longer scheduled (removed from the configuration, or children waiting for the first run of their parent without a cached inventory) are withdrawn at the first check.

## Processing

An output may declare `spec.processing`, an ordered list of rules applied to the samples of the jobs sent to it only ([ADR 0012](../adr/0012-output-processing.md)), with the semantics of Prometheus `metric_relabel_configs`:

| Action | Fields | Effect |
|---|---|---|
| `keep` | `sourceLabels` (required), `separator` (default `;`), `regex` (default `(.*)`) | keeps the samples whose joined source label values match |
| `drop` | same | drops the samples whose joined source label values match |
| `labelkeep` | `regex` (required) | keeps only the labels whose name matches |
| `labeldrop` | `regex` (required) | removes the labels whose name matches |

- `regex` is RE2, fully anchored. `__name__` is the metric name; a missing label is the empty string. Rules apply in order; a dropped sample stops at once.
- Rules apply on the delivery of each output: after the mapping and the counter reconstruction, before deduplication and before the `externalLabels` of the output. Without rules, nothing changes.
- Staleness follows the series the output receives: a series that a rule starts dropping (after a reload) or that `labeldrop` renames gets a stale marker once, at the next successful run of its instance. A rule change applies from that run.
- Two samples of one run that end up with the same series and timestamp (a `labeldrop` removing the label that told them apart): the first is kept, the others are dropped and counted in `tapyr_output_samples_dropped_total{reason="duplicate"}`.
- Samples removed by `keep` and `drop` are counted in `tapyr_output_samples_filtered_total`.
- Self-metrics are never processed, on `/metrics/self` or pushed through an output.
- `tapyr validate` rejects an unknown action, a missing or extra field, an invalid regex or label name, and a rule that would remove `__name__` (`labelkeep` not matching it, `labeldrop` matching it); it warns about a `keep` or `drop` on `__name__` alone that matches no metric of the jobs sent to the output.
- **Jobs not collected** ([ADR 0013](../adr/0013-unconsumed-jobs.md)): a job none of whose metrics can reach an output, judged by the rules on the metric name, is not collected: no API call, no quota, its series withdrawn as for a removed job. The parent of a fan-out is collected when one of its children is. The rules are evaluated on the name alone: a `keep` or `drop` on `[__name__]` is decided, one on any other label cannot be before the call and keeps the job collected, `labelkeep` and `labeldrop` never drop a sample. Without any output, every job is collected. `settings.collection.skipUnconsumed: false` turns it off; `validate`, `plan` and `run` warn about every job not collected, the status API reports `collected: false`, and `tapyr_job_collected` is 0.
- Not yet: `replace`, `labelmap`, conditions in CEL, aggregation, an interval per output, sending on change (follow-ups of ADR 0012).

## Timestamps

- If `timestamp` is set in the mapping, the sample carries the source timestamp. Otherwise, the collection time (response received).
- Push outputs always send timestamps.
- Prometheus pull exposition omits timestamps by default (`exposeTimestamps: false`), as recommended by Prometheus; the latest value per series is exposed.
- Out-of-order caveat: Prometheus remote write receivers may reject out-of-order samples unless out-of-order ingestion is enabled; VictoriaMetrics accepts them. See [`docs/outputs.md`](../outputs.md#out-of-order-samples): the push engine splits a refused batch so that only the refused samples are lost, counted in `tapyr_output_samples_dropped_total{reason="out_of_order"}`.

## Outputs

All outputs implement:

```go
type Output interface {
    Name() string
    Enqueue(batch model.Batch)           // non-blocking; drops with accounting when the queue is full
    Run(ctx context.Context) error       // sender loop
    Flush(ctx context.Context) error     // on shutdown
}
```

### `prometheus` (pull)

- Served by the main HTTP server on `/metrics` (collected data only; Tapyr's own metrics are on `/metrics/self`, see `07-self-observability.md`).
- Holds the samples of the last successful run of every job instance: a successful run replaces the series of its instance, a failed run keeps them (until `staleAfter`, see above). Content negotiation: Prometheus text format and OpenMetrics, gzip-compressed when the scraper accepts it (`Accept-Encoding`).
- `HELP` and `TYPE` from the mapping. Metric name conflicts (same name, different type) are rejected at validation.

### `remote_write`

- Prometheus remote write 1.0 (protobuf + snappy), HTTP POST.
- Batching (`maxBatch`, `flushInterval`), bounded queue (`capacity`, drop-oldest with accounting), retries with backoff on 5xx and 429 (respecting `Retry-After`), no retry on 4xx other than 429.
- Metadata (`HELP`, `TYPE`) sent periodically.
- Compatible targets: Prometheus (with the remote write receiver enabled), VictoriaMetrics (`/api/v1/write`), Grafana Mimir, Thanos Receive, Cortex.
- Remote write 2.0 is not supported yet.

### `otlp`

- OTLP metrics over HTTP (protobuf) or gRPC.
- Payloads are built directly with `pmetric` (`go.opentelemetry.io/collector/pdata`), **not** with the OpenTelemetry metrics SDK, which aggregates and cannot carry source timestamps (ADR 0008).
- Mapping: gauge to OTLP Gauge; counter to OTLP Sum (monotonic, cumulative, start time = series first seen or persisted start). Labels to data point attributes. Resource attributes: `service.name=tapyr`, `service.instance.id`, `tapyr.provider`, `tapyr.account` (configurable), plus `resourceAttributes`.
- Batching, bounded queue and retries as for remote write; honors OTLP partial success responses and throttling hints.

### Implementation notes

- **Push engine** (`internal/output/push`), shared by `remote_write` and `otlp`: the queue counts samples, stale markers included; when full, the oldest samples are dropped (`queue_full`). One request is in flight per output, so samples of a series leave in order. A batch is sent when it reaches `maxBatch` or when its oldest sample waited `flushInterval`. Network errors, 5xx and 429 are retried with exponential backoff (1s doubling up to 1m, full jitter), honoring `Retry-After` (capped at 5 minutes); a batch is retried until it succeeds or is pushed out by newer samples, so a slow backend costs memory up to `capacity`, never collection. Other 4xx drop the batch (`rejected`), except when the response mentions out of order, too old or out of bounds samples (`out_of_order`): such a backend refuses the whole request for one of its samples, so the batch is split in halves sent one after the other, a refused half is split further, and the samples refused alone are dropped (`out_of_order`). The spans of a split batch keep their order and stay the batch in flight; after 64 requests for one batch, the spans left are sent once each and dropped if refused. A retryable error during the split retries the current span with the backoff; a split batch can still be pushed out by newer samples during it. On shutdown, `Flush` sends what is queued within the shutdown deadline, without retry delays beyond it.
- **External labels** are added to every sample; a label set by the sample wins.
- **Stale markers** carry the time the series went stale (the run that no longer produced it, or the withdrawal). The output remembers the type and start time of the series it sent, so a marker is a gauge point when the series is unknown (after a restart).
- **Remote write** sends `Content-Encoding: snappy`, `Content-Type: application/x-protobuf`, `X-Prometheus-Remote-Write-Version: 0.1.0` and `User-Agent: tapyr/<version>`; the time series of a request are sorted by labels with their samples in time order. Metadata (`TYPE`, `HELP`, unit) of every metric family seen is sent in a request of its own on the first send and every 1 minute.
- **OTLP** groups samples by resource (provider and account of the job instance) and metric name; the instrumentation scope is `github.com/kentrow/tapyr` with the Tapyr version. `service.instance.id` is a random identifier created on first start and kept in the state store (`resourceAttributes` may override it). HTTP: `POST` to the endpoint, with `/v1/metrics` appended when the endpoint has no path; gRPC: `http://` endpoints are plaintext, `https://` use TLS with the system roots. Retryable: HTTP 429, 502, 503, 504 and gRPC `UNAVAILABLE`, `RESOURCE_EXHAUSTED` (with `RetryInfo`); rejected data points of a partial success are counted as `rejected`. The start time of a cumulative sum is the first time the output saw the series in this process, or its earliest timestamp when a source timestamp is older; a restart starts the sums again, which receivers treat as a reset.
- **Counter reconstruction** (`accumulate: true`): per job instance and series, the store keeps the running total and the last window counted (the `windowField` value, else the sample timestamp). A sample whose window was already counted adds nothing; negative window values are mapping errors. The totals are persisted with the ledger (table `accumulators`) and dropped when the series goes stale or its instance is removed, a counter reset receivers handle.

### Multiple outputs

A job sends to all outputs by default, or to the subset listed in `spec.outputs`. Each output has its own queue: a slow backend never blocks collection or other outputs, and its own processing rules: a local backend can stay complete next to a reduced hosted one.
