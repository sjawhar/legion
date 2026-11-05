---
title: "A census says what its search did not cover, and disposes of the hits in other repositories"
category: legion
tags:
  - contract-change-census
  - pr-body
  - cross-repository
  - refusal
date: 2026-11-04
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2729"
---
# A census says what its search did not cover, and disposes of the hits in other repositories

Extends docs/solutions/legion/a-contract-census-names-refusals-too-and-lists-every-hit-tests-and-the-prs-own-files-included-and-a-finite-field-is-an-enum-INFRA-229.md.

That document says each change gets its own search and every hit a disposition. This one is about
the edge of the search.

- A `grep` of this tree is a search of this tree. Never write "no caller" from it. State the
  search's boundary in the census, then search the project's other repositories — the list is in
  the `Legion addressing` line of a worker's system prompt — with
  `gh api search/code -f q='<term> repo:<owner>/<name>'`, and give every hit a disposition too.
- A hit in a repository you do not own is still your finding. Dispose of it by naming the issue
  filed against that repository, blocked by yours, rather than by changing a file its owner
  governs or leaving it unmentioned. Say which hits become false at your deploy (a sentence in
  deployment instructions, a test fake of the route's answer that keeps passing while it stops
  describing the server) and which are dated history that stays.
- A code built from parts greps to zero hits. Say that — name the function that builds it and the
  siblings built the same way — rather than recording "no caller".
- An additive response field is safe only if neither side refuses an unknown member. Name the
  mechanism on each side: the schemas that declare no strict mode, and every strict decoder with
  what it actually reads.
- A count you state in prose is a claim the census must derive. Derive it from the pinned test, or
  do not state it: the same number lives in the body, the changelog fragment and the guides, and
  only the test moves when the code does.

## Evidence

INFRA-1017's first census said `HUMAN_ONLY` was "found in no shipped code, this repository's or
any caller's" from a tree-only search. Review round 2 named three hits outside it, all in the
project's other repository: the deployment instructions' sentence that the reads reserved for a
person answer an agent token `403 HUMAN_ONLY`, which the deploy makes false; a test fake of the
Inbox route's answer in that repository's production-check tests, which fakes a server and so
stays green while it stops describing one; and a dated rehearsal record under its
`docs/solutions/`. The first two are disposed of by dispatch://INFRA-1170, filed against that
repository and blocked by INFRA-1017; the third is history.

The same census records `TOKEN_ID_INPUT` at zero hits because `requireUUIDPath` builds it as
`strings.ToUpper(kind) + "_ID_INPUT"`, as `ASK_ID_INPUT` and `HANDOFF_ID_INPUT` are built, and
proves three additive response fields safe by `0` hits for `strict()` in
`packages/contracts/src/dispatch-api.ts` and by reading each of the eleven Go
`DisallowUnknownFields` sites.

The count rule is this pull request's own miss. Its changelog fragment
(`packages/envoy/.changes/unreleased/pr2729-Changed-20261103-221100.yaml`) says "the 41 routes only
a browser could call" while `browserOnly` in
`packages/envoy/internal/dispatch/api/routes_table_test.go` holds 45 names and the body says 44;
41 was the figure before a forward merge carried three later `authHuman` rows onto the rule. The
tester reported it twice and it was still in the fragment at the reviewed head.
