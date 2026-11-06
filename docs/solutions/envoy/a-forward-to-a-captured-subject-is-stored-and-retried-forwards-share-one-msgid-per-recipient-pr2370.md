---
title: "A forward to a captured subject is stored too, and its retries share one MsgId per recipient"
category: envoy
tags:
  - nats
  - jetstream
  - request-reply
  - deduplication
  - listener
date: 2026-10-29
status: active
module: packages/envoy
---

# A forward to a captured subject is stored too, and its retries share one MsgId per recipient

When you send an envelope straight to a session's `notifications.agent.<id>` subject over core
NATS (a request with a receipt, a forward of another topic's message), remember the notification
stream captures that subject:

- Every attempt is stored. Give each attempt of one forward the same `Nats-Msg-Id`, keyed by the
  message and the recipient (`bus.ForwardMsgID(dedupeKey, subject)`), so the stream keeps one copy
  per message and recipient however many times you retry, and a second recipient still gets its
  own copy.
- Adding a header to a core request changes its replies: the server hands them back with a header
  block that is present and empty. A receipt test written as `Header == nil` then stops matching.
  Test `len(Data) == 0 && len(Header) == 0` in the same change that adds the header.
- Every listener's durable reads the stored copy later. Make it acknowledge a copy whose envelope
  `topic` is not its subject without delivering it, and make every receiver that derives routing,
  rendering or receipts from the frame read the envelope's `topic`, not the subject. The
  topic/subject mismatch is the one in-band mark of a forward.

## Evidence

- `packages/envoy/internal/bus/nats.go`: `ForwardMsgID`, which `RequestCoreTo` sets when its caller
  passes it (since #2395: the publish route on every forward, the role lane only for a key that
  names its event);
  the receipt test is `len(response.Data) == 0 && len(response.Header) == 0`.
- `packages/envoy/internal/bus/request_core_test.go`,
  `TestRequestCoreToStoresOneCopyPerMessageAndRecipientAcrossAttempts`: two forwards to one
  recipient leave one stored message, and the same envelope to a second recipient keeps its own.
  With the header line removed it fails with "the stream holds 2 copies … want 1".
- `packages/envoy/cmd/listener/delivery.go`, `listenerDeliveryHandler`: the durable acknowledges an
  envelope on an agent subject whose topic differs (`listener acknowledged a stored forward copy`).
- `packages/claude-envoy/src/envoy-channel-server.ts`: the channel meta's `topic` is
  `envelope?.topic || subject`; before, a forward showed under the session's own agent subject.
