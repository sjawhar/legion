---
title: "A worker pane answers a review thread over REST replies, since the gh shim refuses every GraphQL mutation"
category: legion
tags:
  - review-threads
  - gh-shim
  - graphql
  - rest
  - implementer
date: 2026-10-10
status: active
module: packages/daemon/internal/runtime/workerbin
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
---

# A worker pane answers a review thread over REST replies, since the gh shim refuses every GraphQL mutation

- In a pre-#1843 pane, `legion gh -- api graphql` with any `mutation` is refused by the merge
  guard (`Legion never merges a pull request: publish READY …`), `addPullRequestReviewThreadReply`
  included. Reply on a thread through REST instead:
  `legion gh -- api repos/{owner}/{repo}/pulls/{n}/comments/{id}/replies -f body=@-` (or `-f body=…`),
  where `{id}` is the thread's first comment's `databaseId`, read with a GraphQL *query*
  (`reviewThreads { nodes { id comments(first:1) { nodes { databaseId } } } }`), which the shim
  allows. The footer goes in the body as on every Legion comment.
- `legion threads resolve` then reads the thread as it reads any: an implementer's `Fixed in …`
  reply leaves it `left open … is not an acceptance` until the opener's `Accepted:` is newest.

## Evidence

LEGION-668's first review round: four `addPullRequestReviewThreadReply` mutations refused in one
command; the REST replies landed at
https://github.com/sjawhar/legion/pull/1878#discussion_r4236809837 and the three after it, and
`legion threads resolve` resolved all four once the reviewer's `Accepted:` was newest.
