---
title: "Health Tick Baseline Isolation: Preventing Shared Mutable State Oscillation"
category: daemon
tags:
  - health-tick
  - state-delta
  - shared-mutable-state
  - baseline-oscillation
  - cache-only-mode
  - dependency-injection
date: 2026-04-11
status: active
module: daemon
related_issues:
  - "420"
symptoms:
  - "state delta notifications spam controller with repeated issues every tick"
  - "same issues cycling through state deltas every 40-60 seconds"
  - "envoy_unsubscribe from notifications.role.legion-controller gets overridden within seconds"
  - "false new/removed deltas on every health tick"
---

# Health Tick Baseline Isolation

## Problem

`runPostCollectionProcessing()` in `server.ts` maintains a `previousIssueState` baseline for computing state deltas. Three code paths call this function:

1. `POST /state/collect` — controller-initiated
2. `POST /state/fetch-and-collect` — controller-initiated, may include extra projects
3. `fetchAndProcessState()` — daemon health tick, every ~60s, fetches only from `opts.legionId`

When the health tick fetches a different issue set than the controller (e.g., single board vs multi-board, or timing differences), it overwrites `previousIssueState`. The next controller collection then computes a delta against the health tick's baseline, producing false "new"/"removed" deltas every tick.

This also caused a secondary symptom: the controller appeared to be "re-subscribed" on every tick. In reality, `subscribeControllerToEnvoy()` was only called at startup — the spam came from `publishStateDelta()` delivering via `notifications.role.legion-controller` role routing on every tick because each tick produced a (false) delta.

## Root Cause Pattern

**Two callers with different semantics sharing one mutable baseline.** The health tick needs cache warming (for dispatch validation), not delta computation. But `runPostCollectionProcessing` unconditionally did both, and whoever called it last owned the baseline.

## Solution: `skipDelta` Option

Add an optional `{ skipDelta?: boolean }` parameter to `runPostCollectionProcessing()`:

- **Cache updates always run** — `issueStateCache` and `issueTitleCache` are populated regardless of `skipDelta`, because dispatch validation depends on them
- **Delta computation only runs when `!skipDelta`** — `computeStateDelta`, `publishStateDelta`, and the `previousIssueState = currentDict` baseline update are all gated behind `!options?.skipDelta`
- **Health tick passes `{ skipDelta: true }`** — cache-only mode
- **HTTP handler call sites pass nothing** — delta behavior unchanged for controller paths

This keeps `runPostCollectionProcessing` as a single entry point rather than splitting into separate cache/delta functions, avoiding duplication of the `state.issues` iteration.

## Key Decisions

1. **Single function with mode flag, not two functions.** The shared setup (iterating `state.issues`, populating caches) would be duplicated if split. The `skipDelta` flag makes caller intent explicit at the call site.

2. **Bug 2 was a symptom, not independent.** The "re-subscription on every tick" was actually `publishStateDelta` delivering via role routing because the oscillating baseline produced a delta every time. No subscription code was in the tick loop.

3. **Injectable fetcher for testability.** `fetchAndProcessState` hardcoded `fetchGitHubProjectItems`. Changed to `opts.fetchProjectItems ?? fetchGitHubProjectItems` to match the existing DI pattern already used by `/state/fetch-and-collect`.

## Testing Pattern

For "stop emitting X when Y" fixes, test both directions:

- **Negative cases**: Health tick interleaving produces zero false deltas (same issue set AND different issue set)
- **Positive regression**: Controller-initiated `fetch-and-collect` still publishes real deltas when data changes
- **Cache verification**: Health tick still populates `issueStateCache` (the behavior it's supposed to have)

Expose the tick function (`fetchAndProcessState`) from `startServer()` for deterministic test orchestration — prefer this over mocking `setInterval`/`setTimeout`.

## Generalization

When adding a function that mutates shared state (baseline, cursor, counter), ask: **does every caller intend to advance that state?** If not, the function needs a mode flag or separate state per caller. This applies to any future processing hooks added to `runPostCollectionProcessing` or similar shared paths.
