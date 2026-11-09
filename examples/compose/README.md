# Demo compose

This compose runs Tapyr against the simulators of the built-in providers, without any vendor account. Its services:

| Service | What it does | Address |
|---|---|---|
| `sim` | `tapyr sim --all`: one simulator per built-in provider, with its auth, pagination and limits (429 with `Retry-After` and quota headers) | inside the compose network only |
| `tapyr` | collects from the simulators ([`tapyr/config.yaml`](tapyr/config.yaml)) and pushes the samples to VictoriaMetrics by remote write | <http://127.0.0.1:9790/ui/> |
| `victoriametrics` | stores the samples and scrapes the self-metrics of Tapyr on `/metrics/self` | <http://127.0.0.1:8428/vmui/> |
| `grafana` | the `Tapyr / Overview` dashboard and the dashboard of every provider, anonymous read access | <http://127.0.0.1:3000> |

```sh
docker compose up -d
docker compose logs -f tapyr     # JSON logs
docker compose down -v           # stop and forget the data
```

The first samples arrive within a minute (the demo spreads the first runs over 1% of their interval instead of the default 10%). The ports listen on `127.0.0.1` only.

The simulators listen at 8081 plus the `portOffset` of their package (`sim/sim.yaml`): `aruba-central-classic` 8081, `example-sim` 8082, `meraki-dashboard` 8083, `teltonika-rms` 8084, `teltonika-rutos` 8085. The credentials in `compose.yaml` are the ones the simulators accept; none of them is a real secret.

## Try the fault scenarios

Every provider package has a `quota-tight` scenario (a quota smaller than the plan expects, or another client consuming it) and a `bad-day` scenario (errors, latency, an outage). Start the simulators with one of them and watch Tapyr adapt in the Budgets and Plan pages of the UI and in the Quotas row of the overview dashboard:

```sh
SIM_SCENARIO=quota-tight docker compose up -d sim
SIM_SCENARIO=bad-day docker compose up -d sim
docker compose up -d sim           # back to the default behavior
```

A scenario is a timeline that starts with the simulators. `quota-tight` acts at once. `bad-day` waits an hour before its first fault, then goes through added latency, errors on the main routes and a short outage, at times of their own in every package (`sim/scenarios/bad-day.yaml`): the outage of RutOS after 1 hour, the errors after 2 hours, the outages of Meraki and RMS after 4 hours. To see it sooner, run the simulators faster than real time:

```sh
SIM_SCENARIO=bad-day SIM_TIME_SCALE=10 docker compose up -d sim
```

The scenarios then play ten times faster: the outage of RutOS after 6 minutes, the errors after 12, the other outages after 24. The faults get shorter as well, and the quota windows of the simulators run faster too: well above 10, most faults end before the jobs of the demo run again, and Tapyr hardly sees them. Retries absorb most of the errors: look for them on the `Retries per minute, by reason` panel of the `Tapyr / Overview` dashboard rather than in the logs.

## With your own build

The compose uses the image of the release. To try a local build:

```sh
make docker                                   # from the root of the repository: tapyr:dev
TAPYR_IMAGE=tapyr:dev docker compose up -d
```
