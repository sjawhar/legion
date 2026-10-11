---
title: "A scratch session loads a plugin by its package root, not its entry file, so its agents directory is discovered"
category: testing
tags:
  - oh-my-pi
  - omp-rpc
  - extension-loading
  - discoverAgents
  - capability-report
  - scratch-proof
date: 2026-10-11
status: active
module: packages/pi-legion
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---

# A scratch session loads a plugin by its package root, not its entry file, so its agents directory is discovered

Extends docs/solutions/legion/a-plugin-reaches-oh-my-pis-own-modules-through-string-literal-import-alone-and-measures-their-shapes-from-a-probe-extension-LEGION-663.md.

- `omp --extension <dir>` loads a plugin as a package: Oh My Pi reads the directory's
  `package.json` `omp` manifest (its `extensions` entries and `skills`) and adds the package's
  `agents/` directory to `discoverAgents`. `omp -e <dir>/extensions/legion.ts` loads one module and
  nothing of the package around it: the same code runs, `pi.agents` is exposed, the `task` tool is
  registered, and the plugin's own agents are absent — `discoverAgents(cwd)` returns the profile's
  five (`scout`, `reviewer`, `security-reviewer`, `task`, `sonic`) and the plugin's six are gone.
- For a scratch proof of anything that measures the plugin's surface, load both plugins as
  packages from the tree under test, built first: `(cd packages/pi-envoy && bun run build)`, then
  `omp --mode rpc --no-ui --no-extensions --extension packages/pi-envoy --extension
  packages/pi-legion …`. The daemon starts a pod's `omp` the same way (`--extension
  /opt/legion/pi-envoy --extension /opt/legion/pi-legion`), so the package load is the
  production shape and the file load is not a proof of it.
- The `dispatch` command the Envoy plugin's checks run resolves to `packages/pi-envoy/bin/dispatch`
  under a package load, which execs `../dist/dispatch.js`; an unbuilt tree fails the
  `dispatch-envoy-tools` row with "`dist/dispatch.js is missing; build the plugin`". Build before
  the proof, and read the row's detail before reading a failure as the code's.
- The `pi-shared` test harness (`packages/pi-shared/test/omp-harness.ts`, `spawnRpc`) loads the
  two entry files with `-e`, which is right for its tests (they never measure the agents) and
  wrong for this proof; copy its environment, not its argv.

## Evidence

LEGION-663 (sjawhar/legion#1874), the scratch proof at the retargeted head `cee5c6fc`. Loaded with
`-e packages/pi-envoy/extensions/envoy.ts -e packages/pi-legion/extensions/legion.ts`, the
`claims/ready` report read `subagents` open — "agents the prompts dispatch that the task tool does
not resolve: deep-worker, oracle, plan-gap-analyst, plan-reviewer, thermonuclear-code-quality,
thermonuclear-deep-review" — and `dispatch-envoy-tools` open on the missing `dist/dispatch.js`. A
probe extension's `discoverAgents(cwd)` under the same argv listed the profile's five agents only.
Built and loaded with `--extension packages/pi-envoy --extension packages/pi-legion`, the same
session reported all six rows ok ("task resolves 11 agents, every one the prompts dispatch") in
1066 ms; the credential-less `gh` negative control then failed the `github` row alone.
