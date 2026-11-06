---
title: "Adopt a convention the base owns together with the code on the base that reads it"
category: legion
tags:
  - forward-merge
  - migrations
  - merge-ref
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2163"
---

# Adopt a convention the base owns together with the code on the base that reads it

Extends docs/solutions/github/a-pull-request-github-calls-mergeable-can-still-be-unmergeable-in-substance-INFRA-66.md.

- When your branch renames files to a convention its base introduced (a migration name, a fixture name, an id format), the code that reads that convention is on the base too.
  Rename and merge the base forward in the same round, or do neither.

- A pull request's CI builds the merge ref, so a break that exists only at your branch's own head is green on every lane.
  After such a rename, run the tests that parse the new names at the branch head itself before you push.

## Evidence

Round 24 (`702c4ee9`) renamed this change's two migrations to main's timestamp rule (`20261101163106_parked_decisions`, `20261101163107_claim_delivered_rows`) without merging main.
The branch's `0001_init.up.sql:4` still declared `version integer primary key`, and main's widening of that column (`widenSchemaVersion`, `packages/daemon/internal/store/store.go:238`) was not on the branch.
At the branch head every database-backed test failed with `unable to encode 20261101163106 into binary format for int4`;
CI, testing the merge with main, was green.
The tester caught it; the forward merge `bc514a0d` (round 25) fixed it with no change to the diff's lines.
