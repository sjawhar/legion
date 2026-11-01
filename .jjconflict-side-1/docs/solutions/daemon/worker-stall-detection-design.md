---
title: "Worker Stall Detection: Why Blunt Heuristics Fail and What v2 Needs"
category: daemon
tags:
  - stall-detection
  - monitoring
  - health-tick
  - worker-management
  - feature-reversion
date: 2026-04-16
status: active
module: daemon
related_issues:
  - "596"
  - "585"
  - "578"
  - "593"
symptoms:
  - "controller interfering with healthy workers"
  - "worker_stall false positives"
  - "workers pruned unnecessarily"
  - "stall detection causes more harm than good"
---

# Worker Stall Detection: Why Blunt Heuristics Fail and What v2 Needs

## Context

Issue #585 added daemon-level stall detection: when a busy worker's message count stayed flat for >5 minutes, the daemon emitted a `worker_stall` event to the controller via Envoy. Issue #596 removed it the same day because it caused more harm than good.

## Why v1 Failed

### 1. Single-signal heuristic can't distinguish stall types

"Message count flat" has at least three meanings:
- **Genuinely stuck** — worker hung in a tool call that will never return
- **Legitimately busy** — worker running a long test suite, reading large files, or loading context
- **Between turns** — worker idle between tool calls (session stall, not bash hang)

A single metric (message count) cannot differentiate these. The 5-minute threshold was arbitrary — too short for legitimate long operations, too long for actual hangs.

### 2. Automated intervention amplifies false positives

The controller received stall notifications and escalated aggressively:
1. Nudge the worker (interrupts flow)
2. Force verdicts (overrides worker judgment)
3. Prune the worker (kills productive work)

Each escalation step made things worse. A false positive that merely logged would be harmless; a false positive that triggers pruning destroys work.

### 3. No observation-before-action protocol

v1 went straight from detection to notification. There was no intermediate step to verify the stall was real before alerting the controller.

## What v2 Should Do (#593)

The v2 design addresses these failures with a **nudge-then-observe** protocol:

1. **Detect** — message count flat (same as v1)
2. **Nudge** — send Envoy message to the worker
3. **Wait 60s** — observe the response
4. **Classify**:
   - Message count increased AND new text content → **session stall** (resolved by nudge)
   - Message count increased but NO text content → **bash hang** (needs abort + redispatch)
   - Message count unchanged → **bash hang** (needs abort + redispatch)

This two-phase approach means the system only escalates when it has high confidence the worker is truly stuck.

## Design Principles for Daemon-Level Monitoring

1. **Observe before acting** — monitoring should default to observation, not intervention. Log/metric first, automate later.
2. **High confidence thresholds** — only act on signals with very high confidence. False positives that trigger intervention are worse than missed detections.
3. **Differentiate failure modes** — different stall types need different responses. A single heuristic that treats them identically will be wrong for at least one case.
4. **Graceful degradation** — failed or removed monitoring should not impact healthy operations. The health tick loop was preserved when stall detection was removed, showing good separation of concerns.
5. **Start with logging** — build observation-only mode first, validate the signal quality, then add automated responses.

## Clean Removal Pattern

The removal was straightforward because the feature was well-isolated:
- Dedicated interface (`WorkerStallEvent`), function (`publishStallEvent`), and state (`workerMessageSnapshots`)
- Config in its own fields (`stallThresholdMs`, env var, config file key)
- Feedback schema as a separate discriminated union member
- Dedicated test describe block (5 tests, 428 lines)

This isolation made surgical removal possible without touching unrelated code. When building features that might need removal, this level of isolation is valuable.
