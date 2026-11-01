---
title: "A rebase record the pod's legion gh refuses as a pull request comment goes on the body's Chain line and the Dispatch issue"
category: legion
tags:
  - legion-gh
  - pull-request-comment
  - rebase-record
  - fingerprint
  - dispatch-message
date: 2026-10-11
status: active
module: packages/daemon/cmd/legion
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---

# A rebase record the pod's legion gh refuses as a pull request comment goes on the body's Chain line and the Dispatch issue

- The implementer's role text asks for the fingerprints before and after a retarget to be
  posted. Under a deployment whose `legion` binary predates LEGION-631 (one whose pod `gh` is the
  shim `exec legion gh -- "$@"`), a pull request comment is not a surface that can carry them:
  `gh pr comment`, `gh api repos/<o>/<r>/issues/<n>/comments` and GraphQL `addComment` are all
  classified by `cmd/legion/gh.go`'s gate as a GitHub-issue write (a pull request is an issue to
  GitHub's API) and refused with "Legion issues live on Dispatch; use `dispatch message` or
  `dispatch comment` on <KEY>" or "Legion never merges a pull request …". The gate is the
  deployed binary's, not the branch's: a branch past LEGION-631 has no `legion gh` at all, and
  its plain `gh` posts the comment.
- Put the record where the roles read it instead. The PR body's `Chain` line carries the merge's
  SHA and parents, both fingerprints by the reference command, and the per-file list of hunks
  that differ by design (the merger quotes the body in READY; the reviewer and tester recompute
  the figures from it). A `dispatch message` on the issue carries the same facts for the humans
  and the architect, with the role's attribution footer. Review-thread replies are not refused:
  they are `pulls/<n>/comments/<id>/replies`, a pull-request review write.
- Say in the body that the comment was refused, so a reader of the role text does not look for
  it; and do not retry through another route (`gh api graphql` is gated by the same classifier).

## Evidence

LEGION-663 (sjawhar/legion#1874), the retarget to `main` (`cee5c6fc`). `legion gh -- pr comment`
exited silently with nothing posted; `legion gh -- api repos/sjawhar/legion/issues/1874/comments
--method POST` printed "Legion issues live on Dispatch; use `dispatch message` or `dispatch comment`
on LEGION-663"; `legion gh -- api graphql … addComment` printed "Legion never merges a pull
request: publish READY (merger role) and let a human merge under the repository's code-owner
rule". The pod's `legion` was `(devel) commit f3e990f7`, whose `packages/daemon/cmd/legion/gh.go:186-210`
holds both refusals; the branch under work (past LEGION-631) has no `cmd/legion/gh.go`. The record
went on the body's `Chain` line and in `dispatch://LEGION-663/message/f7d2a8e1-…`; the merger,
tester and reviewer each read it from there.
