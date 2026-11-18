# ADR 0015 - A persistent buffer for the push outputs

- Status: proposed
- Date: 2026-11-18

## Context

The push outputs (`remote_write`, `otlp`) keep their samples in a bounded queue in memory ([06](../spec/06-mapping-and-outputs.md#outputs)). When the backend is down longer than the queue lasts, the oldest samples are dropped (`queue_full`); when Tapyr restarts (an upgrade, a crash, a node drained), everything queued is lost.

For a usual collector this is a gap of a few minutes. For Tapyr it is worse: every sample cost API quota, sometimes scarce (a daily or monthly quota, a credit spent per request), and the budget engine exists to spend that quota deliberately. Losing the samples on a backend outage or a restart wastes exactly what Tapyr promises to save, and the quota spent is not given back. An external review of the project (2026-11-17) ranked this first among the missing features.

[`01-vision-and-scope.md`](../spec/01-vision-and-scope.md) lists "persistent output buffering (WAL)" as out of scope, with the rule that the design must not prevent it. This ADR proposes to take it in, for v0.7.

What exists to build on: one push engine shared by both outputs (`internal/output/push`), one request in flight per output so samples leave in order, a sequence number per queued entry, the out-of-order split of refused batches, and a state store with a known path on persistent storage (ADR 0005).

## Decision

1. **An opt-in disk buffer per push output**, `spec.queue.disk`:

   ```yaml
   spec:
     type: remote_write
     queue:
       capacity: 50000          # in memory, as today
       disk:
         maxSize: 512MiB        # oldest segments dropped beyond it
         maxAge: 2h             # samples older than this are dropped, not sent
   ```

   Off by default in v0.7, so that nothing changes for an existing configuration and no disk is used without being asked for; the default is reconsidered before 1.0. The pull exposition (`prometheus`) has nothing to buffer.

2. **Write-ahead, then acknowledge.** The points an output accepts (after its processing rules and its series limit, stale markers included) are appended to the buffer before they join the in-memory queue. A point leaves the buffer only when the backend accepted the request that carried it (or rejected it for good: `rejected`, `out_of_order`). The memory queue becomes a window over the buffer: when the buffer holds more than `capacity`, the sender reads the next batches from disk in sequence order, so memory stays bounded while the disk absorbs a long outage.

3. **Segment files, not the state store.** Each output gets a directory next to the state store (`<state dir>/buffer/<output>/`, overridable with `disk.path`), with append-only segment files of a few MiB:
   - records are batches of points, length-prefixed, compressed (snappy) and checksummed (CRC-32C);
   - a small checkpoint file records the last sequence acknowledged by the backend, written atomically (temporary file and rename);
   - a segment is deleted once every record in it is acknowledged, or when `maxSize` or `maxAge` pushes it out;
   - a lock file keeps a second Tapyr from using the same directory.

   The records hold Tapyr's own point model (labels, value, timestamp, type, stale flag, provider and account), not a wire format, so one implementation serves both outputs and the encoding of a request stays where it is.

4. **Durability is bounded and stated.** Appends are written at once and synced to disk every second, and at every segment rotation and shutdown: a crash loses at most the last second of points, a clean shutdown none. A sync on every append would make the disk the limit of the collection for no gain that the quota spent can measure.

5. **Replay after a restart.** At start, an output reads its checkpoint and sends the records after it, oldest first, before the points of the new runs (which go behind them in the buffer). Records older than `maxAge` are dropped, counted as `too_old`: most backends refuse samples older than their head or out-of-order window (Prometheus about 1 to 2 hours by default, unless `out_of_order_time_window` is set), and sending them would only end in `out_of_order` drops. A user of VictoriaMetrics or Mimir, which accept older samples, raises `maxAge`.

6. **Failures of the disk never stop the collection.** A record that fails its checksum ends the replay of its segment (the next segment is read), counted and logged once. When the disk is full or unwritable, the output keeps working in memory, as today, logs a warning, and reports it in `/readyz` conditions and the status API, without failing the readiness.

7. **What the user sees.**
   - Self-metrics: `tapyr_output_buffer_bytes{output}`, `tapyr_output_buffer_oldest_seconds{output}` (age of the oldest unsent point), and new reasons of `tapyr_output_samples_dropped_total`: `buffer_full` and `too_old`; `tapyr_output_buffer_errors_total{output, kind}` for corrupt records and write failures.
   - The Outputs page and `GET /api/v1/outputs`: the buffered size against `maxSize`, the age of the oldest point, the replay in progress after a restart.
   - The example alert `TapyrOutputBehind` when the oldest buffered point gets close to `maxAge`; `TapyrOutputDropping` already covers the drops.
   - `tapyr plan` shows the size a buffer needs per hour of outage, from the samples per minute of the output (ADR 0014).

8. **Reloads.** The buffer belongs to the output name: a reload that changes the URL or the processing rules keeps it (the points were accepted under the old rules and are sent as they are); removing the output deletes its buffer after the reload succeeds, with a log line saying how many points were dropped.

## Alternatives considered

- **A table in the SQLite state store.** One file and transactions for free, but a high write rate of short-lived rows means write amplification, `VACUUM` to give the space back, and a lock shared with the ledger flush. Append-only segments delete in one `unlink` and read sequentially, which is all a queue needs.
- **The WAL package of Prometheus** (`tsdb/wlog`). Proven, but tied to the remote write data model and a large dependency for the binary; OTLP would still need its own path.
- **An embedded key-value store** (bbolt, badger). bbolt has the same space reclaim problem as SQLite; badger is large and brings its own compaction and goroutines.
- **Leave it to a collector in front** (the Prometheus agent with its WAL, or the OpenTelemetry Collector with the `file_storage` extension of its sending queue). It works today and is documented as an option, but every user who cares about the quota spent would need a second component, and Tapyr would still lose what is queued when it restarts.
- **Sync on every append.** Exactly-once on a crash at the cost of a disk sync per run of every job; the second of loss bounded above is a better trade for metrics.
- **On by default now.** It would use disk without being asked, and break deployments without a writable volume; Tapyr already asks for persistent storage for its state, so a default on is the natural next step, decided before 1.0.

## Consequences

- An outage of the backend, or a restart of Tapyr, no longer loses the samples already paid for in quota, within `maxSize` and `maxAge`.
- `01-vision-and-scope.md` moves "persistent output buffering" out of the out-of-scope list once this ADR is accepted; Prometheus remote write 2.0 stays out.
- A deployment with the buffer needs a writable volume for it (the state volume already is one); the Helm chart and the systemd unit document it.
- After a long outage, the replay sends a burst to the backend: one request in flight per output, at the size of `maxBatch`, so the burst is paced by the backend's answers, not by Tapyr.
- The push engine gains a second storage layer to test: crashes between append and acknowledge, corrupt records, a full disk, a replay racing new runs, a reload removing an output, all on the fake clock and a temporary directory.
- Remote write 2.0 (the other half of the backlog item) is independent and stays out of scope.
