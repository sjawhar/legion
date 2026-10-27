---
title: "Moving a record to another tree names the fence of every outbox row its old tree can still have pending"
category: daemon
tags:
  - outbox
  - fence
  - generation
  - adoption
date: 2026-10-26
status: active
module: packages/daemon
related_issues:
  - "INFRA-252"
  - "sjawhar/legion#2226"
---

# Moving a record to another tree names the fence of every outbox row its old tree can still have pending

- Code that moves an issue from one tree to another (adoption, hand-over, re-admission) lists
  every outbox kind its old tree can still have queued, and names the fence each one checks
  before it acts. Put that list in the function's doc comment.
- Bumping the issue's generation fences only the rows that compare the issue's generation. A row
  fenced on the old root's linger generation can match again once the new root lingers at the same
  number; give such a row the tree it was queued for and finish it without acting when the issue's
  tree differs.
- Each row kind left unfenced is a test: queue it for the old tree, move the issue, run the row,
  and assert it acts on nothing.

## Evidence

`workflow/adopt.go` `Adopt` bumps the generation and names what that fences: the old tree's linger
close, its supervise rows and its issue suspend. `WorkspaceRemove` was fenced on the linger
generation alone, so an old linger's removal could delete an adopted child's workspace; it now
carries `Tree` (`record/outbox.go:400`), written by `workflow/linger.go` and checked in
`daemon/outbox.go` `removeWorkspace`, covered by `TestAWorkspaceRemovalOfATreeTheIssueLeftActsOnNothing`.
`tree_close` rows are still fenced on the issue's current root, recorded on the pull request as a
risk: a never-run tree has no claims when its linger expires.
