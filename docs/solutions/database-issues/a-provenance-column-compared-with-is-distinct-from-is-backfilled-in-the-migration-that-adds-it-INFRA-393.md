---
title: "A provenance column compared with is distinct from is backfilled in the migration that adds it"
category: database-issues
tags:
  - dispatch
  - migrations
  - postgres
  - null
  - embeddings
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch/store/migrations
related_issues:
  - "INFRA-393"
  - "sjawhar/legion#2208"
---

# A provenance column compared with is distinct from is backfilled in the migration that adds it

- A column added to say where a row came from, read by a predicate such as
  `col is distinct from $current`, counts every existing row as foreign while it is null: a null
  is distinct from every value.
- So in the migration that adds the column, set existing rows to the value they historically
  had. Find that value in the code of the release before yours, not in assumptions: check that it
  was the only value that release could have produced (a constant nobody overrode).
- Pin it with a migration test. Seed a row before the migration under the old origin, then check
  two things: the migration records that origin on it, and the code that acts on "foreign" leaves
  it alone under the old configuration and acts on it once under a new one.
- Say in the migration what the backfill cannot cover. A server of the earlier release that keeps
  writing during the roll writes rows with no origin.
- The census stays `select 0` when the update writes only the column the migration adds.

## Evidence

INFRA-393's migration (shipped as `0118_embeddings_pending_since`) added `embeddings.dead_model`,
the embedder identity a row was confirmed dead under. The coverage pass re-admits a row dead
under any other identity (`readmitForeignDead`, `foreignRows` in `embedqueue/coverage.go`: `dead
and dead_model is distinct from $2`). As first written, the column started null, so on a
deployment that keeps Bedrock and sets nothing new, the first pass would have re-admitted every
row that died before the release. Each would have died again with an ERROR line and spent extra
Bedrock calls, which breaks the spec's "Bedrock unchanged" line. Review caught it, not a test or
a live run, because no proof seeded a dead row from the earlier release.

Every embedder before the release built its client with the package constant
`embed.Model = "us.cohere.embed-v4:0"`, which nothing overrode (checked with `git log -S` back to
LEGION-549). So the migration now runs `update embeddings set dead_model = 'us.cohere.embed-v4:0'
where dead`. `TestMigrationAddsPendingSince` imports `embed.BedrockModel` and fails without that
update; `TestCoverLeavesARowDeadUnderBedrockDeadUntilAnotherIdentity` pins the behaviour.

Live: main's server built a 0116 database with two dead rows. The fixed binary on Bedrock logged
`foreignDeadReadmitted=0` and made no Bedrock call. The same database switched to Azure logged
`foreignDeadReadmitted=2`. The pre-fix binary left `dead_model` null and counted both rows as
foreign.
