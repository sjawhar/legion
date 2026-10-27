---
title: "A Vite Web Worker is its own bundle, so an inventory plugin needs a worker instance"
category: dispatch-web
tags:
  - vite
  - web-worker
  - third-party-notices
  - bundle
date: 2026-10-27
status: active
module: packages/dispatch/web
related_issues:
  - "INFRA-528"
  - "sjawhar/legion#2311"
---

# A Vite Web Worker is its own bundle, so an inventory plugin needs a worker instance

- Vite builds each `new Worker(new URL("./x.ts", import.meta.url), { type: "module" })` as a
  separate bundle while it transforms the module that creates it, so before the page's
  `generateBundle`; the page then sees the worker's files only as assets that name no source. Set
  `worker.format: "es"` for a module worker.
- Any plugin that inventories the bundle (Dispatch's `THIRD_PARTY_NOTICES.txt`) needs a
  `worker.plugins` instance that records each worker bundle's files and inputs; the page instance
  leaves those files out of its own check and merges those inputs in.
- Prove where a heavy dependency landed by searching the built chunks for a string only it holds
  (`grep -l "org.eclipse.elk" web/dist/assets/*.js`), and check the entry chunk named in
  `web/dist/index.html` is not among them.

## Evidence

sjawhar/legion#2311 (d9ff6c61): the first build with the Mermaid worker failed with `the build
emits ["assets/mermaid-render.worker-….js"], which no source file … accounts for`;
`packages/dispatch/web/vite.config.ts` `thirdPartyNoticesFile()` now returns a `page` and a
`worker` plugin. The marker grep showed ELK only in `mermaid-render.worker-*.js` and the lazy
editor chunk.
