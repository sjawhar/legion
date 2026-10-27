---
title: "A stored actor column is decoded by its known shape, and a value that will not decode fails the read"
category: envoy
tags:
  - dispatch
  - jsonb
  - decoding
  - reference-graph
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "INFRA-367"
  - "sjawhar/legion#2389"
---

# A stored actor column is decoded by its known shape, and a value that will not decode fails the read

- Decode a jsonb column whose writers all encode one type into exactly that type. A column of
  one actor (`issues.created_by`, `events.actor`, `asks.author`, `comments.author`,
  `messages.author`, an `ask.edited` payload's `edited_by`) decodes into `model.Actor`. A list
  (`artifact_versions.authors`) decodes into `[]model.Actor`. Never guess the shape from the
  value's first byte.
- Only absence reads as "nobody recorded": SQL NULL (a left join that found no row, a payload
  key that is not there) and JSON `null` (`model.Issue.CreatedBy` is `*Actor`, so an issue row can
  hold it). Any other value that does not decode fails the read with the error, as
  `refs/refs.go` already does for `created_by` and `authors`. Never drop a malformed element
  without saying so.
- A plan rule such as "a fact Dispatch never recorded is `[]`" covers a fact that is absent. It
  does not cover a row that is there but corrupt.
- Before you make a read strict, list every writer of the column and confirm each encodes that
  type, so real data meets no new 500. Lock the rule with a test that writes an undecodable value
  in each column shape and expects the read to fail, and writes JSON `null` and expects `[]`.

## Evidence

sjawhar/legion#2389's first `storedActors` (`refs/graph.go`, at 9386d556) guessed one-or-list
from the first byte, turned a decode failure into `[]` and dropped elements without `kind` or
`id`. The reviewer blocked it as a second decoding convention in the package that hid corrupt
data (thread PRRT_kwDORFy7ds6wpCni). 33cc1fec replaced it with `storedActor` and `storedActors`
and `TestAnActorThatDoesNotDecodeFailsTheRead` (`api/node_facts_test.go`). On the local harness a
comment whose `author` was set to `[1]` made `GET /api/v1/references?to=<comment>` answer 500, and
the same read answered 200 again once the author was restored. The round-2 review checked every
writer of these columns (`issue_create.go`, `artifacts.go`, `docs/mutation.go`,
`docs/ask_blocks.go`, `asks.go`, `reviews.go`, `comment_create.go`, `comment_delivery.go`,
`messages.go`, `message_delivery.go`, `broadcasts.go`, `events/broker.go`). Each one encodes a
`model.Actor` or a `[]model.Actor`.
