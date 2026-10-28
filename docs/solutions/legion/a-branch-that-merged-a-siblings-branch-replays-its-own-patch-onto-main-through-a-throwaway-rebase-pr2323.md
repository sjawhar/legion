---
title: "A branch that merged a sibling's branch replays its own patch onto main through a throwaway rebase"
category: legion
tags:
  - forward-merge
  - squash-merge
  - jj
  - conflicts
date: 2026-10-28
status: active
module: skills/legion-worker
---

# A branch that merged a sibling's branch replays its own patch onto main through a throwaway rebase

Extends docs/solutions/legion/a-stacked-branch-whose-base-squash-merged-conflicts-with-its-own-content-merge-and-fingerprint-against-the-old-base-commit-not-jjs-fork-point-LEGION-629.md.

That document classifies each file by three hashes and merges by hand. When your branch merged a
sibling's branch (not only stacked on it), and the sibling's pull request then squash-landed on
main, let jj do the three-way merge for every file at once, against the right base:

1. Find `S`, the last sibling head your branch merged (the second parent of your newest
   `merge <sibling branch> at …` commit). Check that `S` is an ancestor of your tip and that your
   tip merged no main commit after the one `S` merged; otherwise `S` is the wrong base.
2. Build your own patch as a throwaway commit on `S` and rebase only that commit onto main:

   ```bash
   jj -R "$LEGION_WORKSPACE" new <S> -m "tmp: own patch over S" &&
   jj -R "$LEGION_WORKSPACE" restore --from <your tip> &&
   jj -R "$LEGION_WORKSPACE" rebase -r @ -d '"main"@origin'
   ```

   The rebase merges main with `diff(S → tip)`, which is exactly your branch's work. The
   sibling's own lines are on both sides and cancel, so only files where main and your work touch
   the same lines conflict. Nothing else depends on the throwaway commit, so rebasing it rewrites
   nothing anyone shares.
3. Resolve the remaining conflicts in the throwaway commit, then make the forward merge the
   ordinary way (`jj new legion/<KEY> '"main"@origin'`), and give it the resolved tree with
   `jj restore --from <throwaway>` before abandoning the throwaway. The merge commit keeps both
   parents, so the push is a fast-forward.
4. Confirm the merge's diff against main lists only your files
   (`jj diff --stat --from '"main"@origin' --to @`): the sibling's files and its `.legion/`
   directory drop out on their own.

## Evidence

Pull request #2323 merged its sibling's branch (#2390) four times. After #2390 squash-landed as
`d47dbf05`, the plain forward merge of main conflicted in 26 files. Replaying the branch's patch
over the sibling head it last merged (`76483b14`) left 7 conflicts, every one a real overlap
with main (new fields beside this change's `DashboardEntry`, main's new Playwright projects in
the restructured config, main's new provider harness in `connection.test.ts`). The merge commit
`8675beea` carried the result; its diff against main was 77 files, all this change's, and the
review compared the changed lines before and after for each file and found 59 identical and the
other 14 differing only by main's lines.
