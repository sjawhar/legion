---
title: "A fixture two jobs' tests check each other against is named in every such job's filter: the enumerating filter misses it while the broad one stays green"
category: github
tags:
  - ci
  - paths-filter
  - shared-fixture
  - ci-filters
date: 2026-10-29
status: active
module: .github/workflows
---

# A fixture two jobs' tests check each other against is named in every such job's filter: the enumerating filter misses it while the broad one stays green

Extends docs/solutions/github/join-a-job-to-the-ci-router.md.

- When tests in two jobs exist to agree with each other through one fixture (a Go port and the
  code it ports, each reading the same file), put the fixture in the filter of every job whose
  tests read it, and give each read its `ci_job_reads.py` RULES entry in the same change.
- Check the job whose filter lists fixtures file by file: that is the one that misses a new
  fixture. A job whose filter takes the whole package (`packages/contracts/**`) still runs on a
  fixture-only change, so CI stays green while the comparison has only one side and proves
  nothing about drift.
- A RULES entry alone turns `check-ci-filters.sh` red for the job whose filter misses the file;
  without the entry the check is silent, since it checks only the reads it is told about.

## Evidence

- sjawhar/legion#2478, review round 4: `packages/contracts/fixtures/checks-settlement/listener-rules.json`
  is read by `packages/daemon/internal/daemon/check_settlement_test.go:56` and
  `packages/envoy/internal/cistore/settlement_fixture_test.go:56`. The `test` filter reached it
  through `packages/contracts/**`; `envoy_go` lists its fixtures one by one and did not name it,
  so a change to the fixture alone skipped the Envoy half. Fixed in 72675d21 (`ci.yaml` `envoy_go`,
  three RULES entries); with the RULES and without the filter line the check failed with
  `envoy-go reads packages/contracts/fixtures/checks-settlement/listener-rules.json, which its
  filter envoy_go does not match`.
