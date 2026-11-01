---
title: "A schema-driven feature no shipped type declares is proven by swapping the schema in a testing-only package"
category: testing
tags:
  - dispatch
  - pmdoc
  - block-schema
  - go-test
  - t-cleanup
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/pmdoc
---

# A schema-driven feature no shipped type declares is proven by swapping the schema in a testing-only package

## The rule

- When a feature takes effect only for a type the shipped schema does not declare yet, prove it by
  swapping the schema in tests, never by shipping a type for the test's sake.
- Put the swap behind an exported hook in the owning package. The hook validates the schema before
  it installs it, changes nothing on a refusal, and returns `restore`. Add a small ordinary package
  that only tests import, so the tests of several packages can share it, which `_test.go` files
  cannot do. Keep the `testing` import there, so the server binary never links `testing`.
- Restore through `t.Cleanup`, never `defer`, and swap before the test builds a server. Cleanups
  run last in, first out, so the restore runs after the server's own cleanup has stopped the
  goroutines that read the schema. A `defer` would restore it first and race them. A test that
  swaps does not call `t.Parallel`.
- For an end-to-end proof, build the real server from a scratch schema file, and restore the
  committed file before the run reads anything.

## Evidence

- `pmdoc.SetBlockSchemaForTest` (`pmdoc/testhooks.go`) and `pmdoctest.UseBlockSchema` and
  `BlockPointerSchema` (`pmdoc/pmdoctest/pmdoctest.go`) in sjawhar/legion#2394. The `api` and
  `docs` tests share them.
- The `ce-simplify-code` pass at `a5b79547` moved the helper out of the server binary.
- A package that only tests import still needs its directory in the path filter of each CI job
  whose tests compile it. Review round 12 found `pmdoc/pmdoctest/*` missing from the docs race
  job's filter (`.github/workflows/ci.yaml`); `.github/scripts/check-ci-filters.sh` names such a
  gap.
