---
title: "A real-binary test runs in its own process, and a moved policy keeps its wiring pinned"
category: testing
tags:
  - bun
  - mock-module
  - real-binary
  - wiring
  - bun-serve
date: 2026-10-28
status: active
module: packages/pi-envoy
---

# A real-binary test runs in its own process, and a moved policy keeps its wiring pinned

Extends docs/solutions/testing/bun-mock-module-global-leak.md.

- **A test file that imports a module another file of its package `mock.module`s runs in its own
  `bun test` process.** Make the package `test` script
  `bun test --path-ignore-patterns=<file> && bun test ./<file>`, let CI call that script, and
  say in the package `AGENTS.md` to use `bun run test`. In the file, probe the real API once
  (`beforeAll`) and fail with a message naming `bun run test` when it got the mock.
- **Moving a policy out of an extension into its own module removes the wiring's coverage.**
  The module's tests no longer go through the extension's handlers. Add a table that fires each
  host event through the extension's real handler and asserts the outcome, and prove each row by
  deleting one wiring call at a time.
- **Release a held stand-in request before the stand-in server stops.** `server.stop(true)` in
  Bun waits for fetch handlers still running, and cleanup steps run in reverse order, so push the
  hold's release after the server's own cleanup, or the after-hook times out.

## Evidence

- `packages/pi-envoy/extensions/envoy.test.ts`'s `mock.module("nats")` reached
  `card-delivery-omp.test.ts` when Bun loaded it first: 8 failed with
  `connection.request is not a function`. Fixed in `packages/pi-envoy/package.json` (commit
  6b65dbbd); the probe is in `card-delivery-omp.test.ts` (655b7ac0).
- After `src/card-wake.ts` took the policy (b72b1fd5), deleting four `cardWake.*` calls left
  `envoy.test.ts` green; the first table had no `agent_end` row, so deleting `cardWake.agentEnd`
  still passed (877/0) until 5741f678.
- A round-5 red test in `packages/pi-legion/extensions/controller-start-omp.test.ts` held a
  register request and pushed its release before the stand-in's cleanup; every run timed out in
  the after-hook until commit 5cb1b868 moved the push.
