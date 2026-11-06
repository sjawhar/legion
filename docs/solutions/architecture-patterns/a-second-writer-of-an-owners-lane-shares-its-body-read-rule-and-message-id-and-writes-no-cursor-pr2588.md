---
title: "A second writer of an owner's lane shares its body, read, rule and message id, and writes no cursor"
category: architecture-patterns
tags:
  - shared-state
  - queue
  - jetstream
  - cross-package
date: 2026-11-04
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2588"
---

# A second writer of an owner's lane shares its body, read, rule and message id, and writes no cursor

Applies when one component owns a queue — its order, its cursor, the loop that drains it — and a
second component must put work on that queue, or act on work already there.

- **Share four things, in the package both components import:** the cursor's body, the one read of
  it, the predicate that judges it, and the message id each writer stores a copy under.
  A duration or a struct tag written twice is two components drifting apart with nothing to see it;
  a per-writer message id stores two copies of one message where the stream would have collapsed
  one.
- **Share no write of the cursor.** The second component reads the owner's cursor, honours the
  owner's rule, acts only when its item is the one the owner's loop would take next, and leaves the
  cursor alone.
  The owner keeps the order and the recovery; the second component gains one fast path and no
  second drain to keep in step.
- **The guard is one named predicate**, so a caller that skips it is visible in review, and so the
  rule has one place to change.
- **Prove the agreement with a test in the other component's package**, which drives one writer
  against state the other actually wrote.
  Each package's own tests pass with the two bodies drifted apart, because each writes and reads
  its own copy.
- Where a durable value and a timing constant must be equal, give one of them the other's name
  rather than a second literal with a comment saying they match.

## Evidence

sjawhar/legion#2588. Envoy's listener owns a role's kept lane: the copies, the drain cursor
(`kept`, `claiming`) and the walk that forwards them. Dispatch's outbox is that lane's second
writer — it keeps a copy when its batch finds the role unclaimed or lapsed, and hands that copy
over when a session claimed the role while the keep was in flight.

The first shape had the outbox spell the cursor out for itself: an anonymous struct with the same
two JSON tags, its own not-found handling, and its own claim rule (any non-zero `claiming`
blocked, where the listener's walk ignores a claim older than its window). Review round 12 refused
it: a drifted tag decodes as `kept: 0`, which reads as a lane drained to its start, and then the
oldest copy leads every lane and every handover is skipped with nothing logged.

The settled shape puts all four in `internal/bus`, which both writers import:
`KeptRoleCursor` (`stored.go:105`), `Client.ReadKeptRoleCursor`,
`KeptRoleCursor.ClaimOutstanding` (`stored.go:118`) and `KeptRoleClaimWindow` (`stored.go:87`,
whose doc states the reason: two durations meant to be equal are how two writers of a lane drift
apart). The listener names the shared duration rather than keeping a second literal
(`cmd/listener/role_keep.go:67`, `roleKeepDrainWindow = bus.KeptRoleClaimWindow`), and both writers
store their copy under `bus.KeptMsgID(dedupeKey, role)`, so the stream holds one copy whichever of
them wrote it and whichever retried.

The outbox writes no cursor. Its one guard is `keptCopyLeadsTheLane`
(`internal/dispatch/outbox/role.go:503`): act only when the copy just kept is the first past the
owner's cursor with no claim outstanding, otherwise leave it to the owner's drain, which sends the
lane in order.

The agreement is pinned from the other side:
`TestTheOutboxHandsACopyOverPastTheCursorTheListenersDrainWrote`
(`cmd/listener/dispatch_kept_role_test.go:250`) has the listener's drain write a real cursor and
then has the outbox hand a copy over past it. With the outbox reading the lane from its own record
instead of that cursor it fails with *the holder was handed nothing on its agent subject within
30s*, while both packages' own suites stay green.
