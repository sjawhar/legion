---
title: "A settlement refused behind the durable head is a verdict, not a fault: reload the room; and a write that moves the generation owns the settlement it ended"
category: envoy
tags:
  - dispatch
  - settlement
  - multi-process
  - concurrency
  - go
date: 2026-10-25
status: active
module: envoy
problem_type: logic_error
component: service_layer
symptoms:
  - "inject refused: document settlement failed 3 times: ... behind its durable head"
  - "Every transactional write to one Dispatch document answers 503 DOC_SERVICE_UNAVAILABLE until the failed room's eviction finishes"
  - "A document stays unsettled (open asks not indexed) for about a minute after a write that changed nothing"
root_cause: concurrency
resolution_type: code_fix
severity: high
---

# A settlement refused behind the durable head is a verdict

## Problem

### Retrying a refusal that cannot change

With two envoy-dispatch processes on one document, a room can stand behind the durable head:
the other process appended an update this room instance never took. The durable gate
(`refuseRoomBehindHead`, under the document's advisory lock) refuses a settlement from such a
room, so it never writes a version over text missing that update.

`settleRoomWithin` (`packages/envoy/internal/dispatch/docs/service.go`) treated the refusal as a
transient error: it called `retry`, which counts toward `maxSettleFailures` (3). But a room behind
the head never catches up without a reload (`advanceRoomCursor` advances only by head+1), so both
retries were certain to be refused too. The third refusal ran `failRoomLocked`, and until the
failure's eviction finished every transactional operation on the document answered
`ErrServiceUnavailable` (`document settlement failed 3 times`). In production the stale-room
evictor often reloads the room first; the settlement path still wasted three settlements and
failed a room that only needed a reload. The multi-process regression test failed 10 of 10 runs on
main for seeds 1792831380700443036 and 1792844341416325912 (200 random seeds passed), which is why
the property test alone did not catch it.

### A write that opens silently ends a settlement

Opening a live write (`openLiveWrite`) moves the room's generation, and a settlement that sees a
moved generation writes nothing. The write took over only a timer it stopped
(`settleDeferred = stopSettleTimer(...)`). It missed two other states:

- a timer that has **fired** but whose callback has not returned (still registered,
  `isSettleTimerArmed`), and
- a settlement already **running** (`settling > 0`; `settleLastPeer` counts it from the moment
  it takes the timer).

Those settlements met the moved generation and ended. A write that changed nothing publishes no
update, so no observer armed another, and the document stayed owed until the one-minute
resumption of pending settlements.

## Fix

1. On the gate's refusal, roll the transaction back (releasing the advisory lock the close's
   flush needs) and close and reload the room (`reloadRoomBehindHead`) instead of retrying. The
   pending settlement row survives the rollback, and the settlement is armed again on the
   reloaded room, which loads at the head and settles every update. It does not close a room this
   process is still writing: an open live write gets the settlement when it finishes
   (`settleDeferred`), and an update not yet stored re-arms it. The close itself pins the room
   instance; see the companion learning on closing by name.
2. Opening a write takes over every settlement it ends (`takeOverSettlementLocked`): one whose
   timer it stopped, one whose timer fired and is still registered, and one running. The write
   sets `settleDeferred`, and `finishLiveWrite` arms the settlement whether or not the write
   changed anything. The close's reservation (`reserveRoomCloseLocked`) uses the same take-over,
   because it moves the generation too.

## Rule

- Before you retry an error, ask whether the retry can see a different state. A refusal whose
  cause only a reload removes is a verdict; answer it with the reload, not with a retry budget
  whose exhaustion fails the resource.
- Anything that moves a generation counter which ends in-flight work owns that work. Enumerate
  every state the work can be in at the moment of the move (armed, fired but not yet started,
  running) and take each one over; stopping the timer covers only the first.

## Tests

- `pending_authors_test.go`: `TestTwoProcessesListEveryEditorOnExactlyOneVersionForSeedsThatFailed`
  replays the two seeds that failed on main.
- `stale_room_test.go`: `TestAWriteOpenedAsASettlementStartsArmsItWhenItEnds` (subtests
  `its_timer_fired`, `the_last_browser_left`) and
  `TestAWriteOpenedWhileASettlementRunsArmsItWhenItEnds` (a write that ends having changed nothing).

Mutation evidence (INFRA-218): taking over only a stopped timer (m3) fails
`TestAWriteOpenedWhileASettlementRunsArmsItWhenItEnds`; dropping the fired-timer take-over (m5)
fails the `its_timer_fired` subtest; dropping `settleLastPeer`'s early `settling` count (m6) fails
the `the_last_browser_left` subtest. Each times out waiting for the settlement the write took over.

## Related

- `docs/solutions/envoy/a-close-by-room-name-closes-whatever-room-the-name-holds-pin-the-instance-before-closing-INFRA-218.md`
- `docs/solutions/database-issues/a-transaction-never-waits-in-memory-for-work-that-takes-database-locks.md`:
  why a transaction that meets a failed room answers 503 instead of waiting.
