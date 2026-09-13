---
title: "Judging a long-lived branch's diff: compare against fork_point(main@origin | tip), not main@origin, or main's own additions read as your deletions"
category: legion
tags:
  - jj
  - review
  - retro
  - fork_point
  - phantom-deletion
date: 2026-09-13
status: active
module: legion (review, retro, and fresh-eyes analysis)
related_issues:
  - "LEGION-77"
  - "sjawhar/legion#1017"
---

# Judging a long-lived branch's diff

## What happened

LEGION-77's retro fresh-eyes scout ran `jj diff --from main@origin --to <tip>` on the issue branch
and reported that the branch had **deleted** an unrelated `packages/daemon/src/daemon/AGENTS.md`
bullet (the design-gate state shape) "with nothing replacing it", filing it as an accidental drop to
be caught in review. Against the branch's actual base the file's diff was 24 insertions and 0
deletions. The bullet had been *added to `main`* by #975 (LEGION-20) after the branch forked; a diff
anchored on the moving `main@origin` shows every later `main` addition as a deletion on the branch.

## The rule

Anchor on the fork point, never on `main@origin`, when a Legion branch has been alive for more than
one merge:

```bash
jj -R "$LEGION_WORKSPACE" diff --from 'fork_point(main@origin | <tip-sha>)' --to <tip-sha> --summary
```

The `legion-worker` skill's unchanged-diff fingerprint already does exactly this, for the same
reason. Anything that reviews, audits, or summarizes a branch — a reviewer's inline findings, a
retro scout, a thermo pass, a merger's `--summary` — must use the same anchor or it will report
phantom deletions and, worse, may "fix" them by reintroducing content that already exists on `main`
(a guaranteed conflict at merge). A finding of "the branch deleted X" is not accepted until the
deletion appears against the fork point.

## Where the moving anchor is correct

`jj diff --from <approved-sha> --to <tip>` for the retro/merger gate compares two commits on the same
branch and is unaffected. `main@origin..@` in `jj log` lists the branch's own commits correctly.
Only a *diff* whose `--from` is `main@origin` misleads.

## Related

- `skills/legion-worker/SKILL.md`, "The unchanged-diff check": the fork-point fingerprint recipe.
- `docs/solutions/legion/long-lived-branch-mechanics-jj-new-and-merge-not-rebase.md`: why Legion
  branches live long enough for `main` to move under them.
