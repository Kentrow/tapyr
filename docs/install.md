# Install

Tapyr is a single static binary with the web UI embedded. Releases ship it for Linux, macOS and Windows (amd64, arm64), and as the multi-arch image `ghcr.io/kentrow/tapyr`.

## Binary

Download the archive of your platform and `checksums.txt` from the [releases page](https://github.com/kentrow/tapyr/releases), check it, then put `tapyr` in your `PATH`. On Linux:

```sh
VERSION=0.2.0
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -LO https://github.com/kentrow/tapyr/releases/download/v$VERSION/tapyr_${VERSION}_linux_${ARCH}.tar.gz
curl -LO https://github.com/kentrow/tapyr/releases/download/v$VERSION/checksums.txt
sha256sum --ignore-missing -c checksums.txt
tar xzf tapyr_${VERSION}_linux_${ARCH}.tar.gz tapyr
./tapyr version
```

The archives are `tapyr_<version>_<os>_<arch>`: `linux`, `darwin` or `windows` (a `.zip`), `amd64` or `arm64`. On macOS, `sha256sum` is not installed: check the archive with `shasum -a 256 --ignore-missing -c checksums.txt`.

Each archive has its SBOM next to it (`<archive>.sbom.json`, SPDX), and holds `LICENSE`, `NOTICE` and `THIRD_PARTY_LICENSES`, the licenses and notices of the software the binary includes.

## Container image

```sh
docker run -d --name tapyr \
  -v "$PWD/config:/etc/tapyr:ro" \
  -v tapyr-data:/home/nonroot/data \
  -p 9790:9790 \
  ghcr.io/kentrow/tapyr:0.2.0
```

- The image runs `tapyr run --config /etc/tapyr` as the `nonroot` user (uid 65532), on `gcr.io/distroless/static-debian12`.
- The state store (quota consumption, rotated tokens, counters) defaults to `./data/tapyr.db`, that is `/home/nonroot/data` in the image: keep it on a volume, or Tapyr forgets on restart what it already spent of the daily and monthly quotas.
- Set `settings.server.listen` to `0.0.0.0:9790` in the configuration, so that the published port reaches the UI, the status API and `/metrics`.
- Secrets come from environment variables or files (`{ env: NAME }`, `{ file: /path }`): pass them with `-e` or mount them, never in the configuration files.
- `docker kill -s HUP tapyr` reloads the configuration.

Tags: `X.Y.Z`, `X.Y` (latest patch of a minor version) and `latest` (latest release). A release candidate has its tag `X.Y.Z-rc.N` only.

## systemd

[`examples/systemd`](../examples/systemd/README.md) has a hardened unit: a dynamic user, the configuration in `/etc/tapyr`, the secrets in an environment file read by systemd, the state store in `/var/lib/tapyr`, and `systemctl reload tapyr` for a reload.

## Kubernetes

The [Helm chart](../charts/tapyr/README.md) is published with every release, signed:

```sh
kubectl create namespace tapyr
kubectl -n tapyr create secret generic tapyr --from-env-file=tapyr.env
helm install tapyr oci://ghcr.io/kentrow/charts/tapyr --namespace tapyr --values my-values.yaml
```

Without Helm, [`examples/kubernetes`](../examples/kubernetes/README.md) has Kustomize manifests: a Deployment of one replica (non-root, read-only root file system, probes on `/readyz` and `/healthz`), its Service, a volume for the state store, and the configuration as a ConfigMap whose changes roll the Deployment out. Run a single replica: a second one would spend the same quotas without coordinating.

## Verify the signatures

The checksums and the image are signed with [cosign](https://docs.sigstore.dev/) in keyless mode by the release workflow of the repository. The signature proves that the files and the image were built by that workflow, from a tag of `Kentrow/tapyr`. The identity is compared as written, case included.

```sh
# The checksums of a release (then check the archives with sha256sum, above).
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/Kentrow/tapyr/.github/workflows/release.yml@refs/tags/v$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# The image.
cosign verify ghcr.io/kentrow/tapyr:$VERSION \
  --certificate-identity "https://github.com/Kentrow/tapyr/.github/workflows/release.yml@refs/tags/v$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The Helm chart, from v0.2.1:

```sh
cosign verify ghcr.io/kentrow/charts/tapyr:$VERSION \
  --certificate-identity "https://github.com/Kentrow/tapyr/.github/workflows/release.yml@refs/tags/v$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The SBOM of the image is attached to it as an attestation:

```sh
docker buildx imagetools inspect ghcr.io/kentrow/tapyr:$VERSION --format '{{ json .SBOM }}'
```

## Verify the provenance

From v0.1.1, every archive and the image carry their [SLSA](https://slsa.dev) build provenance, as GitHub artifact attestations: the workflow that built them, from which commit and tag, on which runner. The [GitHub CLI](https://cli.github.com) verifies them, and the options below make it refuse anything not built by the release workflow from the tag of the version:

```sh
# An archive.
gh attestation verify "tapyr_${VERSION}_linux_amd64.tar.gz" --repo Kentrow/tapyr \
  --signer-workflow Kentrow/tapyr/.github/workflows/release.yml \
  --source-ref "refs/tags/v$VERSION" --deny-self-hosted-runners

# The image.
gh attestation verify "oci://ghcr.io/kentrow/tapyr:$VERSION" --repo Kentrow/tapyr \
  --signer-workflow Kentrow/tapyr/.github/workflows/release.yml \
  --source-ref "refs/tags/v$VERSION" --deny-self-hosted-runners

# The Helm chart, from v0.2.1.
gh attestation verify "oci://ghcr.io/kentrow/charts/tapyr:$VERSION" --repo Kentrow/tapyr \
  --signer-workflow Kentrow/tapyr/.github/workflows/release.yml \
  --source-ref "refs/tags/v$VERSION" --deny-self-hosted-runners
```

The attestations of the archives are also a release asset, `tapyr_${VERSION}_provenance.intoto.jsonl`, to verify without the attestations API of GitHub: add `--bundle tapyr_${VERSION}_provenance.intoto.jsonl` to the command of an archive. For the image, `--bundle-from-oci` reads the attestation stored next to it in the registry.

## From source

Go 1.27 and Node.js 22.12 or later (Vite 8 needs it):

```sh
git clone https://github.com/kentrow/tapyr.git
cd tapyr
make build        # the UI, then the static binary in ./bin/tapyr
make docker       # or the image tapyr:dev
```
