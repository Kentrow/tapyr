# 03 - Configuration model

## Principles

- YAML, multi-document, Kubernetes-like envelope: `apiVersion`, `kind`, `metadata`, `spec`.
- `apiVersion: tapyr/v1alpha1` for every document.
- Documents can be spread across any number of files and directories. Tapyr loads all of them and resolves references by name.
- Every `kind` has a JSON Schema generated from Go types, published in `schemas/v1alpha1/` and printable with `tapyr schema`; the [configuration reference](../reference/README.md) lists every field, generated from the same types. `schemas/v1alpha1/tapyr.json` accepts any kind (dispatched on `kind`) and is meant for editors, for example with the YAML language server: `# yaml-language-server: $schema=https://raw.githubusercontent.com/kentrow/tapyr/main/schemas/v1alpha1/tapyr.json`.
- Secrets are never inline. They are references to environment variables or files.
- Durations use Go syntax (`30s`, `5m`, `1h`). Rates use `<n>/<unit>` with unit `s`, `m` or `h` (`10/s`, `600/m`). Percentages are strings `"<n>%"` (`10%`, `2.5%`). Ratios such as `budget.share` are plain numbers.
- Names are DNS-label-like: lowercase, digits, hyphens, max 63 characters. `metadata.name` is unique per `kind` across the whole configuration (provider packages conventionally prefix their template names with the package name). Limit names are unique per provider, target names per account; both follow the same DNS-label rule.
- Identifiers used inside expressions and path templates (params, secrets, variables) match `^[A-Za-z_][A-Za-z0-9_]*$` (`apiKey`, `organizationId`), so they are usable as `vars.organizationId` in CEL and `{organizationId}` in paths.
- Parsing is strict: unknown fields are errors, except inside free-form maps (`labels`, `params`, `headers`, `externalLabels`, `resourceAttributes`, plugin `config`). Values of string maps must be YAML strings: `port: 8080` or `country: NO` are rejected with a hint to quote the value (`"8080"`, `"NO"`).
- Relative paths written in a document (`file` secrets, `caFile`, `providerPaths`, `state.path`, TLS files) are resolved against the directory of the file that declares them. Built-in defaults (`./data/tapyr.db`) are resolved against the working directory.
- Errors are reported as `file:line:column: message`, all at once (validation does not stop at the first error).

### Value types

Three syntaxes coexist, each confined to well-defined fields:

| Syntax | Where | Evaluated |
|---|---|---|
| Go `text/template` (`{{ .params.baseURL }}`) | Fields documented as templates: `Provider.spec.baseURL`, `Provider.spec.defaults.headers` values, `oauth2ClientCredentials.tokenURL` | Once per account and target, at compile time. `.params` is the account params overridden by the target params. Functions: `default`, `trimSuffix`, `lower`, `upper` |
| `{name}` placeholders | `request.path`, `sessionLogin.request.path` | Per request, from variables (discovery vars, then target params, then account params). Values are URL-path-escaped |
| CEL | Fields documented as expressions (`extract.*`, `forEach.vars`, pagination `stop`/`total`/`cursor`/`next`, ...) and `{ expr: ... }` values | At runtime |

Secret and value references:

- **SecretRef** (user side: `Account`, `Output`, `Settings`): exactly one of `{ env: NAME }` or `{ file: /path }`. Secrets are never written inline.
- **Value** (provider and job side): a plain string (literal), `{ secret: <name> }` (a secret declared by the provider and supplied by the account), or `{ expr: <cel> }`. In `auth` blocks, a Value is a literal or a secret reference only: credentials are resolved once per account, before any request, so there is nothing for an expression to read (`tapyr validate` rejects one).
- **Header values** in outputs: a plain string (literal) or a SecretRef.

## Kinds

| Kind | Who writes it | Purpose |
|---|---|---|
| `Provider` | Provider package author | API family: base URL template, auth, limits, pagination, error semantics |
| `JobTemplate` | Provider package author | Reusable, provider-level job definitions (endpoint + mapping) |
| `Account` | User | Instance of a provider with credentials, targets, labels, budget share |
| `Job` | User | What to collect for which account: either from a `JobTemplate` or fully custom |
| `Output` | User | Where data goes |
| `Settings` | User | Global settings (server, state, collection defaults); at most one document |

`Simulation` and `Scenario` (see `08-simulator.md`) also use `apiVersion: tapyr/v1alpha1`, but they are internal to provider packages (`sim/` directory) and are rejected with an explicit error when found in user configuration paths.

## Provider

```yaml
apiVersion: tapyr/v1alpha1
kind: Provider
metadata:
  name: meraki-dashboard
  version: 0.1.0                       # provider package version (semver)
  description: Cisco Meraki Dashboard API v1
  docs: https://developer.cisco.com/meraki/api-v1/
  # Catalog metadata (ADR 0011): optional, required by tapyr provider lint --catalog
  displayName: Cisco Meraki Dashboard
  categories: [network, wireless, sd-wan]   # from providers/categories.yaml, the main one first
  maintainers: [Kentrow]               # GitHub handles, without @
  tier: official                       # official | community
  validation:
    status: simulator                  # simulator | real-platform
    # date: 2026-11-04                 # YYYY-MM-DD or YYYY-MM; with platformVersion, required for real-platform
    # platformVersion: Dashboard API v1.75.1
    notes: Built from the official documentation and OpenAPI description v1.75.1
spec:
  baseURL: "{{ .params.baseURL | default \"https://api.meraki.com/api/v1\" }}"

  params:                              # parameters an Account may or must provide
    - name: baseURL
      required: false
      description: Regional base URL

  secrets:                             # secrets an Account must provide
    - name: apiKey
      required: true

  auth:
    type: bearer
    bearer:
      token: { secret: apiKey }

  defaults:
    headers:
      Accept: application/json
    timeout: 30s

  pagination:                          # default pagination, overridable per job template
    type: link_header
    linkHeader:
      pageSizeParam: perPage
      pageSize: 1000

  limits:
    - name: org-rate
      description: Per-organization throughput
      source:                          # optional, required by tapyr provider lint --catalog
        kind: vendor-docs              # vendor-docs (with url) | measured (with date) | assumed
        url: https://developer.cisco.com/meraki/api-v1/rate-limit/
        date: 2026-11-04               # when the source was checked or the measurement made
      kind: rate
      rate: 10/s
      burst: 10
      scope: { by: [account, "var:organizationId"] }
      appliesTo: { requestsWithVar: organizationId }

    - name: ip-rate
      kind: rate
      rate: 100/s
      scope: { by: [instance] }

    - name: ip-concurrency
      kind: concurrency
      max: 10
      scope: { by: [instance] }

  errors:
    rateLimited:
      statuses: [429]
      retryAfterHeader: Retry-After
    authFailed:
      statuses: [401]
    retryable:
      statuses: [500, 502, 503, 504]
  redirects:                          # optional: other hosts requests may be redirected to
    hosts: ["*.example.com"]          # see "Redirects" in 05-collection.md
```

Notes:

- String fields marked as templates use Go `text/template` with a restricted function set (`default`, `trimSuffix`, `lower`, `upper`), evaluated at compile time per account and target (see "Value types"). Runtime expressions use CEL.
- `limits` are described in detail in `04-budget-engine.md`.
- `errors` declares the provider's error semantics; defaults apply when omitted: `rateLimited` (`statuses: [429]`, `retryAfterHeader: Retry-After`, `defaultPenalty: 2s`, `penalize: narrowest`), `authFailed` (`statuses: [401]`), `retryable` (`statuses: [500, 502, 503, 504]`). `penalize` is `narrowest` or `all` (see `04-budget-engine.md`, "429 handling").
- `auth.type` is one of `none`, `header`, `bearer`, `basic`, `oauth2_client_credentials`, `session_login`, `plugin`. The configuration block of a type is named after the type in camelCase (`bearer`, `oauth2ClientCredentials`, `sessionLogin`, `plugin`). Pagination follows the same rule (`linkHeader` for `link_header`).
- Auth plugins (Go code, for flows that cannot be expressed declaratively) are selected with `type: plugin` and `plugin: { name: <plugin>, config: {...} }`. The core schema treats `config` as free-form; the plugin validates it at compile time. This keeps vendor names out of the core schema.
- `params` and `secrets` declare what an Account may or must provide. An Account providing an undeclared param or secret is an error.
- The catalog metadata (`displayName`, `categories`, `maintainers`, `tier`, `validation`) and the `source` of the limits describe the package for the provider catalog ([ADR 0011](../adr/0011-provider-ecosystem.md)); the engine ignores them. Two axes describe a package: `tier` says who maintains it (`official`: the Tapyr maintainers, `community`: the maintainers listed), `validation` how it was last tested. They are optional, so that user packages need none; `tapyr provider lint --catalog` requires them (see [Provider packages](#provider-packages)).

## JobTemplate

A provider package ships job templates so users get a working setup with a few lines.

```yaml
apiVersion: tapyr/v1alpha1
kind: JobTemplate
metadata:
  name: meraki-organizations
spec:
  provider: meraki-dashboard
  role: inventory                       # inventory | metrics | both (default metrics)
  defaultInterval: 6h
  defaultPriority: low
  request:
    method: GET
    path: /organizations
  extract:
    items: "response.body"
    inventory:
      key: "item.id"
      fields:
        organizationId: "item.id"
        organizationName: "item.name"

---
apiVersion: tapyr/v1alpha1
kind: JobTemplate
metadata:
  name: meraki-device-statuses
spec:
  provider: meraki-dashboard
  defaultInterval: 5m
  defaultPriority: high
  forEach:
    template: meraki-organizations      # fan out over the parent's inventory items
    vars:
      organizationId: "parent.organizationId"
  request:
    method: GET
    path: /organizations/{organizationId}/devices/statuses
  estimate:
    pagesPerRun: 1                      # hint for the planner before history exists
  extract:
    items: "response.body"
    metrics:
      - name: meraki_device_up
        type: gauge
        help: Device online status (1 online, 0 otherwise)
        value: "item.status == 'online' ? 1.0 : 0.0"
        labels:
          serial: "item.serial"
          network_id: "item.networkId"
          organization_id: "vars.organizationId"
    info:
      - name: meraki_device_info
        labels:
          serial: "item.serial"
          name: "item.name"
          model: "item.model"
          product_type: "item.productType"
          lan_ip: "has(item.lanIp) ? item.lanIp : ''"
```

## Account

```yaml
apiVersion: tapyr/v1alpha1
kind: Account
metadata:
  name: acme-meraki
spec:
  provider: meraki-dashboard
  params:
    baseURL: https://api.meraki.com/api/v1
  secrets:
    apiKey: { env: TAPYR_ACME_MERAKI_API_KEY }
  labels:                               # added to every sample from this account
    tenant: acme
  budget:
    share: 0.7                          # Tapyr may use 70% of quota-type limits (others share the key)
```

Multi-target account (one account, N devices):

```yaml
apiVersion: tapyr/v1alpha1
kind: Account
metadata:
  name: branch-routers
spec:
  provider: teltonika-rutos
  secrets:
    username: { env: TAPYR_RUTOS_USER }
    password: { file: /run/secrets/rutos_password }
  targets:
    - name: north-01
      params: { baseURL: "https://10.10.1.1" }
      labels: { site: north }
    - name: south-01
      params: { baseURL: "https://10.20.1.1" }
      labels: { site: south }
      secrets:                          # per-target override
        password: { env: TAPYR_RUTOS_SOUTH_PASSWORD }
  tls:
    insecureSkipVerify: false
    caFile: /etc/tapyr/rutos-ca.pem
```

- Jobs on a multi-target account run once per target. The `target` scope dimension lets providers declare per-device limits.
- Secret references: `{ env: NAME }`, `{ file: /path }`. File secrets are re-read on reload.
- `budget.share` (0 < share <= 1, default 1) scales every `quota` limit for this account. Rate and concurrency limits are not scaled by default; `budget.scaleRates: true` scales them too (`rate`, `burst` and concurrency `max`, rounded down, minimum 1). When a scope key is shared by several accounts (for example an `instance` or `credential` scope), the smallest share among them applies.
- Required params must be provided by the account or by every target. Required secrets must be provided by the account or by every target (target secrets override account secrets).

## Job

From a template:

```yaml
apiVersion: tapyr/v1alpha1
kind: Job
metadata:
  name: acme-device-statuses
spec:
  account: acme-meraki
  template: meraki-device-statuses
  interval: 5m                          # desired interval (planner may stretch it)
  priority: critical                    # critical | high | normal | low
  outputs: [victoria]                   # default: all outputs
  labels: { env: prod }
```

When a template uses `forEach` over a parent template, Tapyr automatically instantiates the parent as an implicit inventory job for the same account if no explicit job references it. Users can declare it explicitly to override its interval.

A `Job` either references a `template` or carries an inline definition, never both:

- With `template`, the job may only set `interval` (default: the template's `defaultInterval`), `priority` (default: the template's `defaultPriority`), `outputs`, `labels` and `staleAfter`. Definition fields (`role`, `request`, `pagination`, `forEach`, `estimate`, `extract`) are rejected.
- Without `template`, the job carries the same definition fields as a `JobTemplate` spec, `interval` is required and `priority` defaults to `normal`.

The definition fields shared by `JobTemplate` and inline `Job`:

| Field | Content |
|---|---|
| `role` | `inventory`, `metrics` (default) or `both` |
| `request` | `method` (default `GET`), `path` (with `{var}` placeholders), `query` (map of Value), `headers` (map of strings), `body` (map of Value, POST queries only), `readOnly` (required `true` for POST), `idempotent` (POST retried only if `true`), `timeout` |
| `pagination` | Overrides the provider default (see `05-collection.md`). Every type accepts `maxPages` (default 100) |
| `forEach` | `template` (a parent `JobTemplate`) or `job` (a parent `Job` of the same account), exactly one; `vars` (map of CEL on `parent`), `filter` (CEL), `maxInstances` (default 10000). The parent must have `role: inventory` or `both` |
| `estimate` | `pagesPerRun` (planner hint) |
| `extract` | `items`, `filter`, `partial`, `metrics`, `info`, `inventory` (`key`, `fields`, `ttl`), `post` (see `06-mapping-and-outputs.md`) |

Fully custom job (no template):

```yaml
apiVersion: tapyr/v1alpha1
kind: Job
metadata:
  name: rms-devices
spec:
  account: home-rms
  interval: 10m
  priority: normal
  request:
    method: GET
    path: /devices
    query:
      limit: "100"
  pagination:
    type: offset
    offset:
      offsetParam: offset
      limitParam: limit
      pageSize: 100
      stop: "size(response.body.data) < 100"
  extract:
    items: "response.body.data"
    metrics:
      - name: teltonika_rms_device_online
        type: gauge
        value: "item.status == 1 ? 1.0 : 0.0"
        labels:
          device_id: "string(item.id)"
          name: "item.name"
```

## Output

```yaml
apiVersion: tapyr/v1alpha1
kind: Output
metadata:
  name: victoria
spec:
  type: remote_write
  remoteWrite:
    url: http://victoriametrics:8428/api/v1/write
    headers: {}
    basicAuth: { username: { env: VM_USER }, password: { env: VM_PASSWORD } }
    timeout: 10s
  queue:
    capacity: 50000                     # samples
    maxBatch: 5000
    flushInterval: 5s
  externalLabels:
    tapyr_instance: lab
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata:
  name: prom
spec:
  type: prometheus                       # served on the main server under /metrics
  prometheus:
    exposeTimestamps: false
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata:
  name: otel
spec:
  type: otlp
  otlp:
    endpoint: http://otel-collector:4318
    protocol: http/protobuf              # http/protobuf | grpc
    headers: {}
    resourceAttributes:
      service.name: tapyr
```

- At most one `prometheus` output may be declared (it is served on the main server under `/metrics`). `queue` applies to push outputs only and is rejected on `prometheus`.
- Queue defaults: `capacity: 50000`, `maxBatch: 5000`, `flushInterval: 5s`. Timeout default: `10s`.
- Header values are plain strings or SecretRefs (`X-Scope-OrgID: tenant-a`, `Authorization: { env: OTLP_TOKEN }`).

## Settings

```yaml
apiVersion: tapyr/v1alpha1
kind: Settings
metadata:
  name: default
spec:
  server:
    listen: 127.0.0.1:9790
    basicAuth:
      username: { env: TAPYR_UI_USER }
      password: { env: TAPYR_UI_PASSWORD }
      protectMetrics: false              # also protect /metrics and /metrics/self
    # tls: { certFile: /etc/tapyr/tls.crt, keyFile: /etc/tapyr/tls.key }
    ui: { enabled: true }
    debug:                               # live debug endpoint (raw responses)
      enabled: false
      maxBodyBytes: 1048576
  state:
    path: ./data/tapyr.db
  providerPaths:
    - ./providers                        # user provider packages, in addition to built-ins
  collection:
    maxConcurrentRequests: 32
    defaultTimeout: 30s
    jitter: 10%
  budget:
    retryReserve: 10%                    # 0% to 50%
    replanInterval: 15m
    replanOnProjection: true             # replan at once when the plan would exhaust a quota before its reset
    maxStretch: 12                       # planner stretch cap per priority class
  retry:
    maxAttempts: 4
    initialBackoff: 1s
    maxBackoff: 60s
    multiplier: 2
  circuitBreaker:
    failureThreshold: 5
    cooldown: 2m
    maxCooldown: 30m                     # cooldown doubles on repeated openings up to this value
  mapping:
    maxCost: 1000000                     # CEL cost limit per expression evaluation
    maxItemsPerRun: 100000
    maxSamplesPerRun: 200000
    labelConflict: override              # override (job > target > account > mapping) | error
  selfMetrics:
    outputs: []                          # optionally push self-metrics through these outputs
    interval: 30s                        # push interval of the self-metrics
  log:
    level: info
    format: json
```

All settings have defaults; the `Settings` document is optional. `collection.jitter` is in [0%, 50%].

Logging precedence: `--log-level` / `--log-format` flags, then `TAPYR_LOG_LEVEL` / `TAPYR_LOG_FORMAT`, then `Settings.spec.log`, then defaults. A bootstrap logger built from flags and environment is used until the configuration is loaded.

## Provider packages

A provider package is a directory named after its provider:

```
providers/<name>/
├── provider.yaml          # Provider (exactly one): base URL, params, secrets, auth, pagination, limits, errors
├── templates/*.yaml       # JobTemplates: request, estimate.pagesPerRun, CEL extract
├── sim/sim.yaml           # Simulation (see 08-simulator.md): inventory, routes, fixtures, auth accepted
├── sim/fixtures/          # static fixtures or Go templates (*.tmpl)
├── sim/scenarios/         # default.yaml, quota-tight.yaml, bad-day.yaml (all three required)
├── tests/contract.yaml    # jobs run once against the simulator
├── tests/contract.env     # the simulator credentials for the contract test
├── tests/expected.prom    # the samples they must produce
├── dashboards/*.json      # Grafana dashboard(s) with a datasource variable
├── examples/*.yaml        # example Account and Job documents that load
└── README.md              # coverage, auth setup, limits with sources, validation status, caveats
```

- `metadata.version` is semver. Breaking changes to metric names or labels bump the major version.
- Built-in packages are embedded in the binary. User packages from `providerPaths` override built-ins with the same name (with a warning).
- Precedence, highest first: `Provider` documents written directly in the configuration paths, then packages from `providerPaths` (in order), then built-in packages. A package that is overridden is dropped entirely, its job templates included. Every override is reported as a warning by `validate` and `run`.
- Each `providerPaths` entry is either a package directory (it contains `provider.yaml`) or a directory of packages (each subdirectory containing a `provider.yaml` is a package).
- In a package, `provider.yaml` holds exactly one `Provider`, `templates/*.yaml` hold only `JobTemplate` documents whose `spec.provider` is the package name, and `sim/sim.yaml` holds at most one `Simulation`. The package directory name must equal the provider name.
- `tapyr provider lint <dir>` validates a package and runs its contract test (see [`08-simulator.md`](08-simulator.md#contract-tests)). With `--catalog`, it also requires the catalog metadata: a `displayName`, at least one category of [`providers/categories.yaml`](../../providers/categories.yaml), at least one maintainer, a `tier`, a `validation` (with its `date` and `platformVersion` when its status is `real-platform`), and a `source` on every limit (with its `url` for `vendor-docs`, its `date` for `measured`); dates must exist. The CI runs it on the built-in packages.

## Validation rules (non-exhaustive)

- Every reference resolves (`account -> provider`, `template -> provider`, `job -> account`, `job -> template`, `forEach -> template | job`, `outputs -> output`, `selfMetrics.outputs -> output`). Provider names resolve against the configuration, the `providerPaths` packages and the built-in packages.
- Names are unique per kind; at most one `Settings` document; at most one `prometheus` output.
- Discovery graph is acyclic; maximum depth 4 (at most 4 fan-out levels from the root inventory job to the deepest child; a job using a template shares the level of its template).
- A job template's `provider` matches the job account's provider.
- Accounts provide every required param and secret, and no undeclared one.
- Template fields parse as Go templates using only the allowed functions.
- All CEL expressions compile and type-check against the job's environment (see `06-mapping-and-outputs.md`).
- Every variable used in a path template is provided by `forEach.vars` or the account params.
- Scope dimensions `var:<name>` refer to variables available in the requests the limit applies to.
- Metric names follow Prometheus naming rules; label names are valid; a metric name has a single type across all jobs.
- `budget.share` in (0, 1]. `retryReserve` in [0%, 50%].
- Required secrets resolve at startup (`validate --check-secrets` checks them offline).
