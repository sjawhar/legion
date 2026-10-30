---
title: "A migration is total over rows the previous schema allowed, and a census counts only rows that must stop the deploy"
category: database-issues
tags:
  - postgres
  - migration
  - expression-index
  - census
  - dispatch
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/store
related_issues:
  - "sjawhar/legion#2483"
---

# A migration is total over rows the previous schema allowed, and a census counts only rows that must stop the deploy

- `create index` evaluates its expression on every row it covers, so a cast or function inside it
  must not raise on any value the previous schema let in. Before you write an unguarded cast, name
  the index, constraint or column type that already casts exactly those rows, and read its
  expression argument by argument: a `coalesce(a, b::uuid)` casts `b` only where `a` is null.
  If nothing already casts them, guard the cast so it answers `null` instead of raising.
- An index expression must be immutable. Guard a uuid cast with a regex (`~` is immutable), not
  `pg_input_is_valid` (stable; `create index` refuses it). A read can use `pg_input_is_valid`,
  which accepts exactly what the cast accepts.
- A census answer above `0` refuses the deploy. Count only rows that are a reason to stop. When the
  rows are merely degraded and production's data cannot be counted beforehand, make the migration
  total over them, leave the census at `select 0`, and log their count at each boot through the
  same expression the index uses, so the count and the index cannot describe different rows.
- Say where that count surfaces and what the left-out rows lose, in the migration's comment and the
  README paragraph: a guard turns a failed boot into rows that quietly behave differently.

## Evidence

- The first index of sjawhar/legion#2483 was `((anchor->>'artifact_id')::uuid) where anchor is not
  null`. Its comment argued the cast could not fail because `asks_anchor_artifact` (migration 0032)
  already cast it, but 0032 indexes `coalesce(block_artifact_id, (anchor->>'artifact_id')::uuid)`,
  which casts the anchor only where `block_artifact_id` is null. A real boot over an ask holding
  both columns with `artifact_id` `not-a-uuid` exited 1: `apply migration 137: ... invalid input
  syntax for type uuid: "not-a-uuid" (SQLSTATE 22P02)`, schema left at 136.
- A census that counted those rows (`not pg_input_is_valid(...)`) made `envoy-dispatch census`
  answer `REFUSED ... its census counts 1`. That was rejected: nobody could count production's
  asks, so it could stop a production deploy until someone edited the data by hand.
- The shipped shape: `store/migrations/0148_asks_anchor_artifact_id.up.sql:31` indexes `case when
  anchor->>'artifact_id' ~ '<lower-case uuid>' then (anchor->>'artifact_id')::uuid end`; its census
  is `select 0` (`.census.sql:7`); `store.AsksOutsideAnchorArtifactIndex` counts the left-out rows
  through `store.CanonicalUUID`, the same expression, and `cmd/dispatch/main.go:261` logs it as a
  warning without stopping the boot. `TestMigration0148BootsOverAnAskWhoseAnchorArtifactIDDoesNotCast`
  pins it.
