---
title: "The Claude bridge's committed bundles inline @legion/contracts and @legion/envoy-client: every change to either package carries a rebuilt dist/ commit, built from a tree that has main's bridge sources, on the pinned Bun"
category: legion
tags:
  - claude-envoy-bridge
  - check-dist
  - committed-bundle
  - contracts
  - envoy-client
  - bun-version
  - rebase
  - fingerprint
  - merge-ref
date: 2026-09-15
status: active
module: packages/claude-envoy-bridge
problem_type: process
severity: medium
related_issues:
  - "LEGION-131"
  - "sjawhar/legion#1106"
  - "sjawhar/legion#1103"
  - "LEGION-171"
  - "LEGION-173"
applies_when:
  - Your branch changes anything under packages/contracts or packages/envoy-client
  - The `claude-envoy-bridge` job fails "Check the committed bundle is fresh" with `dist/ is stale (envoy-channel.js, open-asks-hook.js)` while GitHub reports MERGEABLE
  - You are deciding whether Sami's no-unnecessary-rebase rule (2026-09-11) applies
  - You are reviewing or fingerprinting a pull request that carries a `chore(claude-envoy-bridge)` dist commit
symptoms:
  - "dist/ is stale (envoy-channel.js, open-asks-hook.js); run `bun run build` on Bun 1.3.14 and commit the result"
  - "a PR whose every check was green four minutes ago is red on one job after a main merge, with no push"
  - "the unchanged-diff fingerprint changed after a rebase although no branch line changed: two added files under packages/claude-envoy-bridge/dist"
---

# Committed bridge bundles inline the contracts: every contracts change carries a dist/ commit

## What happened

LEGION-131 (`sjawhar/legion#1106`) tightened `packages/contracts/src/handoff-schema.ts`. Its
`.legion/` deletion head `239aaa28` was pushed at 00:22Z on 2026-09-15 with every earlier check
green and GitHub reporting `MERGEABLE`. Four minutes earlier, at 00:18:39Z, `main` had merged #1103,
which commits two built files — `packages/claude-envoy-bridge/dist/envoy-channel.js` and
`dist/open-asks-hook.js` — and adds a CI step, "Check the committed bundle is fresh"
(`bun run check-dist` = `bun scripts/build.ts --check` in `.github/workflows/envoy-and-contracts.yaml`,
job `claude-envoy-bridge`), that rebuilds them on the Bun `.bun-version` pins and compares bytes.
Those bundles inline `@legion/contracts` and `@legion/envoy-client` (the Claude plugin cache has
no `node_modules`; `packages/claude-envoy-bridge/AGENTS.md` says so and states the rule: rebuild
`dist/` in the same commit as any source change, *including a change to those two packages*).
The job runs on the pull request's **merge ref**, so the merged tree carried the branch's schema
and `main`'s bundles built from the old one: `dist/ is stale (envoy-channel.js, open-asks-hook.js);
run `bun run build` on Bun 1.3.14 and commit the result`. The reviewer's `REQUEST_CHANGES`
(review 5204319646) named it as a deterministic CI failure, not a code defect.

The consequence is fleet-wide and is filed as LEGION-171: **every pull request that changes
`packages/contracts` or `packages/envoy-client` now needs a rebuilt `dist/` commit**, and two such
branches in flight conflict with each other on the bundle files. Whoever owns the bridge decides
whether the bundles stay committed; until then, this is the procedure.

## Why it is a necessary rebase, not a forbidden one

Sami's rule (2026-09-11) forbids rebasing to pick up `main` or refresh CI because the queue is slow.
Here the branch predates #1103, so it has neither `scripts/build.ts` nor a `dist/` to rebuild: no
commit on the old base can produce the bundles the merge ref is checked against. The fix commit
can only be built from a tree that carries `main`'s bridge sources, and it costs one CI run either
way. The LEGION-131 architect recorded the exception on those grounds — the same reasoning as
[`mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md`](mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md):
`MERGEABLE` is a textual test; a merge ref that fails a deterministic gate is a conflict in effect.
Tell the architect before rebasing and say why the `MERGEABLE` read does not apply.

## The mechanics, as run on LEGION-131 (twice)

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch
# 1. before-fingerprint at the current tip (see below for the fileset), keep the stripped diff
# 2. the whole chain, so other roles' handoff commits move with yours
jj -R "$LEGION_WORKSPACE" rebase -s 'roots(main@origin..@)' -d main@origin
bun install --frozen-lockfile                      # main's lockfile may have moved
cat .bun-version; bun --version                    # both 1.3.14 on the box; else: mise x bun@<pin> -- bun run build
cd packages/claude-envoy-bridge
bun run check-dist                                 # reproduces CI's `dist/ is stale (...)` — keep that line
bun run build                                      # `built envoy-channel, open-asks-hook into dist/`
bun run check-dist                                 # must print `dist/ matches a fresh build`
bun run lint && bun run typecheck                  # the job's other steps
env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE -u DISPATCH_TOKEN bun test   # see worker-pane-shell-gotchas.md §2 / LEGION-173
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" split -m 'chore(claude-envoy-bridge): rebuild committed bundles against <what changed>' packages/claude-envoy-bridge/dist
```

- **One dist commit per branch.** A later rebase (LEGION-131's second, conflict-forced by #961)
  rebuilds again and folds the fresh bytes into the *existing* commit —
  `jj -R "$LEGION_WORKSPACE" squash --into <that commit's change id> packages/claude-envoy-bridge/dist` —
  so the chain never carries two bundle commits. Run each squash as its own bash call (the shared
  operation log; `worker-pane-shell-gotchas.md` §15).
- **Path-scope every split** (`.omp/config.yml` sits untracked-added in a pane workspace).
- **Do not read the bundle diff.** A schema change of ~130 source lines produced `envoy-channel.js
  | 120 +-` and `open-asks-hook.js | 78 +-`: the bundler re-minifies whole files and renames
  identifiers throughout. The proof is the `check-dist` line at the head and the CI job `SUCCESS`
  at the head; a sanity grep of the added lines for your change's markers
  (`grep -oE 'trim\(\)|expected one of' …`) is as far as reading goes.
- **`main` moves again while you work** (the shared clone's `main@origin` advanced twice under this
  branch between fetch and push). Before deciding on a second rebase, check what moved *in the
  inlined packages*: `jj diff --from <your rebase base> --to main@origin --stat packages/contracts
  packages/envoy-client packages/claude-envoy-bridge`. Empty means the bundles you built are
  byte-identical to a build on today's `main`, and no second rebase is owed.

## The fingerprint excludes the generated output

The `legion-worker` skill's unchanged-diff fingerprint fileset is `~(.legion | docs/solutions)`.
With a dist commit on the branch that hash changes on every rebase while no branch line changes:
the two bundles appear as added files against the new base. LEGION-131's tester and reviewer both
computed the skill's hash with `packages/claude-envoy-bridge/dist` added to the exclusion —
`'~(.legion | docs/solutions | packages/claude-envoy-bridge/dist)'` — and that hash was equal
before and after the CI-forced rebase (`0e332ee2…28ed` both sides; the stripped diffs line-identical
under `diff`). State the fileset you hashed in the rebase comment. The generated files are proven by
`check-dist`, never by the fingerprint.

## Roles

- **Implementer:** before opening or re-pushing a PR that touches `packages/contracts` or
  `packages/envoy-client`, run `bun run check-dist` in the bridge package on your tree; if it is
  stale, your branch needs the dist commit (and, if it predates #1103, the rebase above). Say in the
  PR body that the dist commit exists and why.
- **Tester / reviewer:** the dist commit's evidence is `check-dist` at the head and the job result;
  exclude `dist/` from the fingerprint; do not ask for the bundle diff to be explained line by line.
- **Architect:** two open branches that both change the inlined packages will conflict on `dist/`;
  sequence them — the second rebuilds after the first merges — and say so in each tree's plan.

## Related

- [`mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md`](mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md)
  — the same "merge ref red while MERGEABLE" shape, for a test harness instead of generated output.
- [`unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md`](unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md)
  — the fingerprint command; §2 carries LEGION-131's two rebases.
- [`conflict-only-rebases-keep-the-diff-auditable.md`](conflict-only-rebases-keep-the-diff-auditable.md)
  — what a conflict-forced rebase may and may not change.
- [`worker-pane-shell-gotchas.md`](worker-pane-shell-gotchas.md) §2 — the bridge's own test that
  reads the pane's `DISPATCH_TOKEN_FILE` (LEGION-173).
