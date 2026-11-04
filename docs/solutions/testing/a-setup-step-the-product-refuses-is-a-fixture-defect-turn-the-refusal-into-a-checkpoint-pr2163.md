---
title: "A setup step the product refuses is a fixture defect: turn the refusal into a checkpoint"
category: testing
tags:
  - live-proof
  - fixtures
  - devbox
date: 2026-11-02
status: active
module: scripts/e2e
related_issues:
  - "sjawhar/legion#2163"
---

# A setup step the product refuses is a fixture defect: turn the refusal into a checkpoint

- When a live proof stalls in setup because the system under test undid a setup write, first decide whether the system is right.

- If it is, do not work around it.
  Make the refusal an assertion: wait for the product's own record of it (its log line, the status it writes back), read it from the product's output, and check the state it leaves.

- Move the cases the proof needs onto actors the product permits, and narrow the spec to what the proof can build.

## Evidence

The devbox run at `395494600965` stopped at `trees-parked`:
a stand-in session outside tree 5 parked the root while it waited on a person, and the daemon set the status back (`Engine.sessionStatusWrite`, `packages/daemon/internal/workflow/engine.go:254`, logging `workflow: a session wrote a lifecycle status; the daemon re-asserts its own`).
That rule is main's and right, so spec version 5 dropped the session park, and round 28 (`418b4320`) made tree 5 the check:
`reasserted_status` (`scripts/e2e/parked-decision-devbox.sh:406`) reads the daemon's log line, the run waits for Dispatch to show the status written back by `legion-daemon:<project>`, and tree 5 must still wait on its gate.
The person-parked and operator-parked trees carry the foreign-park case.
The next devbox run passed every A3 and A4 checkpoint.
