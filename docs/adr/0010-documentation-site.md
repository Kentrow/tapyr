# ADR 0010 - Documentation site built from this repository

- Status: accepted
- Date: 2026-11-05

## Context

The documentation is Markdown in this repository (`docs/`, the provider and example READMEs) and is read on GitHub. A documentation site is easier to read, search and link, and lets a user read the documentation of the version they run. The static documentation website was out of scope of v0.1.0 ([`01-vision-and-scope.md`](../spec/01-vision-and-scope.md)); issue [#7](https://github.com/Kentrow/tapyr/issues/7) is the agreement it asks for.

The documentation describes behavior: a pull request that changes behavior updates the spec, the examples or the provider README in the same commit (definition of done in [`AGENTS.md`](../../AGENTS.md)). Whatever we build must keep that.

## Decision

- **Content stays in this repository.** Every page is written here, reviewed with the code it describes, and frozen by each release tag. Pages keep working on GitHub: links are relative, between files of the repository.
- **The site is built in a dedicated repository.** It holds the engine ([Zensical](https://zensical.org), by the Material for MkDocs team): configuration, theme, home page, build and deployment to **docs.tapyr.eu**. It never edits the content: a fix to a page is a pull request here.
- **The manifest [`docs/site.yml`](../site.yml)** lists the published pages and their order. A page added here appears on the site without a change in the site repository. Every Markdown file of the source directories it names is either published or listed as unpublished, so a new page cannot be forgotten.
- **The contract is a tool of this repository**, `internal/tools/docsite`:
  - `check` validates the manifest and every relative link of the Markdown files of the repository, anchors included (part of `make lint`);
  - `export -ref <tag> -out <dir> -index <file>` writes the pages at their site paths, with links rewritten: to the site page when the target is published, to the file on GitHub at that ref otherwise; images are copied. The index file holds the navigation, in the `nav` format of `mkdocs.yml`, and the source file of every page for the "edit this page" links.

  The site repository checks out a tag of this repository and runs its `export`. Site paths drop the `docs/` prefix, and a `README.md` becomes the `index.md` of its directory (`providers/meraki-dashboard/README.md` is `/providers/meraki-dashboard/`).
- **Anchors** are GitHub heading slugs (lowercase, punctuation removed, spaces as hyphens); the site configures its slug function to produce the same ones (`pymdownx.slugs.slugify` with `case: lower`; an export of every page built with Zensical 0.0.74 resolved all its internal links and anchors).
- **Versions.** One documentation version per minor release, built from the latest tag of that minor (`/0.1/` from `v0.1.2`, then `v0.1.3`), `/latest/` for the latest stable minor, `/dev/` for `main`. Release candidates are not published. Older minors stay online with an outdated banner; the version selector lists the last three and `dev`. The first published version is 0.1, from v0.1.1, the first tag with the manifest.
- **Reference pages are generated**, from the JSON Schemas for the configuration, and are never written by hand.

## Alternatives considered

- **Site and content in this repository.** One repository, one pull request; but the Python tool chain of the site, its theme work and its deployment rights would live in the Go repository and its CI. The manifest and `docsite` keep the boundary small enough that a second repository costs little.
- **Content moved to the site repository.** Documentation changes would need a second pull request, reviewed apart from the code, and would drift from the releases (the Kubernetes website model).
- **Site from GitHub Markdown as is** (no manifest, no export). Links that leave `docs/` (provider and example READMEs, configuration files) break on the site, and the navigation lives in the site repository, out of sight of the pull requests that add pages.
- **MkDocs with Material for MkDocs.** In maintenance mode, its authors moved to Zensical, which reads the same configuration.

## Consequences

- Writing documentation does not change: Markdown here, relative links, `make lint` catches a broken link or anchor before the site does.
- A new Markdown file in `docs/` must be added to `docs/site.yml`, in `nav` or in `unpublished`.
- The output of `docsite export` (paths, link rewriting, index file) is an interface: changing it means a coordinated change of the site repository. So is the output of `tapyr provider list --output json`, from which the site builds the provider catalog ([ADR 0011](0011-provider-ecosystem.md), versioned by its `schemaVersion`).
- Zensical is young (0.0.x) and its native versioning is on its roadmap; the site repository pins its version and uses the `mike` fork it documents meanwhile.
- v0.1.0 has no documentation version on the site.
