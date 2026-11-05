---
title: "A jj command that rewrites the working copy under a running command makes it measure another commit"
category: legion
tags:
  - jj
  - shared-workspace
  - background-job
  - false-red
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2727"
---

# A jj command that rewrites the working copy under a running command makes it measure another commit

Extends docs/solutions/legion/a-file-written-while-the-working-copy-is-the-commit-you-pushed-amends-that-commit-pr2519.md.

That document covers the writer side: a tool that writes into the workspace while `@` is a commit
you care about folds its file into that commit. This is the reader side, which fails the other way
round and leaves no trace at all.

- **A long build, test or lint run in the workspace reads the working copy for its whole
  duration.** `jj new @-`, `jj restore`, `jj abandon`, `jj op restore` and `jj workspace
  update-stale` all rewrite the files on disk. Run one while a build is in flight and the build
  compiles a tree nobody asked for. jj prints nothing about it and neither does the build: the
  output names no commit, so the result reads as an ordinary verdict on your change.
- **So let no jj command that changes the checkout overlap a run you will read.** Wait for the run
  to finish, or make the commit first and start the run after it. A result whose window contains
  such a command is void — re-run it, never reason about which half it measured.
- **A red whose failing cases are exactly the ones the fix repairs is the first thing to
  re-run.** That shape is what a reverted checkout produces, and it is indistinguishable from a
  real regression in the output.
- **When the turn-start working-copy commit already holds your edits** and your role prompt forbids
  `jj describe` and `jj split` on it, do not `jj new @-`: that checks the parent out and strands
  the edits in a commit the branch never reaches. Make the described commit first and bring the
  files across:

  ```bash
  jj -R "$LEGION_WORKSPACE" new @- -m "<message>"
  jj -R "$LEGION_WORKSPACE" restore --from <the turn-start change id> <paths…>
  ```

  The new commit carries your role's author, the turn-start commit is never rewritten, and the
  edits land where the bookmark will point.

## Evidence

On sjawhar/legion#2727 the implementer left `go test ./internal/admit/ -count=1` running in the
background in `$LEGION_WORKSPACE` and, in the same window, ran `jj new @-` to start the described
commit. `@-` was `01c895950bde`, the branch point, so the checkout reverted
`packages/daemon/internal/admit/admit.go` to the code before the fix for part of the run, and the
`jj restore` that followed put it back. The run reported
`--- FAIL: TestAHeldRootKeepsItsPlaceAndNothingBehindItTakesTheSlot/the_Dispatch_consumer_hold`
and `--- FAIL: …/both_holds` — precisely the two cases the fix repairs, and precisely the
pre-fix result — beside a wall of unrelated teardown failures. A second run after the tree had
settled, read verbatim, showed both cases `PASS`.

The recovery recipe is the one that made `00e6cc42101c83b14f08eb479a4f8e7b7fcf0a82`, whose parent
is `01c895950bde` and whose author is `343353655+legion-implementer[bot]`, the role that wrote it:
`jj new @- -m "<message>"` then `jj restore --from tqvvupomwqsk <the three changed paths>`, with
the empty turn-start commit left alone.

The prevention for the second half is already written:
docs/solutions/delegation/a-shared-issue-workspace-hands-your-commit-another-roles-app-check-the-author-next-to-the-commit-INFRA-42.md
says to run `jj new` before the turn's **first edit**, not before its first commit. This is what to
do once that moment has passed.
