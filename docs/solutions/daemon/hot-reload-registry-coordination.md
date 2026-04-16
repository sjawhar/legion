---
title: Hot-Reload Daemon — Registry Coordination and Mock-Masked Integration Bugs
category: daemon
tags:
  - hot-reload
  - registry
  - testing
  - integration-testing
  - mocks
  - process-coordination
  - shutdown
date: 2026-04-16
status: active
module: daemon
related_issues:
  - "sjawhar-legion-598"
---

# Hot-Reload Daemon — Registry Coordination and Mock-Masked Integration Bugs

**Issue:** sjawhar-legion-598
**PR:** https://github.com/sjawhar/legion/pull/600

## Context

Added `legion restart` for hot-reloading the daemon without killing worker sessions. The
architecture separates daemon lifecycle from serve lifecycle: the old daemon shuts down but
preserves the serve process, and the new daemon discovers and adopts it via the legions
registry file.

## What Went Wrong

### Unconditional cleanup of shared coordination state

`shutdown()` unconditionally called `removeLegionEntryFn()` even when `keepServe=true`. This
removed the registry entry that `cleanupStaleServes` needs to discover the preserved serve on
the next daemon startup. Every individual component worked in isolation — serve stayed alive,
`cleanupStaleServes` could find healthy serves, `adoptServe` worked — but the glue between
them was broken.

**Root cause:** The registry entry removal was added as a general cleanup step without
considering that `keepServe=true` changes the shutdown semantics. When you're intentionally
preserving a subprocess, you must also preserve the coordination state that lets the next
process find it.

**Fix:** Wrap `removeLegionEntryFn` in `if (!keepServe)`.

### Unit test mocks hid the integration failure

The "adopts preserved serve PID from cleanupStaleServes" test mocked `cleanupStaleServes` to
return `{ preservedServePid: 42424, preservedServePort: 13381 }`. This bypassed the actual
registry lookup, so the test passed even though the registry entry was being deleted during
shutdown.

The "shutdown with keepServe" test verified that `adapter.stop()` was not called, but didn't
assert that `removeLegionEntryFn` was also skipped.

## Patterns

### When testing "skip this behavior" paths, assert ALL related side effects

When a flag like `keepServe` is supposed to skip behavior, don't just assert the primary
effect (serve not stopped). Assert all related side effects too (registry entry not removed).
The fix was one line: `expect(removeLegionEntryCalls).toHaveLength(0)`.

**Heuristic:** For any conditional skip path, list every side effect in the non-skip path and
add a negative assertion for each one in the skip test.

### Shared state for process coordination requires conditional cleanup

When using files (registry, PID files, lock files) to coordinate between processes, cleanup
must be conditional on whether the coordinated resource is being preserved. Unconditional
cleanup in shutdown is a common default that breaks when you add "graceful restart" or
"preserve subprocess" semantics.

**Pattern:** Any time you add a `keepX` or `preserveX` flag to a shutdown path, audit every
cleanup step and ask: "Does the next process need this state to find X?"

### Mock boundaries should match trust boundaries

The "adopts preserved serve PID" test mocked `cleanupStaleServes` — the function that reads
the registry. This created a trust boundary mismatch: the test trusted that the registry
would contain the right data, but nothing verified that `shutdown()` preserved it.

**Heuristic:** When mocking a function that reads shared state, ask: "Who writes this state,
and is the write path also tested?" If the write path is in a different test with different
mocks, you have a potential integration gap.
