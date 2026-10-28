---
title: "A base-restore negative control leaves gitignored build output at whichever side built it last"
category: testing
tags:
  - negative-control
  - playwright
  - contracts-dist
  - jj-restore
date: 2026-10-27
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-569"
  - "sjawhar/legion#2363"
---

# A base-restore negative control leaves gitignored build output at whichever side built it last

Extends docs/solutions/testing/a-layout-change-is-proven-by-the-whole-project-and-the-rig-needs-contracts-built-and-three-engines-it-cannot-start-INFRA-92.md.

- A negative control can run the head's spec against the base app without a second workspace. In
  a new jj commit, `jj restore --from <base> <the app's source dirs>`, run the spec, then
  `jj abandon @` to return to head.
- `jj restore` and `jj abandon` move tracked source only. Every gitignored output the suite reads
  stays at whichever side built it last. For Dispatch's Playwright that is
  `packages/contracts/dist`, which `bun run e2e` does not rebuild (it rebuilds only `web/dist`).
  Run `cd packages/contracts && bun run build` before the base run and again before the first
  head run after it, or one of the two runs tests the other side's contracts.

## Evidence

- INFRA-569's browser negative control restored `packages/dispatch/web`,
  `packages/envoy/internal/dispatch` and `packages/contracts` from 94ab028a and ran the head's
  `search.e2e.ts`. It failed as intended, on `Expected substring: "Answered"`. That run built
  `packages/contracts/dist` from the base source, and the dist stayed at that build after
  `jj abandon`. The next head run rebuilt contracts first; the handoff recorded the step as
  "rebuild it … after any base-restore run".
