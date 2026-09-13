---
title: "An issue overtaken by main between diagnosis and adoption reduces to proof plus the test the landed fix left out — not a close, not a re-implementation"
category: legion
tags:
  - legion
  - architect
  - spec
  - main-moved
  - duplicate-fix
  - acceptance-criteria
  - new-since-we-talked
date: 2026-09-13
status: active
module: skills/legion-architect
applies_when:
  - An issue's Summary names a defect and cites another tree that is fixing or has fixed the same code
  - The evidence in an issue is timestamped before a merge on main that touched the function it names
  - An architect is tempted to close an issue as "already fixed" on a code-level argument, or to implement the issue's original option anyway
related_issues:
  - "LEGION-55"
  - "sjawhar/legion#1014"
  - "LEGION-28"
  - "sjawhar/legion#980"
---

# An Issue Overtaken by `main` Reduces to Proof Plus the Test the Fix Left Out

LEGION-55 was filed at 01:52Z on 2026-09-13 against a daemon-test flake: two concurrent fake
clones racing on one repository directory, `Incomplete Jujutsu clone … missing …/.jj`. Its
Summary offered two fixes — (a) make the test fixture's fake clone atomic, or (b) make
`ensureRepoClone` tolerate a clone in flight — and told the adopting architect to coordinate
with LEGION-28, whose tree was already editing `ensureRepoClone`. At 02:37Z the same day
LEGION-28's `sjawhar/legion#980` merged and landed (b) in the product: a temporary-sibling clone
renamed into place, with the losing concurrent caller yielding at the rename. Every failure the
issue cited predated that merge. By the time the issue was adopted, the defect it described did
not exist on `main`.

## The architect's first step: check whether the cited fix landed

Before any decomposition, the architect read the cited tree's PR state and the merge commit's
diff against the function the issue named. That one check changed the issue's shape: option (a)
was no longer needed (and would have hidden the product path the real daemon relies on behind a
second atomicity mechanism in the fixture — the spec's Rejected table says so), option (b) was
already done, and the instruction to coordinate with LEGION-28 was satisfied because that tree
was closed. The check is cheap and the alternative is a tree that plans a fixture change, has an
implementer discover on `jj git fetch` that the failure no longer reproduces, and then argues
about whether the acceptance criteria still apply.

Rule: when an issue's Summary cites another issue or PR as touching the same code, the first
architect action is `legion gh -- pr view <n> --json state,mergedAt,mergeCommit` plus
`jj diff -r <merge> -- <file>` for the function named. Timestamps in the issue's evidence against
the merge time decide whether the defect described is still on `main`.

## What was left: the fix's own PR did not carry the proof the issue wanted

"Already fixed" is not "close it". The overtaking PR was reviewed for its own acceptance
criteria; this issue's were different, and two of them were still unmet on `main`:

- **The rename-collision branch had no unit test.** `#980` tested the happy path (temporary
  sibling, rename into place), the clone killed at its budget, the cleanup failure, the
  incomplete clone with no `.jj`, and a plain command failure in `packages/workspace` (`#995`
  added the aborted clone later); the branch where a second concurrent caller's rename hits
  `ENOTEMPTY`/`EEXIST` and yields was exercised only implicitly by the daemon fixture. That
  branch is exactly the one LEGION-55 exists for. The issue's one code
  change became that test — see
  [two-contenders-through-a-rename-collision-gate-both-in-wake-on-the-rename-pin-the-winner](../testing/two-contenders-through-a-rename-collision-gate-both-in-wake-on-the-rename-pin-the-winner.md).
- **Nobody had shown the flake gone.** The issue's Acceptance 1 was a 50-run loop; `#980` shipped
  on its own unit tests. The loop at the fixed head (50/50) *and* at `#980`'s parent (49/50, the
  exact error string on run 47) is what turned the code-level argument into evidence — see
  [loop-a-flaky-suite-sequentially-parallel-lanes-of-it-starve-its-timer-tests](../testing/loop-a-flaky-suite-sequentially-parallel-lanes-of-it-starve-its-timer-tests.md).

So the tree ran with no planner phase (the spec's Design section was the plan), one implementer
change of one test file, and a tester whose whole job was the two proofs. The spec's Rejected
table records the third path explicitly: "Closing this issue on the code-level argument alone,
without the loop and the unit test: … skipping either leaves the collision branch untested on
main."

## Record the reduction in the spec, in "New since we talked"

The spec kept the original Summary verbatim — it is the operator's record of what was reported —
and put every consequence of the overtaking merge under **New since we talked**, one line each,
each tagged `inferred`: the race no longer exists on `main` (with the merge SHA and time), option
(a) is not taken and why, the one remaining gap, the reworded Acceptance 3, no planner phase, and
the coordination instruction being moot. **Decisions needed** stayed `None`. That is the shape
[skills/dispatch's Writing a spec](../../../skills/dispatch/SKILL.md) asks for, and it is what let
the implementer and tester read one document and know both what the issue originally said and
what it now owed.

## Related

- [fix-racing-a-workaround](fix-racing-a-workaround.md) — the neighbouring case: a *plan's*
  premises going stale when `main` lands a competing workaround, and the Rejected table as the
  instrument for telling "acceptance green on main" from "requirement met". This document is the
  case where `main` landed the *right* fix and the issue's remaining value is the proof.
- [race-regression-tests-that-fail-before-the-fix](../testing/race-regression-tests-that-fail-before-the-fix.md)
  — why the proof at the pre-fix parent is not optional.
