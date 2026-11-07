# 08 - Provider simulator and record mode

## Why

Two of the built-in providers cannot be fully tested against real platforms by the maintainer: Aruba Central not at all, Meraki only through a DevNet sandbox organization with a few devices. More generally, contributors and users need to develop, test and demo providers without vendor access, and nobody can safely test quota exhaustion against a production API. The simulator makes every provider testable, and its rate-limit enforcement makes the budget engine testable end-to-end.

## Principles

- **Same definition, two consumers.** The simulator reads the provider package: the `Provider` document (limits, pagination, auth, error semantics) and the package's `sim/` directory (routes, fixtures, scenarios). Limits are declared once.
- **Package-internal kinds.** `Simulation` and `Scenario` documents live only in a package's `sim/` directory; user configuration paths reject them.
- **Server-side enforcement reuses the budget package** (rate, concurrency, quota windows) in "enforcer" mode, so the simulator returns 429, `Retry-After` and quota headers exactly as declared.
- **Deterministic by default** (seeded randomness), reproducible in CI.
- **Runs on the injected clock.** Automated tests share a fake clock between Tapyr and the simulator and run 24 simulated hours in seconds; `--time-scale` accelerates the real clock for demos and manual tests.

## `sim/sim.yaml`

```yaml
apiVersion: tapyr/v1alpha1
kind: Simulation
metadata:
  name: meraki-dashboard
spec:
  inventory:                          # synthetic inventory generator (generic sets)
    seed: 42
    sets:
      - { name: organizations, count: 2 }
      - { name: networks, parent: organizations, count: 5 }   # count items per parent item
      - { name: devices, parent: networks, count: 20 }        # per network
  auth:
    accept:                           # credentials the simulator accepts
      bearer: ["sim-api-key"]
  headers:                            # quota headers emitted, mapped to limit names
    X-RateLimit-Limit-day: "limit:daily.max"
    X-RateLimit-Remaining-day: "limit:daily.remaining"
  routes:
    - method: GET
      path: /organizations
      response:
        template: fixtures/organizations.json.tmpl
    - method: GET
      path: /organizations/{organizationId}/devices/availabilities
      pagination: link_header         # simulator paginates the generated list
      response:
        # values evolving over simulated time come from template helpers,
        # e.g. {{ flap $d "status" "10m" "online:0.95" "offline:0.03" "alerting:0.02" }}
        template: fixtures/availabilities.json.tmpl
```

- `basePath` (default `/`) is the path prefix of the API (the path part of the provider base URL); routes are relative to it.
- Credentials are checked the way the `Provider` declares its auth (`bearer`: `Authorization: Bearer <one of accept.bearer>`; `header`: the declared header with one of `accept.header`; `basic`: one of `accept.basic`). Wrong or missing credentials get the provider's `authFailed` status.
- A route with `pagination` serves the items of its fixture (a JSON array) page by page. The pagination parameters are not redeclared: they come from the job templates requesting that path, or from the provider default.
- Fixtures are JSON templates (Go `text/template` with helpers: `range` over inventory, deterministic pseudo-random values, counters increasing with time, timestamps from the simulated clock).
- Static JSON fixtures (e.g. recorded) are supported directly.
- Unknown routes return 404 like a real API.

### Synthetic inventory and templated fixtures

The inventory is generic: the core knows sets of items and parents, never vendor objects, and dynamics are template helpers rather than per-route declarations.

- Every generated item has `name` (its set), `index` (position in its set, from 0), `n` (position under its parent, from 0), `key` (`<set>-<index>`) and `parent` (the parent item, or nil). Sets are generated in declaration order; a parent set must come first. The inventory is fixed for the lifetime of the simulator.
- `response.template` (instead of `response.file`) is a Go `text/template` rendering JSON. Its data: `.vars` (route variables), `.inventory` (items by set name), `.now` (simulated time) and `.query` (query parameters). Helpers, all deterministic for a given seed:
  - `items "set"` and `children "set" item` (items of a set, or those under an item), `find "set" "key"` (the item with that key), `where list "field" value`;
  - `id item "prefix"` (a stable identifier), `serial item "XXXX-XXXX-XXXX"` (X letters, 9 digits), `mac item`, `ipv4 item` (in 10.0.0.0/8), `pick item "field" "a" "b" ...` (one value per item and field), `num item "field" lo hi` (an integer in [lo, hi], fixed per item and field);
  - dynamics: `rand item "field" min max` (a float in [min, max] rounded to 3 decimals, fixed per item; use `num` for integers such as counts and identifiers), `walk item "field" min max "10m"` (a float, like `rand`, that moves inside [min, max] every period), `flap item "field" "10m" "online:0.95" "offline:0.03" "alerting:0.02"` (a weighted state, drawn again every period), `counter item "field" ratePerSecond` (grows with simulated time, for native counters);
  - `json value` (JSON encoding), `ts "2006-01-02T15:04:05Z07:00" time`, `unix time`, `ago "5m"` (now minus a duration), `add`, `sub`, `mul`, `div`, `mod`, `seq n`, `last list`.
- Dynamics depend only on the seed, the item, the field and the period number, so a contract test at a fixed simulated time always sees the same values.
- Static fixtures (`file`) keep working, recorded ones included. A fixture path may use the route variables (`file: devices/{id}.json`): a missing file answers 404, like an unknown object. A fixture that does not render or parse answers 500 with its cause, which the contract test of `tapyr provider lint` shows.
- The simulator loads its package at startup: restart it after editing a fixture. Limitations to keep in mind when imitating an API: quota reset headers (`limit:<name>.reset`) are sent as a number of seconds until the reset, and the status of a route is fixed (errors come from scenarios, not from the fixtures).
- `spec.portOffset` places the simulator in `tapyr sim --all`: it listens at `--listen-base` plus this offset, so that adding a package moves no other simulator. Packages without one take the offsets after the largest declared one, in provider name order; two packages declaring the same offset are an error. The built-in packages declare 0 (`aruba-central-classic`) to 4 (`teltonika-rutos`).
- `response.itemsField` (a dotted path, for example `data` or `aps`) paginates the list found at that path of the document instead of a top-level array: the simulator replaces the list by the requested page and keeps the rest of the envelope (counts and totals are computed by the template over the whole list).

## Scenarios

`sim/scenarios/*.yaml`, selected with `--scenario`. A scenario overlays faults on top of the default behavior, on a timeline:

```yaml
apiVersion: tapyr/v1alpha1
kind: Scenario
metadata:
  name: bad-day
spec:
  steps:
    - at: 0s
      externalConsumption: { limit: daily, ratePerHour: 300 }   # another tool eats the quota
    - at: 2h
      latency: { p50: 800ms, p99: 5s }
    - at: 3h
      errors: { routes: ["/organizations/**"], status: 503, ratio: 0.3 }  # globs as in appliesTo.endpoints
      for: 20m
    - at: 6h
      outage: true                                               # connections dropped
      for: 10m
    - at: 8h
      tokenExpiry: true                                          # invalidate tokens, force 401
```

Built-in scenarios for every provider: `default`, `quota-tight` (undersized daily quota), `bad-day`.

A step applies from `at` until `at + for`, or until the end when `for` is omitted. `at` is measured from the simulator start, in simulated time. Step actions:

- `limits` overrides the `max` of named quota limits, for example `limits: { daily: { max: 1500 } }`.
- `externalConsumption` makes another client consume a quota at a steady hourly rate.
- `errors` gives a status to a ratio of the requests to matching routes.
- `fixtures` replaces the fixture of routes during the step (`fixtures: { "/orgs/{orgId}/networks": "fixtures/networks-after.json" }`), to simulate inventory changes.
- `latency: { p50: 800ms, p99: 5s }` delays responses following a log-normal distribution with these quantiles (seeded), in simulated time.
- `outage: true` closes connections without a response (the client sees a network error) while the step is active; a step without `for` lasts until the end.
- `tokenExpiry: true` invalidates, when the step starts, every token issued before it (the next request gets `authFailed`).

## Auth and pagination

With `oauth2_client_credentials`, the simulator serves the path of the provider's token URL (rendered without account params, so its defaults point to the simulator) and accepts the client credentials listed in `auth.accept.clients` (`clientId`, `clientSecret`, in the Authorization header or the form); with `session_login`, it serves the login path under `basePath` and accepts a JSON body carrying both values of one of the `auth.accept.logins` username and password pairs. Both issue opaque tokens valid for `auth.tokenLifetime` (default 10m, simulated time), then accepted as the provider applies them (bearer, header with format, or cookie); expired tokens get the `authFailed` status. The token endpoint answers `access_token`, `token_type` and `expires_in`; the login endpoint answers `{"token": ..., "expiresIn": <seconds>}`, or the `auth.loginResponse` fixture with `{token}` and `{expiresIn}` replaced, to match the shape the provider's `token` expression reads. Token and login requests are checked against the limits as auth requests.

A provider whose auth is a plugin is simulated by the plugin itself: the plugin registers a simulator side that serves its token endpoints and validates its tokens, with the credentials of `auth.accept.plugin` (free-form, read by the plugin).

Pagination `page` and `cursor` are served like `offset` and `link_header` (`expression` is client-side only); with `cursor`, the next cursor is an opaque value in the `X-Next-Cursor` response header (absent on the last page), read by templates with `default(response.headers['x-next-cursor'][0], '')`.

## CLI

```
tapyr sim --provider meraki-dashboard [--provider-path ./providers] [--scenario bad-day] [--listen :8081] [--time-scale 60] [--seed 42]
tapyr sim --all --listen-base 127.0.0.1:8081   # one simulator per provider, at --listen-base plus its portOffset
```

`--time-scale N` runs the simulated clock N times faster than real time (windows, refills, penalties and scenario steps follow it). It is meant for demos and manual tests; automated tests share a fake clock between Tapyr and the simulator instead.

The simulator exposes one endpoint of its own, `GET /_sim/status`: a JSON object with the `provider` and its package `version`, the served `routes`, the number of `requests` received, the simulated `time`, the active `scenario`, `stats` (requests served, rate limited by reason, injected errors, tokens issued, dropped connections, sessions, requests per route) and the state of the `quotas`. Useful to cross-check Tapyr's self-metrics in demos.

## Record mode

`tapyr record` captures real API responses, anonymized, to build fixtures for a provider package or to attach responses to a bug report. It writes outside the repository: nothing enters it before a review.

```
tapyr record --config ./config --job rms-devices [--job ...] --out ./recordings/rms [--anonymize ./anonymize.yaml]
```

- Every instance of the selected jobs runs once, through the budget engine. Children of fan-outs run once their parent has run.
- `--out` receives one JSON file per page (`<job>/<n>.json`, anonymized), `index.yaml` (method, templated path, pseudonymized variables, status, rate-limit headers, file per page) and `report.txt`. Request credentials and response headers other than the rate-limit headers of the provider (`sync` headers and `Retry-After`) are never written.
- Default rules replace, by key name and by value pattern: serial numbers, MAC addresses, IMEI, IMSI, ICCID, IPv4 and IPv6 addresses, hostnames, emails, phone numbers, names (keys `name` or ending in `_name`/`Name`), identifiers (keys `id` or ending in `_id`/`Id`, with the same pseudonym in path variables, so that fixtures stay consistent), coordinates and addresses. Pseudonyms keep the format: MACs become locally administered, IPv4 addresses move to 192.0.2.0/24, 198.51.100.0/24 and 203.0.113.0/24, IPv6 to 2001:db8::/32, emails to example.com, serials and identifiers keep their length and character classes (every letter and digit becomes an ASCII one, so no character of the value is kept), numbers stay numbers. They come from an HMAC with a random per-recording key that is never written.
- `--anonymize` adds rules (`replace: [{path: "data[*].company_name", as: name}]`) and exceptions (`keep: ["data[*].model"]`); paths are dotted, with `[*]` for every element.
- `report.txt` lists every replaced path with its rule and count, never an original value, for review before anything is committed. The recording is meant to be turned into static fixtures by hand (`file` routes).

## Contract tests

Each provider package's `tests/` directory holds the expected samples of its templates run against the simulator with a fixed seed. `tapyr provider lint` and the CI run them: a change in mapping or fixtures that changes the produced series must update the expected file explicitly.

- `tests/contract.yaml` holds the Account and Job documents run by the test; `$SIM_URL` is replaced by the URL of the simulator with its base path, and `$SIM_ADDR` by the URL without it (for base URLs that add the path themselves), before the documents are parsed; secrets are read from `tests/contract.env` (`KEY=VALUE` lines of simulator credentials), never from the environment, and the account uses the credentials accepted by the simulator.
- `tapyr provider lint <dir>` validates the package, compiles its templates, lints its dashboards and runs the contract test: the simulator and the collector share a fake clock starting at `2026-01-01T00:00:00Z` with seed 42; every job instance runs once (children after their parents), and the samples are compared with `tests/expected.prom` (Prometheus exposition, sorted, without timestamps). `--update` writes the file instead. The CI runs the lint of every built-in package, with `--catalog` ([`03-configuration.md`](03-configuration.md#provider-packages)).
- Dashboards (`dashboards/*.json`, Grafana) must parse, use a datasource variable, and query only metrics produced by the package templates or `tapyr_` self-metrics.

## Future

- Provider packages distributed with their simulator as OCI artifacts, community registry.
- Simulator scenarios composable across providers for lab use (e.g. a shared "branch office outage" timeline).
