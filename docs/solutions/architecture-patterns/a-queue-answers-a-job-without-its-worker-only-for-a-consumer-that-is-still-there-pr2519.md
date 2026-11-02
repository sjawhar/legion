---
title: "A queue answers a job without its worker only for a consumer that is still there"
category: architecture-patterns
tags:
  - web-worker
  - callbacks
  - failure-handling
  - deadlines
date: 2026-11-01
status: active
module: packages/dispatch/web
related_issues:
  - "sjawhar/legion#2519"
---

# A queue answers a job without its worker only for a consumer that is still there

Extends docs/solutions/architecture-patterns/a-queue-whose-producer-dies-answers-every-waiter-with-a-terminal-value-never-just-clears-pr2472.md.

That document says to answer every waiting callback when the producer dies. This says who counts
as waiting, for a queue whose fallback is not a terminal value but the protected work itself.

- Where the queue's answer without its worker is to **run that work on the thread the worker
  exists to protect**, only a consumer that has not cancelled may be answered that way. A
  deadline, a crashed worker, or no worker at all then costs the page nothing for a job nobody
  reads.
- A place a cancel kept alive, so it can still receive the worker's own answer, is worth that
  answer and nothing else. Mark it on the waiter, and let one predicate over the job's waiters
  decide: is anyone still waiting for an answer of whatever kind?
- Run that predicate at every point the queue answers without the worker, and at the point that
  decides whether a job is still owed at all. A job only kept places wait on is answered by the
  worker or not at all.
- Say in the contract that a consumer which asks to keep a posted job must expect an answer to
  input it has moved on from, and must decide for itself when that answer stops being worth
  using.

## Evidence

`packages/dispatch/web/src/features/refs/worker-queue.ts` (sjawhar/legion#2519). `Waiter` carries
`kept` (`:51`), set by the cancel `wait()` returns when the owner asked for `keepIfPosted` and the
job is the one in flight (`:182-185`). `anyoneIsWaiting(entry)` is
`entry.waiters.some((waiter) => !waiter.kept)` (`:66-68`), and it gates all three places the
queue decides without the worker: `expire()`, the deadline (`:319-321`); `giveUp()`, no worker
will run (`:333-335`); and `dropAbandoned()`, which filters the queue and stops the job in flight
(`:352-356`).

`expire()`'s comment states the defect the gate closes: "A place kept on a posted job is not one
(`Waiter`): the deadline of a body whose element has gone would otherwise park that parse on the
thread of a reader who left it" (`:314-318`). The consumer is the Markdown parse:
`withoutTheWorker` and `withoutAWorker` are both `parseOnThePage`
(`packages/dispatch/web/src/features/refs/markdown-engine.ts:339-340`), which parses the body on
the page's own thread (`:254-263`), and the surface that keeps a posted job is the live view's
streaming turn, through `RenderWithEngineOptions.keepAPostedParse` (`markdown-engine.ts:376`,
`:424`) and `useRenderedMarkdown.tsx:603`. The contract clause is `wait()`'s docblock: "Such an
owner must expect an answer to a source it has moved on from, and must say for itself when that
answer stops being worth painting" (`worker-queue.ts:145-150`).
