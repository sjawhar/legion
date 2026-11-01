---
title: "A close by room name closes whatever room the name holds: pin the instance you checked, then close it"
category: envoy
tags:
  - dispatch
  - ygo
  - concurrency
  - room-lifecycle
  - go
date: 2026-10-25
status: active
module: envoy
problem_type: logic_error
component: service_layer
symptoms:
  - "A close of a Dispatch document room that stands behind the durable head drops the browsers of the room that replaced it"
  - "A write committed on a reloaded room moves its cursor past an update the room never took, and a version records text missing that update"
  - "A close the service checked against the live instance still closes a different instance"
root_cause: concurrency
resolution_type: code_fix
severity: high
---

# A close by room name closes whatever room the name holds

## Problem

Dispatch closes a document room that stands behind the durable head (another envoy-dispatch
process appended an update this room never took) so the next access reloads it at the head.
Two callers do this: the stale-room evictor (`closeRoomBehindHead`) and a settlement the durable
gate refused (`reloadRoomBehindHead`), both in
`packages/envoy/internal/dispatch/docs/service.go`.

Each first checked that the room still held the instance it found behind the head
(`GetDoc(room) == live`), then called ygo's `Server.CloseRoom(room, force)`. That check is not
enough, for two reasons in ygo (sjawhar/ygo v1.51.3-sami.1):

1. `CloseRoom` (`provider/websocket/inject.go`, line 491) takes a **name**. It closes whatever
   room the name holds when it looks the name up, not the instance the caller checked.
2. ygo's idle sweep (`provider/websocket/idle_sweep.go`, `evictIdleRoom`) retires a room without
   the close gate that every other close of the service takes (`roomServer`). So the gate does
   not order the sweep against the check.

Between the check and the lookup, the sweep can retire `live` and a load can put a replacement
under the same name. The close then drops the replacement's browsers and the state that a write
on the replacement holds.

A second race is inside the service itself: a write admitted between the "no writer in flight"
check and the close is forgotten with the room's state. The room that replaces it loads knowing
nothing of the write, while the write's commit moves the new room's cursor past its update
(`Ledger.takeRoomCursors`). A version read from that room then finds no writer to wait for and
records text missing the write at a cursor that says it holds it.

## Fix

The close pins the instance, then checks, then closes (`closeRoomInstance`):

1. Run the close inside a `Server.Apply` on the room (with `WithoutRecoveryWait`). The idle sweep
   never retires a room while an Apply is in it: an Apply holds the room's `inflight` count from
   entry until it returns, and `evictIdleRoom` refuses a room with `inflight > 0`.
2. Inside the Apply, compare the held document with `live`; leave if they differ.
3. Take the close gate and compare again (`roomServer.closeInstance`), then call `CloseRoom`. With
   both held, the room that holds `live` is the room ygo closes.

Before that, the caller reserves the room under the state lock, in the **same hold** that found
no write in flight (`reserveRoomCloseLocked`): it marks the state `reloading`, moves the room's
generation and takes over any settlement. `openLiveWrite` refuses a transaction the writer slot of
a reloading room with `ErrServiceUnavailable` (it does not wait: the close's persistence flush
takes the document's advisory lock). `closeReservedRoom` forgets the state when it closed the
room, or lifts the mark when it did not, and arms the settlement it owes either way.

## Rule

- An API that closes, evicts or replaces by **name** acts on whatever the name holds at its own
  lookup. A check of the instance made before the call is stale unless every path that can
  retire or replace the instance is held off for the span between check and call. A lock that
  only some of those paths take does not hold them off.
- Find every retirement path of the library, not only your own. Here the one that took no gate
  was the library's idle sweep.
- A "nothing in flight" check and the action it licenses go in one hold of the lock that admits
  new work, or the check reserves the resource so new work is refused until the action ends.

## Tests

`packages/envoy/internal/dispatch/docs/stale_room_test.go`:

- `TestACloseOfARoomBehindTheHeadLeavesTheRoomThatReplacedIt`: retires and reloads the room after
  the reservation and before the close; the replacement must stay open and writable.
- `TestACloseOfARoomBehindTheHeadHoldsItAgainstTheIdleSweep`: the idle sweep must not retire the
  room while the close holds it.
- `TestAWriteOpeningWhileARoomBehindTheHeadClosesIsRefused`: a write that opens after the
  reservation is refused, and its retry is made and stored.

Mutation evidence (INFRA-218): closing by name (`CloseRoom` after the check, mutation m1) fails
the first test ("the close of the retired room closed the room that replaced it"); removing the
Apply pin (m2, m2b) fails the second ("the idle sweep retired the room while the close held it").

## Related

- `docs/solutions/database-issues/a-transaction-never-waits-in-memory-for-work-that-takes-database-locks.md`:
  why the refused write fails fast instead of waiting for the close.
