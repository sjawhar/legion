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

- When you add a Dispatch maintenance loop that takes a lease through `duty.Exclusive`, add one to
  `leasePoolSize` in `packages/envoy/internal/dispatch/store/pool.go`, and add the loop's name to the
  list in that constant's comment.
- Count callers, not lease names. A caller that can be inside `Exclusive` at the same moment as
  another needs its own connection. Each delivery durable's intake is its own caller.
- Nothing checks the count. A pool one connection short makes the extra caller wait for a connection
  for as long as the longest pass, which can be minutes for the compaction. It does not fail. So
  recount the production `duty.Exclusive` callers with a grep whenever you add one, or merge a branch
  that may have added one.

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
- The size appears only in `pool.go`. Neither `duty/` nor `packages/envoy/AGENTS.md` mentions it.
