# ADR 0014 - Series per output: estimates, a limit and their visibility

- Status: accepted
- Date: 2026-11-15

## Context

The backends Tapyr writes to count **series**: one series is one metric with one set of label values (the clients of one access point). Hosted backends bill by active series, self-hosted ones pay in memory and disk. Today nothing tells a user how many series an output will receive before connecting it, and nothing stops an account that suddenly reports thousands of new devices from multiplying the bill or overloading the backend.

[ADR 0012](0012-output-processing.md) let an output filter what it receives; [ADR 0013](0013-unconsumed-jobs.md) stopped the API calls whose metrics no output keeps. This is the next follow-up of ADR 0012 (#57): see the series an output receives, and cap them.

What is known: the series registry keeps, per job instance, the series of its last successful run, persisted in the state store, and, per output with processing rules, the processed series last sent. What is not known before a job runs: how many items (devices, sites) the API will return.

## Decision

1. **Count the series of an output from what was collected.** The series an output receives are the series of the last successful run of every job instance routed to it, after its processing rules. The same computation serves:
   - `tapyr plan`, offline, from the series persisted in the state store: a section per output with its series and samples per minute (series of each instance divided by the effective interval of its job), and the collected jobs never run yet, whose series are unknown (the total is then a lower bound);
   - the status API and the web UI, online, from the registry;
   - the self-metric `tapyr_output_series{output}`.

   Nothing is estimated from the templates: the number of items is unknown before the first run, and a made-up figure would be worse than "unknown".

2. **An optional series limit per output**, `spec.limits.maxSeries`:
   - it applies after the processing rules, to every output type, the pull exposition included;
   - the series already sent to the output keep flowing; a **new** series is sent only while the output has fewer series than the limit, and its samples are dropped otherwise, counted in `tapyr_output_samples_dropped_total{reason="series_limit"}`, which the `TapyrOutputDropping` alert already reports;
   - a series that stops (stale, withdrawn) frees its place; a limit lowered by a reload below the current count keeps the existing series and admits no new one;
   - the series admitted per instance are rebuilt after a restart from the persisted series, as for the processing rules.
   - `tapyr_output_series_limit{output}` exposes the limit, so that a dashboard or an alert can compare.

3. **What the user sees.**
   - The Outputs page of the web UI shows, per output, its series against its limit (a meter), the new series held back and the samples dropped by the limit, in words, and the jobs not measured yet.
   - The output chips of the Overview show the series, and a held-back state.
   - `tapyr run` logs a warning when an output starts holding back series, and an info when it stops.

4. **Not in this step**: processing the self-metrics pushed through an output. ADR 0012 decided that they are never filtered, so that no rule can hide a problem of Tapyr; changing it deserves its own discussion (a separate issue). No `tapyr_output_samples_per_minute` gauge either: `rate(tapyr_output_samples_sent_total[5m]) * 60` gives it, and a per-minute gauge would break the naming rules of the self-metrics.

## Alternatives considered

- **Estimate from the templates** (metrics per item times an assumed number of items). It would give a figure before the first run, but a wrong one: the number of items is the unknown.
- **Drop arbitrary series at the limit, or stop the output.** Existing series would flicker or the whole output would stop; keeping what is sent and refusing what is new keeps dashboards stable and makes the cause obvious.
- **A limit per job.** Simpler to enforce, but the bill is per backend, and a job sent to two outputs may deserve two limits.
- **Apply the limit before the processing rules.** It would count series that the output never receives.

## Consequences

- A user sizes a backend with `tapyr plan` after a first run, and caps a hosted one with `maxSeries`.
- The registry keeps the admitted series per instance for the outputs with a limit: memory proportional to their series, as for the outputs with processing rules.
- An output at its limit receives no new series until others stop; the UI, the log and the alert say so.
