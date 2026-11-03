---
title: "A test that recognises a refusal's wording must recompute the claim the wording makes"
category: testing
tags:
  - mutation-testing
  - oracle
  - exhaustive-testing
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2433"
---

# A test that recognises a refusal's wording must recompute the claim the wording makes

Extends docs/solutions/testing/mutation-proof-probe-tests.md.

- A test can sort an error into a branch by matching its text, for example "held back by the
  decision holding it". It must then check that the claim is true. Recompute the claim from the
  rule alone, not from the production plan that wrote the text.
- Before you trust an exhaustive suite, mutate the producer so that it always returns its most
  permissive known branch. If the suite stays green, the test oracle is the bug.
- A "way out" that a refusal offers is a claim too. Test it by taking the way out and following it
  to a valid end. Text that names a condition which never becomes true passes every test that
  only matches wording.

## Evidence

- In review round 11, the mutation
  `if len(askHolderIDs(tree, ask)) > 0 { return askRepairPlan{holder: true} }` in
  `packages/envoy/internal/dispatch/docs/ask_block_text.go` (`planAskRepair`) left every `Ask`
  test green.
- `packages/envoy/internal/dispatch/docs/service_test.go:1600` (`assertHeldBackByItsHolder`)
  applies deletes worked out from the content rule to a copy and requires a holder to read as
  unreadable. The holder-repair rows take the way out and follow it. With the mutation, both
  exhaustive tests fail (`e58298ed`).
- The round-10 wording, "or by an ask edit once the decision holding it reads", named a condition
  that never holds while the held decision is broken, and wording-only tests accepted it.
