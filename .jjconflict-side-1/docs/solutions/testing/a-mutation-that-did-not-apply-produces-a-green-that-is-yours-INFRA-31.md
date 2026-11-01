---
title: "A mutation that did not apply produces a green that is yours, not the check's: confirm the mutation landed before reading its result, and report the near-miss"
category: testing
tags:
  - mutation-testing
  - controls
  - conduct
  - review
date: 2026-10-21
status: active
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
---

# A mutation that did not apply produces a green that is yours, not the check's

The tester of INFRA-31 set out to prove a documentation fence could fail: strike a path from the
reclaim paragraph's delete list, re-run, watch the test go red. The test passed. **The mutation
had not applied** — so the green was its own edit's, not the fence's, and reported as a pass it
would have been evidence for exactly nothing. It said so, in its handoff, as a near-miss it had
caught rather than a result.

Ten minutes later the reviewer ran the same experiment on the same paragraph and hit the identical
thing: its first strike of `- `/legion/uv/cache`` did not apply either, and the tests passed. It
checked instead of recording the green — **because it had just read the tester's note** — struck
the bullet for real, confirmed the list no longer carried it, and got the failure it was looking
for.

The rule, as the reviewer stated it:

> **Confirm a mutation applied before reading its result.** Print the mutated artefact — the
> rendered block, the list, the constant — and see the change in it. A mutation test has two
> steps that can fail silently, and the first one is yours.

And the conduct half, which is why the second instance cost ten minutes instead of a round:

> **A near-miss reported in time is worth more than the error it did not cause.** The tester
> gained nothing by writing down a mistake it had already corrected; the reviewer is the one it
> paid, and neither of them could have known in advance which way that would run.

A process that only records outcomes collects none of this. Write the near-miss where the next
role reads it — the handoff, not a message that scrolls away — and write it as soon as you catch
it, while it is still cheap to admit.
