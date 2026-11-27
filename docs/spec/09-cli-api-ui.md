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
| `tapyr provider list [--output table\|json] [--provider-path DIR ...]` | List built-in and user provider packages with versions; `json` is the [provider catalog output](#provider-catalog-output) | |
| `tapyr provider show <name> [--example] [--output text\|json] [--provider-path DIR ...]` | Describe a provider package: what it is and its validation, its secrets and params, its templates, its limits ([example](#provider-show)); `--example` prints its example configuration, `json` its entry of the [provider catalog output](#provider-catalog-output) | 0 ok, 1 unknown provider |
| `tapyr auth login <account> --secret-file FILE [--force] [--listen \| --device] [--provider-path DIR ...]` | Run the consent of an `oauth2_authorization_code` account ([ADR 0016](../adr/0016-oauth2-authorization-code.md)) and write its refresh token to `FILE` (mode 0600; an existing file needs `--force`). By default it prints the authorization URL and reads the URL the browser was redirected to (or the code) from the standard input, checking its `state`; `--listen` serves the loopback redirect URL itself; `--device` uses the device authorization grant. PKCE S256 unless the package says `pkce: none`. Its token requests go through an in-memory budget engine with the provider limits; it never prints a token and never opens the state store | 0 ok, 1 usage or flow error, 2 invalid configuration or account |
| `tapyr provider new <name> [--dir DIR]` | Write the skeleton of a provider package in `DIR/<name>` (default the current directory): `provider.yaml` with a rate limit and a daily quota, one job template, a simulator with the required scenarios, a contract test and its expected samples, an example, a dashboard and a README, with `TODO` comments where the vendor documentation goes. The skeleton passes `tapyr provider lint --catalog` as written; the name is at most 55 characters (its template is `<name>-devices`) | 0 ok, 2 invalid name or existing directory |
| `tapyr provider lint <dir> [--update] [--catalog]` | Validate a provider package, lint its dashboards and run its contract tests (`--update` rewrites `tests/expected.prom`, `--catalog` also requires the catalog metadata) | 0 ok, 2 errors |
| `tapyr version` | Version, revision, Go version | |

`--provider-path` (validate, plan, record, sim, provider list and provider show) loads the provider packages of a directory in addition to the built-in ones and to the `providerPaths` of the configuration, after the latter: it serves a package being written, before it is built in or referenced by a configuration.

Signals for `run`: SIGHUP reloads the configuration (validated first; an invalid config is rejected and the running config kept); SIGINT/SIGTERM trigger graceful shutdown.

`tapyr plan` reads run history and inventory from the state store when the file exists (read-only); otherwise it uses `estimate.pagesPerRun` and the `--fanout` hints. Quotas are evaluated over a full window from now: plan answers "does this configuration fit", while the running engine replans with the remaining budget of the current window. The implicit jobs, which Tapyr adds to list the items of a fan-out that no job declares, are listed after the quotas with what they list and for which jobs (`demo.example-sim-networks  lists the networks for network-devices`). Scopes read as words: `account demo`, `the Tapyr process` for the `instance` dimension. For an output with a disk buffer, plan says how long an outage of the backend the buffer holds at the samples per minute measured, at about 64 bytes per sample on disk, and whether `maxSize` or `maxAge` bounds it first.

Example `tapyr plan` output, for the example configuration of the `aruba-central-classic` package (a job slowed down to fit a quota says by how much and why). The output ends with a conclusion in words: the plan fits as desired, fits by slowing jobs down, or does not fit, with, for every quota it does not fit, why and what to change (the problem `plan_infeasible`, see [Messages](#messages)):

```
QUOTA                        WINDOW       AVAILABLE      PROJECTED                FOR RETRIES  STATUS
aruba-central-classic/daily  24h          4500           529                      500          fits (11.8%)
  account campus
  job aruba-aps              desired 5m   effective 5m   288 runs x 1 page = 302
  job aruba-gateways         desired 15m  effective 15m  96 runs x 1 page = 101
  job aruba-sites            desired 1h   effective 1h   24 runs x 1 page = 25
  job aruba-switches         desired 15m  effective 15m  96 runs x 1 page = 101

The plan fits: every job runs at its desired interval.
```

### Provider show

`tapyr provider show <name>` describes a package for the person writing an account: what it is and how it was validated, the secrets and params of its accounts with their descriptions, its templates (default interval, priority when not `normal`, the template whose items a fan-out runs for, the role of a template that lists items for others, the metrics), and its limits (value, scope, description, source), in the words of the [status API](#messages): no scope keys, no internal names. Templates come parents first. It ends with the command of the example configuration. The text of the built-in package `meraki-dashboard` is kept in `cmd/tapyr/testdata/provider-show.txt`; an excerpt:

```
meraki-dashboard 0.1.0 - Cisco Meraki Dashboard
Cisco Meraki Dashboard API v1

Validation:  on a real platform (Dashboard API v1, 2026-11-07) and the simulator
API docs:    https://developer.cisco.com/meraki/api-v1/
Origin:      built in
Simulator:   tapyr sim --provider meraki-dashboard
Auth:        bearer

Secrets, on the account as secrets: { NAME: { env: VAR } }:
  apiKey  required  Dashboard API key (read-only administrator recommended)

Templates, named by a job as template: NAME (parents first):
  meraki-organizations
    every 1h, priority low, also lists items for other templates
    metrics: meraki_organization_api_enabled, meraki_organization_info
  meraki-device-availabilities
    every 5m, priority high, for each item of meraki-organizations
    metrics: meraki_device_alerting, meraki_device_up

Limits, which Tapyr keeps to:
  org-rate
    10/s, burst 20, per organizationId
    10 requests per second per organization, plus 10 more in the first second
    (token bucket), shared by every application of the organization
    source: vendor documentation (2026-11),
            https://developer.cisco.com/meraki/api-v1/rate-limit/

Example configuration: tapyr provider show meraki-dashboard --example
```

`--example` prints the example configuration of the package (`examples/*.yaml`, the files one after the other) as it is, comments included, to copy and adapt. When it declares no `Output`, it ends with a commented one (a Prometheus endpoint) and the link to [Configure your accounts](../configure-accounts.md): without an output, its jobs collect nothing ([ADR 0013](../adr/0013-unconsumed-jobs.md)). For every built-in package, that example with its output uncommented passes `tapyr validate` (`TestProviderShowExample`).

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
  - `params` and `secrets`: `name`, `required` and `description`, in the order of the package;
  - `templates`: `name`, `description`, the request `path`, `defaultInterval` (`5m`, `1h`), `defaultPriority` (`normal` when the template sets none), `role` (`metrics` when the template sets none), `forEach` (the template whose items it runs for, for a fan-out) and the sorted `metrics` names it produces;
  - `limits`: `name`, `kind`, `value` (for humans, defaults applied: `10/s, burst 20`, `4 concurrent requests`, `100000 per month, reset on day 1 at 00:00 UTC`, `auto (5000 until learned) per day, reset at 00:00 UTC`), `scope` (the dimensions of `scope.by`), `description` and `source` (`kind`, `url`, `date`).

`tapyr provider show <name> --output json` prints the entry of one package, as an object of the same shape (the active package of that name).

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
| `GET /api/v1/status` | Version, uptime, config hash, last reload result and errors, readiness reasons, plan feasibility, whether the live debug is enabled (`debug_enabled`) |
| `GET /api/v1/providers` | Providers with package version, accounts count, health summary |
| `GET /api/v1/accounts` / `{name}` | Accounts (no secrets), targets, labels, circuit state, auth status, health and its reasons |
| `GET /api/v1/jobs` / `{name}` | Effective config (after defaults and template expansion), desired and effective intervals, instances, last run summary, next run, `collected` (false, with `not_collected_reason`, for a job that is not collected, [ADR 0013](../adr/0013-unconsumed-jobs.md)), and for an implicit job what it lists (`items`, from the name of its template without the start the templates of its package share: `networks`) and the configured jobs it lists them for (`discovery_for`) |
| `GET /api/v1/jobs/{name}/instances` | Fan-out instances with their variables and last result |
| `GET /api/v1/jobs/{name}/runs?instance=` | Recent runs with statistics and redacted errors |
| `GET /api/v1/budgets` | Every limit and scope key: kind, max, used, remaining (internal and observed), external usage, reserve, window bounds, projection, and the accounts whose requests count in the scope (named by its account, target or credential dimension; every account of the provider for a scope without one); the description, the source (`kind`, `url`, `date`) and the calendar period of the limit, from the provider definition |
| `GET /api/v1/budgets/{limit}/timeline?scope=` | `{limit}` is `provider/limit`, URL-encoded (`meraki-dashboard%2Fdaily`). Consumption time series for the current and previous window (per-minute buckets from the ledger) |
| `GET /api/v1/plan` | The current plan (same data as `tapyr plan`) |
| `GET /api/v1/graph` | Nodes and edges: provider, account, target, job, job instance groups, output |
| `GET /api/v1/outputs` | Outputs with queue state and last send results, the series they receive, their series limit, the new series it holds back and the samples it dropped, and the jobs not measured yet ([ADR 0014](../adr/0014-output-series.md)), and their disk buffer (`buffer`: size and `maxSize`, samples not acknowledged and when the oldest was queued, `maxAge`, writable; [ADR 0015](../adr/0015-persistent-output-buffer.md)) |
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

### Messages

Every message that reports a problem to a user is written the same way, and every new one follows it; the errors of the runs do, in the status API and the web UI:

1. **What happened**, in one plain sentence, from the point of view of the user: "The provider rejected the credentials of lab.", not "auth /sites: HTTP 401".
2. **What to do**, always: a step to take, or "Nothing to do: Tapyr tries again." when Tapyr recovers on its own. A secret is named by its reference (the environment variable or the file), never by its value.
3. **Where to read more**: a link to the section of the [troubleshooting guide](../troubleshooting.md) in the documentation of the running version (`https://docs.tapyr.eu/<minor>/troubleshooting/#...` for a release, `/dev/` for a release candidate or a development build).
4. **The technical detail** last, smaller: the redacted error, for those who need it.

In the status API, a run that did not succeed (`last_run`, the runs of a job, the instances, the live debug) carries a `problem`: `class`, `title` (the sentence), `action`, `until` when it should end on its own (a quota reset, a pause), and `docs_url`. An account that is not ok carries the problem to fix first: credentials to renew, then the error of a failing job (rejected credentials first), then of a degraded one. A job waiting for a failing parent carries the problem of the parent. The `run_finished` event carries the sentence as `problem`.

| Class | When | Section |
|---|---|---|
| `auth` | The provider rejected the credentials (401, a rejected token or login request) | Authentication fails |
| `forbidden` | The credentials lack a permission (403) | Authentication fails |
| `reauth` | The credentials can no longer be renewed | Authentication fails |
| `rate_limited`, `penalized` | The provider rate limits Tapyr, or asked it to wait | Quota problems |
| `quota_exhausted` | A quota is used up until its reset | Quota problems |
| `deadline` | The run gave up waiting for the budget | Jobs run less often than configured |
| `server_error`, `unreachable`, `circuit_open` | A 5xx, no answer or a timeout, or the circuit open after repeated failures | The provider fails or does not answer |
| `client` | Another 4xx (404, 400) | Metrics are wrong or missing |
| `plan_infeasible` | A quota scope the plan does not fit: the title says what its jobs need and what it leaves, the action which critical job to slow down and to which interval (the one with the most calls when it is enough on its own), or to lower its priority | Quota problems |
| `decode`, `mapping` | A response that cannot be read, or turned into metrics | Metrics are wrong or missing |
| `unknown` | Anything else | Asking for help |

The live debug keeps the response that failed (an error status, a body that does not decode), redacted and truncated like the other pages, and says why a capture has no response or no sample.

### Waiting is not failing

A collected job that has not run yet **waits**; it does not fail. `waiting` on the job (`GET /api/v1/jobs`) says for what, and the graph gives the reason on its job nodes:

- `first_run`: its instances are scheduled; `until` is their first run.
- `parent`: a fan-out without instances, whose parent has not listed its items yet (it waits for its first run, the budget, or its own parent); `parent` names it and `until` is its next run (the next run of the first parent up the chain that has instances).
- `parent_failing`: the parent, or a parent of the parent, fails: `failing_job` names it, with the redacted `error` and `failed_at` of its last run (`failed`, or skipped by an open circuit), and `until` is its next try.

`waiting` is absent once the job has run, for a job not collected, and for a fan-out whose parent ran and listed no item. The health of a waiting job stays `unknown`, left out of the health of its account and its provider. The web UI says it in words everywhere: the Overview counts the jobs succeeded, failing and waiting and marks the waiting squares with a dashed border and a legend, the Graph names the reason on the tiles and leaves the waiting jobs out of the health of their group, the jobs list and the job page say what a job waits for and until when (and link to the failing job), and the Plan page lists the jobs it does not hold yet, with what they wait for. The badge of the health `unknown` reads "no run yet".

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

1. **Overview**: health summary, plan feasibility (an alert per quota the plan does not fit, with its problem; the quota tile tells when the tightest quota runs out at the current pace, the plan tile when it would with the planned intervals), providers and accounts cards with status, quota gauges (used / projected / limit per window, named by the accounts of their scope), recent errors.
2. **Graph**: provider -> account -> (targets) -> inventory jobs -> metric jobs -> outputs. Node color by health. Click a node to open its detail panel. Fan-out shown as grouped nodes with instance counts. The graph opens readable: framed on its content, never enlarged past 100% and never below 80%, the zoom at which the tiles still read; a graph too large for that opens on its top left corner, and the wheel scrolls it (Ctrl or ⌘ and the wheel, or a pinch, zooms). Tiles are wide enough for the names of the built-in packages; the full name, and the technical name of an implicit job (named as on the Jobs page), are in the tooltip and the detail panel.
3. **Job detail**: effective config (YAML view), desired vs effective interval with stretch reason, instances table, recent runs (result, duration, requests, pages, retries, samples), errors. A job that is not collected says so with its reason, on the jobs list and its detail, and stays out of the counts of the Overview and of the Graph.
4. **Budgets**: one card per limit and scope, named for people ("Account rate", "10 requests per second, for account demo", "5,000 calls per day"), with the description and the source of the limit from the provider definition (the vendor documentation with its link, a measurement with its date, or an assumption): timeline of consumption over the window with projection line, reserve, external usage, window reset time, each figure explained by a tooltip (what "used by others", the "retry reserve" and the remaining "reported by the API" mean). Rate limits show wait time and 429s.
5. **Plan**: the plan table, sortable, with the jobs slowed down highlighted and the reason in words, and the collected jobs the plan does not hold yet (fan-outs without instances) with what they wait for. A plan that does not fit opens with "Why it does not fit" (the problem of every quota it does not fit) and marks the critical jobs of those quotas, which are never slowed down. The plan of the running Tapyr covers what is left of each window; the page says so.
6. **Outputs**: the series each output receives (against its limit, as a meter, when it has one), in words: within the limit, close to it, at it, or reached with the number of new series held back and samples dropped, and the jobs not counted yet; queue fill, send rate, failures, last success. An output holding series back also shows on the Overview, as an alert and on its chip. An output with a disk buffer shows it under its queue: its size against `maxSize`, and in words whether it is empty, how many samples wait and since when, close to `maxAge`, or not writable (the queue then keeps its samples in memory only); a full memory queue is not shown as a loss, since the samples wait on disk.
7. **Live debug** (only when enabled, which the page says as it opens): pick a job instance, see the last raw response (pretty JSON, collapsible) side by side with the samples produced. Refresh on new run (SSE).

**Vocabulary.** Every term of the UI is plain English or has its definition one hover away: the column headers of the Plan and Jobs pages and the figures of the Budgets cards carry a tooltip (a dotted underline) with the definition of the [glossary](../glossary.md) in a sentence or two (`ui/src/lib/terms.ts`, kept in step with the glossary). An implicit job is named by what it lists and for whom ("Networks, listed for network-devices"), its technical name next to it, on every page and in sentences ("waiting for the job listing the networks"). A scope keyed by the `instance` dimension reads "the whole Tapyr process".

Every page shows, at the foot of the navigation, the version of the running Tapyr (`Tapyr v0.3.0`; a binary built without a version, such as `go build` in a clone, shows `Tapyr dev` and its short revision), with the full version, the revision and the Go version in its tooltip. It is read from `GET /api/v1/status` when the UI loads and again when the event stream reconnects, so an upgrade shows after the restart.

### Non-functional

- Responsive down to tablet width; usable on a phone for the overview.
- Light and dark themes (follows the system preference).
- Live updates via SSE, with polling fallback. Relative times ("2m ago", "in 45s", "resets in 2h") move every second between two updates, and a next run whose time has passed reads "due".
- No external network calls (fonts and assets bundled).
- Accessible: keyboard navigation, sufficient contrast, no information conveyed by color alone.

### Implementation notes

- **Serving.** The UI is served under `/ui/` (`/` redirects there) with `Content-Security-Policy: default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'none'; frame-ancestors 'none'` (the `CSP` constant of `internal/ui`), so that the browser itself refuses any external call; `settings.server.ui.enabled: false` removes it. Hashed assets are cached for a year, `index.html` is never cached. A binary built without the UI serves a page explaining `make ui`.
- **Events.** `GET /api/v1/events` is a Server-Sent Events stream of `run_finished` (job, instance, result, duration, samples, and the sentence of its problem when it did not succeed), `plan_changed`, `circuit_changed` (account, target, state) and `config_reloaded` (result, error), each `data` a JSON object with `time`. A comment line is sent every 15 seconds to keep proxies from closing the stream. Slow clients lose events rather than slowing the collector: the UI refetches what it shows on every event and polls every 15 seconds when the stream is down.
- **Debug.** With `settings.server.debug.enabled`, the collector keeps, per job instance and in memory only, the pages of its last run (status, the `Content-Type` and quota headers, the body truncated to `maxBodyBytes`), the response that failed, and the samples it produced. Bodies go through the log redactor, so configured secrets and tokens never appear. Without it, the endpoint answers `404` with the code `debug_disabled`.
- **Look.** The colors, fonts and shapes follow tapyr.eu: background `#f2f8fb` / `#0b1824`, surface `#ffffff` / `#102335`, ink `#0f2a3f` / `#e4f1f7`, cyan `#3aa9d6` / `#5cc3ea`, sun `#ffc845` / `#ffd166`, red `#cc2f35` / `#ff7a7a`; Bricolage Grotesque for titles, JetBrains Mono for data. States always carry a word or an icon next to their color.
