---
title: "A cursor that starts from a safe horizon waits for a proven one on its first pass"
category: database-issues
tags:
  - cursor
  - events-horizon
  - dispatch
date: 2026-10-31
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# A cursor that starts from a safe horizon waits for a proven one on its first pass

Extends docs/solutions/database-issues/an-id-cursor-reader-of-a-table-whose-ids-are-drawn-before-commit-stops-at-a-proven-horizon-pr2392.md.

- A new `events.Horizon` answers 0 from `Safe` until a later observation proves an earlier one, because one snapshot never proves its own newest id. A reader that sets a stored cursor from `Safe` on its first pass after a process starts sets it to 0.
- When `Safe` answers 0, take the horizon with `Await` instead, which returns a value at or above every id committed before the call. Pay that wait once, at boot. A cursor planted at 0 makes every later pass page the whole log from its start.
- Test the first pass with a new Horizon, as a process that has just started has one, not with a Horizon an earlier step of the test already proved.

## Evidence

Pull request sjawhar/legion#2391, commit e91f96bf. The suggestion outcome sweep's first pass on a database moves a null cursor to the newest id at or below the horizon. `TestTheFirstPassOfAJustStartedProcessStartsAtTheNewestEvent` runs that pass with a new Horizon on a log that holds three events. Without the `Await` step it failed with `cursor = 0 after the first pass of a just-started process; want 3`. The held-insert test did not catch this, because its earlier steps had already proved the horizon above 0.
