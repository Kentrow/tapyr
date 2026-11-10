<!-- Generated from the JSON Schemas by make schema (internal/tools/schemagen). Do not edit. -->

# Simulation

The simulator of a provider package (sim/sim.yaml).

Schema: [`simulation.json`](../../schemas/v1alpha1/simulation.json). Explanations and examples: [configuration model](../spec/03-configuration.md#provider-packages).

| Field | Type | Required | Description |
|---|---|---|---|
| `apiVersion` | `tapyr/v1alpha1` | yes | Version of the configuration format. |
| `kind` | `Simulation` | yes | Kind of the document. |
| `metadata` | [ObjectMeta](#objectmeta) | yes | Name and description of the document. |
| `spec` | [SimulationSpec](#simulationspec) | yes | Specification of the document. |

## ObjectMeta

ObjectMeta identifies a document.

| Field | Type | Required | Description |
|---|---|---|---|
| `description` | string |  | Description is free text for humans. |
| `name` | [Name](#name) | yes | Name of the document, unique per kind. |

## SimulationSpec

SimulationSpec is the specification of a Simulation.

| Field | Type | Required | Description |
|---|---|---|---|
| `auth` | [SimulationAuth](#simulationauth) |  | Auth lists the credentials the simulator accepts. The way they are checked follows the provider auth type. |
| `basePath` | string |  | `basePath` is the path prefix of the API (the path part of the provider base URL). Routes are relative to it. Default `/`. Pattern `^/`. |
| `headers` | map of string to string |  | Headers added to every response, computed from the limit state: "limit:&lt;name&gt;.max", "limit:&lt;name&gt;.remaining" or "limit:&lt;name&gt;.reset" (seconds until the window resets). |
| `inventory` | [SimulationInventory](#simulationinventory) |  | Inventory generates synthetic items for templated fixtures. |
| `portOffset` | integer |  | `portOffset` is the port of the simulator in tapyr sim --all, as an offset from --listen-base, so that adding a package does not move the others. Simulators without one take the ports after the largest declared offset, in provider name order. Minimum 0. |
| `routes` | list of [SimulationRoute](#simulationroute) | yes | Routes served by the simulator. Unknown routes return 404. Minimum items 1. |

## Name

Type: string. Pattern `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`.

## SimulationAuth

SimulationAuth configures the credentials accepted by the simulator.

| Field | Type | Required | Description |
|---|---|---|---|
| `accept` | [SimulationCredentials](#simulationcredentials) |  | Accept lists the accepted credentials. They are fake values by design. |
| `loginResponse` | string |  | `loginResponse` is a JSON fixture answered by the login endpoint, relative to the sim/ directory; {token} and {expiresIn} (seconds) are replaced. Default: {"token": "{token}", "expiresIn": {expiresIn}}. |
| `tokenLifetime` | [Duration](#duration) |  | `tokenLifetime` of the tokens issued by the token and login endpoints, in simulated time. Default `10m`. |

## SimulationInventory

SimulationInventory is a synthetic inventory: sets of items, each item under a parent item of another set.

| Field | Type | Required | Description |
|---|---|---|---|
| `seed` | integer |  | Seed of the generated values. The same seed gives the same items and the same dynamics. Default `42`. |
| `sets` | list of [SimulationSet](#simulationset) | yes | Sets of items, generated in order: a parent set comes first. Minimum items 1. |

## SimulationRoute

SimulationRoute is an endpoint of the simulator.

| Field | Type | Required | Description |
|---|---|---|---|
| `method` | string |  | Method of the route. Default `GET`. One of `GET`, `HEAD`, `POST`. |
| `pagination` | string |  | Pagination serves the fixture items page by page. Its parameters come from the job templates requesting this path, or from the provider. Default `none`. One of `none`, `offset`, `page`, `link_header`, `cursor`, `expression`. |
| `path` | string | yes | Path of the route, relative to basePath, with {var} segments. Pattern `^/`. |
| `response` | [SimulationResponse](#simulationresponse) | yes | Response returned by the route. |

## SimulationCredentials

SimulationCredentials are accepted credentials per auth type.

| Field | Type | Required | Description |
|---|---|---|---|
| `basic` | list of [SimulationBasicCredential](#simulationbasiccredential) |  | Basic credentials accepted with auth type basic. |
| `bearer` | list of string |  | Bearer tokens accepted with auth type bearer. |
| `clients` | list of [SimulationClient](#simulationclient) |  | Clients accepted by the token endpoint with auth type oauth2\_client\_credentials. |
| `header` | list of string |  | Header values accepted with auth type header, before the format of the provider auth is applied. |
| `logins` | list of [SimulationBasicCredential](#simulationbasiccredential) |  | Logins accepted by the login endpoint with auth type session\_login: the login body must carry both values. |
| `plugin` | object |  | Plugin holds the credentials accepted by the simulator side of an auth plugin (type plugin), read by the plugin. |

## Duration

Type: string. Pattern <code>^(&#91;0-9&#93;+(&#92;.&#91;0-9&#93;+)?(ns&#124;us&#124;µs&#124;ms&#124;s&#124;m&#124;h))+$</code>. Examples `30s`, `5m`, `1h30m`.

## SimulationSet

SimulationSet is a set of generated items.

| Field | Type | Required | Description |
|---|---|---|---|
| `count` | integer | yes | Count of items (per parent item when Parent is set). Minimum 0. Maximum 100000. |
| `name` | [Name](#name) | yes | Name of the set, used by the template helpers. |
| `parent` | [Name](#name) |  | Parent set: count items are generated under each of its items. |

## SimulationResponse

SimulationResponse is the response of a route.

| Field | Type | Required | Description |
|---|---|---|---|
| `file` | string |  | File is the JSON fixture, relative to the sim/ directory. {var} placeholders are replaced by the route variables. Paginated routes need a JSON array, or a list at ItemsField. Minimum length 1. |
| `headers` | map of string to string |  | Headers added to the response. |
| `itemsField` | string |  | `itemsField` is the dotted path of the list a paginated route serves page by page, inside an envelope; empty for a top-level array. |
| `status` | integer |  | Status code of the response. Default `200`. Minimum 100. Maximum 599. |
| `template` | string |  | Template is a Go text/template fixture rendering JSON, relative to the sim/ directory (see docs/spec/08-simulator.md for its data and helpers). Exactly one of file and template is set. Minimum length 1. |

- Exactly one of `file`, `template` is set.

## SimulationBasicCredential

SimulationBasicCredential is a username and password pair.

| Field | Type | Required | Description |
|---|---|---|---|
| `password` | string | yes | Password accepted. |
| `username` | string | yes | Username accepted. |

## SimulationClient

SimulationClient is an OAuth 2.0 client.

| Field | Type | Required | Description |
|---|---|---|---|
| `clientId` | string | yes | `clientId` accepted. |
| `clientSecret` | string | yes | `clientSecret` accepted. |
