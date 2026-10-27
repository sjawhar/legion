---
title: "A deadline that is not the test's subject must not be able to fire in it"
category: testing
tags:
  - flaky-tests
  - timers
  - bun-test
  - parallel
date: 2026-10-27
status: active
module: packages/dispatch/web
related_issues:
  - "INFRA-528"
  - "sjawhar/legion#2311"
---

# A deadline that is not the test's subject must not be able to fire in it

Extends docs/solutions/testing/loop-a-flaky-suite-sequentially-parallel-lanes-of-it-starve-its-timer-tests.md.

- A test-only override of a production deadline is short only in the test that checks the
  deadline. Every other test sets it far past its own length (a minute), so the deadline cannot do
  the work the test asserts was done by something else.
- Judge a test with a timer by the command CI runs (`bun run test` is `bun test --parallel …` in
  `packages/dispatch`), not by its file alone: under load a 100 ms deadline fires before a
  `waitFor` poll that took 250 ms.

## Evidence

sjawhar/legion#2311: the tester's red test `a source no body still waits for is never sent to the
worker` set `resetMermaidDiagrams(100)`. Alone it passed 11 of 11; under the parallel suite the
limit refused `Stuck` 111-129 ms after its send, so `Gone` was sent while its body still waited,
before the unmount at 222-283 ms: 3 of 3 failures (2435 pass / 1 fail). With `60_000` (95dd30e9,
approved by the tree's architect): 2436 / 0, and the test still fails against the code before the
fix.
