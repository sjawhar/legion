---
title: "Work only an event starts strands its backlog when that event fires only in the state the work removes"
category: architecture-patterns
tags:
  - background-work
  - durable-state
  - sweep
  - bounded-work
date: 2026-11-01
status: active
module: packages/envoy/cmd/listener
---

# Work only an event starts strands its backlog when that event fires only in the state the work removes

Extends docs/solutions/daemon/a-hold-is-computed-with-the-check-the-action-that-releases-it-uses-INFRA-252.md.

- When you bound a unit of background work - a forward budget, a scan cap, a time window, a retry
  count - write the bounds down as exits, and name for each one what runs the work again.
- "The next claim, the next request, the next miss" is no answer when that trigger fires only in
  the state the work exists to leave. Then the first bound the work stops on strands everything
  behind it, and every sender it already told "accepted" is wrong.
- Three answers together close it: progress resumable from durable state, so a later run picks up
  where this one stopped; a loop inside the component, so a bound ends one pass rather than the
  work; and a periodic sweep that also runs at start, for the one exit no in-process loop covers,
  the process dying.
- Count progress as the durable state moving, not as an item delivered. A pass that only skipped
  expired items has made progress, and treating it as idle ends the work with the backlog on it.

## Evidence

Envoy's listener keeps a role message no live holder can take, answers its sender `202 kept`, and
hands each kept copy to the session that claims the role next (sjawhar/legion#2588). Three
consecutive blocking review findings were the same hole, each one naming exits the last fix left:

- The keep's own trigger is self-negating. A copy is kept only while no live session holds the
  role, and a session that already holds a role never claims it again, so "the role's next claim
  will take it" meant, for a live holder, never. The reviewer put it plainly at
  `role_keep.go:345`: "A drain that stops early is never run again while the holder stays live."
- The walk's bounds were five exits with no second runner: `roleKeepDrainMax` forwards,
  `roleKeepScanMax` copies read, `roleKeepDrainWindow` elapsed, a failed read, a lane another
  listener held. Each stranded the copies behind it until they aged out unread.
- A pass that only skipped expired copies raised no forward, so a lane of 800 expired copies
  counted as four idle passes and ended the work with the fresh copies behind them untouched.
- The work ran in a goroutine under `context.Background()`, so a restart or a rolling deploy ended
  it with no one to start another.

The settled shape is all four answers: a cursor on the stream against the lane's head
(`readKeptLaneHead`), so any later pass resumes; `handOverKeptRole`, which walks again for the
role's holder as it resolves it each pass until the lane is empty; `roleKeepDrainResult.passed`,
which counts a moved cursor as progress; and `startKeptRoleSweep`, every five minutes and at the
listener's start, which hands over every lane holding copies after its cursor whose role has a
live holder. The tester's proof is a SIGKILL mid-drain: 9 of 40 copies delivered, the listener
replaced, and 40 of 40 delivered after it with no new claim made.

## Applies to

Any component that takes durable responsibility for data on the strength of a condition and
schedules the discharge of it on an event: a cache filled on a miss, a reconciler run on a
transition, a retry driven by the next error report, a queue drained on the next connect. Ask of
each bound, before review does: what fires next, and can it fire while the work is undone?
