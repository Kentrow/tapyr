<!-- Generated from the JSON Schemas by make schema (internal/tools/schemagen). Do not edit. -->

# Output

Where collected samples are sent.

Schema: [`output.json`](../../schemas/v1alpha1/output.json). Explanations and examples: [configuration model](../spec/03-configuration.md#output).

| Field | Type | Required | Description |
|---|---|---|---|
| `apiVersion` | `tapyr/v1alpha1` | yes | Version of the configuration format. |
| `kind` | `Output` | yes | Kind of the document. |
| `metadata` | [ObjectMeta](#objectmeta) | yes | Name and description of the document. |
| `spec` | [OutputSpec](#outputspec) | yes | Specification of the document. |

## ObjectMeta

ObjectMeta identifies a document.

| Field | Type | Required | Description |
|---|---|---|---|
| `description` | string |  | Description is free text for humans. |
| `name` | [Name](#name) | yes | Name of the document, unique per kind. |

## OutputSpec

OutputSpec is the specification of an Output.

| Field | Type | Required | Description |
|---|---|---|---|
| `externalLabels` | map of string to string |  | `externalLabels` added to every sample sent through this output. Keys match `^[a-zA-Z_][a-zA-Z0-9_]*$`. |
| `otlp` | [OTLPOutput](#otlpoutput) |  | `otlp` pushes OTLP metrics (type otlp). |
| `processing` | list of [ProcessingRule](#processingrule) |  | Processing is an ordered list of rules applied to the samples of the jobs sent to this output only, with the semantics of Prometheus metric\_relabel\_configs (ADR 0012). Self-metrics are never processed. |
| `prometheus` | [PrometheusOutput](#prometheusoutput) |  | Prometheus pull exposition on /metrics (type prometheus, at most one). |
| `queue` | [Queue](#queue) |  | Queue of a push output (not allowed for prometheus). |
| `remoteWrite` | [RemoteWriteOutput](#remotewriteoutput) |  | `remoteWrite` pushes with Prometheus remote write 1.0 (type remote\_write). |
| `type` | string | yes | Type of output. One of `prometheus`, `remote_write`, `otlp`. |

- When `type` is `remote_write`: `remoteWrite` is required.
- When `type` is `otlp`: `otlp` is required.

## Name

Type: string. Pattern `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`.

## OTLPOutput

OTLPOutput configures OTLP metrics export.

| Field | Type | Required | Description |
|---|---|---|---|
| `endpoint` | string | yes | Endpoint of the OTLP receiver. Format uri. |
| `headers` | map of string to [StringOrSecret](#stringorsecret) |  | Headers added to every request; values are literals or secrets. |
| `protocol` | string |  | Protocol: http/protobuf or grpc. Default `http/protobuf`. One of `http/protobuf`, `grpc`. |
| `resourceAttributes` | map of string to string |  | `resourceAttributes` added to the OTLP resource. |
| `timeout` | [Duration](#duration) |  | Timeout of a send request. Default `10s`. |

## ProcessingRule

ProcessingRule keeps or drops samples, or labels, of an output.

| Field | Type | Required | Description |
|---|---|---|---|
| `action` | string | yes | Action: keep or drop the samples whose joined sourceLabels match regex; labelkeep or labeldrop the labels whose name matches regex. One of `keep`, `drop`, `labelkeep`, `labeldrop`. |
| `regex` | string |  | Regex is an RE2 regular expression, fully anchored. keep and drop default to (.\*); labelkeep and labeldrop require it. |
| `separator` | string |  | Separator joins the values of sourceLabels (keep and drop). Default `;`. |
| `sourceLabels` | list of string |  | `sourceLabels` are the labels whose values are joined and matched (keep and drop, required); \_\_name\_\_ is the metric name, a missing label the empty string. Each item: pattern `^[a-zA-Z_][a-zA-Z0-9_]*$`. |

## PrometheusOutput

PrometheusOutput configures the pull exposition.

| Field | Type | Required | Description |
|---|---|---|---|
| `exposeTimestamps` | boolean |  | `exposeTimestamps` adds sample timestamps to the exposition. Off by default, as recommended by Prometheus. |

## Queue

Queue is the bounded in-memory queue of a push output.

| Field | Type | Required | Description |
|---|---|---|---|
| `capacity` | integer |  | Capacity in samples; the oldest samples are dropped (with accounting) when full. Default `50000`. Minimum 1. |
| `flushInterval` | [Duration](#duration) |  | `flushInterval` sends a partial batch after this delay. Default `5s`. |
| `maxBatch` | integer |  | `maxBatch` is the maximum number of samples per request. Default `5000`. Minimum 1. |

## RemoteWriteOutput

RemoteWriteOutput configures Prometheus remote write 1.0.

| Field | Type | Required | Description |
|---|---|---|---|
| `basicAuth` | [BasicAuthSecrets](#basicauthsecrets) |  | `basicAuth` credentials. |
| `headers` | map of string to [StringOrSecret](#stringorsecret) |  | Headers added to every request; values are literals or secrets. |
| `timeout` | [Duration](#duration) |  | Timeout of a send request. Default `10s`. |
| `url` | string | yes | `url` of the remote write endpoint. Format uri. |

## StringOrSecret

A literal string, {env: NAME} or {file: /path}.

Either a string, or an object:

| Field | Type | Required | Description |
|---|---|---|---|
| `env` | string |  | Name of an environment variable holding the secret. Minimum length 1. |
| `file` | string |  | Path of a file holding the secret, relative to the declaring file. Minimum length 1. |

- Exactly one field is set.

## Duration

Type: string. Pattern <code>^(&#91;0-9&#93;+(&#92;.&#91;0-9&#93;+)?(ns&#124;us&#124;µs&#124;ms&#124;s&#124;m&#124;h))+$</code>. Examples `30s`, `5m`, `1h30m`.

## BasicAuthSecrets

BasicAuthSecrets are HTTP basic credentials read from secrets.

| Field | Type | Required | Description |
|---|---|---|---|
| `password` | [SecretRef](#secretref) | yes | Password secret. |
| `username` | [SecretRef](#secretref) | yes | Username secret. |

## SecretRef

A secret read from an environment variable ({env: NAME}) or a file ({file: /path}).

| Field | Type | Required | Description |
|---|---|---|---|
| `env` | string |  | Name of an environment variable holding the secret. Minimum length 1. |
| `file` | string |  | Path of a file holding the secret, relative to the declaring file. Minimum length 1. |

- Exactly one field is set.
