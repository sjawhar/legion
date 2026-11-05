---
title: "A validate-once cutover ends by deleting the exported twin, and its test helper calls the production constructor"
category: envoy
tags:
  - validation
  - pmdoc
  - cutover
  - dead-code
  - test-helpers
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/pmdoc
supersedes: docs/solutions/envoy/validate-once-is-a-type-whose-only-constructors-validate-and-the-exported-functions-keep-their-messages-pr2479.md
related_issues:
  - "sjawhar/legion#2522"
---

# A validate-once cutover ends by deleting the exported twin, and its test helper calls the production constructor

Supersedes one clause of
`docs/solutions/envoy/validate-once-is-a-type-whose-only-constructors-validate-and-the-exported-functions-keep-their-messages-pr2479.md`:
"Keep every exported function validating, as the constructor followed by the method."
That is right while a production caller still holds a plain value, and wrong once the last one
moves. Everything else in that file stands.

- **Grep the non-test `.go` files for each exported function the moment its last production caller
  moves.** An exported `Validate`-then-method twin with no caller outside tests is a second entry
  point, a nil branch each way, and a code path only tests reach. Delete it.
- **Write the validation-in-front-of-the-method shape as a test-only helper that calls the
  production constructor** (`ValidateDocument`), never `ValidDocument{doc: doc}`. A hand-built
  value keeps a production method alive for nothing: it was the only reason `Splice`'s body sat in
  a `spliceValidated` method of its own.
- **Keep a helper's check order where the order is what a refusal depends on.** Reading the shape
  before validating the document costs three field reads in a test and keeps the refusal the tests
  pin; say so in the helper, since the next reader will otherwise simplify it away.
- **A helper that forwards to the method it is testing proves nothing.** Keep a helper whose body
  is an independent implementation of the same answer, and delete the forwarding ones.

## Evidence

- Round-1 review of sjawhar/legion#2522 (thread `r4423453498`, `pmdoc/splice.go:104`): `Splice`,
  `BlockIndex`, `BlockBoundary`, `BlockRange`, `BlockDescendantIDs`, `DeleteTextblock` and
  `InsertTableRows` had no caller outside tests, where the base had 14 production calls, each
  leaving two entry points and a nil branch. They were deleted at `ef448091` and the shape moved
  to `pmdoc/plain_tree_test.go`.
- Round 2 (thread `r4432039538`) found the moved helpers still building `ValidDocument{doc: doc}`
  by hand, which pinned `spliceValidated` alive. Folding it into `Splice` at `226c17a1` was the
  single production Go statement this round changed, under a measurement carry rule that allowed
  the fold and comments alone.
- `pmdoc/plain_tree_test.go`'s `splice` keeps `spliceShape` in front of `ValidateDocument`, so a
  splice whose shape cannot be read is still refused with the invalid-splice error rather than the
  document's; the comment there records the reason and the cost.
- `ValidDocument.BlockIndexes` (`pmdoc/splice.go`) is a single-pass walk that does not call
  `documentBlock`, so the test comparing it with the `blockIndex` helper is a differential; the
  forwarding comparisons round 1 found were deleted instead (thread `r4423453521`).
