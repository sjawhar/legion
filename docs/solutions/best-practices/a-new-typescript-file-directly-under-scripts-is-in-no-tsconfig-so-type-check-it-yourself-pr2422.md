---
title: "A new TypeScript file directly under scripts/ is in no tsconfig, so type-check it yourself"
category: best-practices
tags:
  - typescript
  - tsc
  - ci
  - scripts
date: 2026-10-28
status: active
module: scripts
related_issues:
  - "sjawhar/legion#2422"
---
# A new TypeScript file directly under scripts/ is in no tsconfig, so type-check it yourself

- CI type-checks only `scripts/dispatch` and `scripts/github-apps` (`bunx tsc --noEmit -p …`), and
  the repository root has no `tsconfig.json`. A `.ts` file directly under `scripts/`, or in a new
  directory there, is linted by Biome and run by `bun test`, but no CI job type-checks it.
- Before each push, run
  `bunx tsc --noEmit --strict --target esnext --module esnext --moduleResolution bundler --types bun <files>`
  on every such file you change, its tests included. Record the command and its exit in the
  handoff.
- A bare `bunx tsc --noEmit` at the root type-checks nothing. It prints tsc's help and exits 1.

## Evidence

- `.github/workflows/ci.yaml`, `test` job: the only `tsc` steps name
  `-p scripts/dispatch` and `-p scripts/github-apps`.
- sjawhar/legion#2422 added `scripts/semantic-line-breaks.ts` and its two test files. The reviewer
  and the implementer type-checked them by hand with the command above in each round. At e6f60222,
  a bare `bunx tsc --noEmit` printed the help and exited 1.
