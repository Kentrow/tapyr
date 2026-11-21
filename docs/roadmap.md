# Roadmap

Where Tapyr stands and where it goes until 1.0. The releases are described in the [changelog](../CHANGELOG.md); this page says what comes next and why. Items listed as out of scope in [`01-vision-and-scope.md`](spec/01-vision-and-scope.md) need an issue and an agreement on their design (an ADR when they change the architecture) before any code.

## Where Tapyr stands

From v0.1.0 to v0.6.0, Tapyr grew from the first pipeline to a complete gateway:

- **Budget engine and planner**: rate, concurrency and quota limits with scopes, quota headers, 429 penalties, retries and circuit breakers; a planner that stretches intervals to fit, offline (`tapyr plan`) and while running; series per output and a series limit ([ADR 0014](adr/0014-output-series.md)); jobs whose metrics no output keeps make no API call ([ADR 0013](adr/0013-unconsumed-jobs.md)).
- **Collection**: declarative auth (header, bearer, basic, OAuth 2.0 client credentials, session login) and an auth plugin for rotating refresh tokens, every common pagination, discovery fan-out with an inventory cache, multi-target accounts, hot reload.
- **Outputs**: Prometheus exposition, remote write and OTLP, processing rules per output ([ADR 0012](adr/0012-output-processing.md)), and a disk buffer for the push outputs ([ADR 0015](adr/0015-persistent-output-buffer.md)).
- **Providers**: eight packages, five of them validated against real platforms (Cisco Meraki, Aruba Central, Teltonika RMS and RutOS, UniFi Site Manager); a simulator for every package; `tapyr provider show`, `lint` and the catalog metadata ([ADR 0011](adr/0011-provider-ecosystem.md)).
- **Operations**: read-only web UI, status API, self-metrics, Grafana dashboards, example alerts, a glossary, and a public demo on simulated data.
- **Distribution**: static binaries, multi-arch image, Helm chart, deb and rpm packages, Homebrew, cosign signatures and SLSA provenance, documentation on docs.tapyr.eu.

Every release candidate runs for a few days on a canary instance against real platforms before its release ([Releasing](releasing.md)).

## Next

### v0.7: keep what the quota paid for

- The disk buffer of the push outputs ([ADR 0015](adr/0015-persistent-output-buffer.md)): samples survive a backend outage and a restart of Tapyr.
- Polish found on v0.6: plan problems that name a scope keyed by a credential by its hash instead of its accounts, the hint of `tapyr validate` for a duration written as a number, long names in the Graph.
- Test reliability: the integration test against a Prometheus without out-of-order window.

### Announcement

Tapyr has been built in the open but not announced yet. After v0.7:

- the provider catalog page on tapyr.eu, built from `tapyr provider list --output json`;
- an introduction post: API quota as a budget, planned and spent deliberately;
- posts in the communities of network monitoring, homelabs and Grafana.

The feedback of the first users decides the order of what follows.

### v0.8: more APIs

- A declarative OAuth 2.0 authorization code flow with refresh tokens, bootstrapped once with `tapyr auth login <account>`: most SaaS and consumer APIs need it, and YAML-only packages could then cover them.
- `tapyr provider new`: a package skeleton that passes `tapyr provider lint`, so that contributors start in one command.
- New provider packages, chosen from the feedback: another network platform, and a first one beyond networks (energy or the connected home) to show the generic engine ([provider ideas](provider-ideas.md)).

### v0.9: ready for 1.0

- The engine features that change how limits are declared, decided before the format is frozen ([engine gaps](provider-ideas.md#engine-gaps)): a cost per request (an ADR first), quota windows aligned on any duration, quota sync from expressions, rate limiting signaled other than by a status.
- A review of the whole configuration format, which becomes `tapyr/v1beta1`, with a migration of the `v1alpha1` files that says what changed.
- A security review: secrets and redaction, the HTTP surface, the state store.

### v1.0: stable

- The configuration format `tapyr/v1`, with a written compatibility and deprecation policy.
- Upgrade tests from one version to the next, the state store included.
- A soak of 30 days on the canary instance.
- Complete documentation: guides, reference, provider packages.

## Later

Candidates, not committed, in no particular order; the feedback after the announcement decides which come first.

- Events and alarms as log records (OTLP logs, Loki): out of scope today ("metrics only"), the largest change of scope considered.
- History backfill at first start, paced by the budget engine, for APIs that keep days or weeks of history.
- Label enrichment from an inventory (a CSV file, NetBox).
- Secrets from a secret store (Vault or OpenBao, SOPS); the external patterns work today.
- Output options: an interval per output, aggregation before sending, sending a series only when it changes ([#58](https://github.com/Kentrow/tapyr/issues/58), [#59](https://github.com/Kentrow/tapyr/issues/59), [#60](https://github.com/Kentrow/tapyr/issues/60)); processing the self-metrics pushed through an output ([#92](https://github.com/Kentrow/tapyr/issues/92)).
- Webhook and streaming sources, next to polling.
- UI helpers: a budget impact simulation, a mapping authoring assistant (the UI stays read-only).
- Provider skeletons generated from OpenAPI descriptions; provider distribution as OCI artifacts and a community registry (phase 2 of [ADR 0011](adr/0011-provider-ecosystem.md)).
- The Aruba Central provider based on HPE GreenLake.
- Distributed mode (workers, a shared state backend, leader election) and Prometheus remote write 2.0.
