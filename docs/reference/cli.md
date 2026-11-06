<!-- Generated from the commands by TestCLIReference (go test ./cmd/tapyr -run TestCLIReference -update). Do not edit. -->

# Command line

Tapyr polls metric-oriented vendor APIs within their rate limits and exposes or pushes the collected data as Prometheus or OTLP metrics.

Every command reads the configuration from `--config` (files or directories, repeatable) or `TAPYR_CONFIG`.

## Global flags

| Flag | Default | Description |
|---|---|---|
| `--config string` |  | configuration file or directory, repeatable (env TAPYR_CONFIG) |
| `--log-format string` |  | log format: json, text (env TAPYR_LOG_FORMAT) |
| `--log-level string` |  | log level: debug, info, warn, error (env TAPYR_LOG_LEVEL) |

## tapyr plan

Compute, offline, the effective interval of every job and the projected consumption of every quota scope over a full window. Estimates use the run history of the state store when it exists, else estimate.pagesPerRun. Exit code 3 when the plan is infeasible.

```text
tapyr plan [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--fanout string` |  | fan-out hint: &lt;parent&gt;=&lt;instances&gt;, the parent as named in forEach (its template, or its job with forEach.job), repeatable |
| `--output string` | `table` | output format: table or json |
| `--provider-path string` |  | directory of provider packages, repeatable, after the providerPaths of the configuration |

## tapyr provider lint

Validate a provider package (or every package of a directory): schema, CEL compilation, dashboards, and the contract test of tests/contract.yaml, run once against the package simulator at a fixed simulated time. The samples must equal tests/expected.prom; --update rewrites it instead. --catalog also requires the catalog metadata: display name, categories, maintainers, tier, validation and the source of every limit.

```text
tapyr provider lint <dir> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--catalog` |  | also require the catalog metadata (ADR 0011) |
| `--update` |  | write tests/expected.prom instead of comparing |

## tapyr provider list

List the built-in provider packages and, when a configuration is given, the packages of its providerPaths, in precedence order.

```text
tapyr provider list
```

## tapyr record

Run every instance of the selected jobs once, through the budget engine, and write the responses, anonymized, into --out: one JSON file per page, index.yaml and report.txt (the replaced paths, never the values). Parents of the selected fan-out jobs run too. Review report.txt before committing anything.

```text
tapyr record --job J [--job ...] --out DIR [--anonymize FILE] [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--anonymize string` |  | file of additional anonymization rules (replace, keep) |
| `--job string` |  | job to record, repeatable |
| `--out string` |  | directory receiving the recording |
| `--provider-path string` |  | directory of provider packages, repeatable, after the providerPaths of the configuration |

## tapyr run

Load and compile the configuration, then collect: run every job at its interval through the budget engine and expose the samples on /metrics. SIGHUP reloads the configuration; SIGINT and SIGTERM shut down gracefully.

```text
tapyr run
```

## tapyr schema

Print the JSON Schema accepting any document kind (tapyr.json), or the schema of one kind with --kind. With --out, write the schemas to a directory.

```text
tapyr schema [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--kind string` |  | kind to print or write (Provider, JobTemplate, Account, Job, Output, Settings, Simulation, Scenario) |
| `--out string` |  | directory to write the schemas to |

## tapyr sim

Serve the simulator of a provider package: its fixtures, with the auth, pagination and limits declared by the provider (429 with Retry-After and quota headers). --scenario overlays faults; --time-scale accelerates the simulated clock. --all serves every simulator from --listen-base, at the portOffset of its package (the next free offsets, in provider name order, for packages without one).

```text
tapyr sim [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--all` |  | serve every simulator, at its port offset from --listen-base |
| `--listen string` | `127.0.0.1:8081` | listen address |
| `--listen-base string` | `127.0.0.1:8081` | address of the first simulator with --all |
| `--provider string` |  | provider package to simulate |
| `--provider-path string` |  | directory of user provider packages, repeatable |
| `--scenario string` |  | scenario of the package (sim/scenarios), for example quota-tight; with --all, applied to the simulators that have it |
| `--seed uint64` | `42` | seed of the simulated randomness |
| `--time-scale float64` | `1` | simulated time runs this many times faster than real time |

## tapyr validate

Load the configuration, validate every document against its JSON Schema, check references and semantic rules, and report every error as file:line:column. Exit code 2 when the configuration is invalid.

```text
tapyr validate [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--check-secrets` |  | also check that every referenced secret (environment variable or file) is set and not empty |
| `--provider-path string` |  | directory of provider packages, repeatable, after the providerPaths of the configuration |

## tapyr version

Print the version, revision and Go version

```text
tapyr version
```

## Shell completion

`tapyr completion bash|zsh|fish|powershell` prints the completion script of a shell; `tapyr completion <shell> --help` explains how to load it.
