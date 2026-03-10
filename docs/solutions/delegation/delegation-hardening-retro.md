---
title: Delegation Hardening — Implementer Retro
category: delegation
tags:
  - delegation
  - concurrency
  - task-persistence
  - finalize-pattern
  - stale-detection
  - testing
  - resource-accounting
date: 2026-02-15
status: active
module: delegation
related_issues:
  - "LEG-133"
---
# Delegation Hardening — Implementer Retro

## Context

LEG-133 hardened `packages/opencode-plugin/src/delegation/` from a 481 LOC / 5-file prototype to production-ready delegation with 305 tests across 21 files. The work spanned 12 tasks across 5 waves, plus a bug-fix pass after Oracle/Ultrabrain review.

## What Was Hard

### The finalize() insight didn't come from planning — it came from investigating a bug in the original

We started with a plan that had tasks for each feature (persistence, notifications, concurrency, etc.). The `finalize()` primitive — the single idempotent cleanup function that ALL terminal transitions go through — wasn't in the original plan. It emerged when the user asked us to investigate a toast-leak bug in `~/oh-my-opencode/original`. That investigation revealed the root cause: multiple completion paths that each did their own partial cleanup. The fix was obvious once the bug was understood: one function, one place, idempotent.

**Learning:** Bug investigation in the reference implementation was more valuable than upfront design. The plan should have started with "audit the original for bugs" rather than "port the original's patterns."

### The stale detection design went through three iterations

1. **Original plan:** 30-minute wall-clock timeout → auto-cancel. User pushed back: "workers can run >30 minutes."
2. **Revised:** Activity-based polling with `lastActivityAt`. But then: "does OpenCode give us activity signals?" Investigation showed the plugin event model has NO per-message events during prompt execution.
3. **Final:** Add a `messageCount` endpoint to OpenCode itself (indexed COUNT query, negligible cost), poll every 30s, notify parent instead of auto-cancel. The user suggested the endpoint; we confirmed the index existed.

**Learning:** The constraint discovery process (events → no events during execution → need an endpoint → index exists → feasible) required multiple research rounds. Each round narrowed the solution space. Don't try to design the final solution upfront for features that depend on API capabilities you haven't verified.

### The concurrency double-release bugs were invisible to the test suite

Both Oracle and Ultrabrain independently found the same concurrency accounting bugs. The tests covered the happy path and the rejection path separately, but never checked concurrency state AFTER a rejection. The bugs were:
- `concurrencyKey` set before `acquire()` check → finalize releases a never-acquired slot
- Fallback model retry releases original slot, then finalize releases it again

**Learning:** For resource accounting (slots, leases, locks), always assert on the accounting state after every operation, not just the operation's direct output. The test should have called `getUsage()` after every launch/cancel/fail path.

## What I Would Do Differently

### Start with the lifecycle diagram, not the feature list

The plan was organized by feature (persistence, notifications, concurrency). It should have been organized by lifecycle state transitions. The `finalize()` primitive would have emerged in planning rather than mid-implementation if we'd started with "draw every path from creation to terminal state."

### Run the review BEFORE implementing, not after

Oracle and Ultrabrain reviewed the completed implementation and found 3 bugs. If they'd reviewed the plan with the `finalize()` primitive BEFORE implementation, they might have caught the concurrency accounting issues in the design. We did run them on the plan initially, but that was before `finalize()` existed — the plan review and code review happened on different artifacts.

### Don't skip Task 0 (upstream dependency) until the end

Task 0 (adding messageCount to OpenCode) was independent and could have been done first. Instead, we implemented everything else and did Task 0 last. This worked because Task 5 (stale detection) was the only dependent, but it meant Task 5 had to use a fallback path (fetching full messages) during development. In a real deployment, you'd want the upstream change deployed first.

## Patterns That Emerged

### Idempotent finalize as universal cleanup

The pattern: every terminal state transition goes through one async function that:
1. Guards against double-transition (if already terminal, no-op)
2. Does all cleanup in a fixed order (status → result cache → release resources → notify → persist)
3. Provides hook points for future features (concurrency, notification, stale tracking all hook into finalize)

This prevented the toast-leak bug pattern where N completion paths each did M/N cleanup steps.

### Defense-in-depth for tool visibility

Three layers for preventing leaf agents from delegating:
1. SDK `tools` parameter on `promptAsync` — tool never appears to the agent
2. Runtime `isLeafAgent()` check in delegation-tool.ts — catches edge cases
3. `subagent-question-blocker` hook — prevents questions from background sessions

Each layer catches what the previous one misses. The SDK layer is the primary defense; the others are safety nets.

### Notify parent, don't auto-cancel

For stale detection, the parent has context the delegation system doesn't. Instead of making a policy decision (cancel after N minutes), the system reports the observation (no activity for N minutes) and lets the parent decide. This is a general pattern: monitoring systems should report, not act.

### Sequential notification queue per parent

Instead of time-based batching (wait 5s, send batch), notifications fire immediately but are serialized per parent via promise chaining. This gives the parent real-time updates without overwhelming it with overlapping promptAsync calls. The `noReply` flag prevents the parent from waking on intermediate completions while still waking it for the "all complete" message.

## Decisions That Aren't Obvious From the Code

- **Why file persistence instead of SQLite:** The OpenCode database is a 6.7GB SQLite file with 3-4 second session creation times. Adding more tables would make it worse. JSON files in `.legion/tasks/` are isolated, workspace-scoped, and don't contend with the main database.
- **Why 10-minute inactivity threshold:** Messages = user+assistant turns, NOT tool calls. A tool call (like running tests for 5 minutes) doesn't increment the message count. 10 minutes accounts for a long tool execution plus LLM response time.
- **Why reject on concurrency limit instead of queue:** The parent (orchestrator/conductor/controller) should handle scheduling decisions. The delegation system is a resource allocator, not a scheduler.
- **Why `noReply: false` for failed tasks regardless of pending count:** A failure needs immediate attention even if other tasks are still running. The parent might want to cancel the remaining tasks or adjust its strategy.
