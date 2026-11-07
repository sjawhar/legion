---
title: "A new duty.Exclusive caller raises leasePoolSize, and a shortfall is silent"
category: envoy
tags:
  - connection-pools
  - leases
  - maintenance-loops
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/store
---

# A new duty.Exclusive caller raises leasePoolSize, and a shortfall is silent

- When you add a Dispatch maintenance loop that takes a lease through `duty.Exclusive`, add its
  lease name to `leaseNames` in `packages/envoy/internal/dispatch/duty/exclusive.go` and raise
  `leaseNameCount` in `packages/envoy/internal/dispatch/store/pools.go` with it. `leasePoolSize`
  is `leaseNameCount + 2`, so the pool grows with the list.
- Count lease names, not callers. The pool holds a connection for every name on the list and two
  spare, and `Exclusive` refuses a name that is not on it, so a name added anywhere else takes a
  connection a listed lease counted on. Each delivery durable's intake takes its own consumer
  lease, and each of those names is on the list.
- `TestTheLeasePoolCountsEveryLeaseName` (`store/pool_test.go`) fails when `leaseNameCount` or
  `leasePoolSize` does not match `duty.LeaseNames()`, so a name added to the list without the
  count is caught by the store's tests rather than by a pass that waits for a connection for as
  long as the longest pass, which can be minutes for the compaction.

## Evidence

- In sjawhar/legion#2233, the ask repair (lease `issue-ask-repair`, `api.RunIssueAskRepair`) became
  the ninth caller, and `leasePoolSize` went from 8 to 9.
- Main's sjawhar/legion#2210 then added the block-index reconciliation (lease `block-index`,
  `docs.RunBlockIndexReconciliation`), and each side of the fourteenth forward merge (4440fc7b) held
  `leasePoolSize` at 9. That merge recounted ten production callers on the merged tree, and
  `leasePoolSize` went from 9 to 10. The comment there states that the headroom is zero. The ten:
  - the ask repair, the suggestion outcomes, the architecture import;
  - the delivery reconcile, the reference excerpts, the block index, the document compaction;
  - the webhook redelivery sweep;
  - one per delivery durable through `delivery/intake.go`, which makes two.
- It happened again on the sixteenth forward merge: main's sjawhar/legion#2137 added the edit-notice
  sweep (lease `edit-notices`), and each side held `leasePoolSize` at 10 for its own tenth caller.
  The merged tree has eleven callers, and `leasePoolSize` went from 10 to 11.
- The size appears only in `pools.go`, and the names it is counted from only in
  `duty/exclusive.go`. `packages/envoy/AGENTS.md` states the rule under "Every pool's size is set
  in code" and in its pool table (leases 15: one per lease name, 13, and 2 spare).
