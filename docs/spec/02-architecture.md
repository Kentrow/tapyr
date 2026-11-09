# 02 - Architecture

## Overview

Tapyr is a single Go process. Configuration files are loaded and compiled into an in-memory runtime graph (providers, accounts, jobs, outputs). A scheduler triggers job runs; each run executes HTTP requests through the collection layer, which asks the budget engine for a reservation before every request. Responses are mapped by CEL into an internal metric model, then fanned out to the configured outputs. Everything is observable through self-metrics and a read-only status API, consumed by an embedded web UI.

```
                 ┌──────────────────────────── tapyr process ────────────────────────────┐
 config files ──▶│ config loader ─▶ compiler (schema + CEL type-check) ─▶ runtime graph   │
                 │                                                          │             │
                 │   planner ◀──────────── budget engine ◀───────┐          ▼             │
                 │      │                (ledger, buckets,       │      scheduler          │
                 │      ▼                 header sync)           │          │             │
                 │ effective intervals                           │          ▼             │
                 │                                               └── collection executor  │──▶ provider APIs
                 │                                          (auth, pagination, retries,   │    (or simulator)
                 │                                           circuit breaker)             │
                 │                                                          │             │
                 │                                                          ▼             │
                 │                                       mapping (CEL) ─▶ metric model     │
                 │                                                          │             │
                 │                          series registry (staleness, dedup)            │
                 │                                                          │             │
                 │                ┌────────────────┬───────────────┬────────┘             │
                 │                ▼                ▼               ▼                      │
                 │        prometheus pull   remote write        OTLP                      │──▶ TSDB / backends
                 │                                                                        │
                 │  state store (SQLite)   self-metrics   status API ─▶ embedded web UI   │
                 └────────────────────────────────────────────────────────────────────────┘
```

## Components

### Config loader and compiler (`internal/config`)

- Loads one or more paths (files or directories, recursively `*.yaml`/`*.yml`), multi-document YAML.
- Resolves built-in provider packages (embedded in the binary) and user provider packages (from `providerPaths`).
- Validates every document against the JSON Schema of its `kind`, then performs semantic validation (references exist, no cycles in discovery, scopes reference known dimensions, limits are coherent).
- Compiles every CEL expression against a declared environment and type-checks it.
- Produces an immutable `runtime.Graph`. Hot reload builds a new graph and swaps it atomically; in-flight runs finish on the old graph.
- Go types (and their doc comments, used as schema descriptions) are the single source of truth: JSON Schemas are generated from them (`make schema`), committed in `schemas/`, and embedded in the binary for validation and `tapyr schema`. A test fails when the committed schemas drift from the Go types.
- Loading pipeline: YAML AST (positions kept) -> generic tree -> JSON Schema validation (errors mapped back to `file:line:column`) -> strict decoding into Go types -> defaults -> semantic validation.

### Runtime compiler (`internal/runtime`)

- Turns a validated configuration into the immutable `runtime.Graph`: CEL programs compiled, template fields evaluated per account and target, effective request, pagination, auth and labels resolved per job.
- Used by `validate` (compilation errors with positions) and by `run`.

### Secrets (`internal/secret`)

Resolved secret values are held in a type whose string, JSON and `slog` forms are `[REDACTED]`, so they cannot leak through formatting by accident.

### Provider registry (`internal/provider`)

- Loads provider packages (see `03-configuration.md`, "Provider packages").
- Exposes provider definitions to the compiler and to the simulator.

### Budget engine (`internal/budget`)

The heart of the project, see `04-budget-engine.md`. Responsibilities: limit evaluation for each request, reservations, waiting, ledger persistence, header sync, 429 penalties, retry reserve, projections.

### Planner (`internal/budget/planner`)

Computes effective intervals for all jobs given quotas, priorities and estimated calls per run. Used at startup, on reload, on inventory changes, periodically, and offline by `tapyr plan`.

### Scheduler (`internal/scheduler`)

- Triggers job runs according to effective intervals, with jitter to avoid synchronized bursts: every job instance gets a random phase in `[0, jitter x interval)` and runs at `phase + k x interval`, without drift.
- Manages discovery fan-out: parent job results produce child run instances.
- Enforces one run at a time per job instance (no overlap). If a run overruns its interval, the next run is skipped and counted.
- Orders pending work by priority when the budget engine signals contention.

### Collector (`internal/collector`)

The process of `tapyr run`, wired by `cmd/tapyr`:

- What outlives a configuration reload: the state store, the series registry, the outputs, the token sessions and the HTTP server (`/metrics`, `/metrics/self`, `/healthz`, `/readyz`, the status API and the UI).
- A generation per compiled graph: its budget engine, executor, discovery and scheduler. A reload builds the next generation, stops the old one once its runs end, moves the ledger to the new engine, keeps the unchanged outputs and the schedule of the instances that stay, and retires the others.
- The run of a job instance: the executor, the mapping, the inventory update, then the samples to the outputs and the run history.
- Discovery: the children of every parent instance follow its inventory, start from the cached inventory after a restart, and are removed when it expires.
- The online plan (every `replanInterval`, on a learned quota, on inventory changes), the flush loop (ledger, series, expiry), the push of the self-metrics, and the adapter of the status API.

### Collection executor (`internal/collect`)

- Builds requests from job definitions and variables, applies auth, handles pagination.
- Calls the budget engine before every request (including pages, retries, token requests).
- Classifies errors, applies the retry policy, and reports successes and transient failures to the circuit breakers, which live in the budget engine (one per account, or per target).
- Returns the raw responses (all pages) for mapping, plus run statistics.

### Auth (`internal/auth`)

Pluggable auth providers implementing a common interface (decorate a request, react to 401, refresh tokens). Declarative types plus Go plugins for flows that cannot be expressed declaratively (`aruba_central_classic`). Tokens are persisted in the state store when the flow requires it.

### Mapping (`internal/mapping`)

Evaluates compiled CEL programs on responses to produce samples in the internal metric model. See `06-mapping-and-outputs.md`.

### Metric model and series registry (`internal/model`, `internal/series`)

- Internal pivot model, close to the OpenTelemetry data model: metric name, type (gauge, counter), unit, description, labels (attributes), samples with optional source timestamps.
- The series registry tracks, per job instance, the series seen in the last successful run and the last timestamp sent per series. It drives staleness and deduplication.

### Outputs (`internal/output`)

Pluggable outputs, each with its own bounded in-memory queue and retry policy: `prometheus` (pull), `remote_write`, `otlp`. See `06-mapping-and-outputs.md`.

### State store (`internal/state`)

SQLite (`modernc.org/sqlite`, pure Go) behind a `state.Store` interface. Holds: quota ledger, auth tokens, inventory cache, series registry snapshots, run history (bounded). See "State" below.

### Self-observability (`internal/selfobs`)

Self-metrics registry (Prometheus `client_golang`), health checks, structured logging setup. See `07-self-observability.md`.

### Status API and UI (`internal/api`, `internal/ui`, `ui/`)

Read-only JSON API under `/api/v1`, plus the embedded React UI served under `/ui/`. See `09-cli-api-ui.md`.

### Simulator (`internal/sim`)

HTTP server that serves provider fixtures and enforces provider limits, reusing the budget package server-side. See `08-simulator.md`.

## Request lifecycle

1. The scheduler triggers run `R` of job `J` for account `A` (and target `T` if multi-target), with variables `V` (from discovery).
2. The executor builds request 1 (method, URL template resolved with `V`, query, headers).
3. The auth provider decorates the request. If a token is needed and absent or expired, the auth provider performs a token request, itself subject to a budget reservation against the provider's auth limits.
4. The executor asks the budget engine to `Reserve(descriptor)`. The descriptor carries provider, account, target, credential, templated endpoint, variables, job priority, and whether the request is a retry.
5. The budget engine evaluates every limit whose scope matches, atomically. It returns a reservation, possibly after waiting (rate, concurrency), or a refusal (quota exhausted, scope penalized, circuit open) with a reason.
6. The request is sent with a per-request timeout.
7. On response, the executor reports the outcome to the budget engine (status code, quota headers, `Retry-After`), releases concurrency, and records self-metrics.
8. Errors are classified; retryable errors re-enter step 4 as retries (consuming from the retry reserve).
9. Pagination computes the next request (back to step 2) until done or `maxPages`.
10. All pages are passed to the mapping, which produces samples.
11. The series registry applies dedup and staleness, then samples are enqueued to the job's outputs.
12. Run statistics are stored (run history) and exposed.

## State

| Data | Why persistent | Notes |
|---|---|---|
| Quota ledger (fixed and rolling windows) | A restart must not reset daily or monthly consumption | Rolling windows store per-minute buckets |
| Auth tokens | Rotating refresh tokens (Aruba Classic) are lost forever if not persisted | Encrypted at rest with a key from `TAPYR_STATE_KEY` if set; warn if not set |
| Inventory cache | Avoid re-spending quota on discovery after restart | TTL-based |
| Series registry | Staleness markers and dedup across restarts | Snapshotted periodically |
| Run history | Status API and UI | Bounded (last N runs per job) |

Token buckets and concurrency counters are in-memory only (short horizon, safe to reset).

Tables: `meta` (key, value: schema version, credential salt, `instance_id` used as `service.instance.id` by OTLP), `ledger` (limit, scope key, window start, used, observed remaining, observed max, updated at), `ledger_minutes` (limit, scope key, minute, count: the buckets of rolling windows and of the current fixed window), `runs` (job, instance, run id, start, end, result, requests, pages, retries, items, samples, error; the last 50 per job instance), `tokens` (session key, sealed token, expiry; the key carries the provider, account, credential hash, target with `perTarget`, and a hash of the auth configuration, never a secret), `inventory` (job instance, item key, fields, updated at), `series_instances` (job instance, last success), `series` (job instance, output, series key, last source timestamp; rows with an empty output list the series of the last successful run) and `accumulators` (job instance, series key, total, last window) for reconstructed counters.

Hot reload (SIGHUP): the new configuration is loaded and compiled first; on any error it is rejected and logged, and the running graph stays. On success, scheduling of the old graph stops, its runs in flight finish, the budget engine is rebuilt with the ledger carried over, unchanged outputs are kept (the pull exposition keeps serving), removed job instances are retired, and the new graph starts. A reload never resets quota consumption.

The instances that stay keep their schedule: the new scheduler starts each one at the next run that was due in the old one (at once when it fell due during the reload, at most one new interval away when the interval shrank), so a reload neither loses nor adds runs and never postpones instances with long intervals. Children of fan-outs start again from the cached inventory. Token sessions are kept (a changed auth configuration opens a new one), run IDs continue one sequence per process, and the HTTP client is shared so that no connection is left behind. Changes to `server`, `state.path` and `log` settings are logged as taking effect after a restart. In flight runs get up to 30 seconds to finish before they are canceled.

The state file path defaults to `./data/tapyr.db`. Writes are batched; the ledger is flushed at least every few seconds and on shutdown. Losing the last seconds of ledger updates is acceptable; quota header sync corrects drift.

## Concurrency model

- One goroutine pool for collection, bounded globally (`settings.collection.maxConcurrentRequests`) and further bounded by concurrency limits from providers.
- Each output has its own sender goroutines and queue.
- The budget engine is safe for concurrent use; reservations across multiple limits are atomic: every limit is acquired under the engine lock (no partial acquisition, no deadlock; see `04-budget-engine.md`).
- Graceful shutdown: stop scheduling, wait for in-flight runs up to a timeout, flush outputs up to a timeout, flush state, exit.

## Repository layout

`cmd/tapyr` holds the CLI and wires the dependencies: the commands parse their flags, load and compile the configuration, and `tapyr run` hands the state store, the logger, the secrets and the auth plugins to `internal/collector`, which runs the collection.

```
tapyr/
├── .agents/skills/            # agent skills (Agent Skills format)
├── .claude/skills/            # points Claude Code to .agents/skills
├── .github/                   # CI and release workflows, issue and pull request templates, Dependabot
├── cmd/tapyr/                 # main, CLI (cobra), wiring
├── internal/
│   ├── api/                   # read-only status API (/api/v1), OpenAPI description
│   ├── auth/                  # auth interface, declarative types, token sessions
│   │   └── plugins/           # auth plugins registry (arubacentral/)
│   ├── budget/                # limits, scopes, ledger, reservations
│   │   ├── budgettest/        # engine doubles for tests
│   │   └── planner/           # effective interval planning
│   ├── buildinfo/             # version, revision and Go version (set by ldflags)
│   ├── clock/                 # Clock interface, real and fake implementations
│   ├── collect/               # executor, pagination, retry
│   ├── collector/             # the process of tapyr run: generations, reload, discovery, outputs, status API adapter
│   ├── config/                # types, loading, defaults, validation
│   ├── mapping/               # CEL environment, compilation, evaluation
│   ├── model/                 # internal metric model
│   ├── output/                # output interface
│   │   ├── otlp/
│   │   ├── prometheus/
│   │   ├── push/              # queue, batching and retries shared by the push outputs
│   │   └── remotewrite/
│   ├── provider/              # provider package loading and registry
│   ├── runtime/               # compiler: config -> runtime.Graph
│   ├── scheduler/             # scheduling, discovery fan-out
│   ├── secret/                # redacted secret values
│   ├── selfobs/               # self-metrics, health, logging
│   ├── series/                # series registry, staleness, dedup
│   ├── sim/                   # simulator server, scenarios, record mode
│   ├── state/                 # Store interface, SQLite implementation
│   ├── tools/docsite/         # documentation checks and export to the site (make docs-check)
│   ├── tools/schemagen/       # JSON Schema generator (make schema)
│   └── ui/                    # HTTP handler of the web UI (assets embedded by ui/embed.go)
├── charts/tapyr/              # Helm chart, published as an OCI artifact by the release
├── packaging/                 # what the deb and rpm packages add: scripts, example configuration
├── providers/                 # built-in provider packages
│   ├── embed.go               # embeds the packages in the binary
│   ├── aruba-central-classic/
│   ├── example-sim/           # vendor-neutral reference package
│   ├── meraki-dashboard/
│   ├── teltonika-rms/
│   └── teltonika-rutos/
├── ui/                        # React + Vite + TypeScript app, and embed.go (package ui) embedding ui/dist
├── schemas/                   # generated JSON Schemas (committed, embedded with go:embed)
├── examples/
│   ├── config/                # example configuration
│   ├── compose/               # demo docker compose (tapyr, sims, VictoriaMetrics, Grafana)
│   └── alerts/                # example alert rules on the self-metrics, with promtool tests
├── dashboards/                # Grafana dashboards for Tapyr self-monitoring
├── docs/
│   ├── spec/
│   ├── adr/
│   ├── reference/             # configuration reference, generated from the schemas (make schema)
│   ├── alerts.md              # the example alerts and what to do when they fire
│   ├── install.md
│   ├── outputs.md             # Prometheus, remote write, OTLP
│   ├── providers.md           # writing a provider package
│   ├── site.yml               # pages of the documentation site (ADR 0010)
│   └── roadmap.md
├── scripts/                   # repository checks and release helpers
├── test/
│   ├── e2e/                   # opt-in tests against real platforms (build tag e2e)
│   └── integration/           # push outputs against VictoriaMetrics and the OpenTelemetry Collector
├── Makefile
├── Dockerfile                 # image built from source
├── Dockerfile.release         # image of a release, from the GoReleaser binaries
├── .goreleaser.yaml
├── .golangci.yml
├── AGENTS.md                  # instructions for coding agents (CLAUDE.md points to it)
├── CHANGELOG.md
├── CODE_OF_CONDUCT.md
├── CONTRIBUTING.md
├── LICENSE                    # Apache-2.0
├── README.md
└── SECURITY.md
```

## Main dependencies

| Concern | Library |
|---|---|
| CLI | `github.com/spf13/cobra` |
| YAML | `github.com/goccy/go-yaml` (good error positions) |
| JSON Schema generation | `github.com/invopop/jsonschema` |
| JSON Schema validation | `github.com/santhosh-tekuri/jsonschema/v6` |
| CEL | `cel.dev/cel-go` |
| SQLite | `modernc.org/sqlite` (no CGO) |
| Prometheus exposition and self-metrics | `github.com/prometheus/client_golang` |
| Remote write | minimal protos encoded with `google.golang.org/protobuf/encoding/protowire`, compressed with `github.com/klauspost/compress/snappy` |
| OTLP | `go.opentelemetry.io/collector/pdata` (`pmetric` payloads built directly), sent over OTLP/HTTP or gRPC (`google.golang.org/grpc`) |
| Logging | `log/slog` (standard library) |
| UI | React, Vite, TypeScript, `@xyflow/react` (graph); charts are hand-written SVG |
| Release | GoReleaser |
| Lint | golangci-lint |
