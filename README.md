<p align="center">
  <img src="docs/assets/logo-light.svg#gh-light-mode-only" alt="tapyr" width="360">
  <img src="docs/assets/logo-dark.svg#gh-dark-mode-only" alt="tapyr" width="360">
</p>

<p align="center"><strong>Sniff out metrics from any API, without ever blowing your rate limits.</strong></p>

<p align="center">
  <a href="https://github.com/kentrow/tapyr/actions/workflows/ci.yml"><img src="https://github.com/kentrow/tapyr/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/kentrow/tapyr/releases/latest"><img src="https://img.shields.io/github/v/release/kentrow/tapyr?sort=semver" alt="Latest release"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/Kentrow/tapyr"><img src="https://api.scorecard.dev/projects/github.com/Kentrow/tapyr/badge" alt="OpenSSF Scorecard"></a>
  <a href="https://www.bestpractices.dev/projects/16342"><img src="https://www.bestpractices.dev/projects/16342/badge" alt="OpenSSF Best Practices"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="License: Apache-2.0"></a>
  <img src="https://img.shields.io/badge/go-1.27-00add8" alt="Go 1.27">
</p>

<p align="center">
  <a href="https://tapyr.eu">Website</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="docs/install.md">Install</a> ·
  <a href="https://docs.tapyr.eu">Documentation</a> ·
  <a href="CONTRIBUTING.md">Contributing</a>
</p>

Tapyr is an open source, self-hosted **API-to-metrics gateway**. It collects data from the REST APIs of cloud-managed network gear (Aruba Central, Cisco Meraki, Teltonika...), turns it into clean metrics, and exposes or pushes them to Prometheus, VictoriaMetrics, Mimir or any OTLP backend. One declarative definition per API, no code to write. Network gear is the first use case, not the limit: the engine knows nothing about networks, and any API that serves metrics can be described the same way.

Vendor APIs ration their calls: per second, per day, per month, per organization. Tapyr treats that quota as a budget it plans and spends deliberately, retries included, so that your monitoring never runs the quota of your account dry.

<p align="center">
  <img src="docs/assets/overview-light.svg#gh-light-mode-only" width="900" alt="Cloud APIs of SD-WAN, SD-LAN, Wi-Fi, 4G and 5G routers and any other platform feed Tapyr, which polls, paginates and retries, normalizes labels and units, and spends the API quota on a budget, then exposes or pushes the metrics as Prometheus and OTLP.">
  <img src="docs/assets/overview-dark.svg#gh-dark-mode-only" width="900" alt="Cloud APIs of SD-WAN, SD-LAN, Wi-Fi, 4G and 5G routers and any other platform feed Tapyr, which polls, paginates and retries, normalizes labels and units, and spends the API quota on a budget, then exposes or pushes the metrics as Prometheus and OTLP.">
</p>
<p align="center"><sub>Prometheus and OpenTelemetry logos are trademarks of their respective owners.</sub></p>

## Why Tapyr

- **Rate limits are first-class.** Each provider declares its limits once: rate, concurrency, daily or monthly quotas, scoped by account, organization, credential or IP. Every request goes through the budget engine, retries, pages and token requests included, and the quotas stay in sync with the headers the API returns.
- **A plan before the first call.** `tapyr plan` tells you how many calls a configuration spends against each quota, and how intervals stretch to fit: low-priority jobs slow down first, critical jobs keep their schedule.
- **Declarative providers.** Endpoints, auth, pagination, discovery and the mapping to metrics (CEL) are YAML. Supporting a new API needs no Go code.
- **A simulator for every provider.** It serves the same definitions with the real limits, fault scenarios and synthetic inventories: develop, test and demo without a vendor account.
- **Monitoring of monitoring.** Tapyr exposes its own metrics (calls, latencies, retries, 429s, quota left, projected exhaustion, job freshness), with a Grafana dashboard and alert rules.
- **Standard outputs.** Prometheus exposition, Prometheus remote write and OTLP, with source timestamps and staleness.
- **A read-only web UI** to see what runs: providers and quotas, the graph of accounts and jobs, the plan, outputs, and the live debug of a mapping.

<p align="center">
  <img src="docs/assets/screenshot-overview-light.png#gh-light-mode-only" width="900" alt="The overview of the Tapyr web UI: providers healthy, jobs succeeding, the tightest quota, the plan, the jobs of every provider and the live activity">
  <img src="docs/assets/screenshot-overview-dark.png#gh-dark-mode-only" width="900" alt="The overview of the Tapyr web UI: providers healthy, jobs succeeding, the tightest quota, the plan, the jobs of every provider and the live activity">
</p>

## How it works

<p align="center">
  <img src="docs/assets/how-it-works-light.svg#gh-light-mode-only" width="900" alt="A provider definition feeds the budget engine, which plans every call of the collector; tapyr plan projects those calls before you deploy. The collector calls the vendor API or the built-in simulator, the mapping turns the answers into metrics, and the outputs deliver them, with Tapyr's own metrics and a read-only web UI.">
  <img src="docs/assets/how-it-works-dark.svg#gh-dark-mode-only" width="900" alt="A provider definition feeds the budget engine, which plans every call of the collector; tapyr plan projects those calls before you deploy. The collector calls the vendor API or the built-in simulator, the mapping turns the answers into metrics, and the outputs deliver them, with Tapyr's own metrics and a read-only web UI.">
</p>

1. **Describe the API once.** A provider definition says where the endpoints are, how to page through them, how the JSON becomes metrics, and every limit the vendor enforces. YAML and CEL, no Go.
2. **The budget engine plans every call.** Rates, quotas and concurrency are tracked per scope, with a reserve kept for retries. Low-priority jobs stretch before anything gets refused, and `tapyr plan` shows the cost before you deploy.
3. **The collector fetches.** It follows the pagination, retries what fails, and honors `Retry-After` when the vendor pushes back. Point it at the built-in simulator to try everything without an account.
4. **The mapping turns JSON into metrics.** CEL expressions pick the items, compute the values and set the labels, so every vendor comes out as clean, consistent series.
5. **The outputs deliver.** Prometheus exposition and remote write, OTLP over HTTP and gRPC. Tapyr exports its own metrics too, so you see the quota run down before it bites.

The planner computes an effective interval for every job so that the configuration fits each quota, and replans while running with what is left in the window and what other clients consume. The scheduler runs the jobs, discovery jobs fan out one instance per inventory item, and no request leaves without a reservation from the budget engine. The specification in [`docs/spec/`](docs/spec/) describes every part.

## Built-in providers

<!-- begin provider table: generated from the provider packages by TestProviderTables (go test ./cmd/tapyr -run TestProviderTables -update), do not edit -->

| Package | API | Tier | Validation |
|---|---|---|---|
| [`aruba-central-classic`](providers/aruba-central-classic/README.md) | HPE Aruba Networking Central (classic platform) REST API | Official | Simulator only |
| [`example-sim`](providers/example-sim/README.md) | Vendor-neutral example API, served by tapyr sim, for development, tests and demos | Official | Simulator only |
| [`meraki-dashboard`](providers/meraki-dashboard/README.md) | Cisco Meraki Dashboard API v1 | Official | Real platform (Dashboard API v1, 2026-11-07) and simulator |
| [`teltonika-rms`](providers/teltonika-rms/README.md) | Teltonika Remote Management System (RMS) cloud API | Official | Real platform (RMS API v3-BETA, 2026-11) and simulator |
| [`teltonika-rutos`](providers/teltonika-rutos/README.md) | Local REST API of Teltonika routers running RutOS 07.06 or later | Official | Real platform (RutOS 07.24.5, 2026-11) and simulator |

<!-- end provider table -->

Each package README lists what it collects, the credentials it needs, and its limits with their sources. Packages validated against their simulator only are waiting for reports from real accounts: see [`CONTRIBUTING.md`](CONTRIBUTING.md). [`docs/providers.md`](docs/providers.md) explains how to write a package for another API, and [`docs/provider-ideas.md`](docs/provider-ideas.md) lists candidates.

## Quick start

The demo runs Tapyr against the simulators of the built-in providers, with VictoriaMetrics and Grafana. It needs Docker with Compose, and no vendor account.

```sh
git clone https://github.com/kentrow/tapyr.git
cd tapyr/examples/compose
docker compose up -d
```

Then open:

- Grafana: <http://127.0.0.1:3000>, with the `Tapyr / Overview` dashboard and one dashboard per provider;
- the Tapyr UI: <http://127.0.0.1:9790/ui/>;
- VictoriaMetrics: <http://127.0.0.1:8428/vmui/>.

The first samples arrive within a minute. [`examples/compose/README.md`](examples/compose/README.md) explains what runs and how to switch the simulators to their fault scenarios (`quota-tight`, `bad-day`). Without Docker, [Getting started](docs/getting-started.md) walks through a first configuration of your own with the binary alone: its plan, its run, and a quota that shrinks.

<p align="center">
  <img src="docs/assets/screenshot-graph-light.png#gh-light-mode-only" width="900" alt="The graph of the Tapyr web UI: providers, accounts, targets and the job groups of each account, wired to the outputs">
  <img src="docs/assets/screenshot-graph-dark.png#gh-dark-mode-only" width="900" alt="The graph of the Tapyr web UI: providers, accounts, targets and the job groups of each account, wired to the outputs">
</p>

## Install

Releases ship static binaries for Linux, macOS and Windows (amd64, arm64), and the multi-arch image `ghcr.io/kentrow/tapyr` (distroless, non-root):

```sh
docker run --rm ghcr.io/kentrow/tapyr:0.1.2 version
```

The binaries are on the [releases page](https://github.com/kentrow/tapyr/releases) with their SBOM. The checksums and the image are signed with cosign in keyless mode: [`docs/install.md`](docs/install.md) shows how to verify them, run the image, deploy with [systemd](examples/systemd/README.md) or on [Kubernetes](examples/kubernetes/README.md), and build from source.

## Use it with your own accounts

1. Pick a provider in [`providers/`](providers/): its README gives the credentials it needs and an example configuration.
2. Write the accounts, the jobs and an output (see [`examples/config`](examples/config), [`docs/outputs.md`](docs/outputs.md) and [`docs/spec/03-configuration.md`](docs/spec/03-configuration.md)). Secrets come from environment variables or files, never from the configuration files. In a container, set `settings.server.listen` to `0.0.0.0:9790`.

   ```yaml
   apiVersion: tapyr/v1alpha1
   kind: Account
   metadata: { name: campus }
   spec:
     provider: meraki-dashboard
     secrets: { apiKey: { env: TAPYR_MERAKI_API_KEY } }
   ---
   apiVersion: tapyr/v1alpha1
   kind: Job
   metadata: { name: device-availabilities }
   spec: { account: campus, template: meraki-device-availabilities }
   ---
   apiVersion: tapyr/v1alpha1
   kind: Output
   metadata: { name: prom }
   spec: { type: prometheus }
   ```

3. Check the configuration and what it spends before running it:

   ```sh
   tapyr validate --config ./config --check-secrets
   tapyr plan --config ./config
   tapyr run --config ./config        # metrics on :9790/metrics, the UI on :9790/ui/
   ```

| Command | What it does |
|---|---|
| `tapyr run` | Collect, with the UI, the status API, `/metrics` and `/metrics/self`; `SIGHUP` reloads the configuration |
| `tapyr validate` | Check the configuration, every error with its file, line and column |
| `tapyr plan` | Project the consumption of every quota and the effective interval of every job |
| `tapyr sim` | Serve the simulator of a provider, or of all of them (`--all`) |
| `tapyr record` | Record real responses, anonymized, to build fixtures |
| `tapyr provider list`, `tapyr provider lint` | List the provider packages, check one with its contract test |
| `tapyr schema` | Print the JSON Schemas of the configuration, for editors |

## Documentation

The documentation is published on [docs.tapyr.eu](https://docs.tapyr.eu): the development version, and from v0.1.1 one version per minor release. Its sources live in this repository:

| Guide | |
|---|---|
| [Getting started](docs/getting-started.md) | A first configuration against a simulator, its plan, its run, a shrinking quota |
| [Concepts](docs/concepts.md) | Providers, accounts, jobs, discovery, the budget engine and the planner |
| [Install](docs/install.md) | Binaries, image, systemd, Kubernetes, signature verification, build from source |
| [Outputs](docs/outputs.md) | Prometheus exposition, remote write, OTLP |
| [Operations](docs/operations.md) | State store, reloads, secrets, monitoring Tapyr, exposing the server, upgrades |
| [Troubleshooting](docs/troubleshooting.md) | From the symptom to the cause: configuration, auth, quotas, mappings, outputs |
| [Provider packages](docs/providers.md) | Writing, recording, linting and testing a package |
| [Alerts](docs/alerts.md) | Alert rules on the self-metrics, and what to do |
| [Configuration reference](docs/reference/README.md) | Every field of every kind, generated from the JSON Schemas |
| [Releasing](docs/releasing.md) | How a version is published |

The design lives in the [specification](docs/spec/) (vision, architecture, configuration, budget engine, collection, mapping, self-observability, simulator, CLI and UI, providers) and the [architecture decisions](docs/adr/). The [roadmap](docs/roadmap.md) lists what comes next, and the [changelog](CHANGELOG.md) what changed.

## Contributing

Contributions are welcome, provider packages first: read [`CONTRIBUTING.md`](CONTRIBUTING.md). The repository is set up for coding agents too: [`AGENTS.md`](AGENTS.md) and the skills of [`.agents/skills/`](.agents/skills/). Report vulnerabilities privately as described in [`SECURITY.md`](SECURITY.md). Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

## Status

Tapyr is before 1.0: the configuration format is `tapyr/v1alpha1` and may change between minor versions, every change listed in the [changelog](CHANGELOG.md). Versions follow [Semantic Versioning](https://semver.org/).

## License

Copyright 2026 The Tapyr Authors. Licensed under the [Apache License 2.0](LICENSE); the releases include the notices of the third-party software they contain ([`NOTICE`](NOTICE)).

<p align="center"><a href="https://tapyr.eu">tapyr.eu</a> · Open source, self-hosted, built in Europe</p>
