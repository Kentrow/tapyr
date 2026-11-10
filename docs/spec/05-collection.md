# 05 - Collection: auth, pagination, discovery, retries

## Auth

All auth types implement one interface:

```go
type Authenticator interface {
    // Decorate adds credentials to the request, obtaining or refreshing tokens if needed.
    Decorate(ctx context.Context, req *http.Request) error
    // OnAuthFailure is called on a response classified as authFailed.
    // It returns true if the request should be retried once after re-authentication.
    OnAuthFailure(ctx context.Context, resp *http.Response) (retry bool, err error)
}
```

Token requests issued by authenticators go through the budget engine with `authRequest: true`.

### Declarative types

| Type | Config | Notes |
|---|---|---|
| `none` | - | |
| `header` | `name`, `value` (literal or secret), `format` (optional, `{value}` replaced by the value) | Static API key header, e.g. `X-Cisco-Meraki-API-Key`, or `Authorization: Token <key>` with `format: "Token {value}"` |
| `bearer` | `token` (secret) | `Authorization: Bearer <token>` |
| `basic` | `username`, `password` | |
| `oauth2_client_credentials` | `tokenURL`, `clientId`, `clientSecret`, `scopes`, `audience`, `extraParams`, `authStyle` (header or body) | Token cached until `expires_in - skew`; persisted |
| `session_login` | see below | Generic login flow returning a token or cookie |

### `session_login`

For APIs that require posting credentials to obtain a session token (e.g. Teltonika RutOS).

```yaml
auth:
  type: session_login
  sessionLogin:
    request:
      method: POST
      path: /api/login
      body:                                   # JSON body, values are CEL or secret refs
        username: { secret: username }
        password: { secret: password }
    token: "response.body.data.token"         # CEL, extracts the token
    expiresIn: "has(response.body.data.expires) ? duration(string(response.body.data.expires) + 's') : duration('300s')"
    apply:
      header: Authorization
      format: "Bearer {token}"                # or apply.cookie: <name>
    refreshBefore: 30s
    perTarget: true                           # one session per target (multi-target accounts)
    logout: { path: /api/logout }             # optional, closes replaced sessions
```

On `authFailed`, the session is invalidated and the request retried once after a new login.

### Plugins (Go)

Reserved for flows that cannot be expressed declaratively. Registered by name in `internal/auth/plugins` and selected with `auth.type: plugin`, `auth.plugin.name: <name>`. The plugin receives `auth.plugin.config` (free-form in the core schema) and validates it at compile time.

**`aruba_central_classic`**: Aruba Central (classic) uses OAuth access tokens with a limited lifetime and **rotating refresh tokens** (each refresh returns a new refresh token, the previous one becomes invalid). Requirements:

- Bootstrap from an initial access token and refresh token provided as secrets (generated in the Central UI), or from client credentials plus an initial refresh token.
- Refresh proactively before expiry; persist the new token pair in the state store **before** using it (crash-safety: losing a rotated refresh token means manual re-bootstrap).
- Serialize refreshes per account (never two concurrent refreshes).
- If the stored token pair is newer than the bootstrap secrets, the stored pair wins. A changed bootstrap secret (detected by hash) overrides the stored pair.
- Expose its refreshes in `tapyr_auth_token_requests_total{result}` and a health condition when re-bootstrap is required (`tapyr_auth_reauth_required`).
- Verified behavior: see [Implementation notes](#implementation-notes) below.

## Request building

- `path` is a template with `{var}` placeholders resolved from variables (discovery vars, account params, target params). Values are URL-path-escaped.
- `query` values are Values: literal strings, CEL expressions (`{ expr: "..." }`) or secret references (`{ secret: name }`, redacted everywhere).
- `headers` merge provider defaults, template, then job (job wins).
- `body` (for POST-based query APIs such as GraphQL) is supported: JSON with CEL-evaluated values. Tapyr remains read-only: POST is allowed only for queries, declared explicitly with `readOnly: true` in the template, and never retried unless `idempotent: true`.

### Redirects

The HTTP clients never follow a redirect by themselves: a redirect followed inside the client would be a request outside the budget engine, and it would carry the credentials to the host of the `Location`. The executor follows a redirect (3xx with a `Location`, 304 excepted) as a new request of its own: a reservation, the credentials of the account, and a request counted in the run. It does so only within the origin of the request (scheme, host and port), or to a host listed by the provider in `redirects.hosts` (a host name, or `*.` and a domain), never from `https` to another scheme, and at most 5 times in a row. 307 and 308 keep the method and the body; the other redirects become a `GET` without body. Any other redirect fails the request as a `client` error naming the host. Token and login requests and the push outputs do not follow redirects at all.

## Pagination

Every pagination type supports `maxPages` (default 100, hard safety cap) and contributes each page as one budgeted request.

| Type | Config | Stop condition |
|---|---|---|
| `none` | - | single request |
| `offset` | `offsetParam`, `limitParam`, `pageSize`, `start` (default 0), `stop` (CEL, optional), `total` (CEL, optional) | `stop` true, or `offset >= total`, or fewer items than `pageSize` |
| `page` | `pageParam`, `sizeParam`, `pageSize`, `start` (default 1), `stop`, `total` | same logic with page numbers |
| `link_header` | `pageSizeParam`, `pageSize`, `rel` (default `next`) | no `rel=next` link (RFC 8288). Links are followed only when they keep the scheme, host and port of the base URL, so credentials are never sent elsewhere |
| `cursor` | `cursor` (CEL extracting next cursor from response), `param` (query param name) or `header`, `pageSizeParam`, `pageSize` | cursor empty or null |
| `expression` | `next` (CEL returning a map `{query: {...}}` or `null`; in a conditional, write `dyn(null)` so both branches type-check: `more ? {'query': {'from': n}} : dyn(null)`) | `next` returns null |

The mapping receives all pages: `responses` (list) and `response` (the current page when evaluating per page). `extract.items` is evaluated per page and items are concatenated.

## Discovery (fan-out)

- A template with `role: inventory` produces **inventory items**: a key and a map of fields, cached in the state store with a TTL (default: 3x the job's effective interval).
- A template or inline job with `forEach` runs once per inventory item of its parent (`forEach.template` or, for inline jobs, `forEach.job`), with `parent.<field>` available in `forEach.vars` CEL expressions.
- Inventory jobs can also produce metrics (`role: both`).
- Fan-out instances are scheduled with spread: the N child instances of a job are distributed over the interval rather than fired simultaneously. A newly discovered instance runs first within the jitter of its interval, so that the data of a discovery chain does not wait one interval per level.
- Inventory changes (items added or removed) trigger a replan and the creation or removal of child instances. Series of removed instances become stale.
- `forEach.filter` (CEL on `parent`) selects a subset of items (e.g. only networks with wireless product type).
- The cardinality of fan-out is exposed (`tapyr_job_instances`) and capped by `forEach.maxInstances` (default 10000) to protect against runaway discovery.

### Implementation notes

- **Job instances.** A job runs as one instance per target (multi-target accounts) and per inventory item of its parent (fan-out). An instance is identified by the job name, the target and its variable values: `devices[target=north-01,orgId=42]`. The identity never appears in self-metric labels.
- **Variables.** The variables of a child instance are those of its parent instance, overridden by its own `forEach.vars`, so a third level can use variables of the first (`/orgs/{orgId}/networks/{networkId}/devices`). Target params, then account params, fill the variables not set by discovery.
- **Implicit parents.** A `forEach.template` that no job of the same account instantiates runs as an implicit job named `<account>.<template>`, with the template defaults and priority `low`.
- **Inventory.** A successful run of an inventory job replaces the items of its instance (key and fields, cached in the state store). Items not refreshed within the TTL are dropped. Children are recomputed after every parent run, then created or removed; removed instances have their series retired.
- **Before the first parent run** (or after a restart), children start from the cached inventory; without cache, they wait for the parent.
- **Spread.** The instances of a fan-out get a phase derived from a hash of their instance key (the first 8 bytes of its SHA-256, so that keys differing only by their last characters still spread), uniformly distributed over the interval. The phase of an instance is stable across restarts and does not move when other instances come or go (an even spacing in key order would shift every phase on each inventory change).
- **First run of a new instance.** An instance discovered while Tapyr runs (added by a run of its parent, not loaded from the cached inventory nor kept across a reload) runs first at a random point within the jitter of its interval (`settings.collection.jitter`, 10% by default), like a configured job at start. It then joins its phase, at the first slot at least half an interval after that first run: at most one more run per new instance, and the instances discovered together stay spread. The planner projects the steady state; every run goes through the budget engine, the first ones included.
- **Multi-target accounts.** Target params override account params (base URL included), target secrets override account secrets, target labels are added between account and job labels, and CEL sees `target.name`, `target.labels`, `target.params`. Circuit breakers and `perTarget` sessions are per target.
- **Token auth.** Token and login requests go through the budget engine with `authRequest: true`. Tokens are kept until `expires_in` (or `expiresIn`) minus `refreshBefore` (default 30s), persisted in the state store, encrypted with AES-GCM when `TAPYR_STATE_KEY` is set (a warning is logged once otherwise).
- **Token sessions.** The jobs of an account share one token: per credential and token URL for `oauth2_client_credentials`, per account (or per target with `perTarget`) for `session_login`, whose login request uses the base URL of the target that opens the session. A changed auth configuration opens a new session. Renewals are serialized: concurrent requests wait for one token request. A token is renewed `refreshBefore` (30s for OAuth2) before it expires, or halfway through its lifetime when it is shorter; without `expires_in` or `expiresIn`, it lives 1 hour unless rejected. On `authFailed`, the token is dropped (from the store too) and the request retried once with a new one.
- **Order.** A request obtains its credentials before its own budget reservation, so that a token request never waits behind a reservation held by the request it serves. Token requests are reserved as `critical` auth requests and count in `maxConcurrentRequests`. Their errors name the endpoint and the status, never the response body (it may carry tokens).
- **Stored tokens.** A version byte, then the token (no `TAPYR_STATE_KEY`) or the nonce and AES-GCM ciphertext under SHA-256(`TAPYR_STATE_KEY`), authenticated with the session key. A token that cannot be opened (another key, or stored before a key was set) is ignored and replaced.

- **Logout.** `sessionLogin.logout` (optional) declares a request (`method`, default `POST`, and `path` under the base URL) sent with the token applied, when the session replaces its token and when Tapyr stops. It goes through the budget engine as an auth request; its failures are logged at debug level and never fail a run. RutOS keeps sessions open after their token expires (its memory grows), hence this request.
- **Rejected credentials.** A token or login request answered 400, 401 or 403 is a rejection: the session sends no other token or login request for 1 minute, then 2, 4... up to 1 hour, and the runs needing it fail at once with the rejection. A successful request, or a new session (changed secrets or auth configuration, after a reload), ends the backoff. A wrong password thus never hits the API, nor locks the account, at every run.
- **TLS.** `Account.spec.tls` applies to every request of the account, auth requests included: `caFile` adds a PEM bundle to the system roots, `insecureSkipVerify` disables verification and logs a warning at every load. Accounts with TLS settings get their own HTTP transport; the others share one.
- **Plugins.** A plugin is Go code registered in `internal/auth/plugins` (an explicit registry built by `cmd/tapyr`, no `init` side effects). It validates `plugin.config` at compile time, where `{secret: <name>}` values name account secrets, and creates an authenticator per account (or target) from: the base URL, the resolved secrets, a sender of auth requests through the budget engine, a sealed key-value store in the state store (encrypted with `TAPYR_STATE_KEY` like tokens), the clock, a logger and a read-only flag. Read-only (`tapyr record`, next to `tapyr run`), the store does not persist and the plugin must not renew credentials whose renewal invalidates the previous ones: it returns an error instead. It reports its token requests and its re-bootstrap condition, shown by `tapyr_auth_reauth_required`, `/readyz` (as a reason, without failing readiness) and the status API. A plugin also provides its simulator side (`08-simulator.md`).
- **`aruba_central_classic`.** Verified against the Aruba developer documentation ([access token management](https://developer.arubanetworks.com/central/docs/access-token-management), [OAuth](https://developer.arubanetworks.com/aruba-central/docs/api-oauth-access-token), and the official `pycentral` SDK):
  - config: `clientId`, `clientSecret`, `refreshToken` (required) and `accessToken` (optional) as secret references, `tokenPath` (default `/oauth2/token`), `refreshBefore` (default 10m);
  - refresh: `POST <baseURL>/oauth2/token?client_id=...&client_secret=...&grant_type=refresh_token&refresh_token=...` (parameters in the query, as documented), answering `access_token`, `refresh_token`, `token_type`, `expires_in` (7200 seconds); the access token is applied as `Authorization: Bearer`;
  - every refresh returns a new refresh token, valid 15 days; the previous one is treated as spent. The new pair is written to the store before it is used. A crash between the answer and the write loses the pair: the refresh is the only request in flight for the account, and the README documents the re-bootstrap;
  - the stored pair records a hash of the bootstrap refresh token: a changed bootstrap secret (after a reload) replaces the stored pair, an unchanged one never does;
  - a refresh answered `400` or `401` sets the re-bootstrap condition: requests of the account fail without calling the API until the bootstrap secrets change; other failures are retried at the next request;
  - a request answered `401` triggers one refresh (serialized per account) and one retry.

## Error classification

| Condition | Class | Action |
|---|---|---|
| Matches `errors.rateLimited` | `rate_limited` | Penalty via budget engine, retry after penalty if within run deadline |
| Matches `errors.authFailed` | `auth` | `OnAuthFailure`, single retry if it returns true |
| Matches `errors.retryable`, network error, timeout | `transient` | Retry with backoff |
| Other 4xx | `client` | No retry, job run fails, logged with response excerpt (redacted) |
| Response decoding error | `decode` | No retry, run fails |
| Budget refusal | `budget` | No retry in this run, run marked `skipped_budget` |

## Retry policy

- Exponential backoff with full jitter: `sleep = random(0, min(maxBackoff, initialBackoff * multiplier^attempt))`.
- `maxAttempts` (default 4, including the first attempt).
- Every retry goes through the budget engine as a retry (retry reserve).
- A run has a deadline (default: its effective interval, capped at 10 minutes). Retries never extend a run beyond its deadline.
- Only idempotent requests are retried (GET, HEAD, and POST explicitly marked idempotent).
- Retried classes: `transient` (with backoff) and `rate_limited` (after the penalty set by the budget engine, which the next reservation waits for). `auth` is retried once when `OnAuthFailure` returns true (never for static auth types). `client`, `decode` and `budget` are not retried.
- Every retry and every page of a run share the run deadline; a retry that cannot complete before it is not attempted.
- Partial pagination failure: the run fails as a whole by default (no partial data emitted), unless `extract.partial: true` (then emitted samples are flagged with the self-metric `tapyr_job_partial_runs_total`).

## Circuit breaker

Per account (per target for multi-target accounts):

- Opens after `failureThreshold` consecutive `transient` failures (default 5).
- While open, requests are refused by the budget engine (`circuit_open`), so no quota is wasted.
- After `cooldown` (default 2m, doubling up to 30m on repeated openings), half-open: one probe request (the next scheduled one). Success closes it; failure reopens it.
- State exposed as `tapyr_circuit_state{account, target}` (0 closed, 1 half-open, 2 open).

## Run outcome

Each run records: start, end, duration, requests (by class), pages, retries, items, samples produced, result (`success`, `partial`, `failed`, `skipped_budget`, `skipped_overlap`, `skipped_circuit`), error summary (redacted). Runs are kept in the state store (last 50 per job instance) and exposed in the status API.
