---
title: "Resolve a named child against the item the request named, never against a context the server climbed to"
category: architecture-patterns
tags:
  - request-semantics
  - identifier-resolution
  - threads
  - test-design
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2546"
---

# Resolve a named child against the item the request named, never against a context the server climbed to

A handler that already holds a wider context — a thread head it climbed to, an owner it locked, a
parent it resolved on the way in — will reach for that context when a second identifier arrives.
It is the wrong one.

- When a request names both a container and a child id, resolve the child against **the container
  the request named**, and take the container from the request field, not from a value the
  handler derived. Pass the kind and the container id as arguments, so the function cannot see
  anything else.
- Two request shapes that name two different containers get two call sites with their own
  arguments, not one call site that branches on the context. The branch is where the derived
  value gets in.
- A second entry point on the same behaviour (a callback, a webhook, a tool route) repeats the
  rule for itself: it holds a different context, and it is the one that is usually left behind.
- **Write the test from the request's meaning, not from what the code does.** A test authored
  beside the implementation asserts the implementation, including a refusal message that names
  the wrong container; it then passes forever and makes the bug look decided. When you write the
  case, say in one sentence what the caller meant, and assert that — a reviewer who has to ask
  for the test itself to change has already paid for the round.

## Evidence

In sjawhar/legion#2546 the comment route accepted `reply_to_block` beside either `reply_to` (a
comment) or `ask_id` (an ask). `replyBlockInThread` resolved it against whatever `threadHeadOf`
had climbed to, so a reply naming a block of a comment that sits inside an ask's thread stored a
node of the ask instead, and refused an unknown id with
`reply_to_block b9 is not a block of ask <id>`.

`dd2f17197c3f` replaced it with `namedReplyBlock(ctx, tx, kind, itemID, blockID)`
(`packages/envoy/internal/dispatch/api/comment_create.go:296`), called with
`refs.ItemComment, parent.ID` at `:263` and `refs.ItemAsk, *input.AskID` at `:282`; the function
no longer receives the thread target at all. The second entry point,
`packages/envoy/internal/dispatch/api/comment_delivery.go:790`, now resolves against its own
locked comment (`refs.ItemComment, comment.ID`) rather than the thread it hangs under.

The round-1 test ratified the bug: `TestAReplyToAnAskNamesThePartItAnswers`
(`api/reply_blocks_test.go`) asserted exactly the refusal above, and its sibling pinned a callback
reply's node as `ask/<id>#b2`. The reviewer had to ask for the test to change, not just the
handler; the same commit renamed it `TestAReplyNamesABlockOfTheItemTheRequestNamed`, flipped both
refusal expectations to `… is not a block of comment <id>`, and added the positive case for a
reply to a comment inside such a thread.
