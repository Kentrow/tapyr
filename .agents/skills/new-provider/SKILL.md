---
name: new-provider
description: Write a new Tapyr provider package (providers/<name>/) from the documentation of a vendor API - provider.yaml with sourced limits, job templates with CEL mappings, simulator and scenarios, contract test, Grafana dashboard, examples and README. Use when asked to add support for an API or a vendor. Not for changing one template of an existing package (edit it and run tapyr provider lint), nor for an auth flow that needs Go (open an issue first).
---

# New provider package

A provider package is everything Tapyr knows about one API, in YAML and CEL. The result of this skill is a directory `providers/<name>/` that passes `tapyr provider lint`, and a pull request that a maintainer can review against the vendor documentation.

Read first: [`providers/AGENTS.md`](../../../providers/AGENTS.md) (the rules), [`docs/providers.md`](../../../docs/providers.md) (the author guide), and the package closest to your API:

| If the API... | Start from |
|---|---|
| is simple, token auth, link-header pagination | `providers/example-sim` (the reference) |
| has a monthly quota with `X-RateLimit-*` headers, offset pagination | `providers/teltonika-rms` |
| has per-organization rate limits, organization-wide endpoints, envelopes | `providers/meraki-dashboard` |
| runs on each device (one target per device), session login with logout, self-signed TLS | `providers/teltonika-rutos` |
| has a daily quota and an auth flow that needs Go (rotating refresh tokens) | `providers/aruba-central-classic` |

The format is defined in [`03-configuration.md`](../../../docs/spec/03-configuration.md) (Provider, JobTemplate), [`05-collection.md`](../../../docs/spec/05-collection.md) (auth, pagination, discovery), [`06-mapping-and-outputs.md`](../../../docs/spec/06-mapping-and-outputs.md) (CEL extract) and [`08-simulator.md`](../../../docs/spec/08-simulator.md) (Simulation, Scenario). Read the parts you use: do not guess field names, the JSON Schemas in `schemas/v1alpha1/` reject unknown ones.

## 1. Research the API

Before writing YAML, collect from the vendor documentation, with a link for each:

- base URL (regions, clusters, on-premises), API version, read endpoints with the objects and fields worth collecting;
- authentication: header, bearer, basic, OAuth2 client credentials, session login, or something else (if it needs Go, stop and open an issue);
- pagination: offset, page, cursor, link header, or none, with the page size limits;
- **every limit**: requests per second or minute, per day or month, concurrency, and what they are scoped by (token, account, organization, IP); quota headers (limit, remaining, reset) and `Retry-After`;
- errors: the status of rate limiting (429 or other), of expired credentials, of transient failures. `errors.rateLimited` matches on status codes only: when an API also uses the rate-limit status for other errors (403 for permissions, for example), declare it anyway and say in the README that those errors are treated as rate limiting.

Write down what is documented, what you measured (with the date) and what you assume. Ask the person if a limit is unknown: an invented limit makes `tapyr plan` lie.

Pick the endpoints that return the most objects per request (organization-wide lists, large pages) and a default interval that matches the freshness of the data at the vendor.

## 2. Write the package

Create the layout of `providers/AGENTS.md`. Name the package after the vendor and product (`vendor-product`), and pick one metric prefix.

1. `provider.yaml`: the catalog metadata (`displayName`, `categories` from `providers/categories.yaml`, `maintainers`, `tier`, `validation` with `status: simulator` until a run against the real API, see `providers/AGENTS.md`), `baseURL` from a param, `params` for what varies per account (a path variable such as `{org}` can come from an account param), `secrets` with descriptions, `auth`, `defaults`, `pagination`, `limits` (each with a `source` and a `description` naming it, `scope.by`, `sync` headers when the API sends them, `max: auto` and `initialMax` when the maximum depends on the account), `errors.rateLimited`. Scope dimensions ([`04-budget-engine.md`](../../../docs/spec/04-budget-engine.md#scope-dimensions)) are `instance` (the whole process, which approximates "per source IP"), `provider`, `account`, `target`, `credential`, `endpoint` and `var:<name>`: a limit "per user" or "per token" is `credential` (accounts sharing a secret share it); say in the README when the vendor scope is wider (all the tokens of a user, for example).
2. `templates/*.yaml`: one JobTemplate per endpoint, with `defaultInterval`, `defaultPriority`, `estimate.pagesPerRun` and the `extract` (`items`, `filter`, `metrics`, `info`, `post`). Metric names `<prefix>_<object>_<measure>_<unit>` in base units; descriptive attributes in `<prefix>_<object>_info`; no high-cardinality label. Use `inventory` and `forEach` for per-object endpoints (see `meraki-dashboard` and `example-sim`).
3. `sim/sim.yaml`: the same paths, pagination and envelopes as the real API, `auth.accept` with obviously fake credentials (`sim-...`), a synthetic `inventory` and template fixtures (`sim/fixtures/*.json.tmpl`) in the documented response shape. `example-sim` uses static fixtures; for template fixtures, copy `teltonika-rms` or `meraki-dashboard`, and use the helpers of [`08-simulator.md`](../../../docs/spec/08-simulator.md#synthetic-inventory-and-templated-fixtures) (`num` for integers, `rand` and `walk` return floats). Quota headers in `headers` when the API sends them. The simulator enforces the limits of `provider.yaml` by itself; its known gaps (reset headers as seconds, a fixed status per route) are listed in the spec, mention them in the README when the real API differs.
4. `sim/scenarios/`: `default.yaml` (no fault), `quota-tight.yaml` (a quota smaller than the plan, or another client consuming it), `bad-day.yaml` (errors, latency or an outage on the main routes). Copy the shape of an existing package.
5. `tests/contract.yaml` (an account with `baseURL: "$SIM_URL"` or `$SIM_ADDR`, one job per template) and `tests/contract.env` (the simulator credentials). The contract follows pagination only when the synthetic inventory is larger than the page size of the template; when that would make `expected.prom` too large, check the pagination by hand against the simulator (step 3).
6. `dashboards/<name>.json`: a Grafana dashboard with a `datasource` variable, querying only the metrics of the package and `tapyr_` self-metrics. Copy the structure of an existing one.
7. `examples/<name>.yaml`: an account with secrets from environment variables and the jobs, commented.
8. `README.md`: the skeleton of every package ([`docs/providers.md`](../../../docs/providers.md#writing-a-package), step 5; copy `providers/example-sim/README.md`): validation status (simulator only until someone tests against the real platform), coverage, auth setup step by step, limits table (limit, value, source) with the quota maths of a typical account, simulator scenarios, dashboard, known caveats.

## 3. Check it

```sh
go run ./cmd/tapyr provider lint --update providers/<name>   # writes tests/expected.prom
go run ./cmd/tapyr provider lint --catalog providers/<name>  # must pass
go run ./cmd/tapyr sim --provider <name> --provider-path providers/<name>   # try it by hand
```

When the contract test fails, run the simulator by hand and call it, to see the responses and the simulator logs:

```sh
go run ./cmd/tapyr sim --provider <name> --provider-path providers/<name> --listen 127.0.0.1:8099 &
curl -si -H 'Authorization: Bearer sim-...' http://127.0.0.1:8099/<path>   # headers, Link, quota headers
```

A fixture that does not render answers 500 with its cause; a 404 is an unknown route or a missing fixture file. The simulator loads the package at startup: restart it after editing a fixture.

Read `tests/expected.prom` line by line against the fixtures: names, labels, values and units must be what the dashboard and a user expect. Then run `tapyr plan` on the example and check that the consumption matches the quota maths of the README:

```sh
EXAMPLE_TOKEN=fake go run ./cmd/tapyr plan --config providers/<name>/examples --provider-path providers/<name> [--fanout <parent template>=300]
```

`--fanout` takes the parent as named in `forEach` (its template, or its job with `forEach.job`) and the number of instances to assume.

If the person has access to the real API, offer to validate against it: a short `tapyr run` and the [`record-fixtures`](../record-fixtures/SKILL.md) skill to compare the real responses with the fixtures. Never use real credentials without being asked.

## 4. Wire it in (built-in packages only)

Whether a package becomes built in (embedded in the binary) is decided with the maintainers in the issue; otherwise it is used through `providerPaths`. For a built-in package:

- Add the directory to the `//go:embed` pattern of `providers/embed.go`.
- Declare `portOffset` in `sim/sim.yaml`: the largest offset of the built-in packages plus one (`grep -h portOffset providers/*/sim/sim.yaml`), so that `tapyr sim --all` serves it without moving the others. Add it to the list of `TestSimAll` (`cmd/tapyr/sim_all_test.go`), and to `examples/compose` (`tapyr/config.yaml`, `compose.yaml`, `README.md`) if it belongs in the demo.
- Add the package to the table of `docs/providers.md`.
- Add a `CHANGELOG.md` entry under `## [Unreleased]`, `### Added`.
- Run the [`check-change`](../check-change/SKILL.md) skill.

## Done when

- `tapyr provider lint --catalog providers/<name>` and `make lint test` pass.
- Every limit has a `source` (documented with a link, measured with a date, or assumed) in `provider.yaml`, also in the README.
- The fixtures follow the documented response shapes and hold no real data.
- The pull request lists the documentation pages used and what was validated against the real API, if anything.
