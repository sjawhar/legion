---
title: "A sibling role's uncommitted restore can become your pushed head: check the parent before jj new and before the bookmark move"
category: legion
tags:
  - legion
  - jj
  - shared-workspace
  - forward-merge
  - negative-control
date: 2026-11-01
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2599"
  - "INFRA-429"
---

# A sibling role's uncommitted restore can become your pushed head: check the parent before `jj new` and before the bookmark move

Extends docs/solutions/legion/a-red-main-gives-you-at-a-forward-merge-is-fixed-in-its-own-pull-request-against-main-built-off-the-shared-working-copy-INFRA-366.md,
which tells the role doing side work to keep it out of the shared working copy. This is the other
seat: what the role doing the *assigned* work must check, and how to recover when it did not.

The six roles of an issue share one jj working copy, and jj snapshots it on every command. So a
sibling's uncommitted edit becomes a commit under you at your next `jj new`, and your
`bookmark set -r @-` then **publishes** it. The outcome is worse than the loss INFRA-366 records:
the branch head carries a change nobody reviewed, and if that edit was a negative control, the
published head *undoes* the work.

- **Before `jj new` and again before `bookmark set -r @-`, read the id and description of the
  commit you are about to build on or point at**, and stop if it is not the one you expect. The
  parent `jj new` reports is not necessarily the one you last saw: a sibling can have moved the
  working copy between your two commands.
- **Run a negative control by restore, build, run, restore — and commit nothing.** Put back the
  branch head's bytes the moment the run is over
  (`jj restore --from legion/<KEY> <path>`), and rebuild whatever the restore invalidated, or the
  next role's run reads your control instead of the head.
- **Recovering a published commit that is not yours** is the sanctioned non-fast-forward move,
  after its guard:

  ```bash
  jj -R "$LEGION_WORKSPACE" git fetch
  jj -R "$LEGION_WORKSPACE" log --no-graph -T 'commit_id.short() ++ "\n"' -r 'descendants(<bad>) ~ ::@'   # must be empty
  jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r <good> --allow-backwards
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY>
  git merge-base --is-ancestor <bad> <new head>   # must answer no
  ```

  Prefer this to merging over the bad commit. A merge leaves it in the history and needs its
  content resolved away by hand, which is a second chance to publish it.
- **Tell the sibling at once, on its role topic**, with what its edit was and what you did: it is
  mid-run and its next command will see a working copy it did not leave.

## Evidence

In sjawhar/legion#2599 the architect directed a forward merge while the tester was proving the
change's own negative control, which is a restore of
`packages/dispatch/web/src/features/settings/AgentTokensSection.tsx` to its pre-change text. jj
snapshotted that restore into `569fd73932b7`, "test: scratch working copy at the pull request
head"; the implementer's `jj new` landed on it rather than on the merge it had just made, and
`bookmark set -r @-` pushed it. For about ten minutes `legion/INFRA-429`'s head carried the old
paragraph — the one the change exists to remove.

The first repair made it worse in a small way: a merge of the forward merge over `569fd739`
auto-resolved that file to the *reverted* side, because the merge base changed nothing there, so
the file had to be restored out of the merge by hand before pushing. The clean repair was the
guard and the backwards move above: `descendants(569fd739) ~ ::@` was empty, the bookmark moved
onto the forward merge, and `git merge-base --is-ancestor 569fd739 5f07dadd` answered no.
Recorded on the pull request at `#issuecomment-6541224931`.
