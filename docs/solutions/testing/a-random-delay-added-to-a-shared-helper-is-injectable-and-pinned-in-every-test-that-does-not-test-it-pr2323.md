---
title: "A random delay added to a shared helper is injectable and pinned in every test that does not test it"
category: testing
tags:
  - jitter
  - reconnect
  - flaky-tests
  - hocuspocus
date: 2026-10-28
status: active
module: packages/dispatch
---

# A random delay added to a shared helper is injectable and pinned in every test that does not test it

Extends docs/solutions/testing/a-deadline-that-is-not-the-tests-subject-must-not-be-able-to-fire-in-it-pr2311.md.

- Give a helper that draws a random delay a `random` parameter that defaults to `Math.random`, so
  its own tests pass draws and assert exact values.
- In every test harness that drives the helper's callers with real timers, pin `Math.random`
  (`spyOn(Math, "random").mockReturnValue(0)`), and change the draw only in the test whose subject
  is the spread. A draw near the top of the range outlasts `waitFor`'s default 1 s, so a test that
  never asked about the delay fails now and then.
- After a forward merge, look for tests the other side added that drive the same callers: they
  were written against the old fixed delay and need the same pin.
- Floor any delay handed to the Hocuspocus provider's `initialDelay`: its retry library throws on
  a fractional delay.

## Evidence

Pull request #2323: `packages/dispatch/web/src/api/live.ts:53`
(`reconnectDelayMs(attempt, random = Math.random)`), `web/src/features/doc/connection.ts:141`
(`Math.floor(reconnectDelayMs(0))`), and `web/src/features/doc/connection.test.ts:135-140`, where
main's provider harness, merged in at `8675beea`, gained a pinned draw (default 0) after four of
its new tests failed against the 0-2 s spread.
