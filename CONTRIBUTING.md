# Contributing to Tapyr

Thank you for your interest in Tapyr. This guide explains how to propose a change and what a pull request needs to be merged. By taking part, you agree to follow the [code of conduct](CODE_OF_CONDUCT.md). Security issues are reported privately, see [`SECURITY.md`](SECURITY.md).

If you work with a coding agent, point it to [`AGENTS.md`](AGENTS.md): it holds the same rules, written for agents, and the skills of [`.agents/skills/`](.agents/skills/) cover the recurring contributions.

## What helps most

- **Provider packages** for metric-oriented APIs: YAML and CEL, no Go (see [Provider packages](#provider-packages)).
- **Validation against real platforms** of the packages that are only validated against their simulator (Aruba Central, Meraki): run them, report what differs from the fixtures.
- **Bug reports** with a reproduction, ideally against the simulator (`tapyr sim`), which needs no account.
- **Documentation**: what was unclear when you started.

## Before you start

- **Open an issue first** for a new feature, a new provider or a behavior change, and wait for an agreement before writing much code. Small fixes and documentation can go straight to a pull request.
- Read the scope in [`docs/spec/01-vision-and-scope.md`](docs/spec/01-vision-and-scope.md): items listed as out of scope are not taken without a discussion.
- The specifications in [`docs/spec/`](docs/spec/) describe how Tapyr works and why. **When a spec and the code disagree, the spec wins** until it is updated: a change of behavior updates the spec in the same pull request, and a significant design change comes with an ADR in [`docs/adr/`](docs/adr/).

## Development setup

You need Go 1.27, Node.js 22 or later (for the web UI), [golangci-lint](https://golangci-lint.run) v2, and optionally Docker (integration tests, alert rules, image).

```sh
git clone https://github.com/kentrow/tapyr.git
cd tapyr
make build          # the web UI, then ./bin/tapyr
make test           # unit tests with the race detector
make lint           # golangci-lint, repository checks, provider packages
make ui-check       # type-check, lint and test the web UI
```

`make help` lists every target. To try Tapyr without any account, run the [demo compose](examples/compose/README.md), or a simulator and the example configuration:

```sh
make sim PROVIDER=example-sim &                                  # the simulator on 127.0.0.1:8081
TAPYR_EXAMPLE_API_KEY=sim-api-key ./bin/tapyr run --config ./examples/config
```

## Making a change

- **Tests.** New behavior comes with tests; every bug fix with a regression test that fails without the fix. Tests never touch the network: they use `httptest` servers or the simulator, and an injected fake clock for anything time-dependent (`clock.Fake`). Tests against real platforms are opt-in (`make e2e`, behind the `e2e` build tag and environment variables). Code that parses input Tapyr does not control (configuration files, API responses) has fuzz targets: `make fuzz` runs them (`FUZZTIME=5m make fuzz` to dig longer), and an input that breaks one lands in `testdata/fuzz/` of its package, where it stays as a regression test.
- **Configuration types** changed: regenerate the JSON Schemas and the configuration reference with `make schema` and commit them. Their doc comments become the reference: write them for users.
- **Docs.** A user-visible change updates the spec, the examples or the provider README concerned. The documentation site docs.tapyr.eu is built from these files: use relative links between them, and add a new page to [`docs/site.yml`](docs/site.yml) (`make lint` checks both).
- **Changelog.** A user-visible change adds an entry under `## [Unreleased]` of [`CHANGELOG.md`](CHANGELOG.md), in the same commit, following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Write it for users.
- **Style.** English everywhere in the repository; no em dash character in docs or comments (`make lint` checks it); Go formatted with `make fmt`.
- **Dependencies** are added deliberately: say in the pull request why the standard library or an existing dependency is not enough. The binary stays static (no CGO).

## Commits

Commits follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/), checked by the CI:

```
feat(budget): sync rolling windows from the reset header
fix(collect): retry a page after a 503 within the run deadline
docs: explain the quota maths of the RMS package
```

Types (the set `scripts/check-commits.sh` accepts): `build`, `chore`, `ci`, `docs`, `feat`, `fix`, `perf`, `refactor`, `revert`, `style`, `test`; a scope is optional; `!` or a `BREAKING CHANGE:` footer marks a breaking change. Keep commits small, one concern each, and say why in the body when it is not obvious. Versions follow [Semantic Versioning](https://semver.org/); before 1.0, a breaking change bumps the minor version.

## Pull requests

- Fill in the template: what changes and why, how you tested it, the checklist. Write down only the checks you actually ran, with the command or a link to the run (a workflow can run on your fork through `workflow_dispatch`), and tick only what is true: reviewers rely on it and do not redo every check.
- The CI runs the commit check, lint, tests, builds for linux/amd64 and arm64, the web UI, a smoke test, the integration tests and the alert rules. All must pass.
- A maintainer reviews the change against the specs; expect questions on limits, labels and naming, which are hard to change later.
- Keep the pull request focused. Rebase on `main` rather than merging it into your branch.

## Provider packages

A provider package is a directory of YAML and CEL in [`providers/`](providers/): the API definition with its limits, job templates, a simulator with three scenarios, a contract test, a Grafana dashboard, examples and a README. Start with [`docs/providers.md`](docs/providers.md) and [`providers/AGENTS.md`](providers/AGENTS.md).

- **Every limit cites its source**: the vendor documentation (link), a measurement (date and method), or "assumed". Limits are what makes Tapyr trustworthy: an invented one makes `tapyr plan` wrong.
- **No real data** in fixtures: record real responses only with `tapyr record`, which anonymizes them, and review the result before committing (see [`docs/providers.md`](docs/providers.md#recording-real-responses)).
- **Never commit credentials**, even expired ones. Simulator credentials are obviously fake (`sim-...`).
- `tapyr provider lint providers/<name>` must pass.
- Say in the README and the pull request how the package was validated: against the simulator only, or against a real platform (which version, when).

## Contributions assisted by AI

Coding agents are welcome here, and the repository is set up for them ([`AGENTS.md`](AGENTS.md), skills). The rules are the same as for any contribution, with a few more:

- **You are the author.** You submit only what you have read and understood, and you can explain and defend every line in review. "The agent wrote it" is not an answer to a review question.
- **Run the checks yourself** before opening the pull request: the tests, the linters and the provider lint pass on your machine. Do not open a pull request to let the CI find what the agent got wrong.
- **Report what you ran, not what the agent says it ran.** Agent summaries claim checks that never happened (a pinned SHA "verified", a workflow "tested"). Confirm each one yourself before it goes into the description.
- **Say it.** Mention in the pull request description that an agent helped, which one, and for what (for example "the fixtures and the dashboard were drafted with an agent from the vendor documentation, reviewed by hand"). Do not add co-author or "generated by" trailers to commits: the CI refuses them, the description is the place.
- **Verify against the sources.** Agents invent plausible API fields, endpoints and limits. Every limit, endpoint and field of a provider package must be checked against the vendor documentation or a real response.
- **Protect data.** Do not give an agent real credentials or unanonymized responses unless you have checked where they go; never let it commit them.
- **Keep it reviewable.** A pull request should be small enough to review by a human. Large generated changes without a prior issue may be closed.

Maintainers may close pull requests that do not follow these rules, in particular unreviewed generated code.

## License

Tapyr is licensed under the [Apache License 2.0](LICENSE). By contributing, you agree that your contributions are licensed under the same license.
