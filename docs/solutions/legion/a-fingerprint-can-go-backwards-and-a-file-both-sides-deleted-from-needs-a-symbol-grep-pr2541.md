---
title: "Reading a forward merge: the unchanged-diff fingerprint can go backwards, and a file both sides deleted from is read by grepping each removed symbol"
category: legion
tags:
  - forward-merge
  - unchanged-diff-fingerprint
  - red-main
  - deletion
date: 2026-10-31
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2541"
---

# Reading a forward merge: the unchanged-diff fingerprint can go backwards, and a file both sides deleted from is read by grepping each removed symbol

Two readings a forward merge needs that the fingerprint command and `git` do not give you.

- **A fingerprint is not monotonic: it returns to an earlier value when a merge takes a hunk out
  of the branch's own diff.** Say so in the merge comment, with the head that value belonged to.
  A reader comparing two merge comments and finding a fingerprint it has already seen will
  otherwise read it as a mistake in one of them. The case that produces it: your branch fixed
  something, `main` then fixed the same thing, and the merge takes `main`'s file whole, so the
  branch's copy leaves the diff.
- **Resolve a race with `main`'s own fix by taking `main`'s file whole**, so the branch carries
  no competing copy and the next merge has nothing to conflict on. Looking for `main`'s fix
  first, before writing your own, is
  `docs/solutions/legion/a-red-check-in-a-package-the-branch-does-not-touch-looks-for-mains-fix-first-pr2366.md`;
  fixing a red that is `main`'s in a pull request of its own rather than in the issue branch is
  `docs/solutions/legion/a-red-main-gives-you-at-a-forward-merge-is-fixed-in-its-own-pull-request-against-main-built-off-the-shared-working-copy-INFRA-366.md`.
  A fix made in the issue branch instead costs a fingerprint change, a counted round, and then
  the merge that removes it again.
- **When both sides delete from one file, a clean textual merge is no evidence.** Two deletions
  in separate hunks merge silently, and so does one side's deletion beside the other's
  rewrite of the lines around it. Grep the merge head for every symbol each side removed, and
  re-read the mounting of whatever the removal was about, rather than trusting that no conflict
  was reported.

## Evidence

sjawhar/legion#2541 (INFRA-264), four forward merges. At `525d94c5` the fingerprint went
`bb321eb5…` → `80def57e…`, which is the value `46ce6038` already had: `main`'s #2598 fixed the
claim-list parser this branch had fixed in `ba171533`, and the merge took `main`'s
`claims_doc_test.go` whole. The merge comment says so. At `c9f4cb6e`, `main`'s #2526 removed the
document room's keep-alive from the same `internal/dispatch/docs/websocket.go` and `service.go`
this branch removed the bearer door from; git merged the two deletions in separate hunks, and the
review's reading was a grep at the merge head for each removed symbol (`ActorHeader`,
`X-Dispatch-Actor`, `AgentTokens`, `refuseUnstorableActor`) plus a re-read of `/ws/doc/{room}`'s
mounting.
