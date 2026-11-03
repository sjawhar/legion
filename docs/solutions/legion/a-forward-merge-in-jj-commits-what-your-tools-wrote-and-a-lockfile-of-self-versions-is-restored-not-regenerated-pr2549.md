---
title: "A forward merge in jj commits what your tools wrote, and a lockfile whose only diff is self-versions is restored, never regenerated"
category: legion
tags:
  - jj
  - forward-merge
  - bun-lock
  - legion-worker
date: 2026-11-01
status: active
module: skills/legion-worker
supersedes: docs/solutions/legion/a-forward-merge-into-a-branch-that-moved-files-ports-mains-edits-to-the-new-paths-LEGION-247.md
related_issues:
  - "sjawhar/legion#2549"
---

# A forward merge in jj commits what your tools wrote, and a lockfile whose only diff is self-versions is restored, never regenerated

This corrects one clause of
docs/solutions/legion/a-forward-merge-into-a-branch-that-moved-files-ports-mains-edits-to-the-new-paths-LEGION-247.md
— "take main's manifest versions, contract numbers and lockfile, then regenerate the lockfile
(`bun install`)". Regenerating is what puts main's unreleased version churn inside your pull
request. Every other rule of that page stands.

- **The merge commit is your working copy, so anything a tool writes after the merge lands in it.**
  A merge in jj is the working-copy commit; an install, a build or a formatter you run to check the
  resolution writes into the commit whose message promises a resolution, where no reviewer looks.
  Before you describe it, diff it against **both** parents and account for every line that matches
  neither.
- **Read a lockfile's diff by key before you commit it.** When every changed key is a workspace
  package's own `version` and no dependency edge moved, the repository's committed lockfile is
  behind its own manifests on the base, and your install is landing that drift, not fixing your
  merge. Restore the file from `<base>@origin`.
- **Run no install after the restore.** The install puts the versions straight back and the
  restore commit lands empty. Where a later check needs `node_modules`, install first and restore
  last.
- **Check the restore commit is not empty before you push it.** jj commits and pushes an empty
  commit without a word, and two commits with the same message, one of which does nothing, is what
  a reader then has to disentangle.

## Evidence

At the merged head of sjawhar/legion#2549 the repository itself was inconsistent: `bun.lock` held
`11.1.0`, `13.1.0` and `14.1.0` for `packages/envoy-plugin`, `packages/pi-envoy` and
`packages/pi-legion`, whose own `package.json` files held `12.7.1`, `13.11.1` and `15.7.1`. Main
carries both halves, and the lockfile is the stale one.

The merge commit `947d99df` therefore carried `bun.lock +3 -3`, written by the `bun install` run to
check the resolution: a value neither parent held. `5a069c7e` reverses exactly those three lines.
The first attempt at that restore, `6a197786`, is on the branch as an empty commit with the same
message, because `bun install` ran again after the restore.
