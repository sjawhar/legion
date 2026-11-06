---
title: "A new duty.Exclusive caller raises leasePoolSize, and the server checks the count"
category: envoy
tags:
  - connection-pools
  - leases
  - maintenance-loops
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/store
---

# A new duty.Exclusive caller raises leasePoolSize, and the server checks the count

- When you add a Dispatch maintenance loop that takes a lease through `duty.Exclusive`, add one to
  `leasePoolSize` in `packages/envoy/internal/dispatch/store/pool.go`, add the loop's name to the
  list in that constant's comment, and reserve the loop's connection where `cmd/dispatch/main.go`
  wires it (`reserve("<the caller>")`, `store.LeaseCallers.For`).
- Count callers, not lease names.
  A caller that can be inside `Exclusive` at the same moment as another needs its own connection.
  Each delivery durable's intake is its own caller,
  and each intake's hold is two more: its renewal goroutine and the intake's own.
- The count is checked, since sjawhar/legion#2461.
  Every caller reserves its connection by name while the server wires itself,
  whether or not this configuration runs its loop,
  and `store.LeaseCallers.Complete` refuses to let the process serve unless the reservations are
  exactly `leasePoolSize`, naming the count, the size and the reserved names.
  So a merge that brings in a caller without a connection, or a connection without a caller,
  fails at the server's start rather than leaving the extra caller to wait for a connection for
  as long as the longest pass, which can be minutes for the compaction.

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
- The count is a hand-kept constant no longer.
  In sjawhar/legion#2461 `store.Pool.Leases` stopped handing out the pool and hands out
  `store.LeaseCallers`, whose `For` is the only way to a connection,
  and `cmd/dispatch` checks the reservations against the size before it serves.
