---
title: "handoff_complete checks the handoff commit's App and nothing checks a code commit's, so a push can carry one of each and only one is guarded"
category: legion
tags:
  - jj
  - commit-identity
  - github-app
  - push
date: 2026-10-25
status: active
module: packages/claude-envoy
related_issues:
  - "INFRA-92"
  - "sjawhar/legion#2154"
---

# handoff_complete checks the handoff commit's App and nothing checks a code commit's, so a push can carry one of each and only one is guarded

Extends docs/solutions/legion/a-commit-made-outside-your-assignment-or-outside-your-panes-shell-carries-the-wrong-author-check-author-and-trailer-before-every-push-LEGION-631.md.

That document says how a commit comes to carry the wrong App and how to re-author it. The delta
is why a branch can carry one anyway with every gate green, and what a role does about it.

- **The guard is asymmetric, and reads as though it is not.** `handoff_complete` refuses a
  handoff carried by a commit another App authored, so a role's handoffs are checked on every
  round. Nothing checks the App of a **code** commit. A round that pushes code and a handoff
  therefore has one commit guarded and one not, and the unguarded one is the one that reaches
  `main`. The only thing that would catch it is the pre-push identity read in
  `skill://legion-worker/references/default-branch-check.md`, which is a step a role performs,
  not a gate.
- **Treat the working-copy commit you find at a turn's start as another role's until you have
  read it.** `jj log -r @ -T 'author.email()'`. When it is empty and not yours, `jj new @-`;
  never `jj split` it, which keeps its author and takes you as committer — the pair that put the
  review App on a code commit here.
- **Fixing it is an identity rewrite, and it must be provably content-identical.** Run the guard
  in `skill://legion-worker/references/conflicts-and-rewrites.md` first
  (`descendants(<commit>) ~ ::@` must be empty), take the unchanged-diff fingerprint at the tip,
  `jj metaedit --update-author -r <commit>`, move the bookmark with `--allow-backwards`, push
  once, take the fingerprint again, and post both. Equal fingerprints are what make "identity
  only" a fact rather than a claim.
- **Audit the whole branch afterwards, not the commit you rewrote.** A template that prints a
  line only where author and committer differ, over `main@origin..@`, costs nothing and is the
  only statement worth making: *every* commit above the base carries its own role's App in both
  columns. Each role's own handoffs are legitimately its own App — the planner's, tester's and
  reviewer's are the review App — so "they all match the implementer" is the wrong check.

## Evidence

INFRA-92's round-five code commit reached origin as `author legion-reviewer[bot], committer
legion-implementer[bot]`. Its cause was an empty working-copy commit a previous role's push had
left behind: the implementer split it, which kept that commit's author. Every handoff on the
branch was clean, because `handoff_complete` had refused anything else on six rounds; the code
commit had passed no such check.

The rewrite's fingerprint was `aef279e370c6…ce33fe` before and after, over the branch's whole
diff against its fork point with `.legion/` and `docs/solutions/` excluded, so the content was
provably unchanged. The branch audit afterwards printed nothing, with each handoff still carrying
its own role's App.

The tester found it by reading the branch rather than the handoff, which is why the record of a
record defect is worth keeping: the role that made it is not the role that should verify it.
