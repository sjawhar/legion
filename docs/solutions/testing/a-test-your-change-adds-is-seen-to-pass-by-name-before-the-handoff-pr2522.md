---
title: "A test your change adds is seen to pass by name before the handoff"
category: testing
tags:
  - go
  - verification
  - handoff
  - review-rounds
date: 2026-10-31
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2522"
---

# A test your change adds is seen to pass by name before the handoff

`docs/solutions/testing/read-a-test-runners-counts-line-never-a-tail-of-its-failure-list-INFRA-95.md`
says how to read a suite's verdict. This is the check to run before you read any suite at all.

- **Run the tests this change adds by name, with `-run` and `-v`, and quote each `--- PASS` line
  in the handoff.** It costs seconds against a suite that costs twenty minutes, and it is the only
  check that catches a test which has never been green.
- **A package verdict does not say a new test ran**, and a `-run` of the one test a failure list
  named does not say the others did.
- **A new test's first green is evidence; record where you saw it.** The head it passed at belongs
  beside it, because a later head is a different claim.

## Evidence

- sjawhar/legion#2522 added `TestTheZeroValidDocumentWantsADocument`, whose `BlockIndexes` case
  was red from the moment it was written: `ValidDocument.BlockIndexes` refused with
  `wantsDocument("BlockIndex")`, a function the same change had deleted. The implementer reported
  the pmdoc suite green from its tail (`ok … 936.202s`) and re-ran only the one test a failure
  list had named.
- The tester found it in `envoy-go` and `envoy-go-race` (run 41061824411). The fix cost a
  corrective head, `e4118455`, one word above `ef448091`, the head every CPU measurement of that
  round had been taken at — so the measurements and the merged code no longer shared a commit, and
  the carry had to be argued rather than hashed.
- One `go test -run 'TestTheZeroValidDocumentWantsADocument' -v` at the head that was handed off
  prints eight subtest lines in under a second, and would have shown the red before the handoff.
