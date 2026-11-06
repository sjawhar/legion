---
title: "A condition both sides rewrote merges into a truth table neither side wrote"
category: legion
tags:
  - forward-merge
  - conflict-resolution
  - conditionals
date: 2026-11-04
status: active
module: packages/envoy/internal/store
related_issues:
  - "sjawhar/legion#2588"
---

# A condition both sides rewrote merges into a truth table neither side wrote

Extends docs/solutions/architecture-patterns/a-forward-merge-census-of-a-rule-follows-the-record-it-guards-not-the-function-you-edited-pr2485.md.

- When your branch and main both rewrite the arms of one `switch` or `if` chain that dispatches on
  two or more independent inputs, do not read the merged text as "both sides' changes".
  Write out the merged condition's cases, name the input combination each arm now catches, and
  check each against what each side meant that case to do.
  A text merge joins arms, and joined arms can shadow a case neither side would have given away.
- The case to look for is the one that is new in the merged input space: your side widened an arm
  over an input main had not added yet, so the arm swallows main's new input's case and the rule
  main attached to it never runs.
- Run the test files main added for the feature whose condition you touched, by name, as part of
  the merge.
  Your own suite has no reason to cover a case your branch did not invent, so it stays green;
  main's feature tests are the detector.
- A merge that needs this is not a mechanical merge.
  Record the resolution and its reasoning where the condition lives, so the next merge reads the
  intent rather than guessing it again.

## Evidence

sjawhar/legion#2588 kept a role message no live holder could take.
Its branch had deliberately made a hard role claim an unconditional write
(`packages/envoy/internal/store/kv.go`): the compare-and-swap it replaced refused the loser of two
honest concurrent hard claims with JetStream's wrong-last-sequence error, which the route answered
as a 500 that `POST /v1/roles/set`'s contract does not have.
Main then added the conditional claim (sjawhar/legion#2610), which writes every claim with
`Create`/`Update` and refuses a claim whose `if_claim_revision` no longer holds.

The forward merge at `1ac89c6f28b4` joined the two outside every conflict marker and produced
`case !soft: return roles.Put(role, data)`.
Both sides' lines were present; the merged truth table was neither side's.
A **hard conditional** claim — the normal use of `if_claim_revision` — matched `!soft` and wrote
unconditionally, so main's precondition was never weighed.

The branch's own suite stayed green.
Main's new test file for its own feature failed at once:

```
--- FAIL: TestAConditionalClaimThatLosesTheConditionalWriteIsAMismatchNotAStoreFailure (0.44s)
    conditional_role_claim_test.go:80: the conditional claim never reached a conditional update, so it never raced
--- FAIL: TestAConditionalClaimLostToAClaimOfItsOwnSessionIsStillAMismatch (0.04s)
    conditional_role_claim_test.go:109: the conditional claim never reached a conditional update, so it never raced
```

The settled arm is `case !soft && ifClaimRevision == nil` (`kv.go:1128`), with the whole decision
written above it (`kv.go:1110-1126`): the unconditional write only where the contract has no
refusal for the loser, the compare-and-swap kept wherever a refusal is the point.
