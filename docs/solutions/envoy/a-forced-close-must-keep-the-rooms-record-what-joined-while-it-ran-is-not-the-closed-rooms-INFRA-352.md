---
title: "A forced close must keep the room's record: what joined while it ran is not the closed room's"
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
  - "A person who opens a Dispatch document while its room is force-closed is left off the next version's author list"
  - "A room that replaces a force-closed one loses its issue's closed flag and the edit source it loaded"
  - "A settlement the replacement room armed runs a settle delay late, or not at all"
root_cause: concurrency
resolution_type: code_fix
severity: high
---

# A forced close must keep the room's record

## Problem

Dispatch keeps one in-memory record per document room (`docs.roomState`,
`packages/envoy/internal/dispatch/docs/roomstate.go`): its connected browsers, its writer slot,
its settlement timer and its pending authors. Four closes of the service force a room out rather
than waiting for its last browser to leave: a failed room's eviction, the close of a room behind
the durable head, `Evict` and `Quiesce`. Each of them threw that record away once the close had
returned.

That is wrong because the record is not the closed room's alone. ygo takes the closed room out of
its map before `CloseRoom` returns, and the service registers a browser when it authorizes the
request, before its room is loaded or joined (`authorize`). So while the close runs:

- a browser opens the document and is registered on that record;
- the room that browser's connect loads registers on it too, with what its load read: the issue's
  closed flag, its ask blocks, the latest edit source and the settlement it armed;
- an edit typed in that room is credited on it.

Throwing the record away threw all of it away. The person's keystroke lost its credit, so the next
version left them off its author list; the replacement room lost its closed flag and its edit
source; and the settlement it had armed ran a settle delay late, or was left to the one-minute
resumption.

## Fix

A forced close keeps the record and lets the ordinary rule decide
(`releaseIfUnusedLocked`/`unusedLocked`): the record goes when nothing holds it and its room has
gone, which is what every other end of a room already did. `unusedLocked` is the one list of
holders: an open writer, a running or armed settlement, an unread edit source, a failure being
recovered from, a connected browser, an update the room's persistence has not taken or appended,
and an in-flight credit whose append has not landed.

One record still has to go: a failed room's, because it carries the failure that a later access
must not read (`roomFailure` refuses a load and a connect on it). `handOverFailedRoomLocked`
replaces it under the state lock: a `newRoomState()` successor takes the connected browsers
through `CompareAndSwap`, or `CompareAndDelete` removes it when no browser is connected. A late
callback of the failed room finishes on the record that counted its work, not on the successor:
`consumeUpdateClass` answers the record it took the class from, and `finishDurableAppend` takes
that record, so no count of the closed room's reaches the successor, where one below zero would
hold it for ever.

Two rules follow the kept record:

- A room that loads onto a kept record takes the edit source the stored document names, none
  included, unless the record itself recorded a newer one (`editSourceMark`, `markEditSource`,
  `rowIsCurrent`). Otherwise another process's edit, which several browsers could have made, is
  credited to whoever edited in this process before.
- Settlement failures end where the room's generation moves (`reserveRoomCloseLocked`, `Evict`,
  `Quiesce`, `SetIssueClosed`), never once the close returns, so the closed room's failures never
  wipe out the failures of the room that replaced it.

The close itself is the service's own injection, so it carries the service's mark
(`withOwnerVerified`): `allowInject` refuses an unmarked injection on a closed issue, and a room
left behind the durable head by the other process's write has to be closed whatever its issue's
state.

## Rule

- A record keyed by a name, where the thing it describes is replaced under that name, belongs to
  the name and not to the instance. Whoever ends an instance may only release what the instance
  held; what arrived while it was ending belongs to whoever arrives next.
- Decide the lifetime in one place (here `unusedLocked`), as a list of holders, and have every end
  of the thing ask it. A path that ends the record itself is a second rule, which will disagree
  with the first.
- A hand-over cannot be done field by field. The first design copied the record's fields into a
  successor, and a review showed an edit typed in the replacement room before the close returned
  still lost its credit, while a successor nothing held leaked. Keeping the one record removed the
  question.
- A count a late callback decrements belongs to the record that counted it. Pass the record, never
  look it up again by name.

## Tests

`packages/envoy/internal/dispatch/docs/room_handover_test.go`:

- `TestAForcedCloseKeepsTheBrowserThatJoinedWhileItRan`, over all five forced closes: the browser
  registered while the close ran keeps its credit.
- `TestAForcedCloseKeepsWhatTheRoomThatReplacedItLoaded`: the closed flag, the ask blocks, the edit
  source and the armed settlement of the replacement's load.
- `TestARoomLoadedOntoAKeptRecordTakesTheStoredEditSource`,
  `TestALoadTakesTheStoredEditSourceOnlyWhenNothingNewerIsRecorded`.
- `TestAForcedCloseKeepsTheFailuresOfTheRoomThatReplacedIt`,
  `TestAnIssueCloseEndsItsRoomsSettlementFailures`,
  `TestQuiesceDropsAnEditSourceRecordedWhileItClosesTheRooms`.

`packages/envoy/internal/dispatch/docs/stale_room_test.go`:

- `TestACloseOfARoomBehindTheHeadIsNotRefusedByItsClosedIssue`.

Mutation evidence (INFRA-352): restoring the old drop fails all seven rows of the two hand-over
tests; a nil edit source, a late failure reset and a quiesce that keeps the source each fail their
own test.

## Related

- `docs/solutions/envoy/a-close-by-room-name-closes-whatever-room-the-name-holds-pin-the-instance-before-closing-INFRA-218.md`:
  which instance such a close reaches.
