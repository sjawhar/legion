---
title: "A run-local workaround for an open bug is flagged, printed on every run, recorded, and made a close criterion — never silent"
category: legion
tags:
  - workaround
  - proof
  - smoke-rig
  - kind-smoke
  - kubernetes
  - provisioning
  - close-criteria
date: 2026-09-15
status: active
module: scripts/kind-smoke (up.sh legion-177 keeper, checkpoints.sh apply_legion_177_workaround, README)
applies_when:
  - A proof rig cannot reach its checkpoint because of a bug in the system under test that is tracked elsewhere
  - You are tempted to patch around that bug inside the rig so the run goes green
  - A checkpoint's OK line is being read as evidence that the production path works
related_issues:
  - "LEGION-26"
  - "sjawhar/legion#1114"
  - "LEGION-177"
---

# A run-local workaround is flagged, printed, recorded, and a close criterion

The kind smoke's first real run could not get past its resume checkpoint: on the worker image's
git 2.47, the `credential.interactive=false` that provisioning writes into the tree's shared clone
makes the *next* pod's init container fail its `jj git fetch` with `unable to get password from
user` (LEGION-177) — every phase worker and every resurrected root, not only the first
replacement; the planner failed six generations in a row. The bug is the daemon's, tracked on its
own issue, and no rig change fixes it. The rig still has to run today.

## The shape that keeps the proof honest

- **Flagged.** `SMOKE_LEGION_177_WORKAROUND` (default `1`) gates it; `0` turns every part of it off.
- **Mechanical and narrow.** `up.sh` starts one recorded host loop, `legion-177-keeper`, that
  every `SMOKE_LEGION_177_INTERVAL` seconds runs `git config --unset credential.interactive` in the
  shared clone through each Running Legion pod, and `kill-pod-resume` applies the same one-shot
  unset right before the kill. It changes exactly the setting the bug is about, nothing else.
- **Printed on every run, in every place a reader could be misled.** `up.sh`'s ready block has a
  `legion-177:` line (`keeper (pgid …, every 3s; LEGION-177 workaround)` or `off`);
  `kill-pod-resume` prints `WORKAROUND LEGION-177 applied` (or `applied (already unset)`) before
  its verdict, and its OK detail says whether the keeper was running; a replacement pod whose init
  container fails is reported as `LEGION-177 without the workaround? SMOKE_LEGION_177_WORKAROUND=…`
  with the `workspace-init` log tail; the keeper logs each pod and time it acted.
- **Recorded.** `records/legion-177-workaround` holds `keeper` or `off`, so a transcript read later
  says which kind of run it was.
- **A close criterion.** The README's close rule: the issue's `kill-pod-resume` is signed off green
  only by a run with `SMOKE_LEGION_177_WORKAROUND=0` on an image that carries LEGION-177's fix. A
  green line with the keeper running proves the resume path works *around* the bug; it is not the
  production path and does not close the issue.

## Why not quietly patch it

A silent workaround inside a proof rig turns the rig into the thing that hides the bug: every
later run is green, the runbook's failure row is never exercised, and the day the image finally
carries the fix nobody re-runs without the patch to notice whether it works. Sami's rule for this
deployment is that the agent that built something proves it in production; a rig that papers over
a production bug proves the rig, not the change. Flag it, print it, record it, and write down
what green-without-it looks like.

## Related

- `docs/solutions/legion/fix-racing-a-workaround.md`: the same tension when the fix and the
  workaround land in the same window.
- `docs/kubernetes.md`, "Runbook: the kind smoke", the LEGION-177 row.
