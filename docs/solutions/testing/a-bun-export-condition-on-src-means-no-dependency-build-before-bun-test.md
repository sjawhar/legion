---
title: "A workspace package whose exports carry a bun condition on src/ needs no build step before a consumer's bun test or tsc: read import.meta.resolve, not the sibling job"
category: testing
tags:
  - bun
  - export-conditions
  - package-exports
  - workspace
  - ci
  - build-step
  - contracts
  - envoy-client
  - envoy-plugin
date: 2026-09-14
status: active
module: packages/contracts
related_issues:
  - "LEGION-96"
  - "sjawhar/legion#1086"
---

# A workspace package whose exports carry a bun condition on src/ needs no build step before a consumer's bun test or tsc: read import.meta.resolve, not the sibling job

When #1086 added `Test envoy plugin` to the `envoy-plugin` job in
`.github/workflows/envoy-and-contracts.yaml`, the obvious template was the sibling `envoy-client`
and `pi-envoy` jobs, which both run `Build contracts for …` (`bun run build` in
`packages/contracts`) before their own lint/typecheck/test. The plugin's job needed no such step,
and the reason is in `package.json`, not in the workflow.

## Why

`packages/contracts/package.json` and every subpath of `packages/envoy-client/package.json`
export three conditions:

```json
".": { "types": "./src/index.ts", "bun": "./src/index.ts", "default": "./dist/index.js" }
```

Bun's runtime — `bun test`, `bun -e`, `bun run <script>` — selects the `bun` condition and loads
the TypeScript source; `dist/` is never consulted. `tsc` selects `types`, which also names
`src/`. So a consumer that is executed by Bun and typechecked by `tsc` has no path to `dist/` at
all, and a `bun run build` of the dependency changes nothing it reads. The `default` condition
(`dist/`) is for consumers that are neither — a Node runtime, or the packed artifact.

Proof from the issue workspace, with `packages/contracts/dist` and `packages/envoy-client/dist`
both absent:

```sh
cd packages/envoy-plugin
bun -e 'console.log(import.meta.resolve("@legion/contracts"));
        console.log(import.meta.resolve("@legion/envoy-client/dispatch-execute"))'
# file:///…/packages/contracts/src/index.ts
# file:///…/packages/envoy-client/src/dispatch-execute.ts
bun run typecheck     # exit 0
bun run test          # 61 pass, 0 fail, 7 files
```

And on the pull request itself: the `envoy-plugin` job has no build step and its `Test envoy plugin`
step reported `61 pass` (run 34823844127). The job's `Typecheck envoy plugin` step had been green
without a contracts build for the same reason all along.

The plugin's `tsconfig.json` `paths` also map `@legion/contracts` and three `envoy-client`
subpaths to `../*/src/*.ts`, but that is not what carries it: `server.ts` imports
`@legion/envoy-client/dispatch-config` and `dispatch-execute`, which the `paths` map does not name,
and `tsc` resolves them through the `types` condition. The `paths` entries are belt to the
condition's braces.

## The rule

Before copying a `Build <dependency>` step into a job, ask the consumer what it resolves:
`bun -e 'console.log(import.meta.resolve("<specifier>"))'` from the consumer's directory, with the
dependency's `dist/` absent. A `src/` answer means the step is dead weight for `bun test` and
`tsc`; a `dist/` answer (no `bun` condition, or a `main`-only package) means the build is load-bearing
and its absence fails as `Cannot find module` on the first run. Say which in the workflow step's
name or the PR body so the next reader does not re-derive it.

This issue verified only the `envoy-plugin` job. The `Build contracts for …` steps in the
`envoy-client` and `pi-envoy` jobs were not tested for removability — both packages' `typecheck`
and `test` scripts are the same `bunx tsc --noEmit` / `bun test`, so the same resolution argument
applies to those two steps, but their `bun run build` steps (`bun build --target bun`) may still
need the dependency's `dist/` for reasons this issue did not exercise. Treat them as "probably
removable before test, unverified before build", and prove it with the command above before
touching them.

## Related

- [a-package-no-workflow-names-has-no-ci](a-package-no-workflow-names-has-no-ci.md): the job this
  step was added to, and the check that finds a package a job names but never tests.
- [bunx-from-the-repo-root-runs-the-wrong-tsc-and-biome](../build-errors/bunx-from-the-repo-root-runs-the-wrong-tsc-and-biome.md):
  the other resolution surprise in this monorepo — `bunx` picks the root's tool, not the package's.
