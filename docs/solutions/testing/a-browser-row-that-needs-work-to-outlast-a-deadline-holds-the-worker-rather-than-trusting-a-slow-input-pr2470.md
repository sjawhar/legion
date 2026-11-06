---
title: "A browser row that needs work to outlast a deadline holds the worker rather than trusting a slow input"
category: testing
tags:
  - playwright
  - web-worker
  - page-route
  - timers
  - flaky-tests
  - negative-control
date: 2026-10-28
status: active
module: packages/dispatch/e2e
related_issues:
  - "sjawhar/legion#2470"
---

# A browser row that needs work to outlast a deadline holds the worker rather than trusting a slow input

- **Hold the worker; never pick an input that is "big enough" to be slow.** How long work takes
  depends on the runner: a GitHub-hosted runner can do in time what a 2-CPU issue pod cannot. A
  held worker makes the shipped deadline fire on every machine, the page gets no test hook, and
  any input will do (a 2-node flowchart is enough).
- **Hold a Web Worker by serving its real chunk with one listener in front of its own.** Use
  `page.route(<chunk regex>)`, then `const response = await route.fetch()` and
  `route.fulfill({ response, body: prefix + "\n" + await response.text() })`. In the prefix,
  `self.addEventListener("message", …)` calls `event.stopImmediatePropagation()`, fetches one
  same-origin path, and never returns. The prefix runs before the module body, so its listener is
  registered first. Count that path with a second `page.route` (a URL predicate on
  `workerHeldPath`, answered `204`). `page.route` also sees requests a dedicated worker makes, in
  Chromium, Firefox and WebKit.
- **Assert the hold report before any timing or deadline assertion.** Use `expect.poll` with a
  message. Then a refusal can come only from the deadline, and a page that never hands the job to
  the worker fails at the hold, with a clear message.
- **To show that a page-responsiveness sample can fail, block the page thread with a repeating
  task** (`setInterval(() => { busy 1.2 s }, 0)`). A one-shot `setTimeout(block, 0)` sent through
  `page.evaluate` does not fail the next sample. The sample's own `page.evaluate` waits behind the
  block and starts its timer after the block ends.
- **A timing row that passed only on the pod is not proven.** It needs a settled CI run whose
  job log shows the row's own line. A cancelled CI job proves nothing. Every comment or doc that
  states a duration names the machine that measured it.

## Evidence

sjawhar/legion#2311's row `a diagram too large to draw in time …` used an 800-node flowchart and
the page's 5 s limit, and it passed only on the pod. #2311's `dispatch` job was cancelled. CI run
40692470398 and run 40698817533 failed the row in all four projects. Playwright's error-context
snapshot showed the diagram drawn inside 5 s. Run 40696018569 failed in chromium and iphone only.

#2470 holds the worker (`packages/dispatch/e2e/mermaid.e2e.ts`, `holdingListener` and the two
routes). CI run 40732897855 shows ✓ for the row in chromium, iphone, webkit and firefox. On the pod
it passed 20/20 with `--repeat-each=5`. The tester held a 2-node flowchart, which draws in
1.0-2.4 s without the hold, and the page still refused it in all four projects. The built bundle
contains no `mermaid-worker-held`.

sjawhar/legion#2472 moved both time-limit rows (a comment's and a document's) onto one helper,
`holdWorkerSource`, with a 2-node flowchart each, since the hold, not the input, makes the work
outlast the limit.

The lapses each failed at their own line:

| Lapse | Failed at |
|---|---|
| A chunk route that never matches | the hold poll (`Received 0`) |
| A page that draws on its own thread (tester) | the hold poll |
| `setInterval` 1.2 s block | the first sample (2401.9 ms) |
| The plan's one-shot `setTimeout(block 2 s, 0)` | did not fail: passed |
