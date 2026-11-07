# Troubleshooting

Start from the symptom. Most answers come from four places, all read-only and without secrets:

| Where | What it tells |
|---|---|
| `tapyr validate --config <paths> --check-secrets` | Every configuration error, with its file, line and column |
| `tapyr plan --config <paths>` | What every job will spend of every quota, and at which interval it will really run |
| The web UI (`/ui/`) or the status API (`/api/v1/...`) | Jobs and their last runs with their errors, budgets, the plan, outputs, accounts |
| The logs and `/metrics/self` | Decisions as they happen, and counters over time |

Examples below use the default address `127.0.0.1:9790`.

## The configuration is rejected

`tapyr validate` and `tapyr run` print every problem as `file:line:column: path: message`, and exit with code 2:

```text
config.yaml:8:5: spec.secrets.apiKey: environment variable TAPYR_EXAMPLE_API_KEY is not set
configuration is invalid: 1 error
```

- **Unknown field, wrong type.** Check the field in the [configuration reference](reference/README.md). Editors catch these as you type with the JSON Schema (see the reference for the `yaml-language-server` line).
- **Unknown template or provider.** `tapyr provider list` shows the packages Tapyr knows; user packages need `settings.providerPaths`, and each package README lists its templates.
- **A secret is not set.** `--check-secrets` checks that every referenced environment variable or file exists and is not empty. Without it, `validate` checks the configuration only, which suits a CI without the secrets.
- **Nothing is collected and no error.** `configuration is valid: ... 0 accounts, 0 jobs` means the paths contain no document: check `--config` (files must end in `.yaml` or `.yml`, hidden files are skipped).

## Tapyr does not start

- `bind: address already in use`: another process listens on the address of `settings.server.listen` (default `127.0.0.1:9790`), often another Tapyr. Stop it or change the address.
- The state store cannot be opened: the directory of `settings.state.path` must exist or be creatable, and be writable by the user running Tapyr (uid 65532 in the image).
- `GET /readyz` answers 503 with its `reasons` while something is not ready: the configuration, the state store or an output.

## No data yet

- **First runs are spread.** Jobs start at a random point within the first 10% of their interval (`settings.collection.jitter`), so that a restart does not fire every request at once: a job polled every hour may wait up to 6 minutes for its first run.
- **Discovery comes first.** A fan-out job waits for the first run of its parents (organizations, then networks...). A newly discovered instance runs first within the jitter of its interval, then at its own point of the interval, so that a thousand networks never fire at once: with the default jitter, each level of the chain adds at most a tenth of its interval. After that, the inventory is cached and restarts no longer wait. The Graph page of the UI shows the chain.
- **No output.** `/metrics` serves the samples only with a `prometheus` output; push outputs deliver to their backend (see [their troubleshooting](#samples-do-not-reach-the-backend)).
- **The job fails.** Open it in the UI, or `GET /api/v1/jobs/<name>/runs`: every run has its result and its error.

## Authentication fails (401, 403)

The runs fail with an authentication error, and `tapyr_auth_token_requests_total{result="failed"}` grows for token-based providers.

- Check the secret the account refers to: the right variable, no trailing newline in a file, the permissions the provider README asks for (read-only API keys are enough: Tapyr never writes).
- After a rejected token or login request, Tapyr waits before trying again: 1 minute, then 2, 4... up to 1 hour, so that a wrong password does not lock the account. Fixing the secret and reloading (`SIGHUP`) ends the wait at once.
- `tapyr_auth_reauth_required` at 1 (and the `TapyrReauthRequired` alert) means the credentials can no longer be renewed on their own, for example an expired Aruba Central refresh token: create new ones, update the secret and reload. The provider README explains how.

## Quota problems

The budget engine is meant to make these impossible: when they happen, one of its inputs is wrong. The Budgets page of the UI and `GET /api/v1/budgets` show every limit and scope: what Tapyr used, what is left as Tapyr counts it and as the provider reports it, the external usage and the projection.

| Symptom | Likely cause | What to do |
|---|---|---|
| `tapyr_budget_unexpected_429_total` grows | The provider enforces a limit the definition does not declare, declares too high, or scopes differently (per IP, per organization) | Tapyr backs off on its own; report it on the provider package with the endpoint and the response headers (the debug endpoint shows them, redacted) |
| `tapyr_budget_external_usage` high, the quota runs out early | Another client uses the same quota: the vendor UI, a script, another integration | Give Tapyr its own credentials, or a `budget.share` |
| `tapyr_budget_limit_mismatch` at 1 | The provider announces another maximum than the definition (another plan, another account size) | Tapyr follows the headers; the definition should use `max: auto` |
| Consumption above the plan | More fan-out instances or pages than estimated, many retries | Compare `tapyr plan` with the runs (requests, pages, retries per run) |
| Jobs run slower than their interval | The quota is too small for the desired intervals: the planner stretched them | See below |
| `tapyr plan` exits with code 3, `TapyrPlanInfeasible` | `critical` jobs alone exceed a quota | Raise their intervals, lower some to `high`, reduce the fan-out |
| Requests refused with `quota_exhausted` right after a restart | The state store was lost (no volume): Tapyr restarted from 0 while the provider still counted | Keep the state store on persistent storage ([Operations](operations.md#state-store)) |

### Jobs run less often than configured

The planner gives every job an **effective interval** that fits the quotas, and stretches the lower priorities first ([Concepts](concepts.md#planner)). It is working as intended; to see why, read the plan: `tapyr plan`, the Plan page of the UI, or `GET /api/v1/plan`. Each stretched job says by how much, why, and which quota constrains it:

```text
job network-devices  desired 5m  effective 1h  24 runs x 1 page = 25  (stretched x12.00, priority normal, quota example-sim/daily account=demo)
```

To change the outcome: raise the priority of what matters (and lower the rest), raise intervals of jobs that can wait, filter the fan-out (`forEach.filter`), or get a larger quota. The log line `effective interval changed` records every change with its reason.

Runs can also start late without any stretch when they wait for rate or concurrency limits (`TapyrJobLagging`): `tapyr_budget_wait_seconds` shows the waits by limit.

## Metrics are wrong or missing

- **Mapping errors.** `tapyr_mapping_errors_total{job, metric}` counts the samples a mapping could not produce, for example a field the API no longer sends. Enable the debug endpoint (`settings.server.debug.enabled: true`) and open the Live debug page: the last raw response of the job, side by side with the samples it produced.
- **Series disappear.** A series absent from the last successful run goes stale at once (a device removed from the API). After failed runs, the series stay until `staleAfter`, 3 times the effective interval by default, then go stale too.
- **Labels collide.** A label set by the mapping and by the account, target or job takes the value of the job, then target, then account. `settings.mapping.labelConflict: error` turns collisions into run errors instead.
- **The self-metrics have `exported_job` instead of `job`.** The scrape of `/metrics/self` needs `honor_labels: true` ([Operations](operations.md#monitoring-tapyr)).

## Samples do not reach the backend

`GET /api/v1/outputs` and the Outputs page show the queue and the last results of every push output.

- `tapyr_output_send_failures_total{class="rejected"}` with `401` or `403`: the credentials of the output.
- `class="out_of_order"`: the backend refuses samples older than the ones it has. Prometheus needs out-of-order ingestion for source timestamps ([Outputs](outputs.md#out-of-order-samples)); otherwise another writer sends the same series.
- `tapyr_output_samples_dropped_total{reason="queue_full"}`: the backend is slower than the collection, or was down longer than the queue lasts ([Sizing the queue](outputs.md#sizing-the-queue)).

## A reload changed nothing

- `configuration reload rejected: the running configuration is kept` in the logs, and `tapyr_config_last_reload_success` at 0: the new configuration is invalid; `tapyr validate` shows why.
- `settings changed that take effect after a restart`: `server`, `state.path` and `log` need a restart.

## Asking for help

Open an [issue](https://github.com/Kentrow/tapyr/issues/new/choose) with:

- `tapyr version`, and how Tapyr runs (binary, image, Kubernetes);
- the configuration, secrets removed, and the provider package involved;
- the relevant log lines, `tapyr plan` output and status API responses. Tapyr redacts secrets from all of them, but read them before pasting: they show your inventory;
- best of all, a reproduction against the simulator (`tapyr sim --provider <name> --scenario <scenario>`), or real responses recorded with [`tapyr record`](providers.md#recording-real-responses), which anonymizes them.

Report vulnerabilities privately, as explained in the [security policy](../SECURITY.md), never in a public issue.
