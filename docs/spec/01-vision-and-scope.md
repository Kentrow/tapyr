# 01 - Vision and scope

## Problem

Many platforms are now cloud-managed and expose their telemetry only through REST APIs: network equipment first (Aruba Central, Cisco Meraki, Teltonika RMS, and many others), but also security services, Internet measurement, cloud costs, developer platforms and energy. Getting that telemetry into an open observability stack (Prometheus, VictoriaMetrics, Mimir, any OTLP backend) is surprisingly painful:

- **Vertical exporters** exist for some vendors (one project per vendor, hardcoded endpoints, inconsistent quality). Many vendors have none.
- **Generic tools** (Telegraf `inputs.http`, Redpanda Connect, Vector) can call HTTP APIs, but treat rate limits as an afterthought: at best a per-input interval or a simple token bucket. None of them understands a **daily or monthly quota**, a limit **scoped per organization**, a quota **shared with other consumers**, or the need to **plan** polling to fit a budget.
- **Quota exhaustion is a silent failure.** When a daily quota runs out at 3 pm, every device polled through that API "goes dark" until midnight UTC, and the root cause sits two layers below the symptom.

## Vision

Tapyr is a **declarative API-to-metrics gateway** where:

1. A **provider** (an API family, e.g. "Meraki Dashboard API v1") is described once, declaratively: base URL, auth, pagination, error semantics, and **all of its rate limits**.
2. **Accounts** instantiate a provider with credentials (one Meraki API key, one Aruba Central tenant, one Teltonika router).
3. **Jobs** describe what to collect: an endpoint, a desired interval, a priority, optional discovery fan-out, and a CEL mapping from JSON to metrics.
4. A **budget engine** guarantees that every limit is respected, plans effective intervals to fit quotas, keeps a reserve for retries, and syncs with the quota headers returned by the API.
5. Collected data is **exposed or pushed in standard formats**: Prometheus exposition (pull), Prometheus remote write (push, covers Prometheus, VictoriaMetrics, Mimir, Thanos), OTLP (push).
6. Tapyr **observes itself** and exposes rich self-metrics.
7. Every provider ships with a **simulator** driven by the same definition, enforcing the same limits, so that providers can be developed and tested without access to the vendor platform.

**Network first, not network only.** Networks are the first use case: the built-in packages, the examples and the documentation start there. The core makes no network-specific assumption, and any metric-oriented API must be expressible as a provider package; a mechanism another domain needs is added to the engine as a generic feature ([ADR 0011](../adr/0011-provider-ecosystem.md)).

Long-term, provider packages (definition + default jobs + simulator fixtures + dashboards + tests) become a **community ecosystem**, in the spirit of Terraform providers or snmp_exporter modules, but declarative and therefore accessible to engineers who do not write Go. It grows in phases: first a static catalog on tapyr.eu generated from the packages of this repository, with catalog metadata on every package (who maintains it, how it was validated, the source of every limit); later, if justified, packages distributed as OCI artifacts.

## Positioning

| Tapyr is | Tapyr is not |
|---|---|
| A specialized gateway from REST APIs to metrics | A general-purpose ETL or stream processor |
| Declarative (YAML + CEL) | A collection of hardcoded vendor exporters |
| Read-only towards providers | A configuration or automation tool |
| A single static binary | A platform requiring a database server |

## Target users

- Network and infrastructure engineers who manage cloud-managed equipment and run an open observability stack (the first use case).
- Engineers who monitor any other platform through a metric-oriented API with scarce quotas: security, energy, cloud costs, developer tools.
- Observability consultants who need to integrate many vendor APIs quickly and must size API consumption upfront.
- Contributors who know a vendor API well and want to package it for others.

## Scope of v0.1.0

Functional:

- Declarative config model: `Provider`, `Account`, `Job`, `Output` (+ `Settings`), YAML, multi-document, splittable across files, validated by JSON Schema.
- Budget engine: rate (token bucket), concurrency, fixed-window quotas (minute, hour, day, week, month, with reset time and timezone), rolling-window quotas; scopes (instance, account, target, credential, endpoint, request variables); header sync; 429 and `Retry-After` handling; retry reserve; priority-aware planning; persistent ledger.
- `tapyr plan`: offline estimation of call consumption per quota and effective intervals.
- `tapyr validate`: config and provider validation, including CEL type-checking.
- Collection: auth types `none`, `header`, `bearer`, `basic`, `oauth2_client_credentials`, `session_login` (generic login flow), plus the `aruba_central_classic` auth plugin (rotating refresh tokens).
- Pagination: `none`, `offset`, `page`, `link_header`, `cursor`, `expression`.
- Discovery: jobs that fan out from the results of a parent job, with an inventory cache.
- Multi-target accounts (one account, N device endpoints, e.g. Teltonika RutOS routers).
- Retries with error classification, exponential backoff with jitter, circuit breaker per account.
- CEL mapping: gauges, counters, `_info` metrics, labels, optional source timestamps.
- Series lifecycle: staleness when a series disappears, deduplication of overlapping windows.
- Outputs: Prometheus pull endpoint, Prometheus remote write 1.0, OTLP/HTTP (protobuf) and OTLP/gRPC.
- Self-observability metrics, `/healthz`, `/readyz`, structured JSON logs.
- Persistent state in SQLite (quota ledger, tokens, inventory cache, series registry).
- Config hot reload on SIGHUP.
- Simulator (`tapyr sim`) with fixtures, scenarios and limit enforcement; record mode (`tapyr record`) with anonymization.
- Read-only status API (JSON, versioned) and read-only web UI embedded in the binary.
- Four providers: `teltonika-rms`, `teltonika-rutos`, `meraki-dashboard`, `aruba-central-classic`.

Delivery:

- Single static binary (Linux, macOS, Windows; amd64 and arm64), Docker image.
- Demo `docker compose`: Tapyr + simulators + VictoriaMetrics + Grafana with provider dashboards and a "monitoring of monitoring" dashboard.
- Generated JSON Schemas for editor autocompletion.
- Documentation sources in `docs/` (the documentation site docs.tapyr.eu is built from them after v0.1.0, [ADR 0010](../adr/0010-documentation-site.md)).

## Out of scope (keep in mind, do not build)

- Distributed architecture (workers, sharding, leader election, HA).
- Shared state backends (Redis) and persistent output buffering (WAL).
- Editable UI, budget impact simulation in the UI, mapping authoring assistant.
- Push-based sources: webhooks (Meraki), streaming APIs (Aruba Central websocket/protobuf).
- Logs and events as outputs (metrics only).
- Write operations against provider APIs.
- Provider registry and distribution as OCI artifacts (phase 2 of the provider ecosystem, [ADR 0011](../adr/0011-provider-ecosystem.md)).
- Generation of provider skeletons from OpenAPI specifications.
- Aruba Central "new" (HPE GreenLake-based) provider.
- Prometheus remote write 2.0.
- Tracing of Tapyr runs (OpenTelemetry traces).

These items need an issue and an agreement before any code, but the design must not prevent them. In particular: the status API is the only interface the UI uses; state access goes through an interface so another backend can be added; outputs are pluggable; the provider package format is versioned.

## Success criteria for v0.1.0

- A new user runs the demo compose and sees data and self-metrics in Grafana within 5 minutes, without any vendor account.
- `tapyr plan` on a Meraki or Aruba configuration gives a consumption estimate within 15% of the actual consumption measured against the simulator over 24 simulated hours.
- Over a 24-hour simulated run with an undersized quota, Tapyr never receives an unexpected 429 from a quota limit (only from scenario-injected faults), degrades low-priority jobs first, and keeps critical jobs on schedule.
- Teltonika RMS and Teltonika RutOS are validated end-to-end against real platforms.
- Adding a simple new provider requires only YAML and CEL files, no Go code.
