# Releasing

How a maintainer publishes a version. The release workflow ([`.github/workflows/release.yml`](../.github/workflows/release.yml)) does the work on a tag; this page lists what it needs and the steps around it.

## Once per repository

- **Release environment**: Settings > Environments > New environment `release`, with the maintainers as required reviewers. The release job waits for their approval.
- **Tag protection**: Settings > Rules > Rulesets, a tag ruleset on `v*` that only maintainers may create, update or delete.
- **Branch protection**: Settings > Rules > Rulesets, a branch ruleset on `main`: changes through a pull request only, the jobs of the CI workflow required as status checks (`Conventional Commits`, `Lint`, `Test`, `Vulnerabilities`, `Dependency review`, `Release snapshot (every platform, images)`, `Web UI`, `Smoke test`, `Integration (VictoriaMetrics, OpenTelemetry Collector, Prometheus)`, `Alert rules`, `Fuzz`), no force push, no deletion. `Test` gathers the test jobs (`Test (packages)` and the shards of `Test (cmd/tapyr, ...)`), and `Fuzz` the fuzz targets (`Fuzz (<package>:<target>)`): a new test job or fuzz target is added to their `needs` or matrix, not to the ruleset.
- **Private vulnerability reporting**: Settings > Code security, enabled (see [`SECURITY.md`](../SECURITY.md)).
- **Secret scanning**: Settings > Code security, secret scanning and push protection enabled.
- **Dependabot**: Settings > Code security, Dependabot alerts and security updates enabled; the version updates come from [`.github/dependabot.yml`](../.github/dependabot.yml).
- **Workflow permissions**: Settings > Actions > General, "Read repository contents and packages permissions" as the default token permissions (every workflow asks for what it needs), GitHub Actions not allowed to create or approve pull requests, and approval required to run the workflows of first-time contributors.
- **Homebrew tap**: the public repository `Kentrow/homebrew-tap`, with a deploy key that has write access; its private key is the secret `HOMEBREW_TAP_SSH_KEY` of the `release` environment. To replace it, generate a new key, add it to the tap, set the secret and remove the old key.
- **Labels**: `scripts/sync-labels.sh` creates the labels of [`.github/labels.yml`](../.github/labels.yml).
- **OpenSSF**: the Scorecard workflow publishes its results once the repository is public (badge in the README). The project is registered on [bestpractices.dev](https://www.bestpractices.dev/projects/16342) (passing level, badge in the README): keep its answers true when the project changes (security policy, tests, CI, release signing).
- **Image visibility**: after the first release, the package `ghcr.io/kentrow/tapyr` is private by default: Packages > tapyr > Package settings > Change visibility to public, and link it to the repository. The same for `ghcr.io/kentrow/charts/tapyr`, after the first release with the chart.

## A release

1. Check `main`: CI green, including the release snapshot job (every platform, both images). Check the [versions updated by hand](#versions-updated-by-hand) too; a bump goes through its own pull request before the release.
2. Release a candidate first when the release itself changed: `release.yml`, `.goreleaser.yaml` or the `Dockerfile` changed since the last tag (`git diff --stat vPREVIOUS -- .github/workflows/release.yml .goreleaser.yaml Dockerfile`), or the version is a minor one that changes the behavior or the configuration. Tag `vX.Y.Z-rc.N` on `main` before the release commit, as in steps 4 to 6: its release notes are the `[Unreleased]` section, the GitHub release is a prerelease and the image gets the tag `X.Y.Z-rc.N` only (not `X.Y` nor `latest`). Go on once it is checked. Otherwise, tag the version directly: a candidate would only repeat a release that already worked, and the CI checks the code of every pull request, release snapshot included.
3. In `CHANGELOG.md`, rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, add an empty `## [Unreleased]` above it, and update the links at the bottom:

   ```
   [Unreleased]: https://github.com/kentrow/tapyr/compare/vX.Y.Z...HEAD
   [X.Y.Z]: https://github.com/kentrow/tapyr/compare/vPREVIOUS...vX.Y.Z
   ```

   (for the first release: `[0.1.0]: https://github.com/kentrow/tapyr/releases/tag/v0.1.0`). In the same commit, update the version the docs and examples pin (`git grep -n "tapyr:PREVIOUS\|VERSION=PREVIOUS\|newTag"`: the README, `docs/install.md`, `docs/operations.md`, the demo compose and the Kustomize example), and `version` and `appVersion` in `charts/tapyr/Chart.yaml`. Commit it as `chore(release): vX.Y.Z` through a pull request.
4. Tag the merge commit on `main` and push the tag:

   ```sh
   git tag -s vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

5. The workflow checks that the commit is on `main`, runs the tests, builds the UI without permissions, then waits for the approval of the `release` environment. Approve it.
6. Check the release: the archives, the deb and rpm packages, the cask written to `Kentrow/homebrew-tap` (a release candidate does not write it: its workflow keeps the file as the artifact `homebrew-cask`), the Helm chart, `checksums.txt` and its `.sigstore.json` bundle, `tapyr_X.Y.Z_provenance.intoto.jsonl`, the image tags, the signatures and the provenance with the commands of [`docs/install.md`](install.md#verify-the-signatures) and [the next section](install.md#verify-the-provenance). The certificate identity holds the repository name as GitHub spells it (`Kentrow/tapyr`): if the repository moves or is renamed, update those commands.

If the release job fails on an incident of GitHub (an upload answered 500 or 422, a timeout), rerun the failed job from the run page, and approve the `release` environment again: GoReleaser reuses the draft release the failed run left, replaces the assets it already uploaded, and the provenance asset is replaced as well. The tag stays as it is.

Versions follow [Semantic Versioning](https://semver.org/); before 1.0, a breaking change bumps the minor version. A release candidate (`vX.Y.Z-rc.N`) goes through the same workflow, is published as a prerelease, and takes the `[Unreleased]` section of the changelog as its notes: use one to test the pipeline before a version.

## Versions updated by hand

Dependabot updates the Go modules, the actions, the images and the npm packages. These versions are written by hand, or held back on purpose, and nothing updates them: compare them with the latest releases before every release.

| Version | Where | Rule |
|---|---|---|
| golangci-lint | `GOLANGCI_LINT_VERSION` in the [`Makefile`](../Makefile) (the CI reads it) | The latest v2 release; fix or justify the new findings in the same pull request. |
| GoReleaser | `version:` of the GoReleaser steps in [`ci.yml`](../.github/workflows/ci.yml) and [`release.yml`](../.github/workflows/release.yml) | The same version in both; the release snapshot job of the CI checks it before a tag does. |
| govulncheck | `golang.org/x/vuln/cmd/govulncheck@vX.Y.Z` in [`ci.yml`](../.github/workflows/ci.yml) | The latest release. |
| Node.js major | `node-version` in the workflows, the `ui` stage of the [`Dockerfile`](../Dockerfile), `@types/node` in [`ui/package.json`](../ui/package.json) | The active LTS line, all together; Dependabot updates the minor versions only. |
| Helm | `version:` of the `azure/setup-helm` steps in [`ci.yml`](../.github/workflows/ci.yml) and [`release.yml`](../.github/workflows/release.yml) | The latest release; the same in both. |
| kubeconform, kind | `KUBECONFORM_VERSION` in [`scripts/helm-check.sh`](../scripts/helm-check.sh), `KIND_VERSION` in [`scripts/helm-e2e.sh`](../scripts/helm-e2e.sh) | The latest releases. |
| TypeScript major | `typescript` in [`ui/package.json`](../ui/package.json) | Held back until typescript-eslint supports the new major. |

A separate `tools/go.mod` module, with `tool` directives updated by Dependabot, was considered and set aside: the three Go tools pull about 500 modules into it (a `go.sum` of 1600 lines), whose advisories would raise Dependabot alerts on the repository for code that never enters the binary, and golangci-lint recommends its prebuilt binaries over `go tool`.
