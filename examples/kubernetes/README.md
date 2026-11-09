# Kubernetes

With Helm, use the [chart](../../charts/tapyr/README.md). Otherwise, plain manifests, applied with Kustomize (`kubectl apply -k`): a Deployment of one replica, its Service, a volume for the state store and the configuration as a ConfigMap.

| File | What it is |
|---|---|
| [`kustomization.yaml`](kustomization.yaml) | Namespace `tapyr`, image tag, the ConfigMap generated from `config.yaml` |
| [`config.yaml`](config.yaml) | The configuration of Tapyr, mounted in `/etc/tapyr` |
| [`deployment.yaml`](deployment.yaml) | One replica, non-root, read-only root file system, probes on `/readyz` and `/healthz` |
| [`service.yaml`](service.yaml) | Port 9790: `/metrics`, `/metrics/self`, the UI and the status API |
| [`pvc.yaml`](pvc.yaml) | 1 GiB for the state store |

## Install

1. Write your accounts, jobs and output in `config.yaml` (the README of each provider package has an example), and check it:

   ```sh
   tapyr validate --config config.yaml
   ```

2. Create the namespace and the Secret `tapyr` with the environment variables the configuration refers to (`{ env: NAME }`) and `TAPYR_STATE_KEY`, which encrypts the tokens kept in the state store. From a file of `NAME=value` lines, kept out of Git:

   ```sh
   kubectl create namespace tapyr
   kubectl -n tapyr create secret generic tapyr --from-env-file=tapyr.env
   ```

3. Apply:

   ```sh
   kubectl apply -k .
   kubectl -n tapyr rollout status deploy/tapyr
   kubectl -n tapyr port-forward svc/tapyr 9790:9790    # then http://127.0.0.1:9790/ui/
   ```

## Operate

- **Configuration changes.** The name of the ConfigMap carries a hash of `config.yaml`: edit the file, apply again, and the Deployment restarts with the new configuration. A restart keeps the state store, so nothing is spent twice.
- **One replica.** Never scale the Deployment: a second instance would spend the same quotas without coordinating. The `Recreate` strategy stops the old pod before the new one opens the state store.
- **State.** Keep the volume: it remembers what was already spent of the daily and monthly quotas, and the rotated tokens (every refresh of an Aruba Central token returns a new refresh token, and the previous one is no longer valid).
- **Resources.** About 75 MiB of memory for 22,000 series (10,000 devices): adjust the limit to the size of your inventories.
- **Logs.** `kubectl -n tapyr logs deploy/tapyr`, in JSON.

## Scrape

With the Prometheus Operator, a ServiceMonitor for the collected samples and the self-metrics:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: tapyr
  namespace: tapyr
spec:
  selector:
    matchLabels: { app.kubernetes.io/name: tapyr }
  endpoints:
    - port: http
      path: /metrics
      honorTimestamps: true
    - port: http
      path: /metrics/self
```

To push instead of being scraped, add a `remote_write` or `otlp` output to `config.yaml` (see [`docs/outputs.md`](../../docs/outputs.md)).
