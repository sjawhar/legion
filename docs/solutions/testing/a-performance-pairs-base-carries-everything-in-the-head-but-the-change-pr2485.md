---
title: "A performance pair's base carries everything in the head but the change, and each extra is a pair of its own"
category: testing
tags:
  - load-test
  - measurement
  - base-construction
  - dependency-pin
date: 2026-11-02
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2485"
---

# A performance pair's base carries everything in the head but the change, and each extra is a pair of its own

Extends docs/solutions/testing/two-base-runs-decide-whether-the-box-can-resolve-the-effect-before-the-full-set-is-spent-pr2522.md.

- When your head differs from main by more than your change (a dependency pin, a sibling's unmerged commit, a harness that lives on another branch), build the base to carry the same extra: pin the base to the same release, cherry-pick or merge the same commit. State that before the runs.
- Measure each extra's own cost as a separate, labelled pair (main on the old pin against main on the new one), and report it as that extra's cost, not your change's.
- When the head finishes more work than the base, or in a longer window, compare rates (cores, or cost per unit of work), not CPU-seconds.
- Name the resource that bounds a metric before you read it as your effect. A metric that moves with run order on both sides is the box, not the change.
- Drop and run again a pair whose leg something else disturbed (a pod replacement, your own profiling on the same box), and say in the record why it was dropped.

## Evidence

- The INFRA-512 architect ruled that "not slower" is judged on equal ygo: each replay base was main with only `go.mod`, `go.sum` and the one `newDocumentCopy` line moved to the head's pin (54a63596), and the upgrade's own pair (2b3379a0 on sami.4 against 54a63596 on sami.3) read 1074.4 against 1120.4 Dispatch CPU-s, reported as main's cost.
- The editor harness lived only on INFRA-605's unmerged branch, so both editor legs carried it (ecbaee7d against bdf27ffb), and after #2522 changed the measured render, both legs carried that too (13666dd2 against 7983a59f).
- Editors-10x: the base windows ended at 145 and 155 s and the head's at 184 and 183 s while the head acked about 2.5 times the updates, so the pull request compared cores and cost per unit of work. REST 2xx in the 1x step fell with run order on both sides (31.1, 23.2, 18.2, 17.8%) while Postgres held 0.80-0.92 of 2 cores.
- Pair 1 at 9cc41e31 was dropped because this box's own pprof extraction ran during its base leg, which then served half the work; earlier pairs split by the 2026-10-31 and 2026-11-01 pod replacements were dropped and run again.
