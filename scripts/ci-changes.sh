#!/usr/bin/env bash
# Classifies the files changed by a pull request, one path per line on
# stdin, for the CI workflow. Prints two lines for $GITHUB_OUTPUT:
#
#   docs_only=true|false  every file is documentation that no test reads:
#                         the jobs that build and test the code are skipped.
#   release=true|false    a file can change what the release snapshot
#                         builds (packaging, images, release tooling).
#
# Usage: git diff --name-only BASE...HEAD | scripts/ci-changes.sh
# A Markdown file read by a test (generated tables, the configuration
# reference, spec examples, the self-metric tables) is not documentation
# here: a test that starts reading another one adds it to not_docs.
set -euo pipefail

# Read by tests or generated from the code: a change needs the full CI.
not_docs() {
	case "$1" in
	README.md | docs/providers.md | docs/reference/* | docs/alerts.md) return 0 ;;
	docs/spec/03-configuration.md | docs/spec/07-self-observability.md) return 0 ;;
	esac
	return 1
}

docs() {
	case "$1" in
	docs/* | .agents/* | .claude/* | .github/ISSUE_TEMPLATE/*) return 0 ;;
	*/*) return 1 ;;
	*.md) return 0 ;; # Markdown at the root: CHANGELOG.md, CONTRIBUTING.md...
	esac
	return 1
}

release() {
	case "$1" in
	.goreleaser.yaml | Dockerfile | Dockerfile.release | .dockerignore) return 0 ;;
	packaging/* | examples/systemd/* | scripts/packages-e2e.sh) return 0 ;;
	LICENSE | NOTICE | Makefile | go.mod | go.sum) return 0 ;;
	ui/package.json | ui/package-lock.json) return 0 ;;
	internal/tools/licenses/* | internal/buildinfo/*) return 0 ;;
	.github/workflows/ci.yml | .github/workflows/release.yml) return 0 ;;
	esac
	return 1
}

docs_only=true
release=false
count=0
while IFS= read -r path; do
	[ -n "$path" ] || continue
	count=$((count + 1))
	if not_docs "$path" || ! docs "$path"; then
		docs_only=false
	fi
	if release "$path"; then
		release=true
	fi
done

# No file (an empty diff): run everything rather than guess.
if [ "$count" -eq 0 ]; then
	docs_only=false
	release=true
fi

echo "docs_only=$docs_only"
echo "release=$release"
