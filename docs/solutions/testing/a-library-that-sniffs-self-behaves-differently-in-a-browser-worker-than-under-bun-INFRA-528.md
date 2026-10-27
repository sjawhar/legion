---
title: "A library that sniffs self behaves differently in a browser worker than under Bun, so prove the worker path in every engine"
category: testing
tags:
  - web-worker
  - environment-detection
  - elkjs
  - playwright
date: 2026-10-27
status: active
module: packages/dispatch/web
related_issues:
  - "INFRA-528"
  - "sjawhar/legion#2311"
---

# A library that sniffs self behaves differently in a browser worker than under Bun, so prove the worker path in every engine

- A bundled dependency that picks its mode from `self`, `window` or `document` can work under Bun,
  Node and happy-dom and fail in a real browser worker. Run the worker path in every browser
  engine the app supports before calling it done; a unit test with a fake worker cannot see it.
- In a dedicated worker `self` is an accessor on `WorkerGlobalScope.prototype`, so
  `delete globalThis.self` does nothing. To hide it from such a library, define an accessor that
  reads `undefined` on the global's own prototype (`Object.getPrototypeOf(globalThis)`) for the
  length of the call, and delete it after.

## Evidence

sjawhar/legion#2311: beautiful-mermaid builds ELK's bundled engine on its first flowchart; finding
`self` and no `document`, that engine took the worker for an ELK worker of its own, claimed its
`onmessage`, and every flowchart was refused (`En is not a constructor`) in Chromium, WebKit and
Firefox, while the same code drew under Bun. beautiful-mermaid's own `delete globalThis.self` did
not reach the accessor. The shadow is in
`packages/dispatch/web/src/features/refs/mermaid-render.worker.ts` (d9ff6c61); the unit tests draw
only sequence diagrams because ELK cannot be built under happy-dom at all.
