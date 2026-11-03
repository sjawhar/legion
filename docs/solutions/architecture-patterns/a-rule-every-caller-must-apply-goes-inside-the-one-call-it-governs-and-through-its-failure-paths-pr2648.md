---
title: "A rule every caller must apply goes inside the one call it governs, and through that call's failure paths"
category: architecture-patterns
tags:
  - review-rounds
  - choke-point
  - credentials
  - refactoring
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2648"
---

# A rule every caller must apply goes inside the one call it governs, and through that call's failure paths

Extends docs/solutions/legion/a-fix-that-produces-the-next-finding-is-a-ratchet-stop-it-with-a-rule-written-early-INFRA-66.md.
That note says to write the stopping rule before it is needed; this one says what the stopping
rule is when the findings are "here is another site that forgot to do X first".

- **A review finding of the form "every site that does X must first do Y" is a finding about the
  shape, not about the sites.** Fixing the sites the reviewer named leaves the next site for the
  next round, and the reviewer cannot enumerate what you have not shown them.
- **Move Y inside the one function that does X, so no caller can hand in the unmapped value.**
  Prefer the signature that makes the mistake unspellable: take the raw thing and map it, rather
  than take the already-mapped thing. A caller that loses code in the move is the sign you chose
  right.
- **Where one site genuinely needs no Y, the reason belongs in a comment at that site**, not in
  the pull request's description. The comment survives a merge and a reader of the function; the
  description does not.
- **Carry the mapped value through the function's failure paths too.** A log field, a cache key,
  a `forget`/invalidate key or a retry key that still names the unmapped value turns a one-line
  fix into a second defect, and the happy-path test will not see it.
- **A test proves the shape only if the fake refuses the wrong value**
  (docs/solutions/testing/a-token-fake-that-answers-every-owner-makes-an-owner-assertion-vacuous-pr2648.md).

## Evidence

sjawhar/legion#2648 gave the Legion daemon a configured repository-rename equivalence, so a tree
recorded under a repository's old name keeps working after a GitHub transfer. Every App token the
daemon mints for such a tree has to be minted on the installation that holds the repository now.
Four of the five review rounds found the next site of that one class, each after a fix that
stopped at the sites the previous round had named:

| round | what the previous fix did not reach |
| --- | --- |
| 1 | eight sites at once: `outbox.graphQL`, the production check's compare, `createBranch`, `implementClient`, the API's completion read, and four provisioning mints |
| 2 | `readyNote` and the commit identity — "the PR body's 'every GitHub call' is not true yet" |
| 3 | `defaultBranches.fetch`, behind the boot backfill — "this is the third site of one class" |
| 5 | `reviewAppClient`, behind the review paths |

The sawtooth stopped where the shape changed rather than where a site was patched. Round 3 made
`workflowRuntime.identity` take the record's `ghrepo.Repository` instead of an owner: both callers
lost code, and a third caller can no longer mint on an owner of its own choosing. Round 4 moved
the mapping into `defaultBranches.fetch`, the one mint behind the boot backfill, `nameAccepted`,
`issueRepository` and `prepareUnrecorded` — four callers covered by one line, where mapping at the
two call sites the finding named would have threaded a dependency through three functions and left
the other two reads unmapped.

The failure-path half cost its own near miss. Round 5's finding asked for one line,
`repository = w.repositories.serving(repository)`, at the top of `reviewAppClient`. That alone
would have left `tokenRefused` calling `forget.Forget(appauth.Review, repository.Owner())` on the
*recorded* owner: a 401 on the served owner's lease would never have dropped that lease, and the
refusal would have looped. `reviewAppClient` returns the serving repository as a third result, on
the error path as well, and both callers take it back, so every log line, error and forget below
the mint names what actually answered.

The exemptions are in the code for the same reason. `internal/daemon/kubernetes.go`'s
`implementTokens.Token` and the tmux provisioning mint say at the site that the token follows the
rename while the address stays the recorded name, because that address is a git remote GitHub
redirects and, under the Kubernetes runtime, also keys the pod's feed, its clone and the
repository cache's volume and Service.
