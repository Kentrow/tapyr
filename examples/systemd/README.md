# systemd

[`tapyr.service`](tapyr.service) runs Tapyr as a hardened systemd service: a dynamic user, a read-only system, no capabilities, the network and its state directory only (`systemd-analyze security tapyr` rates it 1.4, "OK").

## Install

The [deb and rpm packages](../../docs/install.md#deb-and-rpm-packages) install this unit: write the configuration and the secrets as below, then `sudo systemctl enable --now tapyr`.

With the binary installed by hand, in `/usr/local/bin/tapyr` (see [`docs/install.md`](../../docs/install.md)):

```sh
sudo install -d -m 0755 /etc/tapyr
sudo install -m 0644 config.yaml /etc/tapyr/           # your Account, Job, Output documents
sudo install -m 0600 /dev/null /etc/tapyr/tapyr.env     # then write the secrets in it
sudo install -m 0644 tapyr.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tapyr
```

`/etc/tapyr/tapyr.env` holds the environment variables the configuration refers to (`{ env: NAME }`), one `NAME=value` per line, and `TAPYR_STATE_KEY`, which encrypts the tokens kept in the state store:

```sh
TAPYR_MERAKI_API_KEY=...
TAPYR_STATE_KEY=...
```

systemd reads it as root before starting Tapyr, so it stays `0600 root:root`. Tapyr reads only the `*.yaml` and `*.yml` files of `/etc/tapyr`, and they must be readable by the dynamic user (`0644`).

## Operate

```sh
systemctl status tapyr
journalctl -u tapyr -f          # JSON logs; settings.log.format: text for plain lines
sudo systemctl reload tapyr     # reload the configuration (SIGHUP)
sudo tapyr validate --config /etc/tapyr
```

- **Listener.** Tapyr listens on `127.0.0.1:9790` by default. For a Prometheus on another host, set `settings.server.listen` to `0.0.0.0:9790`, and protect the UI and the status API with `settings.server.basicAuth` or a firewall.
- **State.** The state store is `/var/lib/tapyr/data/tapyr.db` (`/var/lib/private/tapyr` on disk, because of `DynamicUser`). It remembers what was already spent of the daily and monthly quotas, and the rotated tokens (every refresh of an Aruba Central token returns a new refresh token, and the previous one is no longer valid): back it up with the host, and do not delete it to "reset" Tapyr.
- **Provider packages.** User packages go in `/etc/tapyr/providers` with `providerPaths: [./providers]` in a `Settings` document (relative paths are resolved against the file that declares them).
- **One instance per account.** Run a single Tapyr per set of accounts: two instances polling the same accounts spend the same quotas without coordinating.

## Secrets as files

To keep the secrets out of the environment, use systemd credentials with file secrets. A drop-in (`systemctl edit tapyr`):

```ini
[Service]
LoadCredential=meraki-api-key:/etc/tapyr/credentials/meraki-api-key
```

and in the configuration:

```yaml
secrets: { apiKey: { file: /run/credentials/tapyr.service/meraki-api-key } }
```

File secrets are read again on every reload.
