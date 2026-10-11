---
title: "A forward merge after retro reinstalls when main bumped the lockfile, keeps both changelog insertions, and still runs test, review and a second retro"
category: legion
tags:
  - forward-merge
  - conflict-round
  - bun-lockfile
  - changelog
  - unchanged-diff-fingerprint
  - second-retro
  - legion-handoffs
date: 2026-10-09
status: active
module: packages/pi-legion/CHANGELOG.md, bun.lock, .legion handoffs
related_issues:
  - "LEGION-634"
  - "sjawhar/legion#1865"
  - "LEGION-605"
---

# A forward merge after retro reinstalls when main bumped the lockfile, keeps both changelog insertions, and still runs test, review and a second retro

Extends docs/solutions/legion/a-conflict-round-that-changes-the-fingerprint-is-a-counted-review-round-and-its-approval-starts-a-second-retro-LEGION-578.md.
That record is the conflict round whose fingerprint changes. This one is the other case it names,
a forward merge after retro whose fingerprint is unchanged, and what the round still costs.

## The rules

- **After a forward merge of `main`, run `bun install --frozen-lockfile` before any check when
  `bun.lock` is among main's changed files.** A long-lived issue workspace keeps the
  `node_modules` of its fork point. A type error after the merge in a file the branch never
  touched is the stale install, not the merge: `packages/pi-legion`'s `tsc` reaches
  `packages/pi-envoy/extensions/envoy.ts` through the cross-entry test that imports it by relative
  path, and fails there on a symbol the newer `@oh-my-pi/pi-utils` adds. The tester's pod needs the
  same install before its bare gates; say so in the report to the architect.
- **A changelog entry at the head of `[Unreleased]` → `### Changed` conflicts with every sibling
  that merges first.** This repository's release commits (`chore: release pi-legion vX.Y.Z`) leave
  `[Unreleased]` where it is, so each concurrent branch inserts at the same line. Resolve the one
  conflict by keeping both insertions, this branch's first; it is the conflict a documents-and-tests
  branch should expect, and the only one here.
- **An unchanged fingerprint is a confirmation for the tester and the reviewer, and still a full
  pass through the daemon's table.** The daemon moves the issue to implementing on the conflict,
  the implementer merges and completes, the tester re-runs the bare gates and completes, the
  reviewer records the equal fingerprint and completes with `approved` (no thermonuclear pass, no
  new GitHub review), and the table's `Reviewing` + `TriggerReviewApproved` → `Retro` edge starts
  the implementer on retro again. There is no confirmation edge that goes to the merger directly.
  The second retro writes only what the round taught, as a new file, edits none of the first
  retro's, re-reads `mergeable` before committing, and brings the body's `Size` and `Chain` lines
  up to date for its commit.
- **On a daemon older than LEGION-605's READY refusal, the human merge gate still refuses a head
  carrying `.legion/<issue>/`.** The repository's own `skills/legion-retro/SKILL.md` makes the
  removal retro's last commit (`rm -r .legion/<issue>` then
  `jj split -m "retro: remove .legion/<issue>/ before READY" .legion/<issue>`, after any
  `docs/solutions/` commit, pushed with CI in full); a worker whose installed plugin predates it is
  told so by its architect and does it by hand, publishing the final head and
  `jj diff --from <approved head> --to <final head> --summary` to the reviewer, the merger and the
  architect, so the reviewer approves at the final head and READY names it.

## Evidence

- sjawhar/legion#1865 (LEGION-634): the reviewer approved `bded287fda31`, retro committed
  `5b9d7372c1b8` (four `docs/solutions/` files), then `main` moved (#1837 LEGION-588, #1864, #1861,
  the pi-legion 8.4.2 and 8.5.0 releases) and GitHub reported the head conflicting.
- The forward merge `1511d4fabfa9` (`jj new legion/LEGION-634 main@origin`) had one conflict,
  `packages/pi-legion/CHANGELOG.md`: this branch's LEGION-634 entry and main's two LEGION-588
  entries at the head of `### Changed`; both kept. Seven files overlapped; the other six merged in
  disjoint regions.
- `bunx tsc --noEmit` in `packages/pi-legion` on the merged tree:
  `../pi-envoy/extensions/envoy.ts(1194,17): error TS2339: Property 'refreshShellConfigCache' does
  not exist on type … @oh-my-pi/pi-utils@18.1.15 …`. main's diff listed `bun.lock` and six
  `package.json` files; `bun install --frozen-lockfile` (28 packages) made `tsc` exit 0 and the
  suites pass: pi-shared 23, pi-legion 195 / 701 expect() on omp/18.6.0 (main brought three cases).
- Fingerprint `b66bfa91566f…` before (`5b9d7372`, fork point `5c956b07`) and after (`4da26c38`,
  fork point `df983ada`); the tester's and the reviewer's own computations equal. The round still
  ran: test handoff `89ed15fd8b3e` (bare gates), review handoff `d8889f4605b7` (`verdict:
  approved`, `thermonuclear: Not run this round`), and this second retro.
- The LEGION-578 architect's direction of 2026-10-09T11:59Z for this retro: the merge gate acting
  for the repository owner refuses a head still carrying `.legion/<issue>/`; the deployment's daemon
  predates #1857's READY refusal, so the removal is retro's last commit by hand, as LEGION-629's
  retro had just done.
