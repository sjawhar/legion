---
title: "A query compared with a Go-sorted list is sorted in Go, because CI collates text unlike the pod's Postgres"
category: testing
tags:
  - postgres
  - collation
  - order-by
  - flaky-ci
date: 2026-10-29
status: active
module: packages/daemon
---

# A query compared with a Go-sorted list is sorted in Go, because CI collates text unlike the pod's Postgres

Extends docs/solutions/database-issues/a-batched-lookup-keeps-the-row-loop-and-a-full-order-by-and-its-test-forces-heap-order-against-it-pr2447.md.

- When a test compares a query's text rows with an expected list it sorted in Go (`slices.Sort`),
  read the rows with no `order by` and sort them in Go too. One comparison, one sorter.
- Keep `order by` when the order is what the test asserts, as pr2447's note says; then the
  expected list is written in the database's order, not sorted in Go.
- An SQL `order by` on text follows the database's collation. CI's Postgres collates `en_US.utf8`,
  which ignores punctuation at the first level (`legion-33-checks` before `legion-3-checks`); an
  issue pod's embedded Postgres has only `C`, which is byte order, as Go's sort is. So the pod is
  green and CI is red on the same code.
- To see the CI order in a pod, recreate `template1` under the ICU provider first
  (docs/solutions/testing/recreate-template1-under-the-icu-provider-and-the-pods-postgres-runs-the-collation-test-too-INFRA-56.md).

## Evidence

- sjawhar/legion#2478: `TestABacklogOfOtherPullRequestsDrainsAndAppliesEveryLegionEvent` read
  `processed_events` with `order by 1` and compared it with a `slices.Sort`ed list. Red in the
  required `test` job at 55eb11c8 (run 40762777994, `drain_test.go:230`), green in the pod. Fixed in
  7aa6d160: `processedGitHubEvents` (`packages/daemon/internal/intake/drain_test.go`) selects
  unordered and sorts in Go.
