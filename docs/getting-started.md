# Getting started

In about ten minutes, with the `tapyr` binary alone, you will collect metrics from a simulated API, see how Tapyr plans the quota before spending it, and watch it adapt when the quota shrinks. No vendor account, no Docker: every provider package comes with a simulator built into the binary.

For the full demo with VictoriaMetrics and Grafana dashboards, see the [demo compose](../examples/compose/README.md) instead.

## 1. Install

Download the archive of your platform from the [releases page](https://github.com/kentrow/tapyr/releases), or see [Install](install.md) for the image and the signature checks. Then:

```sh
tapyr version
```

## 2. Write a configuration

Start the simulator of `example-sim`, the example provider, in a terminal of its own. It serves a small API on `127.0.0.1:8081` with 3 sites, their networks and 5 devices, behind an API key, with paginated answers and a daily quota of 5000 calls:

```sh
tapyr sim --provider example-sim
```

In another terminal, create a directory and a file `config.yaml` in it:

```yaml
apiVersion: tapyr/v1alpha1
kind: Account
metadata:
  name: demo
spec:
  provider: example-sim
  secrets:
    apiKey: { env: TAPYR_EXAMPLE_API_KEY }
  labels:
    environment: lab
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata:
  name: devices
spec:
  account: demo
  template: example-sim-devices
  interval: 1m
  priority: critical
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata:
  name: network-devices
spec:
  account: demo
  template: example-sim-network-devices
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata:
  name: prometheus
spec:
  type: prometheus
```

- The **account** connects to the provider. Its secret is a reference to an environment variable, never the value itself, and its labels are added to every sample.
- The **jobs** use templates of the provider package: `devices` lists the devices every minute, at the `critical` priority. `network-devices` needs discovery: it runs once per network, and the networks are listed per site. You declare only the job you want; Tapyr runs the sites and the networks as implicit parent jobs.
- The **output** serves the samples on `/metrics`, for Prometheus to scrape.

[Concepts](concepts.md) explains each piece.

## 3. Check what it will spend

`validate` checks the configuration; `--check-secrets` also checks that the secrets are there:

```console
$ tapyr validate --config . --check-secrets
config.yaml:8:5: spec.secrets.apiKey: environment variable TAPYR_EXAMPLE_API_KEY is not set
configuration is invalid: 1 error

$ export TAPYR_EXAMPLE_API_KEY=sim-api-key
$ tapyr validate --config . --check-secrets
configuration is valid: 5 providers, 21 job templates, 1 account, 2 jobs, 1 output
```

`plan` projects the calls of every job against every quota, before a single request. Discovery has not run yet, so tell it how many instances to expect: 3 sites, and 5 networks in all.

```console
$ tapyr plan --config . --fanout example-sim-sites=3 --fanout example-sim-networks=5
QUOTA SCOPE                      WINDOW       BUDGET         PROJECTED                   RESERVE  STATUS
example-sim/daily                24h          4500           4679                        500      INFEASIBLE (104.0%)
  account=demo
  job devices                    desired 1m   effective 1m   1440 runs x 3 pages = 4536
  job network-devices            desired 5m   effective 1h   24 runs x 5 x 1 page = 126  (stretched x12.00, priority normal, quota example-sim/daily account=demo)
  job demo.example-sim-networks  desired 30m  effective 6h   4 runs x 3 x 1 page = 13    (stretched x12.00, priority low, quota example-sim/daily account=demo)
  job demo.example-sim-sites     desired 1h   effective 12h  2 runs x 2 pages = 4        (stretched x12.00, priority low, quota example-sim/daily account=demo)

The plan is infeasible: critical jobs alone exceed a quota, or stretching every other job to the maximum is not enough.
```

Read it from the top. The daily quota is 5000 calls; 500 are kept in reserve for retries, which leaves a budget of 4500. Listing 5 devices takes 3 pages, so polling them every minute costs 4320 calls a day, 4536 with the 5% the planner adds for retries: more than the budget on its own. The planner stretched every other job 12 times, the most it allows, and it is still not enough. A critical job is never slowed down, so the plan is infeasible, and `tapyr plan` exits with code 3: a CI would have caught it before deployment.

Poll the devices every 2 minutes instead (`interval: 2m` in `config.yaml`) and plan again:

```console
$ tapyr plan --config . --fanout example-sim-sites=3 --fanout example-sim-networks=5
QUOTA SCOPE                      WINDOW       BUDGET         PROJECTED                     RESERVE  STATUS
example-sim/daily                24h          4500           3982                          500      ok (88.5%)
  account=demo
  job devices                    desired 2m   effective 2m   720 runs x 3 pages = 2268
  job network-devices            desired 5m   effective 5m   288 runs x 5 x 1 page = 1512
  job demo.example-sim-networks  desired 30m  effective 30m  48 runs x 3 x 1 page = 151
  job demo.example-sim-sites     desired 1h   effective 1h   24 runs x 2 pages = 50
```

Every job now runs at the interval it asked for, within 89% of the budget.

## 4. Run it

The log lines below leave out their time stamp (`ts=...`).

```console
$ tapyr run --config . --log-format text
level=INFO msg="collector started" component=run address=127.0.0.1:9790 instances=2 discovered_instances=0 outputs=1 ledger_entries=0
level=INFO msg="run finished" component=run provider=example-sim account=demo job=devices instance=devices run_id=000001 requests=3 retries=0 pages=3 items=5 samples=16 deduplicated=0 mapping_errors=0 truncated=false
```

The first runs are spread over the first 10% of their interval, so that a restart never fires every request at once: the devices come within seconds, the sites within 6 minutes. The instances that discovery finds run first within the same tenth of their interval: the networks of the 3 sites come within 3 minutes after the sites, and the devices of each network within 30 seconds after its network, so everything is there within about 10 minutes (under 5 in a test run). After their first run, the instances of a fan-out run at their own point of the interval, so that a thousand networks never fire at once. The Graph page of the UI shows the instances as they appear. A restart does not wait again: the inventory is cached. Tapyr keeps its state (quota consumption, inventories, run history) in `./data/tapyr.db`, so that a restart remembers what was already spent today.

```console
$ curl -s 127.0.0.1:9790/metrics | grep '^example_device_up'
example_device_up{environment="lab",serial="SIM-0001",site_id="site-a"} 1
example_device_up{environment="lab",serial="SIM-0002",site_id="site-a"} 0
example_device_up{environment="lab",serial="SIM-0003",site_id="site-b"} 1
example_device_up{environment="lab",serial="SIM-0004",site_id="site-b"} 1
```

## 5. Look around

Open the web UI on <http://127.0.0.1:9790/ui/>: the overview, the graph of the provider, account, jobs and output, every job with its runs, the budgets with their consumption over the day, and the plan. It is read-only: the configuration stays in your files.

Tapyr reports on itself on `/metrics/self`, and in a JSON status API under `/api/v1`:

```console
$ curl -s 127.0.0.1:9790/metrics/self | grep -E '^tapyr_budget_(limit|used|remaining)\{'
tapyr_budget_limit{limit="daily",provider="example-sim",scope="account=demo"} 5000
tapyr_budget_remaining{limit="daily",provider="example-sim",scope="account=demo"} 4997
tapyr_budget_used{limit="daily",provider="example-sim",scope="account=demo"} 3

$ curl -s 127.0.0.1:9790/api/v1/jobs        # jobs, intervals, instances, last runs
$ curl -s 127.0.0.1:9790/api/v1/budgets     # every limit and scope
```

## 6. Squeeze the quota

Leave Tapyr running. Stop the simulator (`Ctrl-C`) and start it again with its `quota-tight` scenario, where the daily quota is only 600 calls:

```sh
tapyr sim --provider example-sim --scenario quota-tight
```

The provider declares its daily quota as `max: auto`: Tapyr takes the real maximum from the quota headers of every response. At the next run of `devices`, it learns that the quota is now 600, and the planner replans at once, spreading what is left of the quota over what is left of the day until the reset at midnight UTC. Until about 18:00 UTC, there is too much day left for 600 calls, and you see this:

```text
level=INFO msg="effective interval changed" component=planner provider=example-sim account=demo job=demo.example-sim-sites desired=1h0m0s previous=1h0m0s effective=12h0m0s reason="quota example-sim/daily account=demo"
level=INFO msg="effective interval changed" component=planner provider=example-sim account=demo job=demo.example-sim-networks desired=30m0s previous=30m0s effective=6h0m0s reason="quota example-sim/daily account=demo"
level=WARN msg="plan infeasible: critical jobs alone exceed the remaining quota" component=planner limit=example-sim/daily scope="account=demo" budget=523 projected=1056.3525101437106
```

Later in the day, what is left of the 600 calls is enough for the few hours before the reset: Tapyr learns the new limit (`tapyr_budget_limit` drops to 600, the Budgets page shows it) and keeps every interval, which is the right decision. The rest of this step needs more than about 6 hours before midnight UTC.

The low-priority discovery jobs are stretched 12 times. `devices` keeps running every 2 minutes, because a critical job is never slowed down: at this pace, it would spend the rest of the quota before the daily reset, and every request after that would be refused until midnight UTC. The Overview page of the UI shows the warning, and the Budgets page the new limit.

Whether polling the devices every 2 minutes is worth the quota is your decision, not Tapyr's: say so with the priority. Set `priority: high` on `devices` in `config.yaml`, and reload the configuration without stopping Tapyr:

```sh
kill -HUP "$(pgrep -f 'tapyr run')"
```

(`pgrep` comes with `procps`, missing from minimal systems: `kill -HUP` with the process ID of `tapyr run` does the same.)

```text
level=INFO msg="effective interval changed" component=planner provider=example-sim account=demo job=devices desired=2m0s previous=2m0s effective=4m6s reason="quota example-sim/daily account=demo"
level=INFO msg="configuration reloaded" component=run instances=10 discovered_instances=8 retired_instances=0 outputs=1
```

The plan is feasible again: `devices` now runs every 4 minutes or so, which fits what is left of the quota until the reset. The exact figures depend on the time left before midnight UTC, since the planner replans from the remaining budget and the remaining time. When the quota resets, or when you restart the simulator without the scenario, the intervals come back to the ones you asked for.

Stop Tapyr and the simulator with `Ctrl-C` when you are done; `./data` holds the state of this guide and can be deleted.

## 7. Next steps

- **A real provider.** Pick a package among the [providers](providers.md#built-in-packages): its README lists the credentials it needs (read-only keys are enough, Tapyr never writes), what it collects and its limits. Replace the account of this guide with one for that provider, run `tapyr validate --check-secrets` and `tapyr plan`, then `tapyr run`.
- **A time series database.** Scrape `/metrics` with Prometheus, or push with a `remote_write` or `otlp` output: see [Outputs](outputs.md).
- **Dashboards and alerts.** Every package ships a Grafana dashboard, and Tapyr ships one of its own with [alert rules](alerts.md).
- **Deploy.** [Install](install.md) covers the image, systemd and Kubernetes; [Operations](operations.md) covers the state store, reloads, monitoring and security.
- **Every field.** The [configuration reference](reference/README.md).
