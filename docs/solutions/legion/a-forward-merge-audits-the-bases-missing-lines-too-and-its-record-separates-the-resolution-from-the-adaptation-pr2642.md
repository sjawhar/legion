---
title: "A forward merge audits the base's missing lines too, and its record separates the text resolution from the adaptation the base's new refusals force"
category: legion
tags:
  - forward-merge
  - long-lived-branch
  - conflict-resolution
  - acceptance-proof
date: 2026-11-04
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2642"
---

# A forward merge audits the base's missing lines too, and its record separates the text resolution from the adaptation the base's new refusals force

- **A merge's conflict set does not bound its behaviour change.** Run the branch's own acceptance
  proof at the merge head before you post the record, even when nothing conflicted: the base can
  add a refusal, a guard or a parameter that no marker touches and the build accepts.
- **Account for the base's lines that are now missing, not only for your own.** A resolution that
  keeps "your side" of a region the base also changed drops the base's work silently, and nothing
  fails. Name, per file, what of yours survived and what of the base's did; a clean `git
  merge-tree` is not that audit.
- **Resolve a modify/delete by behaviour, and re-home the base's reason.** When your branch moved
  the code and the base modified it where it was, find the behaviour at its new home and prove it
  survives, keep the file deleted, and put the base's explanatory comment on the surviving caller.
- **Where both sides wrote into one handler, the merged statement order is the semantics.** Say in
  the record which side runs first and why neither supersedes the other, then re-anchor the
  branch's assertions on the answer the base now gives — and prove the branch's own half is still
  load-bearing with a control that removes the base's new path.
- **Keep the resolution and the adaptation as separate commits.** The merge commit resolves text;
  a commit above it adapts the branch's rig to the base's new behaviour. A reader of the record can
  then see which is which, and the adaptation's own diff shows no assertion was weakened.

## Evidence

sjawhar/legion#2642 took twelve forward merges over its life. Four resolved cleanly at the site that
later broke:

- main's listener began refusing a subscribe that carried a port; the branch's rig registered with
  one, so no pod became ready and every subtest timed out. The adaptation is its own commit above
  the merge and changed no assertion.
- main's `TestMain` guard stripped the variables the rig hands its role child (see
  `docs/solutions/testing/a-guard-the-base-adds-to-a-shared-testmain-disarms-every-product-child-that-re-enters-the-binary-pr2642.md`).
- main's resume path reached a nil tree store in the branch's release takeover and panicked under a
  test the base had added.
- a resolution that kept the branch's side dropped main's #2318 timeout fix, whose behaviour the
  branch had moved into a shared helper. Nothing failed; the **reviewer** found it, and a later
  commit carried it back.

The modify/delete in the same issue was settled that way: the base added a `dir` argument whose
behaviour was `cmd.Dir = dir`, the branch had moved those tests to another package, and the
resolution proved the behaviour already existed at the new home, kept the file deleted and moved
the base's sentence to the surviving caller's doc.
