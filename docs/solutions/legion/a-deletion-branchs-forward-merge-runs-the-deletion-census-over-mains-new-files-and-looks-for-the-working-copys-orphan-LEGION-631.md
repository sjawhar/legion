---
title: "A deletion branch's forward merge runs the deletion census over main's new files, and looks for the working copy's orphan"
category: legion
tags:
  - jj
  - forward-merge
  - deletion
  - census
  - working-copy
  - orphan
date: 2026-10-10
status: active
module: packages/daemon
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# A deletion branch's forward merge runs the deletion census over main's new files, and looks for the working copy's orphan

Extends docs/solutions/testing/the-comparability-grep-runs-again-after-every-forward-merge-of-main-and-a-port-to-deepequal-keeps-slices-equals-nil-rule-LEGION-578.md.

- The grep that note makes a step of every forward merge is, for a branch that deletes a mechanism,
  the branch's whole deletion census: every name it removed (commands, routes, environment
  variables, tool operations, files). Code `main` gained after the fork point was written against
  the mechanism as it was, and arrives in files the branch never touched — a test fixture that sets
  the deleted variable, a stand-in binary on a harness `PATH`, a prompt sentence naming the deleted
  command, a golden row — with no conflict marker. Run the census over
  `jj diff --from <last base> --to main@origin --name-only` before the merge commit is pushed, and
  keep the pattern list in the implement handoff so the next round runs the same one.
- A forward merge is `jj new <branch> main@origin`, and the working copy you leave is kept as a
  sibling, not carried: edits you had not split into a commit stay on an orphan beside the merge,
  and the merge's head lacks them with no warning. Split or discard the working copy first, and
  after the merge read `jj log -r 'children(<old tip>)'` — one child, the merge, is the expected
  answer; a second is work to recover into the merge commit.

## Evidence

sjawhar/legion#1843 (five forward merges). The round-6 merge cd3f9b30 (main 8cce34e4) brought
LEGION-634's omp-harness stand-in `legion` with `LEGION_GRANT_FILE`, LEGION-640's api test posting a
`commit`, and LEGION-588's prompt wording, each in a file the branch had not touched; the round-5 merge
had brought main's own new `LEGION_JJ_PATH` reader (`cmd/legion/handoff.go`, LEGION-605) and `legion
gh` mentions in the capabilities row and its golden (`.legion/LEGION-631/implement.json`,
`trickyParts`). The same `jj new` left the implementer's uncommitted edits — the `HANDOFF_PHASES`
export and `handoff-schema.ts` deletion, four comment sweeps — on the orphan 59280edf beside
cd3f9b30; they were found in round 7 by `jj log -r 'children(e2a3683493b6)'` and redone in the merge
b643a80b, which the reviewer's round-1 census then read as the deletions "coming back".
