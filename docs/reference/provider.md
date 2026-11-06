<!-- Generated from the JSON Schemas by make schema (internal/tools/schemagen). Do not edit. -->

# Provider

An API family: base URL, auth, pagination, error semantics and rate limits.

Schema: [`provider.json`](../../schemas/v1alpha1/provider.json). Explanations and examples: [configuration model](../spec/03-configuration.md#provider).

| Field | Type | Required | Description |
|---|---|---|---|
| `apiVersion` | `tapyr/v1alpha1` | yes | Version of the configuration format. |
| `kind` | `Provider` | yes | Kind of the document. |
| `metadata` | [ProviderMeta](#providermeta) | yes | Name and description of the document. |
| `spec` | [ProviderSpec](#providerspec) | yes | Specification of the document. |

## ProviderMeta

ProviderMeta identifies a provider package.

| Field | Type | Required | Description |
|---|---|---|---|
| `categories` | list of [Name](#name) |  | Categories of the provider, from providers/categories.yaml, the main one first. Unique items. |
| `description` | string |  | Description is free text for humans. |
| `displayName` | string |  | `displayName` is the name of the provider in the catalog, for example "Cisco Meraki Dashboard". Like categories, maintainers, tier and validation, it is optional, and required by tapyr provider lint --catalog (ADR 0011). |
| `docs` | string |  | Docs links to the vendor API documentation. Format uri. |
| `maintainers` | list of string |  | Maintainers are the GitHub handles, without @, of the people who answer for the package. Unique items. Each item: pattern `^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`. |
| `name` | [Name](#name) | yes | Name of the provider, unique across built-in and user packages. |
| `tier` | string |  | Tier says who maintains the package: official (the Tapyr maintainers) or community (the maintainers listed). One of `official`, `community`. |
| `validation` | [Validation](#validation) |  | Validation says how the package was last tested. |
| `version` | string | yes | Version of the provider package (semver). Breaking changes to metric names or labels bump the major version. Pattern <code>^(0&#124;&#91;1-9&#93;&#91;0-9&#93;&#42;)&#92;.(0&#124;&#91;1-9&#93;&#91;0-9&#93;&#42;)&#92;.(0&#124;&#91;1-9&#93;&#91;0-9&#93;&#42;)(-&#91;0-9A-Za-z.-&#93;+)?(&#92;+&#91;0-9A-Za-z.-&#93;+)?$</code>. |

## ProviderSpec

ProviderSpec is the specification of a Provider.

| Field | Type | Required | Description |
|---|---|---|---|
| `auth` | [Auth](#auth) |  | Auth applied to every request. Defaults to type none. |
| `baseURL` | [Template](#template) | yes | `baseURL` of the API. Template evaluated per account and target, for example {{ .params.baseURL \| default "https://api.example.com/v1" }}. |
| `defaults` | [RequestDefaults](#requestdefaults) |  | Defaults applied to every request. |
| `errors` | [ErrorSemantics](#errorsemantics) |  | Errors declares how the API signals rate limiting, authentication failures and retryable errors. |
| `limits` | list of [Limit](#limit) |  | Limits declares every rate limit, concurrency limit and quota of the API. |
| `pagination` | [Pagination](#pagination) |  | Pagination used by job templates that do not override it. |
| `params` | list of [ParamSpec](#paramspec) |  | Params an Account may or must provide. |
| `redirects` | [Redirects](#redirects) |  | Redirects lists the other hosts a response may redirect requests to. Redirects to another host are not followed otherwise: credentials only go to the base URL and to these hosts. |
| `secrets` | list of [ParamSpec](#paramspec) |  | Secrets an Account may or must provide. |

## Name

Type: string. Pattern `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`.

## Validation

Validation records the last validation of a provider package.

| Field | Type | Required | Description |
|---|---|---|---|
| `date` | string |  | Date of the validation, YYYY-MM-DD or YYYY-MM. Required by tapyr provider lint --catalog with status real-platform. Pattern <code>^&#91;0-9&#93;{4}-(0&#91;1-9&#93;&#124;1&#91;0-2&#93;)(-(0&#91;1-9&#93;&#124;&#91;12&#93;&#91;0-9&#93;&#124;3&#91;01&#93;))?$</code>. |
| `notes` | string |  | Notes is free text: what was covered, what the package was built from. |
| `platformVersion` | string |  | `platformVersion` is the version of the API, platform or firmware validated against, for example "RutOS 07.24.5". Required by tapyr provider lint --catalog with status real-platform. |
| `status` | string | yes | Status is simulator (tested against its simulator only) or real-platform (run against the real API). One of `simulator`, `real-platform`. |

## Auth

Auth configures authentication.

| Field | Type | Required | Description |
|---|---|---|---|
| `basic` | [BasicAuth](#basicauth) |  | Basic sends HTTP basic credentials (type basic). |
| `bearer` | [BearerAuth](#bearerauth) |  | Bearer sends Authorization: Bearer &lt;token&gt; (type bearer). |
| `header` | [HeaderAuth](#headerauth) |  | Header sends a static value in a header (type header). |
| `oauth2ClientCredentials` | [OAuth2ClientCredentials](#oauth2clientcredentials) |  | `oauth2ClientCredentials` obtains tokens with the OAuth 2.0 client credentials grant (type oauth2\_client\_credentials). |
| `plugin` | [PluginAuth](#pluginauth) |  | Plugin delegates to a Go auth plugin (type plugin). |
| `sessionLogin` | [SessionLogin](#sessionlogin) |  | `sessionLogin` posts credentials to obtain a session token (type session\_login). |
| `type` | string | yes | Type of authentication. One of `none`, `header`, `bearer`, `basic`, `oauth2_client_credentials`, `session_login`, `plugin`. |

- When `type` is `basic`: `basic` is required.
- When `type` is `bearer`: `bearer` is required.
- When `type` is `header`: `header` is required.
- When `type` is `oauth2_client_credentials`: `oauth2ClientCredentials` is required.
- When `type` is `plugin`: `plugin` is required.
- When `type` is `session_login`: `sessionLogin` is required.

## Template

Go text/template evaluated per account and target.

Type: string.

## RequestDefaults

RequestDefaults are applied to every request of the provider.

| Field | Type | Required | Description |
|---|---|---|---|
| `headers` | map of string to [Template](#template) |  | Headers added to every request. Values are templates. |
| `timeout` | [Duration](#duration) |  | Timeout of a single request. Defaults to settings.collection.defaultTimeout. |

## ErrorSemantics

ErrorSemantics declares how the provider signals errors.

| Field | Type | Required | Description |
|---|---|---|---|
| `authFailed` | [StatusMatcher](#statusmatcher) |  | `authFailed` matches authentication failures (default 401). |
| `rateLimited` | [RateLimitedErrors](#ratelimitederrors) |  | `rateLimited` matches rate-limited responses. |
| `retryable` | [StatusMatcher](#statusmatcher) |  | Retryable matches transient errors (default 500, 502, 503, 504). |

## Limit

Limit is a constraint of the provider API, enforced by the budget engine for every request (see docs/spec/04-budget-engine.md).

| Field | Type | Required | Description |
|---|---|---|---|
| `appliesTo` | [AppliesTo](#appliesto) |  | `appliesTo` restricts the requests the limit counts. |
| `burst` | integer |  | Burst is the bucket capacity of a rate limit. Defaults to the rate per second, minimum 1. Minimum 1. |
| `description` | string |  | Description is free text for humans, ideally with the source of the limit. |
| `initialMax` | integer |  | `initialMax` is the conservative quota maximum used with max: auto until the first limit header is observed. Minimum 1. |
| `kind` | string | yes | Kind of limit: rate (token bucket), concurrency or quota (window). One of `rate`, `concurrency`, `quota`. |
| `max` | [IntOrAuto](#intorauto) |  | Max is the maximum of a concurrency limit, or of a quota per window. A quota may use "auto" to learn it from response headers. |
| `name` | [Name](#name) | yes | Name of the limit, unique within the provider. |
| `rate` | [Rate](#rate) |  | Rate is the refill rate of a rate limit, for example 10/s. |
| `scope` | [Scope](#scope) |  | Scope partitions the limit (one counter per scope key). |
| `source` | [LimitSource](#limitsource) |  | Source of the limit. Optional, and required on every limit by tapyr provider lint --catalog. |
| `sync` | [QuotaSync](#quotasync) |  | Sync reads quota state from response headers. |
| `window` | [Window](#window) |  | Window of a quota: a calendar period or a rolling duration. |

- When `kind` is `rate`: `rate` is required.
- When `kind` is `concurrency`: `max` is required.
- When `kind` is `quota`: `max` and `window` are required.

## Pagination

Pagination configures how successive pages are requested.

| Field | Type | Required | Description |
|---|---|---|---|
| `cursor` | [CursorPagination](#cursorpagination) |  | Cursor pagination (type cursor). |
| `expression` | [ExpressionPagination](#expressionpagination) |  | Expression pagination computed by CEL (type expression). |
| `linkHeader` | [LinkHeaderPagination](#linkheaderpagination) |  | `linkHeader` pagination following RFC 8288 Link headers (type link\_header). |
| `maxPages` | integer |  | `maxPages` is a hard safety cap on pages per run. Default `100`. Minimum 1. |
| `offset` | [OffsetPagination](#offsetpagination) |  | Offset pagination (type offset). |
| `page` | [PagePagination](#pagepagination) |  | Page pagination (type page). |
| `type` | string | yes | Type of pagination. One of `none`, `offset`, `page`, `link_header`, `cursor`, `expression`. |

- When `type` is `offset`: `offset` is required.
- When `type` is `page`: `page` is required.
- When `type` is `cursor`: `cursor` is required.
- When `type` is `expression`: `expression` is required.

## ParamSpec

ParamSpec declares a param or a secret that accounts provide.

| Field | Type | Required | Description |
|---|---|---|---|
| `description` | string |  | Description is free text for humans. |
| `name` | [Identifier](#identifier) | yes | Name of the param or secret. |
| `required` | boolean |  | Required makes the param or secret mandatory for every account (or every target of a multi-target account). |

## Redirects

Redirects allows HTTP redirects to other hosts.

| Field | Type | Required | Description |
|---|---|---|---|
| `hosts` | list of string | yes | Hosts the requests may be redirected to: a host name, or "\*." and a domain for every host under it (for example "\*.meraki.com"). Minimum items 1. |

## BasicAuth

BasicAuth sends HTTP basic credentials.

| Field | Type | Required | Description |
|---|---|---|---|
| `password` | [Value](#value) | yes | Password, usually {secret: name}. |
| `username` | [Value](#value) | yes | Username, a literal or {secret: name}. |

## BearerAuth

BearerAuth sends Authorization: Bearer &lt;token&gt;.

| Field | Type | Required | Description |
|---|---|---|---|
| `token` | [Value](#value) | yes | Token, usually {secret: name}. |

## HeaderAuth

HeaderAuth sends a static credential in a request header.

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Name of the header, for example X-API-Key. Minimum length 1. |
| `value` | [Value](#value) | yes | Value of the header, usually {secret: name}. |

## OAuth2ClientCredentials

OAuth2ClientCredentials configures the OAuth 2.0 client credentials grant.

| Field | Type | Required | Description |
|---|---|---|---|
| `audience` | string |  | Audience requested, when the server requires one. |
| `authStyle` | string |  | `authStyle` sends the client credentials in the Authorization header or in the request body. Default `header`. One of `header`, `body`. |
| `clientId` | [Value](#value) | yes | ClientID, a literal or {secret: name}. |
| `clientSecret` | [Value](#value) | yes | ClientSecret, usually {secret: name}. |
| `extraParams` | map of string to string |  | `extraParams` added to the token request. |
| `scopes` | list of string |  | Scopes requested. |
| `tokenURL` | [Template](#template) | yes | `tokenURL` of the authorization server. Template. |

## PluginAuth

PluginAuth selects a Go auth plugin, for flows that cannot be expressed declaratively.

| Field | Type | Required | Description |
|---|---|---|---|
| `config` | object |  | Config is passed to the plugin, which validates it. |
| `name` | string | yes | Name of the plugin, as registered in internal/auth/plugins. Pattern `^[a-z][a-z0-9_]*$`. |

## SessionLogin

SessionLogin posts credentials to a login endpoint and applies the returned token to subsequent requests.

| Field | Type | Required | Description |
|---|---|---|---|
| `apply` | [TokenApply](#tokenapply) | yes | Apply describes how the token is attached to requests. |
| `expiresIn` | [Expr](#expr) |  | `expiresIn` is a CEL expression returning the token lifetime as a duration. |
| `logout` | [LogoutRequest](#logoutrequest) |  | Logout closes a session when its token is replaced and when Tapyr stops, for APIs that keep sessions open until they are closed. |
| `perTarget` | boolean |  | `perTarget` keeps one session per target of a multi-target account. |
| `refreshBefore` | [Duration](#duration) |  | `refreshBefore` renews the session this long before it expires. Default `30s`. |
| `request` | [LoginRequest](#loginrequest) | yes | Request is the login request. |
| `token` | [Expr](#expr) | yes | Token is a CEL expression extracting the token from the login response. |

## Duration

Type: string. Pattern <code>^(&#91;0-9&#93;+(&#92;.&#91;0-9&#93;+)?(ns&#124;us&#124;µs&#124;ms&#124;s&#124;m&#124;h))+$</code>. Examples `30s`, `5m`, `1h30m`.

## StatusMatcher

StatusMatcher matches responses by HTTP status code.

| Field | Type | Required | Description |
|---|---|---|---|
| `statuses` | list of integer |  | Statuses that match. Minimum items 1. Each item: minimum 100, maximum 599. |

## RateLimitedErrors

RateLimitedErrors matches rate-limited responses and configures penalties.

| Field | Type | Required | Description |
|---|---|---|---|
| `defaultPenalty` | [Duration](#duration) |  | `defaultPenalty` applies when no Retry-After header is present. It grows exponentially on consecutive rate-limited responses, up to 10 minutes. Default `2s`. |
| `penalize` | string |  | Penalize the narrowest applicable scope, or all applicable scopes. Default `narrowest`. One of `narrowest`, `all`. |
| `retryAfterHeader` | string |  | `retryAfterHeader` carries the penalty in seconds or as an HTTP date. Default `Retry-After`. |
| `statuses` | list of integer |  | Statuses that signal rate limiting (default 429). Minimum items 1. Each item: minimum 100, maximum 599. |

## AppliesTo

AppliesTo restricts the requests a limit counts.

| Field | Type | Required | Description |
|---|---|---|---|
| `authRequests` | string |  | `authRequests` selects regular requests and token requests: include (both), only (token requests only) or exclude (regular requests only). Default `include`. One of `include`, `only`, `exclude`. |
| `endpoints` | list of string |  | Endpoints are globs on the templated path: \* matches within one segment, \*\* matches any number of segments. Each item: pattern `^/`. |
| `methods` | list of string |  | Methods the limit counts. Each item: one of `GET`, `HEAD`, `POST`. |
| `requestsWithVar` | [Identifier](#identifier) |  | `requestsWithVar` counts only requests where this variable is set. |

## IntOrAuto

Type: integer or string. Pattern `^auto$`. Minimum 1.

## Rate

Type: string. Pattern <code>^&#91;0-9&#93;+(&#92;.&#91;0-9&#93;+)?/(s&#124;m&#124;h)$</code>. Examples `10/s`, `600/m`.

## Scope

Scope partitions a limit.

| Field | Type | Required | Description |
|---|---|---|---|
| `by` | list of string |  | By lists the scope dimensions: instance, provider, account, target, credential, endpoint or var:&lt;name&gt;. Empty means instance. Unique items. Each item: pattern <code>^(instance&#124;provider&#124;account&#124;target&#124;credential&#124;endpoint&#124;var:&#91;A-Za-z&#95;&#93;&#91;A-Za-z0-9&#95;&#93;&#42;)$</code>. |

## LimitSource

LimitSource says where a limit comes from.

| Field | Type | Required | Description |
|---|---|---|---|
| `date` | string |  | Date the source was checked or the measurement made, YYYY-MM-DD or YYYY-MM. Pattern <code>^&#91;0-9&#93;{4}-(0&#91;1-9&#93;&#124;1&#91;0-2&#93;)(-(0&#91;1-9&#93;&#124;&#91;12&#93;&#91;0-9&#93;&#124;3&#91;01&#93;))?$</code>. |
| `kind` | string | yes | Kind is vendor-docs (the vendor documentation, with its url), measured (against the real API, with its date) or assumed (nothing documented). One of `vendor-docs`, `measured`, `assumed`. |
| `url` | string |  | `url` of the vendor documentation, or of the notes of a measurement. Format uri. |

## QuotaSync

QuotaSync names the response headers carrying quota state.

| Field | Type | Required | Description |
|---|---|---|---|
| `limitHeader` | string |  | `limitHeader` carries the quota maximum. |
| `remainingHeader` | string |  | `remainingHeader` carries the remaining quota. |
| `resetHeader` | string |  | `resetHeader` carries the reset time (epoch seconds or delta seconds). |

## Window

Window is the window of a quota: exactly one of period or rolling.

| Field | Type | Required | Description |
|---|---|---|---|
| `monthDay` | integer |  | `monthDay` is the day of month windows reset (1 to 31; the last day of shorter months). Default `1`. Minimum 1. Maximum 31. |
| `period` | string |  | Period of a fixed calendar window. One of `minute`, `hour`, `day`, `week`, `month`. |
| `resetAt` | string |  | `resetAt` is the time of day (HH:MM, 24h) at which day, week and month windows reset. Default `00:00`. Pattern <code>^(&#91;01&#93;&#91;0-9&#93;&#124;2&#91;0-3&#93;):&#91;0-5&#93;&#91;0-9&#93;$</code>. |
| `rolling` | [Duration](#duration) |  | Rolling is the duration of a rolling window (1-minute precision). |
| `timezone` | string |  | Timezone of the calendar (IANA name). Default `UTC`. |
| `weekStart` | string |  | `weekStart` is the first day of week windows. Default `monday`. One of `monday`, `tuesday`, `wednesday`, `thursday`, `friday`, `saturday`, `sunday`. |

## CursorPagination

CursorPagination passes a cursor extracted from each response.

| Field | Type | Required | Description |
|---|---|---|---|
| `cursor` | [Expr](#expr) | yes | Cursor is a CEL expression extracting the next cursor; empty or null stops. |
| `header` | string |  | Header carrying the cursor, instead of a query parameter. |
| `pageSize` | integer |  | `pageSize` is the number of items per page. Minimum 1. |
| `pageSizeParam` | string |  | `pageSizeParam` is the query parameter carrying the page size. |
| `param` | string |  | Param is the query parameter carrying the cursor. |

## ExpressionPagination

ExpressionPagination computes the next request with CEL.

| Field | Type | Required | Description |
|---|---|---|---|
| `next` | [Expr](#expr) | yes | Next is a CEL expression returning {query: {...}} for the next page, or null. |

## LinkHeaderPagination

LinkHeaderPagination follows RFC 8288 Link headers.

| Field | Type | Required | Description |
|---|---|---|---|
| `pageSize` | integer |  | `pageSize` is the number of items per page. Minimum 1. |
| `pageSizeParam` | string |  | `pageSizeParam` is the query parameter carrying the page size. |
| `rel` | string |  | Rel is the link relation of the next page. Default `next`. |

## OffsetPagination

OffsetPagination requests pages by item offset.

| Field | Type | Required | Description |
|---|---|---|---|
| `limitParam` | string | yes | `limitParam` is the query parameter carrying the page size. Minimum length 1. |
| `offsetParam` | string | yes | `offsetParam` is the query parameter carrying the offset. Minimum length 1. |
| `pageSize` | integer | yes | `pageSize` is the number of items per page. Minimum 1. |
| `start` | integer |  | Start is the first offset. Default `0`. Minimum 0. |
| `stop` | [Expr](#expr) |  | Stop is a CEL expression; pagination stops when it returns true. |
| `total` | [Expr](#expr) |  | Total is a CEL expression returning the total number of items. |

## PagePagination

PagePagination requests pages by page number.

| Field | Type | Required | Description |
|---|---|---|---|
| `pageParam` | string | yes | `pageParam` is the query parameter carrying the page number. Minimum length 1. |
| `pageSize` | integer |  | `pageSize` is the number of items per page. Minimum 1. |
| `sizeParam` | string |  | `sizeParam` is the query parameter carrying the page size. |
| `start` | integer |  | Start is the first page number. Default `1`. Minimum 0. |
| `stop` | [Expr](#expr) |  | Stop is a CEL expression; pagination stops when it returns true. |
| `total` | [Expr](#expr) |  | Total is a CEL expression returning the total number of items. |

## Identifier

Type: string. Pattern `^[A-Za-z_][A-Za-z0-9_]*$`.

## Value

A literal string, {secret: name} or {expr: cel}.

Either a string, or an object:

| Field | Type | Required | Description |
|---|---|---|---|
| `expr` | string |  | CEL expression evaluated at runtime. Minimum length 1. |
| `secret` | string |  | Name of a secret declared by the provider and supplied by the account. Pattern `^[A-Za-z_][A-Za-z0-9_]*$`. |

- Exactly one field is set.

## TokenApply

TokenApply attaches a session token to requests, as a header or a cookie.

| Field | Type | Required | Description |
|---|---|---|---|
| `cookie` | string |  | Cookie that carries the token. |
| `format` | string |  | Format of the header value; {token} is replaced by the token. Default `{token}`. |
| `header` | string |  | Header that carries the token. |

## Expr

CEL expression.

Type: string. Minimum length 1.

## LogoutRequest

LogoutRequest is the request that closes a session, sent with the token applied.

| Field | Type | Required | Description |
|---|---|---|---|
| `method` | string |  | Method of the logout request. Default `POST`. One of `POST`, `GET`, `DELETE`. |
| `path` | string | yes | Path of the logout endpoint, relative to the base URL. Pattern `^/`. |

## LoginRequest

LoginRequest is the request that opens a session.

| Field | Type | Required | Description |
|---|---|---|---|
| `body` | map of string to [Value](#value) |  | Body is a JSON object; values are literals, secrets or CEL expressions. |
| `headers` | map of string to string |  | Headers added to the login request. |
| `method` | string |  | Method of the login request. Default `POST`. One of `POST`, `GET`. |
| `path` | string | yes | Path of the login endpoint, relative to the base URL. Pattern `^/`. |
