---
title: "A red check that main gives you at a forward merge is fixed in its own pull request against main, built without touching the shared working copy"
category: legion
tags:
  - forward-merge
  - red-main
  - shared-workspace
  - git-data-api
  - skills-guard
date: 2026-10-27
status: active
module: skills/legion-worker
supersedes: docs/solutions/legion/a-conflict-that-arrives-while-another-role-holds-the-phase-is-prepared-in-the-working-copy-reported-and-pushed-after-the-backward-move-LEGION-663.md
related_issues:
  - "INFRA-366"
  - "sjawhar/legion#2210"
  - "sjawhar/legion#2301"
---

# A red check that main gives you at a forward merge is fixed in its own pull request against main, built without touching the shared working copy

This corrects one sentence of the superseded note. That note says the shared working copy is
"yours to shape and nobody else reads until it is pushed". That is false once another role holds
the phase: its next `jj new @-` takes your uncommitted edits off `@`.

- **First, show that the red is main's.** After a forward merge, check whether the failing check's
  inputs are identical to `main@origin`. For example, `jj diff --from main@origin --to <merge>
  <paths>` prints nothing. If they are identical, main is red too, and your branch did not cause
  it.
- **Report the red to the architect, and fix it in a pull request of its own against `main`, never
  in the issue branch.** A fix in the issue branch moves its fingerprint, sends it back to review,
  and leaves every other open branch red. The architect and the controller decide where the fix
  goes.
- **A check a pull request passed on its base does not protect `main`.** With no merge queue
  re-running it, two pull requests that each pass can turn `main` red together. A total budget
  over several files is the easiest check to break this way.
- **While another role holds the phase, do the side work outside the shared working copy:**
  - Build the commit through GitHub's git data API from `main`'s tree: blob, tree, commit, ref.
  - Run the checks in a private copy under `$TMPDIR`. A `cp -al` hardlinked copy of the workspace
    keeps `node_modules` and costs no disk.
  - In that copy, `rm` each file before you write it. A redirect or any in-place write changes
    the workspace's own inode, because the copy shares it.
  - Run no jj or git command in the copy, and delete it after.
- **Arm auto-merge on the side pull request only when the architect or the controller tells you
  to.** The daemon does not track that pull request, so nothing disarms it after a push. After the
  arm, push nothing more to its branch.

## Evidence

- **How main went red:** #2273 changed one line of `skills/dispatch/SKILL.md`, which took it from
  738 to 796 words and the three Dispatch skills from 1999 to 2057. Its own `test` check had
  passed.
- **How it showed at the merge:** at the forward merge of `main` bc98d3e7 into #2210 (737a099b),
  `packages/pi-envoy/src/skills-guard.test.ts` ("fewer than 2,000 words") failed.
  `jj diff --from main@origin --to 737a099b skills/` printed nothing, so the red was main's.
- **Where the fix went:** the architect first asked for the trim in #2210, and then, by the
  controller's decision, moved it to its own pull request against `main`.
- **The lost edit:** the implementer had already drafted the trim, uncommitted, in the shared
  working copy. The tester's `jj new @-` left it as a visible sibling commit (51543820), which the
  tester found and the implementer abandoned.
- **The fix pull request:** #2301 was built as blob, tree, commit and ref on `main`'s tree. Its
  guard tests ran in a `cp -al` copy, with the one file removed before it was written. Auto-merge
  was armed as the architect asked.
