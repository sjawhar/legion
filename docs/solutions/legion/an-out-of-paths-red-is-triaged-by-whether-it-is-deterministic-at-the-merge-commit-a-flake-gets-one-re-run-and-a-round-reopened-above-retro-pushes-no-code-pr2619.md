---
title: "An out-of-paths red is triaged by whether it is deterministic at the merge commit: a flake gets one re-run, and a round reopened above retro pushes no code"
category: legion
tags:
  - ci
  - flaky-test
  - rerun
  - merge-commit
  - retro
  - skip-checks
date: 2026-11-01
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2619"
---

# An out-of-paths red is triaged by whether it is deterministic at the merge commit: a flake gets one re-run, and a round reopened above retro pushes no code

Extends docs/solutions/legion/a-red-check-in-a-package-the-branch-does-not-touch-looks-for-mains-fix-first-pr2366.md.
Its step 3 decides "re-run once" for a pull request that is not conflicting and a `main` with no
fix, and "forward merge" for a conflicting one. It is silent on the fourth case: not conflicting,
`main` already carries a fix, and the failure is a flake. §6 of
docs/solutions/github/release-bump-push-resolves-one-conflict-shape-and-routes-every-failure-through-one-sink.md
says a re-run replays the run's cached potential merge commit, so `main`'s fix never reaches it,
and prescribes a push; followed here, that would have cost a code push above an approved,
post-retro head.

- **Read the failing assertion before you choose, and triage by determinism at the merge commit,
  not by whether `main` has a fix.** A comparison that can go either way on the same tree (two
  clock stamps under `toBeLessThan`, an ordering race) is a flake: `gh run rerun <run-id> --failed`
  once, push nothing, and complete when it settles. A failure that is deterministic at the run's
  merge commit (a type error, a missing symbol, a fixture `main` changed) fails the same way on a
  re-run, since the re-run fetches the same merge sha; only a head push refreshes it, and that is
  the forward-merge trigger "a fix for its only red check" of `skill://legion-worker`.
- **A green re-run is the flake landing the other way, not `main`'s fix arriving.** Say so in the
  handoff and the pull request: the merge commit the re-run tested still held the old assertion.
- **A round reopened above retro changes no code.** Record the round's handoff again with
  `dispatch handoff-write`, which writes a new version on the issue and pushes nothing, and
  complete naming it. The audits of the previous round stand when the compare from the previously
  audited head to the current code head lists only `docs/solutions/`: the tester re-runs the live
  proofs at that head and reads the new prose, the reviewer re-runs no thermonuclear pair, and the
  architect's acceptance and the next retro follow. A code push here withdraws READY and reopens
  the review. Before sjawhar/legion#2354 the handoff was a committed file, so such a round still
  pushed: the handoff went up alone under the `skip-checks: true` trailer, the daemon carried the
  code head's verdict to it, and the compare also listed `.legion/`.
- **Send the flake to the controller with its run, job and line, and file nothing.** Its fix
  belongs to the tree that owns the test; the controller answers where it went, or that `main`
  already carries it.

## Evidence

sjawhar/legion#2619 (LEGION-755), after its first retro:

- Run 41280745563 at retro's head f4f13b79 (`docs/solutions/` plus the removal of
  `.legion/LEGION-755/` above the approved head) was red on `ci-ok` because job 124777035348
  (`pi-legion`) failed `packages/pi-legion/extensions/legion-background-subagent-omp.test.ts:477`,
  `expect(read.arrivedAt).toBeLessThan(releasedAt)`, with `Expected: < 1793518357364, Received:
  1793518357364`: two equal millisecond stamps. The branch holds no such file; it reached the run
  through the merge commit, from `main`'s sjawhar/legion#2571.
- `main` carried the fix already: sjawhar/legion#2621 (081e9e602, "orders its moments by a counter,
  not by a millisecond clock", merged 2026-11-01T06:59:30Z), later than the run's merge commit.
  The controller's record: dispatch://LEGION-474/comment/0e7b4664-a62b-477b-a0a0-392b82ab5a15,
  answering the implementer's evidence at
  dispatch://LEGION-474/comment/87734e93-1b9e-4521-9fde-4fd9d4037ced; nothing was filed.
- One re-run, no code: attempt 2's `pi-legion` (check run 124796051553) passed on the same merge
  commit, `ci-ok` succeeded at 09:23:25Z, and the implementer's handoff went up alone at 10f5eb0f
  under the trailer, the code head's green carried to it. The reviewer's approval of the round,
  review 5727611468 at c4a5d45a, records that the runtime code equals the code round 1 audited
  (compare `e6ec6d44...f4f13b79`: `docs/solutions/` and `.legion/` only), so the thermonuclear pair
  was not run again, while the tester re-ran the three scratch tests at 10f5eb0f (the `E2E
  (tester)` line of the pull request).
