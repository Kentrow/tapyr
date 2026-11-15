<!-- Generated from the JSON Schemas by make schema (internal/tools/schemagen). Do not edit. -->

# Settings

Global settings (at most one document).

Schema: [`settings.json`](../../schemas/v1alpha1/settings.json). Explanations and examples: [configuration model](../spec/03-configuration.md#settings).

| Field | Type | Required | Description |
|---|---|---|---|
| `apiVersion` | `tapyr/v1alpha1` | yes | Version of the configuration format. |
| `kind` | `Settings` | yes | Kind of the document. |
| `metadata` | [ObjectMeta](#objectmeta) | yes | Name and description of the document. |
| `spec` | [SettingsSpec](#settingsspec) | yes | Specification of the document. |

## ObjectMeta

ObjectMeta identifies a document.

| Field | Type | Required | Description |
|---|---|---|---|
| `description` | string |  | Description is free text for humans. |
| `name` | [Name](#name) | yes | Name of the document, unique per kind. |

## SettingsSpec

SettingsSpec is the specification of Settings.

| Field | Type | Required | Description |
|---|---|---|---|
| `budget` | [BudgetSettings](#budgetsettings) |  | Budget configures the budget engine and the planner. |
| `circuitBreaker` | [CircuitBreakerSettings](#circuitbreakersettings) |  | `circuitBreaker` configures the per-account circuit breakers. |
| `collection` | [CollectionSettings](#collectionsettings) |  | Collection configures request execution. |
| `log` | [LogSettings](#logsettings) |  | Log configures logging. Flags and environment variables win. |
| `mapping` | [MappingSettings](#mappingsettings) |  | Mapping configures CEL evaluation limits. |
| `providerPaths` | list of string |  | `providerPaths` lists directories of user provider packages, in addition to the built-in packages. |
| `retry` | [RetrySettings](#retrysettings) |  | Retry configures retries of transient errors. |
| `selfMetrics` | [SelfMetricsSettings](#selfmetricssettings) |  | `selfMetrics` configures Tapyr's own metrics. |
| `server` | [ServerSettings](#serversettings) |  | Server is the HTTP server (metrics, health, status API, UI). |
| `state` | [StateSettings](#statesettings) |  | State is the persistent state store. |

## Name

Type: string. Pattern `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`.

## BudgetSettings

BudgetSettings configures the budget engine and the planner.

| Field | Type | Required | Description |
|---|---|---|---|
| `maxStretch` | number |  | `maxStretch` caps the interval stretch factor applied to a priority class before the planner moves to the next class. Default `12`. Minimum 1. |
| `replanInterval` | [Duration](#duration) |  | `replanInterval` is the period of mid-window replanning. Default `15m`. |
| `replanOnProjection` | boolean |  | `replanOnProjection` replans at once when the consumption projected by the current plan until the end of a quota window exceeds its remaining budget, for example when another client starts spending the same quota, instead of waiting for the next replanInterval. Default `true`. |
| `retryReserve` | [Percent](#percent) |  | `retryReserve` is the share of every quota reserved for retries, from 0% to 50%. Default `10%`. |

## CircuitBreakerSettings

CircuitBreakerSettings configures the per-account circuit breakers.

| Field | Type | Required | Description |
|---|---|---|---|
| `cooldown` | [Duration](#duration) |  | Cooldown before the half-open probe. Default `2m`. |
| `failureThreshold` | integer |  | `failureThreshold` is the number of consecutive transient failures that opens the circuit. Default `5`. Minimum 1. |
| `maxCooldown` | [Duration](#duration) |  | `maxCooldown` caps the cooldown, which doubles on repeated openings. Default `30m`. |

## CollectionSettings

CollectionSettings configures request execution.

| Field | Type | Required | Description |
|---|---|---|---|
| `defaultTimeout` | [Duration](#duration) |  | `defaultTimeout` of a request when the provider sets none. Default `30s`. |
| `jitter` | [Percent](#percent) |  | Jitter spreads scheduled runs, from 0% to 50% of the interval. Default `10%`. |
| `maxConcurrentRequests` | integer |  | `maxConcurrentRequests` bounds in-flight requests across all providers. Default `32`. Minimum 1. |
| `skipUnconsumed` | boolean |  | `skipUnconsumed` does not collect the jobs whose metrics no output keeps (the processing rules of every output receiving them drop them by name), nor the parents whose inventory no collected job takes: no API call, no quota spent (ADR 0013). Set it to false to keep every job running, for example as an authentication probe or for the live debug. Default `true`. |

## LogSettings

LogSettings configures logging.

| Field | Type | Required | Description |
|---|---|---|---|
| `format` | string |  | Format: json or text. Default `json`. One of `json`, `text`. |
| `level` | string |  | Level: debug, info, warn or error. Default `info`. One of `debug`, `info`, `warn`, `error`. |

## MappingSettings

MappingSettings configures CEL evaluation limits.

| Field | Type | Required | Description |
|---|---|---|---|
| `labelConflict` | string |  | `labelConflict` resolves labels set both by the mapping and by account, target or job labels: override (job &gt; target &gt; account &gt; mapping) or error. Default `override`. One of `override`, `error`. |
| `maxCost` | integer |  | `maxCost` is the CEL cost limit per expression evaluation. Default `1000000`. Minimum 1. |
| `maxItemsPerRun` | integer |  | `maxItemsPerRun` caps the items extracted by a run. Default `100000`. Minimum 1. |
| `maxSamplesPerRun` | integer |  | `maxSamplesPerRun` caps the samples produced by a run. Default `200000`. Minimum 1. |

## RetrySettings

RetrySettings configures retries with exponential backoff and full jitter.

| Field | Type | Required | Description |
|---|---|---|---|
| `initialBackoff` | [Duration](#duration) |  | `initialBackoff` before the first retry. Default `1s`. |
| `maxAttempts` | integer |  | `maxAttempts` per request, including the first attempt. Default `4`. Minimum 1. |
| `maxBackoff` | [Duration](#duration) |  | `maxBackoff` caps the backoff. Default `60s`. |
| `multiplier` | number |  | Multiplier of the backoff between attempts. Default `2`. Minimum 1. |

## SelfMetricsSettings

SelfMetricsSettings configures Tapyr's own metrics.

| Field | Type | Required | Description |
|---|---|---|---|
| `interval` | [Duration](#duration) |  | Interval between two pushes of the self-metrics to Outputs. Default `30s`. |
| `outputs` | list of [Name](#name) |  | Outputs that also receive the self-metrics (always served on /metrics/self). |

## ServerSettings

ServerSettings configures the HTTP server.

| Field | Type | Required | Description |
|---|---|---|---|
| `basicAuth` | [ServerBasicAuth](#serverbasicauth) |  | `basicAuth` protects /api and /ui. |
| `debug` | [DebugSettings](#debugsettings) |  | Debug is the live debug endpoint exposing raw responses. |
| `listen` | string |  | Listen address. Default `127.0.0.1:9790`. |
| `tls` | [ServerTLS](#servertls) |  | `tls` serves HTTPS with the given certificate. A reverse proxy is the recommended way to terminate TLS. |
| `ui` | [UISettings](#uisettings) |  | `ui` is the embedded read-only web UI. |

## StateSettings

StateSettings configures the state store.

| Field | Type | Required | Description |
|---|---|---|---|
| `path` | string |  | Path of the SQLite database, relative to the declaring file. The built-in default is relative to the working directory. Default `./data/tapyr.db`. |

## Duration

Type: string. Pattern <code>^(&#91;0-9&#93;+(&#92;.&#91;0-9&#93;+)?(ns&#124;us&#124;µs&#124;ms&#124;s&#124;m&#124;h))+$</code>. Examples `30s`, `5m`, `1h30m`.

## Percent

Type: string. Pattern `^[0-9]+(\.[0-9]+)?%$`. Examples `10%`.

## ServerBasicAuth

ServerBasicAuth protects the server with HTTP basic authentication.

| Field | Type | Required | Description |
|---|---|---|---|
| `password` | [SecretRef](#secretref) | yes | Password secret. |
| `protectMetrics` | boolean |  | `protectMetrics` also protects /metrics and /metrics/self. |
| `username` | [SecretRef](#secretref) | yes | Username secret. |

## DebugSettings

DebugSettings configures the live debug endpoint.

| Field | Type | Required | Description |
|---|---|---|---|
| `enabled` | boolean |  | Enabled exposes the last raw responses (redacted) in the status API. |
| `maxBodyBytes` | integer |  | `maxBodyBytes` truncates the raw responses kept for debugging. Default `1048576`. Minimum 1. |

## ServerTLS

ServerTLS holds the server certificate files, relative to the declaring file.

| Field | Type | Required | Description |
|---|---|---|---|
| `certFile` | string | yes | `certFile` is the PEM certificate chain. Minimum length 1. |
| `keyFile` | string | yes | `keyFile` is the PEM private key. Minimum length 1. |

## UISettings

UISettings configures the web UI.

| Field | Type | Required | Description |
|---|---|---|---|
| `enabled` | boolean |  | Enabled serves the UI under /ui/. Default `true`. |

## SecretRef

A secret read from an environment variable ({env: NAME}) or a file ({file: /path}).

| Field | Type | Required | Description |
|---|---|---|---|
| `env` | string |  | Name of an environment variable holding the secret. Minimum length 1. |
| `file` | string |  | Path of a file holding the secret, relative to the declaring file. Minimum length 1. |

- Exactly one field is set.
