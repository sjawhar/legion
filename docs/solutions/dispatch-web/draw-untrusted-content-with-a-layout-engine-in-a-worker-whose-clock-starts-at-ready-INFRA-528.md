---
title: "Draw untrusted content with a layout engine in a worker whose clock starts at ready, and end a late job by terminating the worker"
category: dispatch-web
tags:
  - web-worker
  - untrusted-input
  - main-thread
  - timeouts
  - mermaid
date: 2026-10-27
status: active
module: packages/dispatch/web
related_issues:
  - "INFRA-528"
  - "sjawhar/legion#2311"
---

# Draw untrusted content with a layout engine in a worker whose clock starts at ready, and end a late job by terminating the worker

A body any person or agent writes reaches readers who did not choose it, so its rendering cost is
untrusted input. When a third-party layout engine's cost grows with that input:

- **Never run it on the page's thread.** Post each job to one shared `new Worker(new URL(…))`
  and draw only its answer (sanitized) on the page.
- **Start a job's deadline when the worker can start on it**: the worker posts `{ ready: true }`
  once its imports load, and the page arms the timer only when it sends a job to a ready worker,
  so a slow chunk download never refuses (and caches the refusal of) a normal job.
- **End a late job by `terminate()`**, not by abandoning it: that is the only thing that stops a
  layout loop. Start a fresh worker for the jobs still queued, and drop any answer whose request id
  is not the job in flight.
- **Prove the page stays free by timing the page**, not by counting results: sample a 100 ms
  page `setTimeout` several times while the job runs, then assert the refusal.

## Evidence

Mermaid in Dispatch bodies (sjawhar/legion#2311): drawn in the layout effect, ELK took 10.7 s for
a 400-node flowchart; the review's important finding. The fix d9ff6c61 moved it to
`packages/dispatch/web/src/features/refs/mermaid-render.worker.ts` with a 5 s limit
(`mermaid-diagrams.ts`, `sendNext` and `refuseLate`). The e2e row `a diagram too large to draw in
time shows the reason, and the page keeps responding` passes in four engines; the same row against
the previous head measured a 100 ms page timer at 11,014.8 ms.

The deadline timer still runs on the page's thread, so a machine that sleeps mid-job can refuse a
diagram that would have drawn; the refusal is cached until reload.
