# Helm chart

Tapyr on Kubernetes with Helm: one replica, non-root, a read-only root file system, probes on `/readyz` and `/healthz`, the state store on a volume, and the configuration as a ConfigMap whose changes roll the pod out.

## Install

1. Write your values: the accounts, jobs and output in `config` (the README of each [provider package](../../docs/providers.md#built-in-packages) has an example).

   ```yaml
   existingSecret: tapyr
   config: |
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

2. Create the Secret with the environment variables the configuration refers to (`{ env: NAME }`) and `TAPYR_STATE_KEY`, which encrypts the tokens kept in the state store. From a file of `NAME=value` lines, kept out of Git:

   ```sh
   kubectl create namespace tapyr
   kubectl -n tapyr create secret generic tapyr --from-env-file=tapyr.env
   ```

3. Install, pinned to a version with `--version`:

   ```sh
   helm install tapyr oci://ghcr.io/kentrow/charts/tapyr --namespace tapyr --values my-values.yaml
   kubectl -n tapyr port-forward svc/tapyr 9790:9790    # then http://127.0.0.1:9790/ui/
   ```

The chart is signed: [Install](../../docs/install.md#verify-the-signatures) shows how to verify it. Its version is the version of Tapyr.

## Values

| Value | Default | What it is |
|---|---|---|
| `config` | empty | Your `Account`, `Job` and `Output` documents, as YAML separated by `---`. No `Settings` document: see `settings` |
| `settings` | `{}` | Merged into the spec of the `Settings` document, which the chart writes. `server.listen` and `state.path` are set by the chart |
| `existingSecret` | empty | A Secret whose keys become environment variables. The chart never holds a secret: values end up in Git and in the Helm history |
| `image.repository`, `.tag`, `.pullPolicy` | `ghcr.io/kentrow/tapyr`, the version of the chart, `IfNotPresent` | The image |
| `imagePullSecrets` | `[]` | For a private registry |
| `persistence.enabled`, `.size`, `.storageClass`, `.existingClaim` | `true`, `1Gi`, empty, empty | The volume of the state store |
| `service.type`, `.port`, `.annotations` | `ClusterIP`, `9790`, `{}` | `/metrics`, `/metrics/self`, the UI and the status API |
| `ingress.enabled`, `.className`, `.annotations`, `.host`, `.tls`, `.paths` | off, empty, `{}`, empty, `[]`, `/`, `/ui` and `/api` | See [Ingress](#ingress) |
| `serviceMonitor.enabled`, `.labels`, `.interval` | off, `{}`, empty | A ServiceMonitor of the Prometheus Operator, for the samples and the self-metrics |
| `resources` | requests `50m`, `64Mi`, limit `256Mi` | About 75 MiB of memory for 22,000 series: adjust to your inventories |
| `podSecurityContext`, `securityContext` | non-root, uid 65532, read-only root, no capability | Security contexts |
| `terminationGracePeriodSeconds` | `30` | Shutdown flushes the state store and the outputs |
| `extraEnv`, `extraVolumes`, `extraVolumeMounts` | `[]` | For secret files (`{ file: /path }`) and provider packages of your own (`settings.providerPaths`) |
| `podLabels`, `podAnnotations`, `nodeSelector`, `tolerations`, `affinity` | empty | Scheduling and metadata of the pod |

Unknown values are refused. There is no replica count on purpose: a second instance would spend the same quotas without coordinating. The `Recreate` strategy stops the old pod before the new one opens the state store.

## Ingress

The web UI and the status API are read-only, but they show your accounts, jobs, endpoints, quotas and errors, and the raw responses of the APIs when the live debug is on. They have no authentication by default: when you enable the Ingress, authenticate at the Ingress controller, or with Tapyr itself, the credentials coming from `existingSecret`:

```yaml
ingress:
  enabled: true
  className: nginx
  host: tapyr.example.com
settings:
  server:
    basicAuth:
      username: { env: TAPYR_UI_USER }
      password: { env: TAPYR_UI_PASSWORD }
```

The default paths are the UI and the status API only: `/metrics` and `/metrics/self` stay inside the cluster, for Prometheus to scrape through the Service. The UI is served at the root of the host, not under a sub-path.

## Operate

- **Configuration changes.** Change the values and run `helm upgrade`: the pod restarts with the new configuration, and the state store keeps what was already spent.
- **State.** Keep the volume: it remembers what was already spent of the daily and monthly quotas, and the rotated tokens. `helm uninstall` deletes the volume the chart created.
- **Logs.** `kubectl -n tapyr logs deploy/tapyr`, in JSON.
