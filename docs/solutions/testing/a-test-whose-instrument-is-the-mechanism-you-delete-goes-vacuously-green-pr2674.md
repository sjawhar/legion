---
title: "A test whose instrument is the mechanism you delete goes vacuously green: re-point it at a record the surviving code writes"
category: testing
tags:
  - deletion
  - test-observability
  - integration-tests
  - negative-control
  - vacuous-pass
date: 2026-11-03
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2674"
---

# A test whose instrument is the mechanism you delete goes vacuously green

## The rule

Before you delete a mechanism, list every test that observes behaviour **through** it — a mock
server's call count, a fake port, the body of a push.
Each of those assertions is about to be true at your head for the wrong reason.
Re-point each one at something the surviving code still writes, in the same commit.

A stand-in must take delivery on the path only the system under test can put the message on.
A stand-in subscribed to the subject the test itself published to observes the message bus, not
your code, and passes with the system under test deleted.

A test package that cannot import the binary under test can only re-implement its handler.
`cmd/<x>` is `package main`, so a test in `internal/integration` that claims to exercise it is a
copy of production logic living beside production.
Delete such a test; do not patch it.
Keep one only when its subject is something that package can really drive — a store, a reaper, a
registry.

A negative control that perturbs the stand-in proves the stand-in.
Perturb the production rule instead, and show the test red.

When you delete a test that could not fail, grep for the branch it claimed to cover.
You may find that branch has no test anywhere.

## Evidence

sjawhar/legion#2674 deleted the Envoy listener's HTTP push to a session's serve port.

`packages/envoy/internal/integration` observed delivery through a `mockSession` HTTP server and a
counter the harness incremented.
The first rewrite replaced the counter with a NATS stand-in — and the stand-in subscribed, on a
connection of its own, to the exact subjects the test published on, so NATS delivered each message
to it directly.
Its `startConsumer` helper was commented "processes messages using the same logic as the listener":
a copy of the handler written in the test, because `cmd/listener` is `package main` and nothing
there can import the real one.
The reviewer's trace: those tests "would pass with no listener at all", and the rewrite's own
negative control "only moved the stand-in's own subscription".
The second rewrite deleted the five tests and the stand-in; what is left in the package answers for
the interest registry, the session registry or the reaper, and each deleted test was recorded
against the `cmd/listener` test that covers its subject.

Where the subject survived, the instrument moved rather than the test:
`cmd/listener/fanout_refusal_reports_test.go` traded `pushes.Load()`, the HTTP mock's counter, for
`harness.config.dedupeCache.Seen(...)`, a record the surviving fan-out writes, and
`packages/envoy/scripts/e2e-local.sh` traded its `Bun.serve` `/prompt_async` fake session for a NATS
consumer stand-in — `wait_for_prompt_count` became `wait_for_consumer_frame_count`,
`session-prompts.jsonl` became `session-frames.jsonl`.

Deleting the tests that could not fail exposed that `followedTopicEcho` — the fan-out's rule that a
session is not handed back the message it published on a topic it follows — had no test anywhere in
the repository.
`TestTheFanoutSkipsAMessageOnAFollowedTopicAsAnEchoOfTheSessionThatPublishedIt` closes that, and its
negative control makes the echo arm fall through rather than moving the observer.
