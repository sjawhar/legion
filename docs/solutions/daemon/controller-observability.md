---
title: "Controller Observability: Trust the System, Not Your Assumptions"
date: 2026-02-15
category: daemon
tags: [controller, observability, state-machine, debugging]
related-issues: [LEG-122, LEG-124, LEG-125, LEG-128, LEG-130]
---

# Controller Observability: Trust the System, Not Your Assumptions

## Context

During the first full controller integration test session, the controller repeatedly lost track of system state — reporting workers as dead when they were idle, feeding stale data to the state machine, and conflating daemon restarts with worker crashes. Every major confusion stemmed from the same root: bypassing the system's built-in observability and substituting ad-hoc checks.

## The Principle

The Legion architecture has three layers of observability, each with a clear responsibility:

1. **State machine** — knows about issue status, labels, PR state, and live workers. Fed by Linear data.
2. **Daemon API** (`/workers`, `/health`) — knows about worker processes, ports, and session IDs.
3. **OpenCode serve** (worker ports) — knows about session state and messages.

The controller should use layer 1 (state machine) as its primary decision source, layer 2 (daemon API) for dispatch and inspection, and almost never layer 3 directly.

## What Went Wrong

### 1. Hand-crafted state machine input

The controller manually constructed JSON with assumed labels instead of piping Linear output directly. A planner had finished and added `worker-done`, but the controller hardcoded `labels: []` because it assumed the planner died before finishing.

**Fix:** The state machine's parser handles raw Linear MCP output. Pass it through untouched. Never inject assumptions.

### 2. PID-based liveness checks

The controller used `kill -0 $pid` to check if workers were alive. This produced wrong answers because:
- `Bun.spawn` returns the wrapper PID; the actual opencode child has a different PID
- Workers that finish go idle (process alive, session complete) — idle ≠ dead
- Daemon restarts don't reliably kill workers (`opencode serve` ignores SIGTERM — see `docs/solutions/daemon/opencode-serve-lifecycle.md`)

**Fix:** Use the daemon's health tick and `/workers` endpoint. The state machine reports `hasLiveWorker` per issue. Don't independently verify.

### 3. Assuming daemon restarts clean up workers

The controller restarted the daemon multiple times, each time assuming workers were killed. They weren't — `opencode serve` ignores SIGTERM. Zombie processes accumulated on ports, causing cascading port allocation failures.

**Fix:** Don't restart the daemon unless it has a bug. If you must, zombie processes survive and hold ports.

## Patterns

### State goes stale in one loop iteration

Labels, statuses, worker liveness, and PR state all change between controller loop iterations. Never carry state from one iteration to the next. Fetch fresh from Linear at the top of every loop, and let the state machine compute actions from that fresh data.

### The daemon is the controller's window into workers

The controller should never curl worker ports directly, check PIDs, or inspect process tables. The daemon tracks workers, health-checks them, and reports status via its API. If the daemon's view is wrong, fix the daemon — don't work around it.

### Workers are long-lived processes

Workers stay alive after completing their work — see `docs/solutions/daemon/opencode-serve-lifecycle.md` for process lifecycle details. The session's `worker-done` label (not process death) is the completion signal.
