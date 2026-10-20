---
title: "A feed built by joining entity lists makes unpaged a correctness precondition, and buckets it by negation go wrong silently"
category: dispatch-web
tags:
  - query-keys
  - pagination
  - discriminated-union
  - conversation
  - react-query
date: 2026-10-20
status: active
module: packages/dispatch/web/src/features/conversation
related_issues:
  - "INFRA-44"
  - "sjawhar/legion#2083"
---

# A feed built by joining entity lists makes unpaged a correctness precondition, and buckets it by negation go wrong silently

When a list stops rendering a stream of self-describing rows and starts deriving cards by joining
entity lists, three things change character. Write each down where the next person will meet it.

- **Unpaged becomes load-bearing for correctness, not just completeness.** A join miss used to be
  a visibly odd row; now it is an omission nobody can see. A reply whose root is not in the list
  is dropped, and a deep link that resolves to a card the list does not hold silently lands on
  nothing. That is safe only while every read returns the issue's whole set, so a later `?limit=`
  on any of those routes breaks the feed in a way no type and no test catches. Say so on the
  routes.
- **Derive a lifecycle bucket by exhaustive mapping over the state union, never by negating one
  member.** `state !== "open"` compiles unchanged when a fourth state appears and silently
  re-buckets every card carrying it — in a surface whose whole premise is that a reader finds a
  conversation by its bucket. A `switch` with a `never` default makes the next lifecycle change a
  typecheck failure instead.
- **The card's sort key is a hand-maintained maximum over named timestamp fields; treat that list
  as part of the lifecycle's contract.** A moment the maximum does not read leaves a conversation
  sitting where it was. Two kinds in one list must answer the same question the same way, or the
  same action lifts one kind and not the other.

**Two surfaces share a React Query key only when they need the same payload; the same endpoint is
not enough.** When one needs more, give it a key *under the same prefix* so existing prefix
invalidation still reaches it and no new invalidation is added anywhere — and verify that by
reading the producers, not by assuming.

## Evidence

INFRA-44 rebuilt the Dispatch issue Conversation from one paged event log into three unpaged
per-issue reads. `features/conversation/conversation-model.ts` drops a comment whose root chain is
broken or whose root hangs off an ask, and `focusConversation` can name a card the list does not
hold; both are unreachable only because the three reads are the issue's whole set.
`askConversation` buckets with `closed: ask.state !== "open"` against a three-member union, while
`isClosedComment` twenty lines above enumerates its cases and the server's `parseAskListState`
refuses an unknown value — one concept, two conventions, one file. Its activity maximum reads an
ask's `answer.at` and `resolution.at` but a comment thread's replies only, so resolving an ask
lifts its card to the top of Closed and resolving a comment thread does not.

The query-key rule is the round trip: a simplify pass pointed the Conversation's two reads at the
keys the review margin already held, which removed a duplicate request per issue page; review then
found that this made every reader of `["asks", key]` — the margin on every issue and document
page, and the Spec tab's decision blocks — download every ask's replies and followers to draw none
of them. It was split back apart behind `?thread=1` under `["asks", key, "thread"]`, verified
against every producer of that prefix, and no invalidation was added.
