# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- The web UI shows the version of the running Tapyr at the foot of the navigation, on every page, with the revision and the Go version in its tooltip (it used to be in the subtitle of the Overview page only).
- `unifi-site-manager`, a provider package for the UniFi Site Manager API v1, the cloud API of Ubiquiti: with one API key, the consoles, sites (device and client counts, WAN state), devices and ISP metrics (latency, packet loss, uptime and expected speeds per WAN interface, in 5-minute or hourly windows stamped with their start) of every console of a UI account, within the limit of 10,000 requests a minute. Validated against a real account.
- Account health in the status API and the web UI: `GET /api/v1/accounts` gives every account a `health` (`ok`, `degraded`, `failing` or `unknown`) from the last runs of its jobs, its credentials and its circuits, and `health_reasons` saying why it is not ok (the redacted errors of its jobs, such as the `401` of a rejected key, a re-bootstrap required, open circuits). The Overview lists the accounts that are not ok with their reasons; the Graph colors the account nodes with their health and shows the reasons in the detail panel.
- `GET /api/v1/budgets` names the `accounts` whose requests count in every scope, so that a scope keyed by a credential hash (`credential=f0e5...`) or a target says which accounts share it. The quota bars of the Overview are named by these accounts (`daily quota of campus-a`, `monthly quota of fleet-north and fleet-south`), where two accounts of a provider used to show two identical titles, and the Budgets page names them under the scope.

### Changed

- The budget ledger is persisted incrementally: every 5 seconds, Tapyr writes the counters and minute buckets that changed, where it used to rewrite every minute of every quota window (up to 1440 rows per scope of a daily quota). Far fewer writes to the state store, which matters on small disks (SD cards, small VPS). The format of the state store does not change.
- The health of a provider aggregates the health of its accounts: one account failing out of several makes the provider `degraded`, where it used to make it `failing`.
- A run skipped by the budget, an overlap or an open circuit, or a partial one, makes its job `degraded` instead of `failing`: nothing failed, but not everything was collected.

### Fixed

- Planner: a quota window shorter than the replan interval (`replanInterval`, 15 minutes by default), such as a one-minute window, is planned over a full window. Planned over what was left of it, a minute window exhausted for a few seconds, by a 429 or by another client, left no budget: every job was stretched to its maximum (12 times) and the plan reported infeasible until the next replan, 15 minutes later. On the public demo, ThousandEyes collected almost nothing. The `cisco-thousandeyes` package has an `org-burst` scenario showing it.
- Simulator: the `externalConsumption` of a scenario is charged to the window it falls in. It was charged all at once at the next request, into the current window, so a short window received everything consumed since the previous request: under its `quota-tight` scenario, the `cisco-thousandeyes` simulator answered 429 to the first request after a few quiet minutes (25 calls of the other integration in a window of 20).
- Simulator: the `latency` of a scenario delays a response after the limits counted the request, as the processing time of a server, and the request holds its concurrency slot meanwhile. Counted after the delay, requests sent at the pace of a rate limit reached it bunched together: the `teltonika-rutos` simulator answered about 50 unexpected 429 a day to Tapyr under its `quota-tight` scenario.

## [0.3.0] - 2026-11-12

### Added

- `cisco-thousandeyes`, a provider package for the Cisco ThousandEyes API v7: the network results of the tests (latency, loss, jitter, health score, per test and agent, stamped with their round), the tests and the agents, within the limit of 240 requests a minute per organization. Validated against its simulator only.
- A request query value whose expression returns `null` omits the parameter, for optional account params (an empty string is still sent).
- Processing rules per output ([ADR 0012](docs/adr/0012-output-processing.md)): `spec.processing` keeps or drops samples (`keep`, `drop` on joined label values, `__name__` for the metric name) and labels (`labelkeep`, `labeldrop`) for one output only, with the semantics of Prometheus `metric_relabel_configs`. Staleness and deduplication follow the series the output receives. Filtered samples are counted in `tapyr_output_samples_filtered_total`; samples made identical by the rules are dropped and counted as `duplicate`.
- Replan on projection: between two `replanInterval`, Tapyr checks at most once a minute that the current plan still fits every quota, and replans at once when it would exhaust one before its reset, for example when another client suddenly spends the same quota (`settings.budget.replanOnProjection`, on by default). Plan computations are counted by reason in `tapyr_plan_recomputations_total`.
- `quota-jump`, a scenario of the `example-sim` simulator: another client suddenly spends the daily quota, twice.
- `juniper-mist`, a provider package for the Juniper Mist cloud API: device status, uptime, clients, CPU and memory of access points, switches and gateways, device and client counts per site, and alarm counts by severity, from organization-level endpoints (about 20 calls an hour of the 5000 per API token). Validated against its simulator only.
- `format` on the `header` auth type, for APIs that expect a scheme name before the key (`format: "Token {value}"` sends `Authorization: Token <key>`). The formatted value is redacted like the secret.

### Changed

- `aruba-central-classic` is validated against a real Central account, strictly read-only: every template, the refresh of the rotating token pair, a restart resuming from the stored pair, and the daily quota headers. Its simulator follows the shape of the real responses (the access point model comes without the `AP-` prefix, as from the API).

### Removed

- `aruba_ap_uptime_seconds` and `aruba_gateway_aps` (`aruba-central-classic`): the real API carries no uptime in the access point list and no access point count in the gateway list, so these metrics were never produced outside the simulator.

### Fixed

- Simulator: an `externalConsumption` step ending between two requests is charged until its end; what it consumed since the last request used to be lost.
- `tapyr record` reads the token pair of auth plugins from the state store, read-only, and no longer renews rotating refresh tokens: with `aruba-central-classic`, it started from the bootstrap pair, which `tapyr run` had already rotated, and a refresh would have left `tapyr run` with a spent refresh token. Past the expiry of the stored access token, it stops with a message instead of renewing it.
- `tapyr record` replaces the site of a device (key `site`, the site name in Aruba Central) and zip codes (key `zipcode`), which a recording used to keep.
- The Grafana dashboards of `Tapyr / Overview` and of the provider packages work with push outputs: the series of slow jobs (inventories, `_info` metrics) are read with `last_over_time`, where they used to vanish between two runs and leave tables and joins empty.
- The dashboards follow one set of conventions (documented in the provider guide): colors with a meaning instead of Grafana's default thresholds (counts turned red from 80), the same quota thresholds and colors everywhere, state timelines for up/down states, mobile signal thresholds from the Teltonika recommendations, rankings as bars, and a message when there is nothing to show. The interval table of `Tapyr / Overview` shows the effective intervals and instances again.

### Security

- Built with Go 1.27.4, which fixes GO-2026-6859: an HTTP/1 server could crash on a race between `MaxBytesReader` and a response header (`net/http`).

## [0.2.1] - 2026-11-09

### Added

- A Helm chart (`charts/tapyr`), published with every release as the signed OCI artifact `oci://ghcr.io/kentrow/charts/tapyr`: one replica, the hardening of the Kustomize example, the state store on a volume, secrets from an existing Secret, an optional Ingress for the web UI and the status API, and an optional ServiceMonitor.
- A Homebrew cask for macOS: `brew install kentrow/tap/tapyr`.
- deb and rpm packages for amd64 and arm64, attached to every release: the binary, the hardened systemd unit and an example configuration. The service starts once configured; an upgrade restarts it; a removal keeps the configuration and the state store.

## [0.2.0] - 2026-11-09

### Added

- Catalog metadata on provider packages ([ADR 0011](docs/adr/0011-provider-ecosystem.md)): `displayName`, `categories`, `maintainers`, `tier` (`official` or `community`) and `validation` (`simulator` or `real-platform`, with a date and the platform version), and a structured `source` on every limit (`vendor-docs`, `measured` or `assumed`). They are optional; `tapyr provider lint --catalog` requires them, and every built-in package has them.
- `tapyr provider list --output json`: every package with its catalog metadata, its templates (request path, default interval, metrics) and its limits (value, scope, source), and the catalog categories, versioned by a `schemaVersion` ([provider catalog output](docs/spec/09-cli-api-ui.md#provider-catalog-output)).
- Documentation: the strategy of the provider ecosystem ([ADR 0011](docs/adr/0011-provider-ecosystem.md)), and a page of [provider ideas](docs/provider-ideas.md) beyond networks, with their rate-limit patterns and the engine features some of them still need.
- Demo compose: `SIM_TIME_SCALE` runs the simulators faster than real time, to watch the `bad-day` scenario in minutes instead of hours.

### Changed

- `meraki-dashboard` is validated against a real Dashboard: every template ran against a DevNet sandbox organization, and an e2e test (`TAPYR_E2E_MERAKI_API_KEY`) joins those of the Teltonika packages. The uplink metrics stay validated against the simulator only: the sandbox appliance reports no uplink.
- A newly discovered fan-out instance runs first within the jitter of its interval (10% by default), then at its point of the interval: the first data of a discovery chain no longer waits up to one interval per level (the network devices of the getting started guide could take about 40 minutes, they now come within about 10). Instances loaded from the cached inventory keep their schedule.

### Fixed

- The web UI declared Node.js 22 as its minimum to build from source; Vite 8 needs 22.12 or later.
- Enabling or changing the live debug (`settings.server.debug`) by a reload logged "settings changed that take effect after a restart", although the reload applies it, as the Live debug page of the UI says.
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

[Unreleased]: https://github.com/kentrow/tapyr/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/kentrow/tapyr/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/kentrow/tapyr/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/kentrow/tapyr/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/kentrow/tapyr/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/kentrow/tapyr/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/kentrow/tapyr/releases/tag/v0.1.0
