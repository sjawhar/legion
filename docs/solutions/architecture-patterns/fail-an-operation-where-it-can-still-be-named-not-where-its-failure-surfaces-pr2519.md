---
title: "Fail an operation where it can still be named, not where its failure surfaces"
category: architecture-patterns
tags:
  - go
  - concurrency
  - panic-recovery
  - identity
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2519"
---

# Fail an operation where it can still be named, not where its failure surfaces

An in-flight operation recorded under a reusable key — a room name, a document id — cannot be
identified after the fact. By the time the error reaches a caller, the record under that key may
belong to the operation that began next, so the caller fails or deletes the wrong one.

- Fail the operation inside the function that owns it, while it still holds the operation's own
  record, and route every way the function can end short through the one function that takes
  that record: the dependency's refusal, your own validation, and a panic.
- Recover the panic there too, on the operation's own goroutine and before any barrier the
  library closes behind it. A library that recovers the panic for you and hands the error to
  waiters still does it after it has removed what the waiters would use to identify the
  operation.
- Read the library's source for what runs between your function and the next hook of yours on
  that goroutine, and cite it. That window is what decides whether failing here is enough.
- The proof of this shape is the deletion: when the failure is handled where the operation is
  named, every symbol that existed to name it afterwards goes, and the call sites that worked
  around it go back to the library's own call.

## Evidence

`packages/envoy/internal/dispatch/docs/websocket.go` (sjawhar/legion#2519).
`servicePersistenceAdapter.LoadDoc` (`:90`) takes `load := a.service.beginRoomLoad(room)` and has
one `defer`: it recovers a panic into `err` and, for any error, calls
`a.service.failRoomLoad(room, load, err)` with the `*roomLoad` itself in hand (`:92-99`).
`failRoomLoad` does `s.roomLoads.CompareAndDelete(room, load)` and then `s.failRoom(room, cause)`
(`:175-178`), so the record it removes is the one that load began and no other.

`LoadDoc`'s docblock states the window and cites the library for it: nothing of ygo's own runs
between this read and the hook it calls next (sjawhar/ygo v1.54.1-sami.3,
`provider/websocket/server.go:1913-1918`), so "the only way a load fails with nothing of this
service still to run on its goroutine is a panic out of this read" (`:73-79`). It also says why
ygo's own recovery is not enough: ygo "recovers such a panic too and hands every waiter the
load's error (server.go:1904-1923), but it does so with the record this load began still standing
under the room's name and the room healthy, which left the failure to whichever caller's Apply
met it - a caller that cannot tell the load it waited on from the load that began after it, since
ygo removes the room before any waiter wakes (server.go:1932-1941)" (`:84-89`).

The deletion is the proof. `applyRoom`, `failUnfinishedLoad`, `roomLoadWaitedOn`, `roomLoadNumber`,
`loadsBegun` and `beforeFailUnfinishedLoad` appear nowhere under
`packages/envoy/internal/dispatch/`; the ten non-test `s.srv.Apply(` call sites in
`internal/dispatch/docs` are the library's own call again (`service.go` 3, `livewrite.go` 2,
`marks.go` 2, `mutation.go` 2, `testhooks.go` 1); and `websocket.go` is 884 lines, 91 fewer than
before.

The cross-head control is in the pull request body: this head's `websocket_test.go`, run against
the previous head's seven source files copied under `$TMPDIR` with the workspace untouched,
fails `TestALoadThatPanicsFailsItsRoomWhereThePanicIsMet/a caller whose context ended while the
load ran` with `the panicking load left the room healthy, so nothing reloads the document` and
`the panicking load left its record behind, which no instance will take`; both cases pass at this
head. The row needs no seam the fix introduces, so it runs at either head. For the mechanics of
that export-and-run shape, see
`docs/solutions/testing/prove-a-go-acceptance-test-fails-on-base-by-running-the-unchanged-file-in-a-git-archive-export-INFRA-487.md`.
The test itself is `websocket_test.go:461`, two rows: a caller still waiting for the load, and a
caller whose context ended while the load ran.
