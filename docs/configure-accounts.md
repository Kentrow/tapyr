# Configure your accounts

[Getting started](getting-started.md) runs Tapyr against a simulator. This guide takes you to your own account: the credentials, the account, its jobs, an output, then the checks before the first request. The example uses Cisco Meraki; every provider works the same way, and its README gives what differs.

## 1. Pick a provider package

`tapyr provider list` lists the packages Tapyr knows, and [Providers](providers.md) has them all with their validation status. `tapyr provider show <name>` describes one: its secrets and params with what they are, its templates with their interval and metrics, its limits with their sources; `tapyr provider show <name> --example` prints its example configuration, ready to copy. The README of a package says more:

- what it collects, template by template, and what each template costs in requests;
- the credentials it needs and the permissions to give them (read-only is always enough: Tapyr never writes);
- its params (a regional base URL, an organization ID...), and its limits with their sources.

For Meraki, that is `tapyr provider show meraki-dashboard` and the [`meraki-dashboard` README](../providers/meraki-dashboard/README.md): an API key of a read-only administrator, and nothing else.

## 2. Keep the credentials out of the configuration

A secret is never written in a configuration file: it is a reference to an environment variable (`{ env: NAME }`) or to a file (`{ file: /path }`), read at start and on every reload. Configuration files can then live in Git.

```sh
export TAPYR_MERAKI_API_KEY='...'   # or a file readable by Tapyr only
```

With systemd, put the variables in the environment file of the unit ([systemd](../examples/systemd/README.md)); on Kubernetes, in a Secret ([Kubernetes](../examples/kubernetes/README.md)). The logs, the status API and the web UI never show a secret: Tapyr redacts their values everywhere.

### Accounts that need a consent in a browser

Some APIs (the `oauth2_authorization_code` auth type, [ADR 0016](adr/0016-oauth2-authorization-code.md)) only give a refresh token after a person accepts in a browser. Run the consent once with `tapyr auth login`, anywhere (your laptop is fine), with the configuration and the client id and secret of the account:

```sh
tapyr auth login strava-home --config config.yaml --secret-file ./strava-refresh-token
```

It prints a URL: open it in any browser, accept, and paste back the address the browser was sent to (the page itself may fail to load). `--listen` serves that address on the machine that runs the command instead, when the redirect URL of the package is on `127.0.0.1`; `--device` asks for a code to enter on a page, for packages whose API offers it. The refresh token goes to the file (mode 0600), never to the screen. Give it to the account like any secret (`refreshToken: { file: ... }`, an environment variable or a Kubernetes Secret), then reload Tapyr.

`tapyr run` then renews the tokens by itself and keeps the latest pair in its state store (sealed with `TAPYR_STATE_KEY`): the file goes stale after the first renewal when the API rotates its refresh tokens, and that is expected. Run `tapyr auth login` again only when Tapyr says so (the status API, `/readyz` and `tapyr_auth_reauth_required` name the account), for example after the access was revoked, or when the state store was lost.

## 3. Write the configuration

One file or several, in one directory: Tapyr reads every `.yaml` and `.yml` file of the paths given to `--config`.

```yaml
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: campus }
spec:
  provider: meraki-dashboard
  secrets:
    apiKey: { env: TAPYR_MERAKI_API_KEY }
  labels: { site: campus }
  # The per-organization rate is shared with the other applications of the
  # organizations: leave them half of it.
  budget: { share: 0.5, scaleRates: true }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: device-availabilities }
spec:
  account: campus
  template: meraki-device-availabilities
  priority: critical
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: wireless-clients }
spec: { account: campus, template: meraki-wireless-client-counts }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prometheus }
spec: { type: prometheus }
---
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: settings }
spec:
  state: { path: ./data/tapyr.db }
```

- **The account** names the provider, its secrets and its params. Its labels are added to every sample. An account with several regional or physical endpoints declares them as `targets` ([Concepts](concepts.md)).
- **The jobs** each use a template of the package, at its default interval unless you set `interval`. Discovery is implicit: the jobs above need the list of organizations, and Tapyr runs it for them. The `priority` decides who slows down first when a quota is short (`low`, `normal`, `high`); a `critical` job is never slowed down.
- **The output** serves the samples on `/metrics` for Prometheus to scrape. [Outputs](outputs.md) covers remote write and OTLP, and how to send an output less.
- **The settings** are optional. The state store keeps what Tapyr already spent of the quotas across restarts: put it on persistent storage. In a container, also set `server: { listen: "0.0.0.0:9790" }`.

The [configuration reference](reference/README.md) has every field. Editors check the files as you type with the JSON Schemas (`tapyr schema`).

## 4. Check before the first request

```sh
tapyr validate --config ./config --check-secrets
tapyr plan --config ./config
```

`validate` reports every error with its file, line and column, and what to change: the closest name for a typo, the description of a missing param, where to align a line. `--check-secrets` also checks that every secret is set. `plan` projects what the jobs will spend of every quota and tells whether it fits; when it does not, it says which job to slow down or which priority to lower. Both run without calling the provider, so a CI can run them on every change of the configuration.

## 5. Run it

```sh
tapyr run --config ./config
```

The first log line gives the addresses of the web UI, `/metrics` and the status API. The first runs are spread over the first tenth of their interval, then discovery fans out ([Getting started](getting-started.md#4-run-it) shows it). Change the configuration and send `SIGHUP` to reload it without a restart: an invalid configuration is rejected, and the previous one keeps running.

When something does not work, [Troubleshooting](troubleshooting.md) goes from the symptom to the cause; the web UI says the same on the page of the job, with a link to the right section.
