---
title: "A check that prints nothing is not green: bunx biome before bun install, and the pod's builtin xargs"
category: build-errors
tags:
  - bunx
  - biome
  - false-green
  - verification
  - xargs
  - worker-pod
date: 2026-10-16
status: active
module: scripts/e2e
related_issues:
  - "LEGION-780"
  - "sjawhar/legion#1982"
symptoms:
  - "bunx biome check <file> prints nothing at all and exits 0 in an issue workspace where bun install has not run"
  - "type xargs answers 'xargs is a shell builtin' in the bash tool's shell of a worker pod"
---

# A check that prints nothing is not green: bunx biome before bun install, and the pod's builtin xargs

Extends docs/solutions/build-errors/bunx-from-the-repo-root-runs-the-wrong-tsc-and-biome.md.

- A second symptom of the same false green: in an issue workspace where `bun install` has not run,
  `bunx biome check <file>` prints nothing — not even the punycode warning that document names —
  and exits 0, and a subagent reports "biome clean". The real Biome always prints
  `Checked N files in …`; a lint, type or test command whose success line is absent is unverified,
  whatever its exit status. Run `bun install --frozen-lockfile` once in the workspace before the
  first check, then check the directory as CI does (`bunx biome check .`, or the package path),
  never a single file as the proof.
- The worker pod's bash-tool shell has `xargs` as a shell builtin beside its jaq `jq`
  (`docs/solutions/legion/worker-pane-shell-gotchas.md` §14): a measurement of GNU xargs's `-x`
  or `-n` behaviour runs `/usr/bin/xargs` by path, or inside `bash -c`, and records which. The
  script's `xargs` is the box's; the tool shell's is a fact about the session.

## Evidence

sjawhar/legion#1982 (LEGION-780). Three deep-worker rounds reported `bunx biome check
scripts/e2e/lib/<file>.test.ts` exit 0 with no output; after `bun install --frozen-lockfile`
(needed anyway for `scripts/e2e/lib`'s `zod` and `nats` imports) `bunx biome check scripts/e2e/lib/`
found two formatting diffs in those files, fixed with `biome check --write` before the first push,
so CI's required `lint` job never saw them. The `agent_setting` xargs proof (`-x -n <count>` refusing
an oversized environ with `argument list too long`, `-x` alone running twice) was measured with
`/usr/bin/xargs` (GNU findutils 4.10.0) after `type xargs` in the tool shell answered
`xargs is a shell builtin`.
