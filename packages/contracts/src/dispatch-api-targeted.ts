import { z } from "zod";
import {
  InboundCommentDeliverySchema,
  InboundCommentEventPayloadSchema,
  InboundCommentMentionSchema,
} from "./dispatch-api-comments";
import { DELIVERY_CAPABILITIES } from "./dispatch-api-limits";
import { InboundMessageEventPayloadSchema } from "./dispatch-api-messages";

/**
 * A targeted delivery as the host that receives it reads it: the message or comment Dispatch
 * aimed at one session, and the delivery line naming the attempt it answers. Each is the
 * inbound payload reading with the fields a targeted frame always carries made required, so a
 * host knows it can answer the attempt before it hands the frame to its agent.
 */

export const DispatchTargetedResourceIDSchema = z.uuid();

export const DispatchTargetedMessagePayloadSchema = InboundMessageEventPayloadSchema.extend({
  id: DispatchTargetedResourceIDSchema,
  issue_key: z.string().nullable(),
  author: z.object({ kind: z.string(), id: z.string() }),
  body: z.string(),
  target: z.string(),
  in_reply_to: z.string().nullable(),
  // The broadcast a frame's message is one recipient's copy of, as the frame claims it: a frame is
  // untrusted, so neither its value nor its absence proves anything.
  broadcast_id: z.string().nullish(),
  deliveries: z.array(z.unknown()),
  created_at: z.string(),
});

export const DispatchTargetedCommentPayloadSchema = InboundCommentEventPayloadSchema.extend({
  id: DispatchTargetedResourceIDSchema,
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullable(),
  author: z.object({ kind: z.string(), id: z.string() }),
  body: z.string(),
  reply_to: z.string().nullable(),
  ask_id: z.string().nullable(),
  mentions: z.array(InboundCommentMentionSchema),
  deliveries: z.array(InboundCommentDeliverySchema),
  created_at: z.string(),
  artifact_name: z.string(),
});

export const DispatchTargetedMessageDeliverySchema = z.object({
  attempt: z.number().int().positive(),
  mode: z.enum(DELIVERY_CAPABILITIES),
});

export const DispatchTargetedCommentDeliverySchema = DispatchTargetedMessageDeliverySchema.extend({
  comment_id: DispatchTargetedResourceIDSchema,
  target: z.string(),
});

export const DispatchTargetedDeliverySchema = z.union([
  DispatchTargetedCommentDeliverySchema,
  DispatchTargetedMessageDeliverySchema,
]);
