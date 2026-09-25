import { expect, test } from "bun:test";
import {
  CommentEventPayloadSchema,
  DispatchTargetedCommentPayloadSchema,
  MessageDeliveryEventPayloadSchema,
} from "./dispatch-api";

// LEGION-271. A delivery attempt now records whether the stream already held the message, so a
// retry after a receipt timeout reads as "already delivered" rather than as a fresh send. These
// are stripping z.objects: a field they do not declare is silently dropped from every
// agent-bound frame, so declaring `duplicate` is what makes it reach an agent at all.
test("a message.delivery payload keeps its duplicate verdict through the schema", () => {
  const parsed = MessageDeliveryEventPayloadSchema.parse({
    message_id: "message-1",
    attempt: 2,
    delivery: "steer",
    session_id: "s1",
    target: "session:s1",
    title: "planner",
    state: "sent",
    duplicate: true,
  });
  expect(parsed.duplicate).toBe(true);
});

test("a message.delivery payload without the field reads as not a duplicate", () => {
  const parsed = MessageDeliveryEventPayloadSchema.parse({
    message_id: "message-1",
    attempt: 1,
    state: "sent",
  });
  expect(parsed.duplicate ?? false).toBe(false);
});

test("a comment delivery keeps its duplicate verdict through both payload schemas", () => {
  const delivery = {
    comment_id: "11111111-2222-4333-8444-555555555555",
    target: "session:s1",
    attempt: 2,
    delivery: "steer",
    session_id: "s1",
    envelope_id: null,
    duplicate: true,
    state: "sent",
    error: null,
    resolve_error: null,
    reply_id: null,
    created_at: "2026-09-25T00:00:00Z",
  };

  const comment = CommentEventPayloadSchema.parse({
    id: "11111111-2222-4333-8444-555555555555",
    body: "The mention landed twice before this.",
    deliveries: [delivery],
  });
  expect(comment.deliveries?.[0]?.duplicate).toBe(true);

  const targeted = DispatchTargetedCommentPayloadSchema.parse({
    id: "11111111-2222-4333-8444-555555555555",
    issue_key: "LEGION-271",
    artifact_id: null,
    artifact_name: "spec.md",
    author: { kind: "user", id: "alice" },
    body: "The mention landed twice before this.",
    reply_to: null,
    ask_id: null,
    created_at: "2026-09-25T00:00:00Z",
    mentions: [{ target: "session:s1", delivery: "steer", session_id: "s1" }],
    deliveries: [delivery],
  });
  expect(targeted.deliveries[0]?.duplicate).toBe(true);
});
