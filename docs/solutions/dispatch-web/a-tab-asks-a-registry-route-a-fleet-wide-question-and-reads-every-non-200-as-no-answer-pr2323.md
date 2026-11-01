---
title: "A tab asks a registry route a fleet-wide question and reads every non-200 as no answer"
category: dispatch-web
tags:
  - rolling-update
  - version-notice
  - registry
date: 2026-10-28
status: active
module: packages/dispatch/web
---

# A tab asks a registry route a fleet-wide question and reads every non-200 as no answer

- During a roll, `/` or `/healthz` names only the build of the process that answered. A question
  about the whole fleet ("is there a new version?") goes to one route that reads the registry of
  live server processes, answered the same by every process, in one request.
- Act only on an unambiguous answer: offer a reload when exactly one build is live and it is not
  the tab's own.
- Read every non-200 as no answer: a 404 from a process older than the route (one of the two in
  the first roll that carries it), a 401, a 5xx, a network failure.
- Compare like with like: a page that names no entry chunk (the Vite dev server's) has no build
  to compare and offers nothing.

## Evidence

Pull request #2323, `packages/dispatch/web/src/features/shell/DeploymentResilience.tsx:82`
(`status !== 200`), `:86` (`live.length !== 1`) and `:91` (`INDEX_ASSET_PATH.test(running)`,
added in `ba1703c9` after the dev page showed the notice on every focus). The two-builds browser
spec shows no notice while both builds are live and the notice once only the other build is.
