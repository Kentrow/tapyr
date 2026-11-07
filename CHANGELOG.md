# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Catalog metadata on provider packages ([ADR 0011](docs/adr/0011-provider-ecosystem.md)): `displayName`, `categories`, `maintainers`, `tier` (`official` or `community`) and `validation` (`simulator` or `real-platform`, with a date and the platform version), and a structured `source` on every limit (`vendor-docs`, `measured` or `assumed`). They are optional; `tapyr provider lint --catalog` requires them, and every built-in package has them.
- `tapyr provider list --output json`: every package with its catalog metadata, its templates (request path, default interval, metrics) and its limits (value, scope, source), and the catalog categories, versioned by a `schemaVersion` ([provider catalog output](docs/spec/09-cli-api-ui.md#provider-catalog-output)).
- Documentation: the strategy of the provider ecosystem ([ADR 0011](docs/adr/0011-provider-ecosystem.md)), and a page of [provider ideas](docs/provider-ideas.md) beyond networks, with their rate-limit patterns and the engine features some of them still need.

### Changed

- A newly discovered fan-out instance runs first within the jitter of its interval (10% by default), then at its point of the interval: the first data of a discovery chain no longer waits up to one interval per level (the network devices of the getting started guide could take about 40 minutes, they now come within about 10). Instances loaded from the cached inventory keep their schedule.

### Fixed

- The instances of a fan-out were not spread over their interval when their keys differ only by their last characters (`devices[orgId=1]`, `devices[orgId=2]`): their phases came within a second of each other, and they ran in bursts. Phases now come from a hash that mixes the whole key; they move once when upgrading.

## [0.1.2] - 2026-11-06

### Fixed

- A push output (remote write or OTLP) dropped the whole batch when the backend refused it for one out-of-order or out-of-bounds sample, as Prometheus does without out-of-order window: the fresh samples of the other jobs sent with it were lost. The batch is now split until only the refused samples are left, and only those are dropped and counted in `tapyr_output_samples_dropped_total{reason="out_of_order"}`.

## [0.1.1] - 2026-11-05

### Added

- Configuration reference: every field of every kind, with its type, default and constraints, generated from the JSON Schemas ([`docs/reference/`](docs/reference/README.md)).
- Command line reference: every command and flag, generated from the commands ([`docs/reference/cli.md`](docs/reference/cli.md)).
- Guides: getting started with the binary and a built-in simulator, concepts, operations and troubleshooting.
- SLSA build provenance for the release archives and the image, as GitHub artifact attestations, verifiable with `gh attestation verify`; the attestations of the archives are also a release asset (`tapyr_X.Y.Z_provenance.intoto.jsonl`).

### Fixed

- The JSON Schemas carry the defaults of the duration and percentage settings (`collection.defaultTimeout`, `collection.jitter`, `budget.retryReserve`, `retry.initialBackoff`, `circuitBreaker.cooldown`, output timeouts and others), which editors did not show.
- A YAML anchor containing its own alias (`&a [*a]`), or a document nested thousands of levels deep, made `tapyr validate`, `tapyr run` and configuration reloads hang until the stack overflowed; such documents are now rejected with their position.
- `tapyr record` kept the letters and digits outside ASCII of serials and identifiers (`Сер-123` became `Сер-706`); every letter and digit is now replaced.

## [0.1.0] - 2026-11-04

### Added

- Declarative configuration `tapyr/v1alpha1` (`Provider`, `JobTemplate`, `Account`, `Job`, `Output`, `Settings`) in YAML files or directories, validated against embedded JSON Schemas with every error reported as `file:line:column`; secrets from environment variables or files.
- Provider packages (`provider.yaml`, job templates, simulator, scenarios, contract test, Grafana dashboard, examples), built in or loaded from `providerPaths`, with CEL mappings to gauges, counters and `_info` metrics.
- Built-in providers: Aruba Central (classic), Cisco Meraki Dashboard, Teltonika RMS and Teltonika RutOS (validated against the real platforms for both Teltonika providers).
- Collection: auth `none`, `header`, `bearer`, `basic`, `oauth2_client_credentials`, `session_login` and Go auth plugins (Aruba Central rotating refresh tokens); pagination `offset`, `page`, `cursor`, `link_header` and `expression`; retries with backoff, circuit breakers per account and target, account TLS settings; redirects followed only to the hosts a provider allows, so that credentials never reach another host.
- Discovery: fan-out jobs (`forEach`) with one instance per inventory item, multi-target accounts, inventories cached across restarts.
- Budget engine: rate, concurrency and quota limits (calendar windows in any timezone, rolling windows) with scopes, budget shares, a retry reserve and sync with the quota headers of the providers (learned max, remaining, consumption of other clients, reset); every request goes through it, retries, pages, auth and discovery included.
- Planner: effective intervals that fit every quota, stretching low priorities first and never critical jobs, replanned while running with the remaining budget and the projected consumption of other clients sharing the quota.
- Outputs: Prometheus exposition (`/metrics`, text or OpenMetrics, gzip-compressed), Prometheus remote write and OTLP (HTTP and gRPC), with source timestamps, staleness, deduplication, bounded queues and retries; counter reconstruction from window values.
- State store on embedded SQLite (no CGO): quota ledger, run history, inventories, series and counters; tokens encrypted at rest with `TAPYR_STATE_KEY`.
- Self-observability: self-metrics on `/metrics/self` (calls, budgets, jobs, auth, circuits, outputs), optionally pushed through the outputs, `/healthz` and `/readyz`, JSON logs with secrets and tokens redacted.
- Hot reload of the configuration on SIGHUP, an invalid configuration being rejected while the running one keeps going.
- CLI: `tapyr run`, `validate`, `plan`, `sim`, `record` (anonymized recordings of real responses), `provider list`, `provider lint`, `schema` and `version`.
- Provider simulator driven by the same definitions as the collector: auth, pagination and limits (429 with `Retry-After` and quota headers), fault scenarios (`quota-tight`, `bad-day`), synthetic inventories; `tapyr sim --all` serves every provider at a stable port (`--listen-base` plus the `portOffset` of its package).
- Read-only status API under `/api/v1` with its OpenAPI description, a Server-Sent Events stream and a live debug endpoint, errors redacted like the logs; optional basic auth and HTTPS.
- Read-only web UI embedded in the binary under `/ui/`: overview, graph, jobs, budgets, plan, outputs and live debug, updated live, light and dark themes, keyboard accessible, no external network call.
- Grafana dashboards for every provider and `tapyr-overview` for Tapyr itself, example alert rules (`examples/alerts`) and `docs/alerts.md`.
- Demo compose (`examples/compose`): the simulators, Tapyr, VictoriaMetrics and Grafana, without any vendor account.
- Deployment examples: a hardened systemd unit (`examples/systemd`) and Kustomize manifests for Kubernetes (`examples/kubernetes`).
- Releases: static binaries for Linux, macOS and Windows (amd64, arm64) with SBOMs and checksums, and the multi-arch image `ghcr.io/kentrow/tapyr` (distroless, non-root), signed with cosign; the archives and the image carry the licenses and notices of the third-party software they include.

[Unreleased]: https://github.com/kentrow/tapyr/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/kentrow/tapyr/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/kentrow/tapyr/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/kentrow/tapyr/releases/tag/v0.1.0
