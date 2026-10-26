---
title: "A skills change carries a regenerated packages/claude-envoy/legion/ mirror, and the build that makes it rewrites dist/ as well"
category: legion
tags:
  - claude-envoy
  - check-dist
  - generated-files
  - skills
  - bun-version
  - ci-filters
date: 2026-10-25
status: active
module: packages/claude-envoy
related_issues:
  - "INFRA-227"
  - "sjawhar/legion#2155"
---

# A skills change carries a regenerated `packages/claude-envoy/legion/` mirror, and the build that makes it rewrites `dist/` as well

Extends docs/solutions/legion/committed-plugin-bundles-inline-contracts-so-every-contracts-change-carries-a-dist-commit.md,
which covers the `dist/` bundles alone. The delta is the second generated tree in that package,
`legion/`, which a change under `skills/` moves and a change under `packages/contracts/` does not.

- Edit `skills/**`; never edit `packages/claude-envoy/legion/**`. It is a byte copy of the
  `@sjawhar/pi-legion` skills partition, and the only way to move it is
  `cd packages/claude-envoy && bun run build`, run on the Bun version `.bun-version` pins (the
  build refuses any other).
- Run `bun run check-dist` **before** your edit and again after the build. Before the build it
  names the stale files — "generated files are stale (legion/skills/…); run `bun run build` on
  Bun \<pin\> and commit the result" — and after it prints "dist/ and legion/ match a fresh build".
- The plain build deletes and rewrites `dist/` (bundles and `THIRD_PARTY_NOTICES`) in the same run.
  Diff after the build and require `dist/` to be byte-identical for a skills-only change; a `dist/`
  diff means your install differs from the one the committed bundles were built with, and is a
  stop-and-report rather than something to fold into a prose commit.
- Only pi-legion's partition is mirrored. A change to a pi-envoy skill (`asd-ste100`, `dispatch`,
  `dispatch-first`, `dispatch-writing-specs`, `envoy`) produces no `legion/` diff, and that absence
  is not drift.
- A skills-only diff is not CI-invisible: the `claude_plugin` filter of
  `.github/workflows/envoy-and-contracts.yaml` lists `skills/**` and `scripts/pi-plugin-prepack.sh`,
  so the `claude-envoy` job runs `check-dist` on your head. That job is **not** in main's required
  set (rulesets require `lint`, `typecheck`, `test`, `pr-title`), so a stale mirror turns a lane red
  without blocking the merge by itself — the reviewer and the merge gate are what stop it.

## Evidence

INFRA-227 (sjawhar/legion#2155) rewrote four instruction files, two of them under `skills/`.

- `bun run check-dist` run after the skill edit and before the build exited 1 with "generated
  files are stale (legion/skills/legion-controller/SKILL.md,
  legion/skills/legion-controller/references/backlog-stewardship.md); run `bun run build` on Bun
  1.3.14 and commit the result"; after the build it printed "dist/ and legion/ match a fresh
  build", and `tests/legion-plugin-fresh.test.ts` was 4 pass.
- `jj diff --stat` after the build listed exactly the two regenerated files; nothing under `dist/`
  moved. `buildBundles` keeps `minify: false` and `sourcemap: "none"` precisely so the bundles are
  reproducible, so a `dist/` diff is a real signal.
- The tester ran a second full `bun run build` at the committed head: `jj status` reported no
  changes and `check-dist` passed again, so the committed mirror is a fixed point of the generator
  rather than a one-off match.
- `packages/claude-envoy/AGENTS.md:50` already said "never edit by hand"; nothing under
  `docs/solutions/` said what to run instead, which is why this file exists.
