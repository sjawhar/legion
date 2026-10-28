---
title: "A pull request never commits the claude-envoy build, and ships a new bundle before the entry that runs it"
category: legion
tags:
  - claude-envoy
  - build
  - dist
  - rebuild-workflow
  - jj
date: 2026-10-28
status: active
module: packages/claude-envoy
related_issues:
  - "sjawhar/legion#2423"
---

# A pull request never commits the claude-envoy build, and ships a new bundle before the entry that runs it

## The rule

1. Never commit a change under `packages/claude-envoy/dist/` or `packages/claude-envoy/legion/`.
   Main's Rebuild Claude Envoy plugin workflow (`.github/workflows/claude-envoy-rebuild.yaml`)
   commits them after each merge. When a branch carries such a change, drop it with
   `jj -R "$LEGION_WORKSPACE" restore --from main@origin packages/claude-envoy/dist packages/claude-envoy/legion`.
2. To read or run a build of your tree, use `bun run verify-build "$TMPDIR/<name>.tar"` (the
   required `test` job's command) or `bun scripts/build.ts --out <dir outside the repository>`.
   Plain `bun run build` exits 2, and no mode of `scripts/build.ts` writes the tracked
   directories.
3. Add a new or renamed bundle in one pull request, and the `hooks/hooks.json`, `.mcp.json`,
   `bin/` or `.claude-plugin/` entry that runs it in a later one, after the rebuild commit is on
   main. Those hand-written files go live at the merge, beside the previous build, for one run of
   the rebuild workflow including its runner queues (more than an hour under a long queue).
4. A production check of a change to the built output names the rebuild run after its merge and
   the commit it landed, and runs `bun run check-dist` at that commit.

## Evidence

- sjawhar/legion#2423 made the rule (`packages/claude-envoy/AGENTS.md`, the convention that opens
  "A pull request never commits `dist/` or `legion/`"; `scripts/build.ts` `parseMode`).
- Review round 1 named the bundle/entry hazard (fixed in 7fb39f1e). Review round 3 found that the
  first `verify-build` built in place, so `jj st` showed the rebuilt files in `@`, the commit a
  worker pushes; 79c191d1 moved every build into a scratch directory and removed `--in-place`.
- Ten earlier notes carry `status: superseded … by sjawhar/legion#2423`; their bodies still give
  the old rebuild-and-commit steps, so read the status line before following one.
