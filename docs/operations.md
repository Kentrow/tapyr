# Operations

What to know to run Tapyr for real: where it keeps its state, how to change its configuration, how to watch it and how to expose it safely. [Install](install.md) covers the binary, the image, the [systemd unit](../examples/systemd/README.md) and the [Kubernetes manifests](../examples/kubernetes/README.md).

## One instance per set of accounts

Run a single `tapyr run` for a given set of accounts. Two instances polling the same accounts do not coordinate: each one spends the whole quota it was given, and together they exceed it. On Kubernetes, keep one replica with the `Recreate` strategy, as the example manifests do.

When another tool must keep using the same credentials, give Tapyr its part with `budget.share` on the account (`0.5` for half of every quota). Tapyr also measures what others consume through the quota headers, when the provider sends them, and slows down accordingly; `budget.share` makes the split explicit.

## Configuration

`--config` takes files or directories, and can be repeated (`TAPYR_CONFIG` in the environment, paths separated like in `PATH`: `:` on Linux and macOS). In a directory, Tapyr reads the `*.yaml` and `*.yml` files of the directory and its subdirectories, sorted by path, and skips hidden files and directories; a file can hold several documents separated by `---`. Relative paths in a document (a secret file, a CA bundle, `providerPaths`, the state path) are resolved against the file that declares them.

Before applying a change, check it:

```sh
tapyr validate --config /etc/tapyr --check-secrets   # exit code 2 when invalid
tapyr plan --config /etc/tapyr                        # exit code 3 when infeasible
```

Keep the configuration in Git, and run both commands in the CI of that repository: `tapyr plan` turns "this change would exhaust the daily quota at 3 pm" into a failed check instead of an incident.

### Reload

`SIGHUP` reloads the configuration without a restart (`systemctl reload tapyr`, `docker kill -s HUP tapyr`, or a rollout on Kubernetes, where the example ConfigMap is hashed):

- The new configuration is loaded and validated first. When it is invalid, the reload is rejected, the error is logged (`configuration reload rejected`) and the running configuration keeps going; `tapyr_config_last_reload_success` drops to 0.
- Otherwise the running jobs finish, the consumption of every quota carries over, unchanged instances keep their schedule, and the series of removed jobs go stale.
- Changes to `server` (except `server.debug`, applied on reload), `state.path` and `log` need a restart; Tapyr logs a warning when a reload contains one.

### Secrets

Secrets are references, never values: `{ env: NAME }` for an environment variable, `{ file: /path }` for a file (a Docker or Kubernetes secret, a systemd credential). Tapyr redacts their values, and the tokens it obtains, from its logs, errors, status API and debug output. A secret shorter than 4 characters cannot be redacted reliably and is reported at startup.

## State store

The state store is a SQLite database, `settings.state.path` (default `./data/tapyr.db`, relative to the working directory; `/home/nonroot/data/tapyr.db` in the image). It holds:

| Data | Why it matters |
|---|---|
| Quota consumption per window | Without it, a restart forgets what was already spent today or this month, and the first requests may hit a quota the provider still counts |
| Tokens and rotated refresh tokens | Some providers rotate them at every refresh (Aruba Central): losing the store means bootstrapping new credentials |
| Inventories and run history | Discovery restarts from the cache instead of waiting for its parents; the planner estimates from real runs |
| Series and last timestamps sent | Stale markers and deduplication work across restarts |

Keep it on persistent storage (a volume, `StateDirectory=` with systemd), and back it up with the host. Do not delete it to "reset" Tapyr. It is written every 5 seconds and on shutdown; `tapyr_state_flush_failures_total` counts failed writes (full disk, permissions).

Set `TAPYR_STATE_KEY` to encrypt the tokens in the store (AES-GCM, with a key derived from its value). Without it, Tapyr logs a warning once and stores them in clear. Changing the key makes the stored tokens unreadable: they are requested again, except for rotated tokens that need a new bootstrap.

## Monitoring Tapyr

Tapyr exposes its own metrics on `/metrics/self`. Scrape them with `honor_labels: true`, since they carry the `job` label of the Tapyr job:

```yaml
scrape_configs:
  - job_name: tapyr
    metrics_path: /metrics/self
    honor_labels: true
    static_configs:
      - targets: ["tapyr:9790"]
```

Where nothing scrapes Tapyr, `settings.selfMetrics.outputs` pushes them through an output instead, every 30 seconds.

- **Dashboard.** `dashboards/tapyr-overview.json` (Grafana): health, quotas left and projected, job freshness and intervals, provider calls, outputs. Every provider package ships a dashboard of the data it collects as well.
- **Alerts.** [`examples/alerts/tapyr-alerts.yaml`](../examples/alerts/tapyr-alerts.yaml) has 14 rules for Prometheus or vmalert, explained in [Alerts](alerts.md): process down, stale jobs, open circuits, re-authentication required, quotas about to run out, unexpected 429s, outputs dropping samples, rejected reloads, state store failures.
- **Probes.** `/healthz` answers 503 when the internal loops are stuck (liveness); `/readyz` answers 503 until the configuration is loaded, the state store answers and every output has started (readiness). Neither is ever protected by basic auth.
- **Logs.** JSON on stderr by default, with `component`, `provider`, `account`, `job` and `run_id` fields; `--log-format text` (or `TAPYR_LOG_FORMAT`, or `settings.log.format`) for plain lines. Requests are logged at `debug` level only; repetitive warnings are logged at most once per minute.

## Exposing the server

Tapyr listens on `127.0.0.1:9790` by default. To reach it from another host or from outside a container, set `settings.server.listen: 0.0.0.0:9790`. The UI and the status API show your infrastructure (accounts, endpoints, inventories, errors): protect them before exposing them.

```yaml
apiVersion: tapyr/v1alpha1
kind: Settings
metadata:
  name: default
spec:
  server:
    listen: 0.0.0.0:9790
    basicAuth:
      username: { env: TAPYR_UI_USER }
      password: { env: TAPYR_UI_PASSWORD }
      protectMetrics: false     # true also protects /metrics and /metrics/self
```

- **Basic auth** protects `/api` and `/ui`, and the metrics with `protectMetrics`. Prometheus then needs the same credentials (`basic_auth` in the scrape configuration).
- **TLS.** A reverse proxy is the recommended way to terminate TLS. `settings.server.tls` (`certFile`, `keyFile`) serves HTTPS directly.
- **Debug endpoint.** `settings.server.debug.enabled` keeps the last raw responses of every job, redacted and truncated to 1 MiB, for the Live debug page and `/api/v1/debug/...`. It helps writing and fixing mappings; turn it off when you do not need it, since the payloads describe your devices.

## Upgrading

Releases follow [Semantic Versioning](https://semver.org/). Before 1.0, a minor version (0.2.0) may change the configuration format, with a new `apiVersion` and the migration in the [changelog](../CHANGELOG.md); a patch version (0.1.1) never does. To upgrade:

1. Read the changelog between your version and the new one.
2. Run `tapyr validate` and `tapyr plan` of the new version on your configuration.
3. Replace the binary or the image tag, keep the state store, restart.

Pin the image to a version (`ghcr.io/kentrow/tapyr:0.3.0`, or `0.3` for the patches of a minor version) rather than `latest`, and [verify its signature](install.md#verify-the-signatures).

## Capacity

- **Outputs.** Each push output has a queue of `capacity` samples. Size it for the backend outage you want to ride out: samples per minute across your jobs, times the minutes (see [Sizing the queue](outputs.md#sizing-the-queue)).
- **Concurrency.** `settings.collection.maxConcurrentRequests` (default 32) bounds the requests in flight across all providers; the concurrency limits of the providers apply on top of it.
- **Runaway discovery.** `forEach.maxInstances` (default 10000) caps the instances of a fan-out, and `settings.mapping` caps the items and samples of a run and the cost of CEL expressions.
