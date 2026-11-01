---
title: "The authorship guard reads false once you have edited, and a mis-authored commit still unpushed is rebuilt rather than recorded"
category: legion
tags:
  - jj
  - commit-identity
  - working-copy
  - shared-workspace
  - review-round
date: 2026-10-22
status: active
module: packages/pi-legion
related_issues:
  - "INFRA-127"
  - "sjawhar/legion#2108"
---

# The authorship guard reads false once you have edited, and an unpushed mis-authored commit is rebuilt

Extends docs/solutions/legion/the-issue-workspace-outlives-your-turn-INFRA-31.md.

That page's rule — run `jj log -r @ --no-graph -T 'empty'`, then `jj new @-` or `jj new`, as the
turn's **first** repository action — is right, and two deltas make it survive contact.

- **The guard does not fail loudly when you run it late; it lies.** Run after your own edits, it
  prints `false`, which the documented reading turns into "`@` holds only your own resumed edits,
  so `jj new`" — the one branch that keeps the working-copy commit you inherited, and its author.
  A turn that starts after another role's push is exactly when that commit is somebody else's.
  Treat `false` at your first commit as unresolved unless you read `true` before you touched
  anything: read the author instead (`jj log -r @ --no-graph -T 'author.email()'`), and compare it
  with your role's App.
- **A review round is a second turn in the same session, and it starts on another role's commit.**
  The guard is easy to associate with "the start of my phase" and then skipped on the round that
  answers a review, where the reviewer's push is what the working copy now sits on.
- **Unpushed, rebuild it; pushed, record it.** INFRA-31's remedy (record the SHAs in the pull
  request body rather than rewriting published history) applies to a commit origin already has.
  While it is still local, the clean fix costs three commands and leaves no trace:

  ```bash
  old=$(jj -R "$LEGION_WORKSPACE" log -r @ --no-graph -T 'commit_id')
  jj -R "$LEGION_WORKSPACE" new <the branch head> -m "<your message>"
  jj -R "$LEGION_WORKSPACE" restore --from "$old"
  jj -R "$LEGION_WORKSPACE" abandon "$old"
  ```

  The new commit is yours, carries your edits and your `Omp-Session:` trailer, and the mis-authored
  one never existed.
- **The identity check before the push is what catches it**, so run it on every push, not only the
  first of a phase: `default=$(gh api repos/<owner>/<repo> --jq '.default_branch | @json')` then
  `jj log -r "${default}@origin..@" -T 'author.email() ++ " | " ++ committer.email() ++ "\n"'`,
  and read **your own** commits in both columns.

## Evidence (INFRA-127, sjawhar/legion#2108)

Round 2 of this pull request answered a review. jj had left the working copy on an empty commit on
top of the reviewer's handoff push, authored by the review App; the first edits of the round went
into it, the guard was run after them and read `false`, and the `jj split` that followed produced
a commit authored by `legion-reviewer[bot]` holding the implementer's work. The default-branch
identity check caught it before the push, the commit was rebuilt by the four lines above as
`f255cf20`, and the mis-authored one was abandoned, so nothing wrong reached origin and no
published history was rewritten.
