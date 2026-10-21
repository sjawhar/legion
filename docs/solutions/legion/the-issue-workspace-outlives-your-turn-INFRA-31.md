---
title: "The issue workspace outlives your turn and belongs to five other roles: take your own change before anything writes, and announce a destructive proof with a checksum"
category: legion
tags:
  - workspace
  - jj
  - authorship
  - proofs
  - issue-pod
date: 2026-10-21
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
---

# The issue workspace outlives your turn and belongs to five other roles

Extends docs/solutions/legion/a-commit-made-outside-your-assignment-or-outside-your-panes-shell-carries-the-wrong-author-check-author-and-trailer-before-every-push-LEGION-631.md.

One jj workspace on the issue volume serves all six roles across every phase and every pod
replacement. Three consequences, each of which cost a round somewhere tonight.

- **Create your own change before anything writes to the working copy, subagents included.** The
  documented guard — `jj log -r @ --no-graph -T 'empty'`, then `jj new @-` or `jj new` — is correct
  only if nothing has written yet. A turn whose subagent edits a file first finds `empty` = false,
  takes `jj new`, and the later `split` inherits the **previous role's author**: four such commits
  on three branches, direction never varying (dispatch://INFRA-53). The obvious rule, "check before
  your first commit", is exactly the check that passed in all four cases. Run the guard as the
  turn's first repository action. When it has already happened, record the SHAs in the pull request
  body rather than rewriting published history.
- **A destructive proof is announced, stamped and restored in the same breath.** Proving a fence by
  removing it, or proving a replacement by restoring what it replaced, means the shared tree is
  briefly wrong for everybody. Before you touch it: say on the role topic what you are removing and
  for how long; record the head SHA and a checksum of the file you are about to edit; make the
  change, run the one command that is the proof, restore, and re-check the checksum against the
  recorded one. The checksum is what separates "restored" from "believed restored" — the file's
  mtime and your memory both say restored either way.
- **Prove a replacement by restoring what it replaced.** The strongest evidence that the new path
  is doing the work is that putting the old one back breaks it: delete the `flock`, see the install
  race return; unset the relocation, see the cache land on the container filesystem. A proof that
  only exercises the new path cannot distinguish "the fix works" from "the problem was not here".

## Deciding what to re-prove after someone else moves the branch

A forward merge or another role's round changes what your earlier proofs cover. Read the **shape**
of the diff before deciding: which files it touched, which of your proofs name those files, and
which claim rests on a symbol that moved. Then grep for the moved name **repository-wide, not over
the diff** — a rename that the merge applied in one package leaves prose, tests and a Dockerfile
elsewhere naming the old symbol, and the diff cannot show you a file it did not touch.
