---
title: "A step the harness could not measure is not judged, and a recorded run is read by what it recorded"
category: testing
tags:
  - load-test
  - measurement
  - curves
  - knee
  - comparability
date: 2026-11-03
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2462"
---

# A step the harness could not measure is not judged, and a recorded run is read by what it recorded

Extends `docs/solutions/testing/an-optional-wire-field-is-pinned-by-presence-in-a-lane-that-always-runs-pr2508.md` and `docs/solutions/testing/a-local-cpu-figure-belongs-to-the-box-and-the-hour-so-only-runs-interleaved-in-one-session-compare-pr2466.md`.

- **Keep "the system failed the criterion" apart from "the harness could not measure this step".**
  A step that the harness marked failed (a container restart, a sampler gap, a failed copy-back) gives no number.
  It is neither a knee nor the end of a bracket, and it does not stop the walk.
  The walk compares each step with the last step it judged, and lists each failed step as "not judged" with its failure kinds.
  If a failure must count as a load limit (an OOM under load), give it its own kind and keep only that kind in the walk.

- **Take a step's kind from the fields its producer recorded, never from its name.**
  When you add the deciding field, test for the field, not for the object that holds it.
  A run written before the field existed must still reach the old rule. Test on such a run.

- **Record what makes two runs comparable in the run itself.**
  That is the seed's own census (the same counts query for every seeding path) and every image by digest.
  Compare that metadata before comparing two runs' numbers.
  A new reference seed can move a knee from 10x to 5x on the same commit.

## Evidence

sjawhar/legion#2462:
- `packages/envoy/loadtest/curves.go`:
  - `findKnees` (line 624) keeps the last judged step;
  - KNEES.md lists each failed step as not judged (lines 987-991);
  - a step's kind comes from `copy-meta.json` only when its `phase` is set (line 154, `ok && c.Phase != ""`).
- `packages/envoy/loadtest/common.sh`: `SEED_COUNTS_SQL` (line 362) is shared by `stack.sh` and `cluster.sh`.
- Round 1, thread `r4421076798`: a `sampler_gap` at 5x was reported as the knee and hid the real knee at 7x. Fixed in 6facd647, with `TestCurvesWalkTheKneesPastAFailedRow`.
- The name-based kind was replaced in 6facd647 and reopened twice, because any `copy` object skipped the name rules for older runs. Fixed in 005e8aa7, with `TestCurvesNameTheKindOfACopyWithoutAPhase`.
- With main's reference seed (#2469), the knee on Dispatch 9b92ac14 moved from 9.5x-10x to 5x. The published results v4 were measured before that seed (the PR body's "Not proven / risk").
