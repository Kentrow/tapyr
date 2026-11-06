# Releasing

How a maintainer publishes a version. The release workflow ([`.github/workflows/release.yml`](../.github/workflows/release.yml)) does the work on a tag; this page lists what it needs and the steps around it.

## Once per repository

- **Release environment**: Settings > Environments > New environment `release`, with the maintainers as required reviewers. The release job waits for their approval.
- **Tag protection**: Settings > Rules > Rulesets, a tag ruleset on `v*` that only maintainers may create, update or delete.
- **Branch protection**: Settings > Rules > Rulesets, a branch ruleset on `main`: changes through a pull request only, the jobs of the CI workflow required as status checks (`Conventional Commits`, `Lint`, `Test`, `Vulnerabilities`, `Dependency review`, `Release snapshot (every platform, images)`, `Web UI`, `Smoke test`, `Integration (VictoriaMetrics, OpenTelemetry Collector, Prometheus)`, `Alert rules`), no force push, no deletion.
- **Private vulnerability reporting**: Settings > Code security, enabled (see [`SECURITY.md`](../SECURITY.md)).
- **Secret scanning**: Settings > Code security, secret scanning and push protection enabled.
- **Dependabot**: Settings > Code security, Dependabot alerts and security updates enabled; the version updates come from [`.github/dependabot.yml`](../.github/dependabot.yml).
- **Workflow permissions**: Settings > Actions > General, "Read repository contents and packages permissions" as the default token permissions (every workflow asks for what it needs), GitHub Actions not allowed to create or approve pull requests, and approval required to run the workflows of first-time contributors.
- **Labels**: `scripts/sync-labels.sh` creates the labels of [`.github/labels.yml`](../.github/labels.yml).
- **OpenSSF**: the Scorecard workflow publishes its results once the repository is public (badge in the README). The project is registered on [bestpractices.dev](https://www.bestpractices.dev/projects/16342) (passing level, badge in the README): keep its answers true when the project changes (security policy, tests, CI, release signing).
- **Image visibility**: after the first release, the package `ghcr.io/kentrow/tapyr` is private by default: Packages > tapyr > Package settings > Change visibility to public, and link it to the repository.

## A release

1. Check `main`: CI green, including the release snapshot job (every platform, both images).
2. In `CHANGELOG.md`, rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, add an empty `## [Unreleased]` above it, and update the links at the bottom:

   ```
   [Unreleased]: https://github.com/kentrow/tapyr/compare/vX.Y.Z...HEAD
   [X.Y.Z]: https://github.com/kentrow/tapyr/compare/vPREVIOUS...vX.Y.Z
   ```

   (for the first release: `[0.1.0]: https://github.com/kentrow/tapyr/releases/tag/v0.1.0`). In the same commit, update the version the docs and examples pin (`git grep -n "tapyr:PREVIOUS\|VERSION=PREVIOUS\|newTag"`: the README, `docs/install.md`, `docs/operations.md`, the demo compose and the Kustomize example). Commit it as `chore(release): vX.Y.Z` through a pull request.
3. Tag the merge commit on `main` and push the tag:

   ```sh
   git tag -s vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

4. The workflow checks that the commit is on `main`, runs the tests, builds the UI without permissions, then waits for the approval of the `release` environment. Approve it.
5. Check the release: the archives, `checksums.txt` and its `.sigstore.json` bundle, `tapyr_X.Y.Z_provenance.intoto.jsonl`, the image tags, the signatures and the provenance with the commands of [`docs/install.md`](install.md#verify-the-signatures) and [the next section](install.md#verify-the-provenance). The certificate identity holds the repository name as GitHub spells it (`Kentrow/tapyr`): if the repository moves or is renamed, update those commands.

If the release job fails on an incident of GitHub (an upload answered 500 or 422, a timeout), rerun the failed job from the run page, and approve the `release` environment again: GoReleaser reuses the draft release the failed run left, replaces the assets it already uploaded, and the provenance asset is replaced as well. The tag stays as it is.

Versions follow [Semantic Versioning](https://semver.org/); before 1.0, a breaking change bumps the minor version. A release candidate (`vX.Y.Z-rc.N`) goes through the same workflow, is published as a prerelease, and takes the `[Unreleased]` section of the changelog as its notes: use one to test the pipeline before a version.
