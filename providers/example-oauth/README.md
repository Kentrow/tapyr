# example-oauth

A vendor-neutral example of the `oauth2_authorization_code` auth type ([ADR 0016](../../docs/adr/0016-oauth2-authorization-code.md)), served by the built-in simulator: a consent once with `tapyr auth login`, then access tokens renewed and refresh tokens rotated by Tapyr. It is the reference for packages of APIs that use the OAuth 2.0 authorization code flow, and the package of the end-to-end test of that flow.

**Validation status**: simulator only, by design. All fixture data is synthetic.

## Coverage

| Template | Endpoint | Default interval | Requests | Metrics |
|---|---|---|---|---|
| `example-oauth-me` | `GET /me` | 6h | 1 | `example_oauth_user_info` |
| `example-oauth-usage` | `GET /users/{userId}/usage`, one instance per user | 30m | 1 | `example_oauth_project_items`, `example_oauth_project_info`, `example_oauth_storage_used_bytes`, `example_oauth_storage_quota_bytes` |

`example-oauth-me` is the inventory of `example-oauth-usage` (the usage endpoint takes the id of the user the token belongs to): a job using `example-oauth-usage` alone is enough.

## Auth setup

[Configure your accounts](../../docs/configure-accounts.md#accounts-that-need-a-consent-in-a-browser) explains the consent. Against the simulator:

```sh
tapyr sim --provider example-oauth --listen 127.0.0.1:8081 &
export TAPYR_LAB_CLIENT_ID=sim-client TAPYR_LAB_CLIENT_SECRET=sim-client-secret
tapyr auth login lab --config examples/example-oauth.yaml --secret-file ./lab-refresh-token
```

The command prints the authorization URL; the simulator consents at once, so opening it in a browser lands on `127.0.0.1:8085/callback?code=...`: paste that address back (the page itself does not load), or add `--listen` to let Tapyr receive it. `--device` runs the device authorization grant. The refresh token goes to `./lab-refresh-token`, given to the account in [`examples/example-oauth.yaml`](examples/example-oauth.yaml). The simulator also accepts `sim-refresh-token` as a bootstrap, for tests.

## Limits

| Limit | Value | Source |
|---|---|---|
| `rate-15min` | 100 requests every 15 minutes per application, rolling | Assumed, example only |
| `daily` | 1000 requests per day per application, reset at 00:00 UTC | Assumed, example only |

The limits are illustrative. The simulator enforces them (429), and counts the token requests too. With the default intervals, one user costs 4 user requests and 48 usage requests a day, plus about 4 token renewals.

## Simulator

```sh
tapyr sim --provider example-oauth --listen 127.0.0.1:8081
```

Under `tapyr sim --all --listen-base 127.0.0.1:8081`, it listens on 8089 (`portOffset: 8`). The API is under `/api/v1` and the OAuth endpoints (`/oauth/authorize`, `/oauth/token`, `/oauth/device`) at the root. It accepts the client `sim-client` / `sim-client-secret`, consents at once, checks PKCE, rotates refresh tokens; access tokens last 6 hours. The data is synthetic: one user, three projects.

| Scenario | Effect |
|---|---|
| `default` | No fault |
| `quota-tight` | The daily quota drops to 40 requests, below the 52 of the default intervals: Tapyr stretches the jobs |
| `bad-day` | Another client of the application spends 20 requests an hour; from 2h to 2h20, 30% of the usage requests fail with 503 |

## Dashboard

`dashboards/example-oauth.json` (Grafana, Prometheus data source, user variable): storage used and its share of the quota, items per project, storage over time, and the two quotas (limit, used by Tapyr and by others).

## Known caveats

- Not a real API: the endpoints, limits and payloads only exist in the simulator.
