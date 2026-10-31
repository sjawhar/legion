---
title: "Pin generated test data by a digest of the whole structure in the generator's order, not by its totals"
category: testing
tags:
  - test-design
  - mutation
  - load-test
  - seed
date: 2026-10-29
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2469"
---

# Pin generated test data by a digest of the whole structure in the generator's order, not by its totals

- When a test says it pins the data a seeded generator makes, hash all of it. Write every
  element, with every drawn field, into one sha256 in a fixed serialization, and assert it against
  a pinned hex constant. Totals (how many items, their sum, how many are non-empty) stay the same
  when a draw only moves values between items. That is the change most likely to break a
  measurement: two seeds with different contents still pass every check.
- Serialize in the generator's own order. Do not sort or copy before hashing: a re-sort in the
  test hides a change of order in the generator.
- Prove the pin with a mutation that keeps the totals, such as a different slice of the same
  permutation. Run it with `go test -overlay` against a scratch copy and quote the failure.
- Drop any assertion an earlier assertion already implies. It reads as a second check and is not
  one.

## Evidence

- sjawhar/legion#2469's `TestMakeRefPlan` (`packages/envoy/loadtest/refs_test.go`) at first
  asserted 1,000 targets, 19,471 versions and 310 citing issues. Review round 3 replaced
  `rng.Perm(refTargetCount)[:m]` with the permutation's last `m` elements in a `go test -overlay`
  copy, and the test still passed. That mutation changes which targets each issue cites, so every
  seed would hold different references.
- Since 9511fa836f8a, `refPlanDigest` hashes each target's number, issue, version count, writers
  and per-version writer, then each LGA key with its citations, against
  `refPlanSHA256 = 7414809c…`. The same mutation fails with
  `the plan hashes to b37b4497…, want 7414809c…`.
- Round 4 found that the digest sorted copies of keys and citations that `makeRefPlan` had
  already sorted. Since c281bc3c6d8e it ranges over `p.lga` and `p.sources[key]` as made. The
  constant did not change, and the mutation still fails it. Round 3 also dropped a
  `cites == 0` check that could not fail once `cited == 310` passed.
