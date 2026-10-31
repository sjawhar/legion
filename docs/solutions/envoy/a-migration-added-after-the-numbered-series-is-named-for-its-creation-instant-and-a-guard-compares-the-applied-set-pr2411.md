---
title: "A migration added after the numbered series is named for its creation instant, and a guard compares the applied set"
category: envoy
tags:
  - migrations
  - pgmigrate
  - dispatch
  - daemon
date: 2026-10-31
status: active
module: packages/envoy/migrationid
related_issues:
  - "sjawhar/legion#2411"
supersedes:
  - docs/solutions/envoy/two-pull-requests-take-the-same-migration-number.md
  - docs/solutions/envoy/a-migration-number-is-checked-against-mains-highest-at-every-push-and-the-local-harness-database-is-recreated-after-a-renumber-INFRA-367.md
---

# A migration added after the numbered series is named for its creation instant, and a guard compares the applied set

The two superseded files tell an author to renumber; sjawhar/legion#2411 replaced the next number with a timestamp, so nothing is renumbered any more.

- Name a new daemon or Dispatch migration `YYYYMMDDHHMMSS_<slug>.up.sql`, the UTC moment you create it (`date -u +%Y%m%d%H%M%S`), with its census under the same timestamp.
  Never take the next number: `migrationid.Check` refuses a number above the series' recorded closing file, on your branch alone.
- Never renumber or rename a numbered file: the closing file is how the check finds where the numbered series ends.
- Do not write a migration that depends on its order relative to one in another open pull request: a deployed database applies each when its release deploys, so two databases can apply them in opposite orders.
- Compare the set of applied versions, never `max(version)`, in anything that decides whether the schema moved (a rollback guard, a seed check): a migration merged below one a database already applied runs there later and leaves the maximum unchanged.

## Evidence

`packages/envoy/migrationid/migrationid.go` holds the rule (`Check`, `closings`: daemon `0064_pull_request_pass_settlement.up.sql`, Dispatch `0149_event_horizon.up.sql`).
Before `closings` existed, a branch adding `0062_*` passed its tests alone and failed only once merged with another (tester, round 8).
`packages/envoy/deploy/scripts/autodeploy.sh` `schema_changed` and the loadtest seed check in `packages/envoy/loadtest/common.sh` compare sets.
