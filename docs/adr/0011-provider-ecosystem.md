# ADR 0011 - Generic engine, network-first launch and a provider catalog

- Status: accepted
- Date: 2026-11-06

## Context

v0.1 ships four provider packages, all for network equipment: Aruba Central (classic), Cisco Meraki, Teltonika RMS and Teltonika RutOS. The problem Tapyr solves, collecting metrics from APIs whose quota is scarce, is not specific to networks: a survey of candidate APIs ([provider ideas](../provider-ideas.md)) found it in security services, Internet measurement, cloud costs, developer platforms, energy and sport. Most of them fit the configuration model as it is; some need mechanisms the budget engine does not have yet.

At the same time, an open source project that claims to do everything struggles to find its first users, and the provider ecosystems that work (Terraform Registry, Ansible Galaxy, Grafana dashboards) show that discoverability and trust signals decide whether contributions keep coming. Today a provider package carries a name, a version, a description and a documentation link: nothing tells who maintains it or how it was tested, except the prose of its README. Users can already load their own packages from `settings.providerPaths`; the built-in packages are the ones in [`providers/`](../../providers/), embedded in the binary.

## Decision

1. **Generic engine.** The core makes no network-specific assumption: any metric-oriented API must be expressible as a provider package. A provider that needs a missing mechanism gets a generic, configurable one, never vendor logic in `internal/` (the design principles of [`AGENTS.md`](../../AGENTS.md)).
2. **Network-first launch.** Networks are the first use case, not the boundary. The README, the site and the specs lead with network equipment, where the built-in packages are; other domains come with the engine features they need, ideally from contributions.
3. **Catalog metadata on provider packages**, in v0.2.0. Two independent axes describe a package instead of a single trust level:
   - `tier`: who maintains it. `official` packages are maintained by the Tapyr maintainers; `community` packages by their contributors, named in `maintainers`;
   - `validation`: how it was tested. `status` is `simulator` or `real-platform`, with the `date` and the `platformVersion` of the last validation.

   With `displayName`, `categories` (a controlled vocabulary kept in the repository) and `maintainers`, and a structured `source` on every limit (`kind`: `vendor-docs`, `measured` or `assumed`, with a `url` and a `date`). The fields are optional in the schema, so that user packages keep working; `tapyr provider lint --catalog` requires them, and the CI of this repository runs it on the built-in packages.
4. **One repository for official and community packages.** A community package is contributed by pull request to `providers/`, passes the same CI as the official ones (lint, contract test, simulator) and is embedded in the binary. Its `tier` and `maintainers` say who answers for it, and the catalog shows them. A community package that is broken and whose maintainers no longer answer may be removed in a minor release, announced in the changelog; users who still need it load the last version from `providerPaths`.
5. **A phased ecosystem.**
   - Phase 1, from v0.2: a static provider catalog on tapyr.eu, built by the site repository from `tapyr provider list --output json`, as the documentation site is built from this repository ([ADR 0010](0010-documentation-site.md)). Each entry links the page of its package on docs.tapyr.eu.
   - Phase 2, later and only if justified by packages maintained outside this repository: provider packages distributed as OCI artifacts, evaluating Artifact Hub before building a registry. It stays out of scope until an issue and an agreement ([`01-vision-and-scope.md`](../spec/01-vision-and-scope.md#out-of-scope-keep-in-mind-do-not-build)).

## Alternatives considered

- **Network-only scope.** Simpler to explain, but it limits a generic engine and the community that could grow around it for no technical reason.
- **Three trust tiers** (`official` in the main repository, `verified` tested on a real platform, `community` passing CI only). They mix who maintains a package with how it was tested: `meraki-dashboard` is official but validated on its simulator only, the Teltonika packages are official and validated on real platforms. Two axes keep both facts.
- **Community packages outside the binary** (a `providers/community/` directory, or a separate repository, loaded through `providerPaths`). The binary would not ship packages its maintainers do not answer for, but users would download a directory and track its versions by hand, which phase 2 solves properly. Revisited with phase 2.
- **An open catalog from day one** (Galaxy style, anyone publishes). Packages of uneven quality, abandoned ones, and no trust signal yet to tell them apart.
- **A registry with accounts and uploads now.** A high cost with no value before there are packages to distribute.

## Consequences

- Writing a package takes a little more work (metadata, structured sources of limits), and building the catalog much less: it is generated, never written by hand.
- The engine is generic in principle but not yet in features. The survey found five mechanisms that other domains need: a cost per request, quota windows aligned on any duration, quota sync from expressions, windows starting at the first request, rate limiting signaled other than by a status ([engine gaps](../provider-ideas.md#engine-gaps)). Each one is designed in its own issue, with a spec change and an ADR when it changes the reservation algorithm or the ledger.
- The binary grows with every community package, and the maintainers review packages for APIs they cannot test: the review rests on the simulator, the contract test and the cited sources, and the catalog says what was validated.
- A metric naming convention per domain will be needed when the second domain lands (today `<prefix>_<object>_<measure>_<unit>`, [`docs/providers.md`](../providers.md#writing-a-package)); it is decided then, with the first package of that domain.
- The output of `tapyr provider list --output json` becomes an interface of the site repository, versioned like the output of `docsite export`.
