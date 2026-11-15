# 09 - CLI, status API and web UI

## CLI

Single binary `tapyr`, built with cobra. Global flags: `--config <path>` (repeatable, files or directories; env `TAPYR_CONFIG`, a list separated by the OS path list separator, `:` on Unix and `;` on Windows), `--log-level`, `--log-format` (env `TAPYR_LOG_LEVEL`, `TAPYR_LOG_FORMAT`). Flags win over environment variables.

Common exit codes: `0` success, `1` usage error or internal error, `2` invalid configuration (including missing files and YAML syntax errors), `3` infeasible plan.

| Command | Purpose | Exit codes |
|---|---|---|
| `tapyr run` | Run the collector (server, scheduler, outputs) | 0 on clean shutdown, 1 on fatal error |
| `tapyr validate [--check-secrets] [--provider-path DIR ...]` | Load, schema-validate, semantic-validate, compile CEL; optionally resolve secrets | 0 valid, 2 invalid (errors with file:line:column) |
| `tapyr plan [--fanout tmpl=N ...] [--output table\|json] [--provider-path DIR ...]` | Compute effective intervals and projected consumption per quota scope, offline. Uses the state store inventory and run history if present | 0 feasible, 3 infeasible |
| `tapyr sim ...` | Run provider simulators (see `08-simulator.md`) | |
| `tapyr record --job J [--job ...] --out DIR [--anonymize FILE] [--provider-path DIR ...]` | Run jobs once and record their responses, anonymized (see `08-simulator.md`) | 0 ok, 1 error, 2 invalid configuration |
| `tapyr schema [--kind K] [--out dir]` | Print or write the JSON Schemas embedded in the binary | |
| `tapyr provider list [--output table\|json]` | List built-in and user provider packages with versions; `json` is the [provider catalog output](#provider-catalog-output) | |
| `tapyr provider lint <dir> [--update] [--catalog]` | Validate a provider package, lint its dashboards and run its contract tests (`--update` rewrites `tests/expected.prom`, `--catalog` also requires the catalog metadata) | 0 ok, 2 errors |
| `tapyr version` | Version, revision, Go version | |

`--provider-path` (validate, plan, record, and sim) loads the provider packages of a directory in addition to the built-in ones and to the `providerPaths` of the configuration, after the latter: it serves a package being written, before it is built in or referenced by a configuration.

Signals for `run`: SIGHUP reloads the configuration (validated first; an invalid config is rejected and the running config kept); SIGINT/SIGTERM trigger graceful shutdown.

`tapyr plan` reads run history and inventory from the state store when the file exists (read-only); otherwise it uses `estimate.pagesPerRun` and the `--fanout` hints. Quotas are evaluated over a full window from now: plan answers "does this configuration fit", while the running engine replans with the remaining budget of the current window.

Example `tapyr plan` output, for the example configuration of the `aruba-central-classic` package (a job stretched to fit a quota shows its stretch factor and reason):

```
QUOTA SCOPE                  WINDOW       BUDGET         PROJECTED                RESERVE  STATUS
aruba-central-classic/daily  24h          4500           529                      500      ok (11.8%)
  account=campus
  job aruba-aps              desired 5m   effective 5m   288 runs x 1 page = 302
  job aruba-gateways         desired 15m  effective 15m  96 runs x 1 page = 101
  job aruba-sites            desired 1h   effective 1h   24 runs x 1 page = 25
  job aruba-switches         desired 15m  effective 15m  96 runs x 1 page = 101
```

### Provider catalog output

`tapyr provider list --output json` describes the packages the command lists, in precedence order, for the provider catalog of tapyr.eu ([ADR 0011](../adr/0011-provider-ecosystem.md)): the site repository builds the catalog from the output of a tagged release, as it builds the documentation from `docsite export` ([ADR 0010](../adr/0010-documentation-site.md)). It is an interface of the site repository. The keys are the camelCase names of the configuration fields they come from; an optional field without a value is left out.

- `schemaVersion` (1): changes when a field is removed, renamed or changes meaning, never when one is added. Readers ignore unknown fields.
- `tapyrVersion`: the version of the binary.
- `categories`: the controlled vocabulary of [`providers/categories.yaml`](../../providers/categories.yaml), `name` and `description`.
- `providers`: one entry per package:
  - from `metadata`: `name`, `version`, `displayName`, `description`, `docs`, `categories`, `maintainers`, `tier`, `validation` (`status`, `date`, `platformVersion`, `notes`);
  - `origin`: `builtin`, or the directory of a user package; `status`: `active`, or `overridden` by a package of the same name listed before it;
  - `readme`: for a built-in package, the path of its README in this repository (`providers/<name>/README.md`); the site maps it to the page of the documentation version it publishes;
  - `simulator`: whether the package has one; `auth`: the auth type, `plugin:<name>` for an auth plugin;
  - `templates`: `name`, `description`, the request `path`, `defaultInterval` (`5m`, `1h`) and the sorted `metrics` names it produces;
  - `limits`: `name`, `kind`, `value` (for humans, defaults applied: `10/s, burst 20`, `4 concurrent requests`, `100000 per month, reset on day 1 at 00:00 UTC`, `auto (5000 until learned) per day, reset at 00:00 UTC`), `scope` (the dimensions of `scope.by`), `description` and `source` (`kind`, `url`, `date`).

The output of the built-in packages is kept in `cmd/tapyr/testdata/provider-list.json` (`go test ./cmd/tapyr -run TestProviderListJSON -update`), so that a change of the interface shows in the diff of a pull request.

## HTTP server

One listener (`settings.server.listen`, default `127.0.0.1:9790`) serving:

| Path | Content |
|---|---|
| `/metrics` | Collected data (prometheus output) |
| `/metrics/self` | Self-metrics |
| `/healthz`, `/readyz` | Health |
| `/api/v1/...` | Status API (read-only, JSON) |
| `/ui/` | Embedded web UI (redirect `/` to `/ui/`) |

Optional basic auth (`settings.server.basicAuth`) protects `/api` and `/ui` (and optionally `/metrics*` with `settings.server.basicAuth.protectMetrics: true`). Put Tapyr behind a reverse proxy for TLS, or set `settings.server.tls` (`certFile`, `keyFile`) to serve HTTPS directly.

## Status API

Read-only, JSON, versioned under `/api/v1`. It is the **only** interface the UI uses. An OpenAPI 3 description is maintained by hand in `internal/api/openapi.yaml` and served at `/api/v1/openapi.yaml`.

| Endpoint | Content |
|---|---|
| `GET /api/v1/status` | Version, uptime, config hash, last reload result and errors, readiness reasons, plan feasibility |
| `GET /api/v1/providers` | Providers with package version, accounts count, health summary |
| `GET /api/v1/accounts` / `{name}` | Accounts (no secrets), targets, labels, circuit state, auth status, health and its reasons |
| `GET /api/v1/jobs` / `{name}` | Effective config (after defaults and template expansion), desired and effective intervals, instances, last run summary, next run, and `collected` (false, with `not_collected_reason`, for a job that is not collected, [ADR 0013](../adr/0013-unconsumed-jobs.md)) |
| `GET /api/v1/jobs/{name}/instances` | Fan-out instances with their variables and last result |
| `GET /api/v1/jobs/{name}/runs?instance=` | Recent runs with statistics and redacted errors |
| `GET /api/v1/budgets` | Every limit and scope key: kind, max, used, remaining (internal and observed), external usage, reserve, window bounds, projection, and the accounts whose requests count in the scope (named by its account, target or credential dimension; every account of the provider for a scope without one) |
| `GET /api/v1/budgets/{limit}/timeline?scope=` | `{limit}` is `provider/limit`, URL-encoded (`meraki-dashboard%2Fdaily`). Consumption time series for the current and previous window (per-minute buckets from the ledger) |
| `GET /api/v1/plan` | The current plan (same data as `tapyr plan`) |
| `GET /api/v1/graph` | Nodes and edges: provider, account, target, job, job instance groups, output |
| `GET /api/v1/outputs` | Outputs with queue state and last send results |
| `GET /api/v1/debug/jobs/{name}/last?instance=` | Last raw response pages (redacted, truncated to `settings.server.debug.maxBodyBytes`, default 1 MiB) and the samples produced. Only when `settings.server.debug.enabled` |
| `GET /api/v1/events` | Server-Sent Events stream: run finished, plan changed, circuit changed, config reloaded |

Rules: never expose secrets; redact headers as in logs; paginate list endpoints (`?limit=&cursor=`); stable field names (snake_case); ISO 8601 timestamps.

### Health

Every job, account and provider has a health: `ok`, `degraded`, `failing`, or `unknown` before its first run. Parts combine the same way at every level: `ok` or `failing` when every known part is, `degraded` when they differ, `unknown` parts left out.

- **Run**: `success` is ok, `failed` is failing; `partial` and the skipped runs (`skipped_budget`, `skipped_overlap`, `skipped_circuit`) are degraded: nothing failed, but not everything was collected.
- **Job**: the last runs of its instances.
- **Account**: the last runs of its job instances, made worse by its credentials (a manual re-bootstrap required fails it) and by its circuits (open for the account or for every target fails it; open for some targets, or half open, degrades it). `health_reasons` says why, when it is not ok: the redacted errors of its jobs (the jobs that share an error share a reason, three reasons at most, then how many more), the re-bootstrap, the open circuits. A rejected key shows as a failing account with the `401` of its jobs.
- **Provider**: its accounts. One account failing out of several degrades the provider instead of failing it.

The Overview lists the accounts that are not ok with their reasons, and the Graph colors the account nodes with their health and shows the reasons in the detail panel.

### Implementation notes

- `internal/api` serves the API from a read-only `Source` interface implemented by `tapyr run`; it never reaches into the collector state directly. The OpenAPI description (OpenAPI 3.1) is maintained by hand; a test checks that every route is documented and that every response of the test suite validates against its schema.
- Pagination: `limit` (default 100, at most 1000) and an opaque `cursor`; responses of list endpoints are `{"items": [...], "next_cursor": "..."}`, `next_cursor` omitted on the last page.
- Errors are `{"error": {"code": "not_found", "message": "..."}}` with the matching HTTP status.
- Budget timelines use buckets of 1 minute for windows up to one day and of 1 hour beyond; the current window's buckets are persisted with the ledger, the previous window's are kept in memory only.
- Basic auth compares in constant time and answers `401` with `WWW-Authenticate`; `/healthz` and `/readyz` are never protected. With `tls`, the listener serves HTTPS only.

## Web UI (read-only)

Inspired by the Grafana Alloy UI: see what is configured and how it behaves, never edit.

### Technology

React + Vite + TypeScript, compiled to static assets in `ui/dist/app`, embedded with `go:embed` by `ui/embed.go` and served by `internal/ui`. Graph rendering with `@xyflow/react`. Budget timelines and gauges are hand-written SVG. No server-side rendering. In development, Vite proxies `/api` to a running `tapyr run`.

The UI is built in CI and in `make build`; a binary built without UI assets serves a minimal page explaining how to build them.

### Pages

1. **Overview**: health summary, plan feasibility banner, providers and accounts cards with status, quota gauges (used / projected / limit per window, named by the accounts of their scope), recent errors.
2. **Graph**: provider -> account -> (targets) -> inventory jobs -> metric jobs -> outputs. Node color by health. Click a node to open its detail panel. Fan-out shown as grouped nodes with instance counts.
3. **Job detail**: effective config (YAML view), desired vs effective interval with stretch reason, instances table, recent runs (result, duration, requests, pages, retries, samples), errors. A job that is not collected says so with its reason, on the jobs list and its detail, and stays out of the counts of the Overview and of the Graph.
4. **Budgets**: one card per limit and scope: timeline of consumption over the window with projection line, reserve, external usage, window reset time. Rate limits show wait time and 429s.
5. **Plan**: the plan table, sortable, with stretched jobs highlighted.
6. **Outputs**: queue fill, send rate, failures, last success.
7. **Live debug** (only when enabled): pick a job instance, see the last raw response (pretty JSON, collapsible) side by side with the samples produced. Refresh on new run (SSE).

Every page shows, at the foot of the navigation, the version of the running Tapyr (`Tapyr v0.3.0`; a binary built without a version, such as `go build` in a clone, shows `Tapyr dev` and its short revision), with the full version, the revision and the Go version in its tooltip. It is read from `GET /api/v1/status` when the UI loads and again when the event stream reconnects, so an upgrade shows after the restart.

### Non-functional

- Responsive down to tablet width; usable on a phone for the overview.
- Light and dark themes (follows the system preference).
- Live updates via SSE, with polling fallback.
- No external network calls (fonts and assets bundled).
- Accessible: keyboard navigation, sufficient contrast, no information conveyed by color alone.

### Implementation notes

- **Serving.** The UI is served under `/ui/` (`/` redirects there) with `Content-Security-Policy: default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'none'; frame-ancestors 'none'` (the `CSP` constant of `internal/ui`), so that the browser itself refuses any external call; `settings.server.ui.enabled: false` removes it. Hashed assets are cached for a year, `index.html` is never cached. A binary built without the UI serves a page explaining `make ui`.
- **Events.** `GET /api/v1/events` is a Server-Sent Events stream of `run_finished` (job, instance, result, duration, samples), `plan_changed`, `circuit_changed` (account, target, state) and `config_reloaded` (result, error), each `data` a JSON object with `time`. A comment line is sent every 15 seconds to keep proxies from closing the stream. Slow clients lose events rather than slowing the collector: the UI refetches what it shows on every event and polls every 15 seconds when the stream is down.
- **Debug.** With `settings.server.debug.enabled`, the collector keeps, per job instance and in memory only, the pages of its last run (status, the `Content-Type` and quota headers, the body truncated to `maxBodyBytes`) and the samples it produced. Bodies go through the log redactor, so configured secrets and tokens never appear. Without it, the endpoint answers `404` with the code `debug_disabled`.
- **Look.** The colors, fonts and shapes follow tapyr.eu: background `#f2f8fb` / `#0b1824`, surface `#ffffff` / `#102335`, ink `#0f2a3f` / `#e4f1f7`, cyan `#3aa9d6` / `#5cc3ea`, sun `#ffc845` / `#ffd166`, red `#cc2f35` / `#ff7a7a`; Bricolage Grotesque for titles, JetBrains Mono for data. States always carry a word or an icon next to their color.
