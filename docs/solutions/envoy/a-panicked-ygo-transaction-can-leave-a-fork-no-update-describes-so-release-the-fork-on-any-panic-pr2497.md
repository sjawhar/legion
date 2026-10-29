---
title: "A panicked ygo transaction can leave a fork no update describes, so release the fork on any panic"
category: envoy
tags:
  - ygo
  - crdt
  - panic
  - dispatch-docs
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2497"
---

# A panicked ygo transaction can leave a fork no update describes, so release the fork on any panic

- Do not judge "this operation changed the document" by the updates a ygo document emitted when
  the operation panicked. ygo (sjawhar/ygo v1.51.3-sami.4, `crdt/doc.go` `transactInternal`)
  emits the update of a panicking transaction, except when phase 2 fails before `OnUpdate`:
  `buildPhase2` panicking, or a type observer that phase 2 fires first. The document then holds
  a change that no update carries.
- So code that keeps a document copy (a write transaction's fork) and a rendering of it across
  operations drops both whenever the operation panicked, whatever its updates say. In
  `docs/mutation.go`, `recoverMutation` reports `panicked` and `liveOperation.abandon` releases
  when `panicked || (!recorded && len(updates) > 0)`.
- To test it, make phase 2 fail for real: in the mutate, install a `text.Observe` callback that
  panics, write inside `fork.Transact`, and remove the observer as the panic leaves the mutate.
  Then assert that later reads, writes and versions of the same transaction see no panicked
  text, and that every version equals the committed document
  (`TestAPanickedWriteLeavesNoForkForTheTransactionsLaterWrites`).

## Evidence

With `abandon` back to `op.recorded || len(op.updates) == 0`, that test fails: `after the panic
the write holds a fork = true and a rendering = true, want neither`, `the fork after the further
write holds the panicked text`, and `version 3 records 7297 bytes of markdown, not the committed
document's 7286 bytes`. The later edit's update was appended but is missing from the committed
document and the room, while the version holds it. The only production observer on a fork is
`watchMargin`, which fills a Go map, so no production route to this panic is known; the release
closes a hazard. sjawhar/legion#2497.
