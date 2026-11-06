## What and why

<!-- What changes, and why. Link the issue: "Closes #123". -->

## How it was tested

<!-- Tests added, checks run, simulator scenarios, real platform (which version, when). Only what you actually ran: the commands, their output or a link to the run. -->

## Checklist

- [ ] `make lint` and `make test` pass (`make ui-check` for the web UI)
- [ ] New behavior has tests; a bug fix has a regression test
- [ ] JSON Schemas regenerated (`make schema`) if configuration types changed
- [ ] Docs updated (spec, examples, provider README) if user-visible behavior changed
- [ ] `CHANGELOG.md` entry under `## [Unreleased]` for a user-visible change
- [ ] No secret, serial, MAC, IP or other real data anywhere in the diff
- [ ] Provider package: every limit cites its source, `tapyr provider lint` passes

## AI assistance

<!-- Did a coding agent help? Which one, and for what? See "Contributions assisted by AI" in CONTRIBUTING.md. -->

- [ ] No agent was used
- [ ] An agent helped, and I have reviewed and understood every change:
