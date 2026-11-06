---
title: "fs.watchFile reports a missing file with zeroed stats"
category: integration-patterns
tags:
  - bun
  - node
  - fs-watchfile
date: 2026-10-28
status: active
module: packages/claude-envoy
related_issues:
  - "sjawhar/legion#2396"
---

# fs.watchFile reports a missing file with zeroed stats

- Node's and Bun's `fs.watchFile` call the listener with all-zero stats (`ino === 0`) when the file
  is missing as polling starts, or when it is removed. That call is not a change to the file.
- A listener that acts on every call runs one extra pass at startup when the file is missing,
  next to the startup's own explicit pass. Guard it with `current.ino !== 0` and keep the explicit
  startup request.

## Evidence

- `packages/claude-envoy/src/envoy-channel-server.ts:1141` (`if (current.ino !== 0)
  requestHeartbeat()`). The duplicate heartbeat used up the "one immediate retry per outage"
  budget, and the reconciliation test failed 4 of 194 full-suite runs.
- An unconditional heartbeat fails that test 50 of 400 with "Expected: 3 Received: 4".
