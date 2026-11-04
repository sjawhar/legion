---
title: "A record standing in for a cursor you do not write is keyed on the recipient"
category: architecture-patterns
tags:
  - queue
  - ordering
  - idempotency
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/outbox
related_issues:
  - "sjawhar/legion#2588"
---

# A record standing in for a cursor you do not write is keyed on the recipient

Extends docs/solutions/architecture-patterns/a-second-writer-of-an-owners-lane-shares-its-body-read-rule-and-message-id-and-writes-no-cursor-pr2588.md.

When you act on an owner's queue without writing its cursor, the items you delivered yourself stay
in front of your next item, and your next item declines because of a delivery you made. The remedy
is a record of your own, and it has three rules.

- **Key it on the recipient, not on the position alone.** The position means "this recipient has
  everything up to here" and means nothing for any other recipient. Treat a record held for
  another recipient as absent and fall back to the owner's cursor, which is what the queue said
  before your record existed.
- **Record only what a delivery actually reached.** An item your resolution held back, and one
  whose send failed, is still the owner's to deliver, so a later item must find it in front of it
  and decline. Recording an item nobody took is the same reordering with a different cause.
- **Scope the record to the run that made it**, not to the process. It stands for work you did
  with no cursor behind it; the next run resolves the recipient afresh and the owner's cursor is
  the authority again for whatever is left.

## Evidence

sjawhar/legion#2588. Dispatch's outbox hands over a copy it has just kept when a session claimed
the role while the keep was in flight, and it writes no cursor for it. Review round 12 traced the
first consequence: two events of one issue kept in one batch after the claim, the first handed
over, and the second declining because the first was still on the lane — the holder took event N,
then a live event N+2, and event N+1 only from the listener's five-minute sweep.

A per-batch record of `{sequence}` closed that and opened the next: review round 13 traced a role
whose claim changes hands between two keeps of one batch. The second holder was handed the later
copy while the earlier one, which it had never received, was still on the lane for its own drain.

The settled record is `keptHandovers`, made in `sendChains` and dropped with the batch, holding
`{holder, sequence}` per role: `handedTo(role, holder)` answers 0 for a record of any other holder
(`internal/dispatch/outbox/role.go:432`), and `handed(role, holder, seq)` is called only after the
forward reached that holder (`:450`). `keptCopyLeadsTheLane` then reads the lane from past the
greater of the owner's cursor and that record (`:503`).

`TestASecondCopyKeptForARoleIsNotHandedToAHolderTheFirstDidNotReach`
(`internal/dispatch/outbox/role_keep_test.go`) drives the second claim on real NATS. With the
holder dropped from the record it fails: *the session that claimed the role under the second keep
received [dispatch-2], want nothing: dispatch-1 is still on the lane in front of dispatch-2 for
its own drain to send.*
