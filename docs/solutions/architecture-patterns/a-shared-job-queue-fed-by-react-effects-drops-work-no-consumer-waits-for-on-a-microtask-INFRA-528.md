---
title: "A shared job queue fed by React effects drops work no consumer waits for, on a microtask"
category: architecture-patterns
tags:
  - react
  - layout-effect
  - serial-queue
  - cancellation
  - web-worker
date: 2026-10-27
status: active
module: packages/dispatch/web
related_issues:
  - "INFRA-528"
  - "sjawhar/legion#2311"
---

# A shared job queue fed by React effects drops work no consumer waits for, on a microtask

When components enqueue jobs on a shared serial worker from their effects:

- An effect's cancel removes its own callbacks from each job's waiter list; clearing a flag the
  callback reads leaves the job queued and the worker still spends its time on it.
- A job left with no waiters is never sent; the job in flight with no waiters is stopped (for a
  worker, `terminate()`), nothing is cached for it, and the queue moves on.
- Run that sweep on `queueMicrotask`, never inside the cancel: React runs a layout effect's cleanup
  and its next run in one commit, so a component that re-paints and asks for the same job again
  must re-register before the sweep, keeping its place and the draw in flight.
- Test both halves: unmounting the only consumer stops the job and the next one starts without
  waiting for any deadline; a re-paint mid-job neither stops nor re-sends it.

## Evidence

sjawhar/legion#2311, `dropAbandoned` and the cancel in
`packages/dispatch/web/src/features/refs/mermaid-diagrams.ts` (fix b9e7ea3b). Before it, leaving a
page of two 450-node diagrams delayed the next page's trivial diagram by 11,378 ms in Chromium;
after it, 1,573-2,212 ms. Running the sweep synchronously makes the re-paint case
(`MarkdownBody.test.tsx`) fail, because the live view's per-frame repaint then aborted the draw.
