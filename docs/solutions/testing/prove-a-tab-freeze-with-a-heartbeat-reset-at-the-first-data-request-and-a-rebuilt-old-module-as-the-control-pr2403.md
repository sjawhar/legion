---
title: "Prove a tab freeze with a heartbeat reset at the first data request, and a rebuilt old module as the control"
category: testing
tags:
  - playwright
  - main-thread
  - dispatch
  - negative-control
date: 2026-10-28
status: active
module: packages/dispatch/e2e
---

# Prove a tab freeze with a heartbeat reset at the first data request, and a rebuilt old module as the control

Extends docs/solutions/dispatch-web/draw-untrusted-content-with-a-layout-engine-in-a-worker-whose-clock-starts-at-ready-pr2311.md.

- Install a main-thread heartbeat with `page.addInitScript`: a `setTimeout(0)` loop that records
  the largest gap between ticks, plus a reset function.
- Call the reset inside `page.route` for the data request the drawn content needs, on its first
  GET, before `route.continue()`. The window then leaves out bundle evaluation and the first render,
  and the content cannot draw before the answer arrives. Fail the row when that request never comes.
- Push the measured gap into `test.info().annotations` even when the row passes, so the run carries
  its own evidence.
- For the negative control, swap in the old module (relink a dependency the change dropped if
  needed), rebuild the SPA, run the same row, then restore. Set the limit well away from both the
  passing and the failing measurements.
- Measure in the headless engine first (`createHeadlessProof`), which is cheap; the browser row is
  the proof.

## Evidence

- `packages/dispatch/e2e/nested-directive.e2e.ts` resets the gap at the first comments GET
  (lines 73-79) and records it as an annotation (line 107). At the change's head it measured
  377–424 ms. With main's `lib-remark-directive-plugin.ts` swapped in and the SPA rebuilt, the
  same rows failed: one hit the 30 s test timeout, the other measured a 10,962 ms gap.
- Measured from navigation instead, the same fixed parse showed up to 748 ms against the 1,000 ms
  limit on the phone project, because bundle evaluation and the first render counted too. The
  review flagged that as a flake risk (thread PRRT_kwDORFy7ds6wwjjs, fixed in 1642bee9).
