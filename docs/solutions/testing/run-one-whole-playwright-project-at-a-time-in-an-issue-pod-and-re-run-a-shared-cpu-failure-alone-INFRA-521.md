---
title: "Run one whole Playwright project at a time in an issue pod, and re-run a shared-CPU failure alone"
category: testing
tags:
  - playwright
  - issue-pod
  - harness
  - timeouts
date: 2026-10-27
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-521"
  - "sjawhar/legion#2313"
---

# Run one whole Playwright project at a time in an issue pod, and re-run a shared-CPU failure alone

Extends docs/solutions/testing/a-layout-change-is-proven-by-the-whole-project-and-the-rig-needs-contracts-built-and-three-engines-it-cannot-start-INFRA-92.md.

**Update (2026-11-01, INFRA-317): the suite can now run its workers in parallel, and CI does.**
`DISPATCH_E2E_WORKERS` is how many Playwright workers a run has, unset or empty being one, and
`packages/dispatch/e2e/run.ts` starts one lane per worker before Playwright: a whole harness —
server, the three fakes, the plain-HTTP proxy, a bucket, ports the OS chose — and a Postgres of
its own, found by the worker through `TEST_PARALLEL_INDEX`. In a pod the Postgres comes from
`DISPATCH_E2E_PG_BIN`, a directory holding `initdb` and `pg_ctl`. The `dispatch` CI job runs the
whole suite that way. None of that changes the rule below: a pod has two CPUs, so a role there
runs the files it is working on, one project at a time, at one worker.
`packages/dispatch/AGENTS.md`'s end-to-end section has the model.

- An issue pod has two CPUs. Two or three whole-project Playwright runs side by side, each on a
  harness of its own, overrun the 30 s test timeout in the heaviest rows (the collaborative
  margin, the board's touch rows, document anchors), and fail them.
- For proof, run one whole project at a time. When runs did share the pod, re-run every failure
  alone before you count it. Report it as a load failure that passed alone, and show the base
  failing the same way under the same load.
- A long-running harness is a background job under `setsid nohup`. Wait on its PID with
  `tail --pid=<pid> -f /dev/null` in a named service, not with a sleep loop.

## Evidence

sjawhar/legion#2313: the head `iphone` run shared the pod with two other whole-project runs: 217
passed and 5 failed in 44.8 min, and those 5 passed alone in 1.6 min. The head `chromium` run: 758
passed and 3 failed, and the 3 passed alone. Under the same load at base, `board.e2e.ts:652` and
`document.e2e.ts:114` failed the same way. The tester's `iphone` run with nothing else in the pod:
223 passed, 0 failed, 21.2 min.
