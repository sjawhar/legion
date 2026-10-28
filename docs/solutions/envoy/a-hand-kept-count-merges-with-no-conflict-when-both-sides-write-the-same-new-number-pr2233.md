---
title: "A hand-kept count merges with no conflict when both sides write the same new number"
category: envoy
tags:
  - forward-merge
  - counts
  - leases
date: 2026-10-27
status: active
module: packages/envoy
---

# A hand-kept count merges with no conflict when both sides write the same new number

Extends docs/solutions/envoy/a-new-duty-exclusive-caller-raises-lease-pool-size-and-a-shortfall-is-silent-pr2233.md.

- After a merge in which both sides added an item to the same list, recount every hand-kept count of
  that list from the code, whether or not the merge conflicted: a constant such as `leasePoolSize`,
  prose such as "N periodic maintenance loops", and test comments such as "N leases".
- Search both the digit and the word form of each number.
- The counts of one list can differ in what they count: here the loops (nine) and the
  `duty.Exclusive` callers (eleven, since one intake serves two durables) are different numbers.
- The lease-pool size is not only in `pool.go`: the README's loop count and the multiprocess test's
  lease comment count the same loops.

## Evidence

At sjawhar/legion#2233's sixteenth forward merge (b3855fb2), main's #2137 had added the edit-notice
sweep and this branch the ask repair. `packages/envoy/cmd/dispatch/README.md` merged without a
conflict, but each side had written "Eight periodic maintenance loops", and the truth was nine.
`store/pool.go` did conflict, because each side wrote `leasePoolSize = 10` with a different comment,
and the merged tree needed 11. The same thing happened to a contract number in
docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md.
