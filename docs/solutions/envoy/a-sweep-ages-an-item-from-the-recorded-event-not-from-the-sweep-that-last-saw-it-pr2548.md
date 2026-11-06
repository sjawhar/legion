---
title: "A sweep ages an item from the recorded event, not from the sweep that last saw it"
category: envoy
tags:
  - settlement
  - sweep
  - ttl
  - room-state
date: 2026-10-31
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2548"
---

# A sweep ages an item from the recorded event, not from the sweep that last saw it

A periodic pass that keeps an item "while some condition holds" must age it from a **recorded
timestamp of the condition ending**, never from the last pass that still saw the condition. The
two differ by up to one whole period, and the difference is the entire grace the feature promises.

- **Record the event where it happens.** The moment the last connection, lease or claim of a
  holder goes, write the time under that holder; clear it when the holder returns. A sweep that
  only resets "first seen" while the holder is present gives an item that lost its holder a second
  before the next pass no grace at all.
- **Age from the later of the item's own first sighting and that event.** Re-arm the next pass from
  the same instant, so the deadline the documentation states is the deadline the code keeps.
- **Place the test's event just before the next scheduled pass.** A test whose event lands right
  after a pass is green against the broken code, which is exactly how the defect shipped.
- **Prune the event map where the pass already walks the state.** An entry older than the period
  can no longer change a decision.
- **Write and read the key under every spelling a client can send.** A browser writes a bare login;
  a server write carries `user:<login>`. The lookup answers both or the rule silently never fires.

## Evidence

sjawhar/legion#2548 keeps a document's provisional comment mark while its author still has a
browser on the room (`sweepUnrecordedMarks`, `packages/envoy/internal/dispatch/docs/marks.go`).
The first implementation reset the mark's first-seen time at each sweep that saw the author and
re-armed settlement a full time-to-live later, so on a quiet document:

1. a sweep at t0 sees the author, and the next is due at t0+60 s;
2. a two-second socket drop — an ordinary idle reconnect — starts at t0+59 s;
3. the sweep at t0+60 s finds no author and removes the mark one second after the drop, not a
   minute after it.

That is the failure the whole change exists to stop, and
`TestAnUnrecordedMarkIsSweptOneTTLAfterItsAuthorDisconnects` was green against it, because its
disconnect came immediately after a sweep. The fix records each user's departure in
`roomState.departed` from `removeConnection` (`websocket.go`), clears it on `addConnection`, and
ages from the later of that and the first sighting; the test that pins it drops the author's
browser one second before the re-armed sweep, and a control that ignores the recorded departure
fails it.
