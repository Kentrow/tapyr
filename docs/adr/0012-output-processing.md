# ADR 0012 - Processing per output, first step: keep and drop metrics and labels

- Status: accepted
- Date: 2026-11-10

## Context

Every series Tapyr sends has a cost downstream. Hosted backends bill by active series and data points per minute; self-hosted ones pay in memory, disk and query time. Today every output receives every metric of the jobs routed to it, and the only way to send less is to change the mappings of a provider package, which changes every output at once and forks the package.

Users ask for what every collection pipeline offers (Prometheus, the OpenTelemetry Collector, Alloy, vmagent): keep or drop metrics and labels per destination, then aggregate, send less often to an expensive backend, and see what each choice costs. Tapyr also knows where every metric comes from, since it makes the API call: when no output consumes the metrics of a job, the call itself could be skipped, saving API quota as well as ingestion.

This ADR decides the first step only: filtering metrics and labels per output. The other parts are follow-up issues (see Consequences), each to be designed with its own constraints (counters, staleness, source timestamps).

## Decision

1. **Rules per output.** An `Output` gains `spec.processing`, an ordered list of rules applied to the samples of the jobs sent to that output only. The semantics are those of Prometheus `metric_relabel_configs`, with camelCase field names, so that rules written for Prometheus carry over:

   ```yaml
   spec:
     type: remote_write
     processing:
       - action: drop
         sourceLabels: [__name__]
         regex: "meraki_uplink_.*"
       - action: labeldrop
         regex: firmware
   ```

   | Action | Fields | Effect |
   |---|---|---|
   | `keep` | `sourceLabels` (required), `separator` (default `;`), `regex` (default `(.*)`) | keeps the samples whose joined source label values match |
   | `drop` | same | drops the samples whose joined source label values match |
   | `labelkeep` | `regex` (required) | keeps only the labels whose name matches |
   | `labeldrop` | `regex` (required) | removes the labels whose name matches |

   `regex` is RE2, fully anchored (`^(?:regex)$`). `__name__` is the metric name; a missing label is the empty string. Rules apply in order, a dropped sample stops at once.

2. **Where it applies.** On the delivery of each output, after the mapping and the counter reconstruction, before deduplication, and before the `externalLabels` that the output adds when it sends. The series registry stays shared: outputs without rules cost nothing more. Without rules, nothing changes.

3. **Staleness and deduplication follow the series an output receives.**
   - For an output with rules, the registry keeps, per job instance, the set of processed series last sent to it. A series in the previous set and absent from the current one goes stale for that output: a stale marker for push outputs, removal from the exposition for the pull output. This covers a series that a rule starts dropping after a reload (marked stale once at the next run of its instance) and a label that `labeldrop` removes (the old series goes stale, the new one appears).
   - After a restart, the previous set of an instance is derived from the raw series of the snapshot with the current rules: nothing new is persisted.
   - Deduplication uses the processed series key.
   - Two samples of one run that end up with the same series (a `labeldrop` removing the label that told them apart) are a configuration problem: the first is kept, the others are dropped and counted in `tapyr_output_samples_dropped_total{reason="duplicate"}`, which the `TapyrOutputDropping` alert reports.

4. **The pull output** (`prometheus`) is processed like the others: it exposes the processed samples of the last successful run of every instance.

5. **Not processed:** Tapyr's self-metrics, on `/metrics/self` or pushed through an output (`selfMetrics.outputs`): they are the view on Tapyr itself, and filtering them could hide a problem.

6. **Validation** (`tapyr validate`, at load):
   - errors: an unknown action, a missing required field, a field an action does not take, an invalid regular expression or label name, a `labelkeep` whose regex does not match `__name__`, a `labeldrop` whose regex matches `__name__`; the fields of later actions (`targetLabel`, `replacement`) are unknown fields;
   - warnings: a `keep` or `drop` on `__name__` alone that matches no metric declared by the templates of the jobs sent to the output.

7. **Visibility:** `tapyr_output_samples_filtered_total{output}` counts the samples removed by `keep` and `drop`. It is a separate counter because filtering is intended, while `tapyr_output_samples_dropped_total` and its alert are about losses.

## Alternatives considered

- **Leave it to a pipeline downstream** (OpenTelemetry Collector, Alloy, vmagent). It works today and stays possible, but it is one more component to run and it filters data already collected.
- **The full relabeling set at once** (`replace`, `labelmap`, `hashmod`). Renaming `__name__` changes the metadata (type, help, unit) and the OTLP metric names, and can create type conflicts between metrics that validation checks today; it deserves its own design.
- **CEL conditions** (`when`). One language everywhere, but simple keep and drop rules become verbose and do not carry over from Prometheus; kept for later, for what relabeling cannot express.
- **OTTL.** Standard in the OpenTelemetry Collector, but tied to its data model and unknown to Prometheus users.
- **Rules per job instead of per output.** Simpler, but one output could not get more detail than another, which is the main use case (a complete local backend, a reduced hosted one).
- **Stale markers computed with the current rules only** (no per-output sets). No memory cost, but a series dropped by a new rule, or renamed by a new `labeldrop`, would stop without a stale marker and linger in the backend for its lookback.

## Consequences

- Users reduce what an output receives without forking a provider package; a local backend can stay complete next to a reduced hosted one.
- The registry costs memory proportional to the series of the outputs with rules (one more set per instance and output).
- A rule change applies at the next successful run of each instance, not at the reload.
- `labeldrop` can merge series; the duplicates it creates are dropped and counted, not silently overwritten.
- Follow-up issues, out of this step: skipping the API calls whose metrics no output consumes (#56), cost visibility with series estimates in `tapyr plan` and a series limit per output (#57), aggregation before sending (#58), an interval per output (#59), sending a series only when its value changes (#60). `replace`, `labelmap` and CEL conditions come with the first of them that needs them.
