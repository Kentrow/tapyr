---
name: record-fixtures
description: Record real API responses with tapyr record, anonymized, to build or check the fixtures of a provider simulator or to attach responses to a bug report. Use when the person has access to a real platform and wants responses captured. Not for writing fixtures from documentation (see new-provider), and never without the person asking, since it spends real quota.
---

# Record fixtures

`tapyr record` runs jobs once against a real API, through the budget engine (so within the limits of the provider), and writes every response page anonymized. The goal is responses that keep the real shape and edge cases of the API, with nothing that identifies the account, its devices or its people.

**This touches a real platform with real credentials.** Do it only when the person asks, with credentials they provide in their own environment. Never print, read back, copy or commit a credential, and never put one on a command line that ends up in a log or a history file.

## 1. Prepare

- A configuration with one account on the real platform and the jobs to record, in a directory **outside the repository** (for example `~/tapyr-record/config.yaml`), with secrets as `{ env: NAME }`.
- The person sets the variables in their shell or an env file of their own (`chmod 600`), loaded only into the command that needs it:

  ```sh
  set -a; . ~/.config/tapyr/record.env; set +a; tapyr record ...
  ```

- If the provider rotates its refresh tokens (`aruba-central-classic`) and `tapyr run` already uses the account, record with the state path of its configuration and its `TAPYR_STATE_KEY`: `tapyr record` reads the current pair, read-only, and never renews it. A separate state starts from the bootstrap pair, which `tapyr run` has spent.
- Run `tapyr plan --config ~/tapyr-record` first: it shows the requests a run costs. One run of a fan-out job costs one request per instance; select what you need.

## 2. Record

```sh
tapyr record --config ~/tapyr-record --job devices [--job ...] \
  --out ~/tapyr-record/out [--anonymize ~/tapyr-record/anonymize.yaml]
```

The output directory holds one JSON file per page, `index.yaml` (job, instance, request, status, quota headers) and `report.txt` (every replaced path and the rule applied, never a value).

The default rules, the format of `anonymize.yaml` (extra `replace` rules for fields the defaults miss, `keep` exceptions for fields that are safe and useful, such as models and firmware versions) and the recorded headers are described in [`docs/providers.md`](../../../docs/providers.md#recording-real-responses). Pseudonyms are consistent: the same value gets the same pseudonym everywhere, so relations between objects survive.

## 3. Review, before anything enters the repository

- Read `report.txt`: every field holding an identifier must be listed as replaced.
- Read the pages themselves, looking for what rules cannot know: free-text fields (descriptions, notes, comments, tags), names inside URLs or messages, account or company names, locations in strings, tokens or session identifiers in bodies.
- Search the recording for the values the person knows (their organization name, a serial, a domain, a public IP). Ask them to do it too: you do not know their data.
- If anything leaks, add a rule and record again; do not hand-edit pseudonyms (consistency breaks).

## 4. Use it

- **Simulator fixtures**: turn the pages into `sim/fixtures/` of the package, preferably as templates (`*.json.tmpl`) that keep the shape of the recording and generate the values from the synthetic inventory (see `providers/teltonika-rms/sim/fixtures`). Keep the edge cases the recording revealed (null fields, missing keys, odd units).
- **Contract and mapping**: compare the recording with the existing fixtures and `tests/expected.prom`; a field the mapping expects but the real API lacks is a bug to fix in the templates.
- **Bug report**: attach the relevant pages, never the full recording, after the review above.

Delete the raw recording outside the repository once its useful parts are committed.

## Done when

- Nothing in the diff identifies the account, its devices, its people or its network, and the person has confirmed it.
- No credential appears anywhere: files, commands shown, logs, the pull request.
