# AGENTS.md - Tapyr

Instructions for coding agents (and the humans who drive them) working on this repository. Read this file fully before changing anything. Humans start with [`CONTRIBUTING.md`](CONTRIBUTING.md); everything here applies to them too.

Directories with rules of their own have their own `AGENTS.md`: [`providers/`](providers/AGENTS.md), [`internal/budget/`](internal/budget/AGENTS.md) and [`ui/`](ui/AGENTS.md). Read the one of every directory you touch. Reusable procedures are skills in [`.agents/skills/`](.agents/skills/) (see [Skills](#skills)).

## What Tapyr is

Tapyr is an open source, declarative **API-to-metrics gateway** written in Go. It polls metric-oriented vendor APIs (Aruba Central, Cisco Meraki, Teltonika RMS, Teltonika RutOS devices, and more later), respects each provider's rate limits as a first-class concern, and exposes or pushes the collected data in standard formats (Prometheus exposition, Prometheus remote write, OTLP).

The core idea: **API quota is a scarce resource that Tapyr plans and spends deliberately.** Rate limits (per second, per day, per month, concurrency, per organization, per IP...) are declared once in a provider definition, and the budget engine guarantees they are respected, retries included. Tapyr also observes itself: every API call, retry, quota consumption and job run is exposed as a metric.

Status: pre-1.0. The configuration format is `tapyr/v1alpha1`. Work follows [`docs/roadmap.md`](docs/roadmap.md); items listed as out of scope in [`01-vision-and-scope.md`](docs/spec/01-vision-and-scope.md) need an issue and an agreement before any code.

## Read order

1. [`docs/spec/01-vision-and-scope.md`](docs/spec/01-vision-and-scope.md) - what we build and what we do not build
2. [`docs/spec/02-architecture.md`](docs/spec/02-architecture.md) - components, data flow, repository layout
3. [`docs/spec/03-configuration.md`](docs/spec/03-configuration.md) - the declarative model (Provider, Account, Job, Output)
4. [`docs/spec/04-budget-engine.md`](docs/spec/04-budget-engine.md) - the heart of the project
5. [`docs/spec/05-collection.md`](docs/spec/05-collection.md) - auth, pagination, discovery, retries
6. [`docs/spec/06-mapping-and-outputs.md`](docs/spec/06-mapping-and-outputs.md) - CEL mapping, metric model, outputs
7. [`docs/spec/07-self-observability.md`](docs/spec/07-self-observability.md) - Tapyr's own metrics, health, logs
8. [`docs/spec/08-simulator.md`](docs/spec/08-simulator.md) - provider simulator and record mode
9. [`docs/spec/09-cli-api-ui.md`](docs/spec/09-cli-api-ui.md) - CLI, status API, read-only web UI
10. [`docs/spec/10-providers.md`](docs/spec/10-providers.md) - the built-in providers
11. [`docs/adr/`](docs/adr/) - architecture decisions and their rationale

Read the specs that concern your change, not only the code: they record decisions the code does not explain. **When a spec and the code disagree, the spec wins until it is explicitly updated.** If you think a spec is wrong, say so and propose a change to the spec before diverging in code. Significant design changes need a new ADR (copy [`docs/adr/0000-template.md`](docs/adr/0000-template.md)).

Your training data does not know this project, and may be out of date about its dependencies (Go 1.27, cel-go, React 19, Vite 8, `@xyflow/react` 12). Read the code and the specs instead of assuming.

## Non-negotiable design principles

- **Declarative first.** Users never write Go to support a new API. Vendor knowledge lives in provider packages (`providers/<name>/`) as YAML + CEL. Go code is reserved for generic mechanisms and for the rare auth flows that cannot be expressed declaratively (auth plugins).
- **No vendor-specific logic in the core.** If you are about to write `if provider == "meraki"` in `internal/`, stop: the behavior belongs in the provider definition or in a generic, configurable mechanism.
- **The budget engine is never bypassed.** Every outbound request to a provider, including retries, pagination pages, auth token requests and discovery calls, goes through a budget reservation.
- **Same definition, two consumers.** A provider definition drives both the real collector and the simulator. Limits, pagination and auth are declared once.
- **Read-only towards providers.** Tapyr never sends a write operation to a provider API.
- **Read-only UI.** The web UI never modifies configuration. Configuration is files, versioned in Git.
- **Standard outputs only.** Prometheus exposition, Prometheus remote write, OTLP. No proprietary output format.

## Engineering rules

- **Inject a clock everywhere.** All time-dependent code (budget windows, scheduling, backoff, token expiry, staleness) takes a `clock.Clock`. Tests use `clock.Fake`. Daily and monthly quota logic must be testable in milliseconds.
- **No real network in tests.** Use `httptest` servers or the built-in simulator. Real-provider tests are opt-in, behind the `e2e` build tag and environment variables.
- **Context everywhere.** Every blocking call accepts a `context.Context` and honors cancellation.
- **No global mutable state.** Wire dependencies explicitly from `cmd/tapyr`.
- **Secrets never appear** in logs, errors, the status API, the UI, debug dumps or recorded fixtures. Redaction is tested.
- **Cardinality discipline.** Self-metrics use templated endpoint paths (`/organizations/{organizationId}/networks`), never resolved URLs. Never put IDs, serials or IPs in self-metric labels.
- **Errors** are wrapped with context (`fmt.Errorf("...: %w", err)`) and classified (retryable or not) at the collection boundary.
- **Logging** uses `log/slog` with structured fields (`component`, `provider`, `account`, `job`, `run_id`).
- **Tests** are table-driven where possible. Every bug fix comes with a regression test. The budget engine and the planner keep a coverage of 90% or more.
- **Dependencies** are added deliberately: prefer the standard library and well-maintained, widely used modules, say why in the pull request. No CGO: the binary stays static (`modernc.org/sqlite`, not `mattn/go-sqlite3`).
- **Do not use the OpenTelemetry metrics SDK** to export collected data (it aggregates and cannot carry source timestamps, ADR 0008). It may be used for Tapyr's own telemetry only.
- **Do not hardcode rate limits in Go.** They belong to provider definitions.

## Commands

| Command | What it does |
|---|---|
| `make build` | The web UI, then the static binary in `./bin/tapyr` (`make build-go`: the binary alone) |
| `make test` | Unit tests with the race detector |
| `make lint` | golangci-lint, the em dash check, the documentation links (`make docs-check`) and `tapyr provider lint` of the built-in packages |
| `make schema` / `make schema-check` | Regenerate / check the JSON Schemas in `./schemas` and the configuration reference in `./docs/reference` from the Go types |
| `make tidy-check` | `go.mod` and `go.sum` are tidy |
| `make ui-check` | Type-check, lint and test the web UI (`make ui`: build it, `make ui-dev`: hot reload) |
| `make fuzz` | Every fuzz target for `FUZZTIME` (default 30s): configuration parsing, pagination, mappings, anonymizer |
| `make providers-update` | Rewrite `tests/expected.prom` of the built-in packages after a reviewed change |
| `make sim PROVIDER=name` | Run the simulator of a provider |
| `make smoke` | The built binary against the simulator |
| `make integration` | The outputs against VictoriaMetrics, the OpenTelemetry Collector and Prometheus (Docker) |
| `make helm-check` / `make helm-e2e` | Lint and check the Helm chart / install it on a kind cluster against a simulator (Docker, helm, kubectl) |
| `make packages-e2e` | The deb and rpm packages of `dist/` (from a GoReleaser snapshot) in Debian and Fedora containers with systemd (Docker) |
| `make alerts-check` | promtool checks and unit tests of the example alert rules (Docker) |
| `make e2e` | Opt-in tests against real platforms (credentials in environment variables) |
| `make docker` | The image `tapyr:dev` from source |

Single packages: `go test -race ./internal/budget/...`, `go run ./cmd/tapyr provider lint providers/<name>`. Long acceptance tests (24 simulated hours) are skipped with `go test -short`.

Never `go build ./cmd/tapyr` at the root of the repository without `-o`: it writes a `tapyr` binary there (ignored by Git, but `git add -A` habits have committed one before). Use `make build` or `go build -o bin/tapyr ./cmd/tapyr`.

## Definition of done

- The code compiles; `make lint` and `make test` pass (`make ui-check` for UI changes).
- New behavior is covered by tests, with the fake clock and the simulator where relevant.
- JSON Schemas and configuration reference regenerated (`make schema`) when configuration types changed.
- Docs updated when user-visible behavior changed: the spec, the examples, the provider README. A new Markdown file in `docs/`, `examples/` or `providers/` is added to [`docs/site.yml`](docs/site.yml), published or not.
- A `CHANGELOG.md` entry under `## [Unreleased]` for every user-visible change, in the same commit ([Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/); released sections are never edited).
- No secret, serial, MAC, IP or other identifier from a real environment anywhere in the diff.

The `check-change` skill walks through this list.

## Conventions

- English for code, comments, docs, commit messages and provider definitions.
- Do not use the em dash character in docs or comments: use a hyphen `-` or rephrase (`make lint` checks it).
- Documentation is written in this repository and published on docs.tapyr.eu ([ADR 0010](docs/adr/0010-documentation-site.md)). Links between files are relative (never `/path` or a GitHub URL to a file of the repository), so they work on GitHub and on the site; `make docs-check` checks them, anchors included.
- Go module path: `github.com/kentrow/tapyr`. Config API version: `tapyr/v1alpha1`; a breaking change of the format bumps it.
- Self-metric prefix `tapyr_`; environment variable prefix `TAPYR_`.
- Commits: [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/), with the types listed in [`CONTRIBUTING.md`](CONTRIBUTING.md#commits), optional scope (`feat(config): ...`), `!` or a `BREAKING CHANGE:` footer for breaking changes. Small commits, one concern each. CI checks the messages.
- No AI co-author or "generated by" trailer in commits. Say in the pull request description that an agent helped, and how (see [`CONTRIBUTING.md`](CONTRIBUTING.md)).
- Versioning: [SemVer 2.0.0](https://semver.org/); before v1.0.0, breaking changes bump the minor version.

## Safety rules for agents

- **Real credentials** are never written to a file of the repository, printed, or passed on a command line that gets logged. E2E credentials come from the environment of the person running `make e2e`; load them only into the command that needs them, and never read or echo them.
- **Recordings** (`tapyr record`) are anonymized before they enter the repository, and reviewed: free-text fields can still hold identifying data. The `record-fixtures` skill has the procedure.
- **Real platforms** are touched only when the person asks for it: e2e tests spend real quota and can lock accounts (rejected logins). Prefer the simulator.
- **Expected samples** (`tests/expected.prom`) are a contract: rewrite them with `--update` only after reading the diff and understanding why every value changed, and say why in the commit.
- **Generated files** are regenerated, never edited by hand: `schemas/` and `docs/reference/` (`make schema`, from the doc comments of the configuration types; `docs/reference/cli.md` with `go test ./cmd/tapyr -run TestCLIReference -update`, from the commands), the provider tables of the README and `docs/providers.md` (`go test ./cmd/tapyr -run TestProviderTables -update`, from the catalog metadata of the packages), `ui/dist/app/` (`make ui`), the README logo and diagrams `docs/assets/logo-*.svg`, `docs/assets/overview-*.svg` and `docs/assets/how-it-works-*.svg` (`scripts/diagram.py`). `ui/dist/app/` is never committed; `ui/dist/README.md` is the placeholder `go:embed` needs.
- **Irreversible or shared actions** (pushing to `main`, force-pushing, tagging, publishing images, changing repository settings) are never done by an agent on its own initiative.

## Skills

Procedures for recurring tasks, in [`.agents/skills/`](.agents/skills/) (the [Agent Skills](https://agentskills.io) format, read by Codex; `.claude/skills/` points Claude Code to the same files). Agents without skill support can read them as plain Markdown.

| Skill | Use it to |
|---|---|
| [`new-provider`](.agents/skills/new-provider/SKILL.md) | Write a provider package from the documentation of an API |
| [`record-fixtures`](.agents/skills/record-fixtures/SKILL.md) | Record real responses, anonymized, for a simulator or a bug report |
| [`check-change`](.agents/skills/check-change/SKILL.md) | Check a change against the definition of done before a commit or a pull request |
| [`add-self-metric`](.agents/skills/add-self-metric/SKILL.md) | Add or change a self-metric with its documentation, dashboard and alerts |
| [`investigate-quota`](.agents/skills/investigate-quota/SKILL.md) | Explain a 429, an exhausted quota or an infeasible plan |
