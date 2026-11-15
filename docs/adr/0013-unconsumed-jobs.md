# ADR 0013 - Jobs whose metrics no output keeps are not collected

- Status: accepted
- Date: 2026-11-14

## Context

[ADR 0012](0012-output-processing.md) gave every output its own `processing` rules: an output can drop metrics it does not want. When every output drops all the metrics of a job, Tapyr still calls the API on schedule, spends quota, maps the responses, and throws every sample away. The quota is the scarce resource Tapyr exists to plan ([01-vision-and-scope.md](../spec/01-vision-and-scope.md)), and Tapyr is the only component of the pipeline that knows where a metric comes from, since it makes the call. A pipeline downstream (Prometheus, the OpenTelemetry Collector) can drop the samples, but it cannot give the quota back.

This is the first follow-up of ADR 0012 (#56).

The metric names a job can produce are known before it runs: they are fixed strings in its extraction (`extract.metrics`, `extract.info` and `extract.post` of its template or inline definition). `tapyr validate` already uses them to warn about a `keep` or `drop` rule that matches nothing. The label values are only known after the call.

## Decision

1. **A job is collected when one of its metrics can reach an output, or when a collected job takes its inventory.** Tapyr decides it when it loads the configuration, and again at every reload:
   - a metric reaches an output when the output receives the job (`spec.outputs` of the job, or every output when it is empty) and the processing rules of the output may keep it;
   - the parent of a `forEach` (an explicit `Job` or an implicit parent instantiated for a `forEach.template`) is collected when one of its children is;
   - a job with `role: inventory` and no collected child, and a job with `role: metrics` or `both` none of whose metrics can reach an output and none of whose children is collected, are **not collected**.

2. **The decision is conservative.** The rules are evaluated on the metric name alone, in order:
   - a `keep` or `drop` whose `sourceLabels` is exactly `[__name__]` is decided;
   - a `keep` or `drop` on any other label cannot be decided before the call: the metric may be kept, and the evaluation stops there;
   - `labelkeep` and `labeldrop` never drop a sample.

   A metric is dropped only when a decided rule drops it before any undecidable rule. A job is never skipped on a guess: rules on label values filter samples as before, but do not save calls.

3. **What happens to a job that is not collected.** It is not scheduled and not planned. Its calls leave the projected consumption of its quotas, so the plan gives them to the other jobs. Its series are withdrawn as those of a job removed from the configuration: stale markers for push outputs, removal from the exposition. A reload that makes it collected again schedules it like a new job.

4. **Without any output, everything is collected.** A configuration without an `Output` (the UI or the live debug on their own, a test) keeps its jobs running: the decision is about processing rules, not about the absence of outputs.

5. **It is on by default**, with `settings.collection.skipUnconsumed: false` to keep every job running, for example to keep a job as an authentication probe or to look at its responses in the live debug.

6. **Visibility.**
   - `tapyr validate`, `tapyr plan` and `tapyr run` print a warning for every job that is not collected, with the reason (`no output keeps its metrics`, or `no collected job takes its inventory`): a rule broader than intended shows at once.
   - `tapyr plan` lists those jobs apart, with their desired interval and the reason (`not_collected` in `--output json`).
   - The status API gives every job `collected` (boolean) and, when false, `not_collected_reason`; the web UI shows them as not collected, outside the health of their account and provider.
   - The self-metric `tapyr_job_collected{provider,account,job}` is 1 or 0. Job names come from the configuration, so the cardinality rule of the self-metrics holds.

## Alternatives considered

- **Decide at runtime.** Count, per job and output, the samples that processing drops, and stop calling a job after a few runs whose samples all went away. It would cover rules on label values, but a job would need calls to be judged, the plan would change on its own, and a job stopped on the values of some runs would miss the values of later ones (a site coming back, a new device). A static decision stays predictable and explainable.
- **Off by default.** Users who already drop every metric of a job pay for calls that bring nothing, and nothing tells them. `replanOnProjection` set the precedent of a behavior on by default with a setting to turn it off.
- **A warning only.** It tells about the waste but does not save the quota.
- **Fewer pages or fields instead of no call.** The APIs return whole objects or pages; a job whose metrics are partly kept needs the same calls.

## Consequences

- Dropping every metric of a job by name in the output rules now saves its API calls and frees its quota for the other jobs.
- Rules on label values still filter, but cannot save calls; the docs say so, and suggest a rule on `__name__` when the goal is the quota.
- A change of rules can start or stop API calls at a reload. The warnings and `tapyr plan` show which jobs change before the reload is applied.
- A job that is not collected has no live debug and no health; the setting brings them back.
- Follow-up issues stay as they are: #57 (series estimates and a series limit per output) can reuse the same evaluation of the rules on the metric names.
