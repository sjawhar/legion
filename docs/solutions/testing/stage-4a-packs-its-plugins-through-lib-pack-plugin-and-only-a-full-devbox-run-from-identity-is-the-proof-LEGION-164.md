---
title: "Stage 4a packs its plugins through lib/pack-plugin.sh, and only a full devbox run from identity is the proof"
category: testing
tags:
  - stage-4a
  - e2e
  - pack-plugin
  - npm-pack
  - operator-ask
  - devbox
date: 2026-10-13
status: active
module: scripts/e2e
related_issues:
  - "LEGION-164"
  - "sjawhar/legion#1932"
---

# Stage 4a packs its plugins through lib/pack-plugin.sh, and only a full devbox run from identity is the proof

Extends docs/solutions/testing/proof-rig-loads-the-production-plugin-tree.md.

- Every script that packs a branch-built `@sjawhar/pi-envoy` or `@sjawhar/pi-legion` calls
  `scripts/e2e/lib/pack-plugin.sh <pi-envoy|pi-legion> <out dir>` and reads its one stdout line,
  the tarball's path. Never parse `npm pack --json` or `bun pm pack` stdout: the prepack's
  `bun build` writes its bundle report there ahead of the JSON (`Bundled 196 modules in 96ms`), so
  `jq -r '.[0].filename'` dies on column 8 on an operator's npm 10. The helper rewrites
  `omp.extensions` to the dist bundle as the release does, packs with the report on stderr, and
  restores the committed manifest on every exit; it needs `bun install --frozen-lockfile` at the
  root first, and `jq`, which the worker image lacks (prove the step in a pod with a stand-in on
  PATH, and say so).
- An operator ask for a stage 4a checkpoint asks for the full run: `STAGE4A_FROM=<checkpoint>`
  (the script's variable — not `LEGION_E2E_FROM`, which it sets for the Go test itself) is "a
  development run, never the proof" by the script's own line, and a reviewer discounts it. Read
  the script's header for its variables and its definition of the proof before writing the ask;
  the run needs a shell on the devbox host (the harness binds the host's private address and reads
  instance metadata), not an agent box.
- Make a live checkpoint's negative control exercise the shape production will use (the published
  package by exact version, fetched from the registry inside the pod), not a second tarball you
  copied in: then the devbox run proves the version→package mapping end to end.

## Evidence

LEGION-164's `stale-plugin-install` checkpoint first packed with `npm pack --pack-destination …
--json | jq -r '.[0].filename'`; the scrum master's devbox run failed `CHECK plugins` on
`jq: parse error: Invalid numeric literal at line 1, column 8` and traced the 2,559 stdout bytes
to the prepack's build report ahead of a valid JSON array. Routed through `lib/pack-plugin.sh`
(94325e57), the full run passed: `stage 4a e2e: PASS`, 31 checks, `CHECK stale-plugin-install:
PASS (9s)`, the pod uid unchanged across the installing Resume, `@sjawhar/pi-legion@9.6.1
(contract 24)` under `legion-plugins/g3`, the published 9.6.1 by exact version refused at contract
23 (dispatch://LEGION-164/comment/4b8a6aef-9c57-462a-ad58-c8b4c240cf34). The implementer's first
ask named `LEGION_E2E_FROM=stale-plugin-install`, copied from the plan; the release-speed lane's
attempt from an agent box failed the metadata read, and the scrum master declined the
`STAGE4A_FROM` run as not citable before running the full stage.
