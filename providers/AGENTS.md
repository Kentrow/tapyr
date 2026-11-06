# AGENTS.md - provider packages

Rules for the provider packages of `providers/`, on top of the [root `AGENTS.md`](../AGENTS.md). The guide for package authors is [`docs/providers.md`](../docs/providers.md), the format is in [`03-configuration.md`](../docs/spec/03-configuration.md#provider-packages) and the simulator in [`08-simulator.md`](../docs/spec/08-simulator.md). To write a new package, follow the [`new-provider`](../.agents/skills/new-provider/SKILL.md) skill.

## What a package is

One directory per API, everything Tapyr knows about it, in YAML and CEL. The layout, with the role of every file, is in [`03-configuration.md`](../docs/spec/03-configuration.md#provider-packages): a `provider.yaml`, job templates, a simulator with its three required scenarios, a contract test, dashboards, examples and a README.

`example-sim` is the reference package: small, vendor-neutral, every feature exercised.

## Rules

- **No Go in a package**, and no vendor logic in `internal/`. When an auth flow truly cannot be declared, it becomes an auth plugin (`internal/auth/plugins/<name>`, see `aruba-central-classic`), discussed in an issue first.
- **Every limit cites its source**: in its `source` (`kind: vendor-docs` with the `url`, `measured` with the `date`, or `assumed` when nothing is documented), and in its `description` and the README table for humans (how a measurement was made, why a value is assumed). A limit that is not real is worse than a missing one: it makes `tapyr plan` lie.
- **Catalog metadata** ([ADR 0011](../docs/adr/0011-provider-ecosystem.md)): `displayName`, `categories` from [`categories.yaml`](categories.yaml) (the main one first; a new category is added there in the same pull request), `maintainers` (GitHub handles), `tier` (`official` for the packages of the Tapyr maintainers, `community` otherwise) and `validation`. `validation.status` is `real-platform` only after a run against the real API, with its `date` and `platformVersion`; keep it in agreement with the validation status of the README.
- **Quotas sync from headers** when the API sends them (`sync.limitHeader`, `remainingHeader`, `resetHeader`); `max: auto` with `initialMax` when the maximum depends on the account.
- **Spend quota deliberately**: prefer organization-wide endpoints and large pages over one request per object, and set `estimate.pagesPerRun` to what a typical account needs. Default intervals follow the freshness of the data at the vendor (polling faster than the data changes only spends quota).
- **Metric names** are `<prefix>_<object>_<measure>_<unit>` in base units (`_seconds`, `_bytes`, `_celsius`, `_ratio`), one prefix per package; descriptive attributes go to `_info` metrics joined on an identifier.
- **No high-cardinality label**: no client MAC or IP address, no free text, no timestamp. Device identifiers are fine on device metrics.
- **Read-only**: templates use read endpoints only (`GET`, or `POST` for APIs whose queries are POSTs, never a mutation).
- **The simulator follows the documented API**: same paths, pagination, envelopes and quota headers, with the limits of `provider.yaml` (the simulator enforces them itself). Where it cannot imitate the API (reset headers are sent as seconds, a route has a fixed status: see [`08-simulator.md`](../docs/spec/08-simulator.md#synthetic-inventory-and-templated-fixtures)), say so in the README. Synthetic inventories and template fixtures are preferred over large static fixtures.
- **Fixtures hold no real data.** Recorded responses are anonymized with `tapyr record` and reviewed before they are committed ([`record-fixtures`](../.agents/skills/record-fixtures/SKILL.md)).
- **New built-in packages** are added to the `//go:embed` pattern of [`embed.go`](embed.go), to the table of [`docs/providers.md`](../docs/providers.md) and to [`10-providers.md`](../docs/spec/10-providers.md) when they are part of the supported set.

## Checks

```sh
go run ./cmd/tapyr provider lint --catalog providers/<name>  # loads, compiles, dashboards, examples, scenarios, contract test, catalog metadata
go run ./cmd/tapyr provider lint --update providers/<name>   # rewrite tests/expected.prom, then read the diff
go run ./cmd/tapyr sim --provider <name> --scenario bad-day  # try the simulator by hand
```

`make lint` lints every built-in package with `--catalog`; the CI does too. Read the `expected.prom` diff before accepting it: it is the contract of the package, and a value that moved without a reason is a bug.
