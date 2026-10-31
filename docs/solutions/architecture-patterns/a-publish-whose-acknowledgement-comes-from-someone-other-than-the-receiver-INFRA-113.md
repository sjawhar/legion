---
title: "A publish whose acknowledgement comes from someone other than the receiver can fail after it was delivered"
category: architecture-patterns
tags:
  - nats
  - jetstream
  - delivery
  - debugging
  - proofs
date: 2026-10-22
status: active
module: envoy
related_issues:
  - "INFRA-113"
  - "sjawhar/legion#2105"
---

# A publish whose acknowledgement comes from someone other than the receiver can fail after it was delivered

**This page records the design before dispatch://INFRA-771, not how Envoy works now.** The
listener no longer publishes an ad-hoc topic over core NATS: `POST /v1/messages/publish` stores a
`notifications.team.<name>` topic in `ENVOY_NOTIFICATIONS` and refuses every other uncaptured
`notifications.` topic by name, so no publish of its own waits on a core subscriber's reply. The
measurement rule below — name who can be handed the acknowledgement request, and assert the
transport rather than only the receipt — still holds for any such transport.

Extends docs/solutions/architecture-patterns/core-nats-receipt-backed-current-holder-delivery.md.

That page records one half of the decoupling: a core-NATS publish reports success with nobody
listening, so delivery needs a receipt of its own. This is the other half, and the two belong in
one table.

When a publish's success signal comes from a different party than the receiver, enumerate who can
be handed the acknowledgement request. A failure on such a publish is not evidence that nothing was
delivered, and a test asserting "the subscriber received it" can be green while the user-visible
call is a 500.

Assert the transport, not only the receipt: bound the publish's elapsed time, and assert the
message arrived carrying no reply subject. Those two together say "this went over the transport I
think it did"; receipt alone does not.

## Evidence

JetStream answers a publish to a subject no stream captures with `no responders` — unless a core
NATS subscriber holds that subject. Then the server hands the acknowledgement request to that
subscriber, which receives the message and replies to nothing, and the publisher waits out its
whole five-second deadline before answering `500 context deadline exceeded`
(`packages/envoy/internal/bus/nats.go`, the `usesCoreTransport` comment).

Three states look alike from the publisher and only the third hangs:

| Who holds the subject | What the publisher sees | What the subscribers got |
| --- | --- | --- |
| nobody | `no responders`, at once | nothing |
| a stream | the stream's acknowledgement | the stream retains it |
| a core subscriber, no stream | the deadline, then a 500 | the message, already |

That third row inverted the whole investigation. The symptom was "the publish fails", so the search
went to the publish path; the message had in fact reached every core subscriber before the error.
A local probe on nats 2.10 recorded it directly: the publish failed after 5.003 s and the
subscriber received the message.

The regression lock built for it asserts the timing and the shape rather than the receipt
(`packages/envoy/internal/bus/adhoc_topic_test.go`,
`TestAnAdHocTopicIsPublishedOverCoreNATSAndReachesItsSubscriber`): it fails if the publish takes a
second or more, and separately requires `msg.Reply == ""`, because a JetStream publish would arrive
as a request. Either assertion alone passes under the defect.

## When to apply

Any transport where the success signal is a reply rather than a write: JetStream publishes,
request-reply over a bus, a webhook whose 2xx comes from a proxy, a queue whose ack is produced by
a consumer. Before reading a timeout as "it did not happen", name who could have answered.
