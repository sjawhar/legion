import { z } from "zod";
import { ActorSchema, InboundBySchema } from "./dispatch-api-core";

/**
 * The sessions Dispatch knows: the registry rows the Agents page lists, who is subscribed to an
 * owner's topics, and the payload of `subscription.remove_requested` and `subscription.removed`.
 */

export interface Agent {
  readonly session_id: string;
  readonly title: string;
  readonly dir: string;
  readonly machine_id: string;
  /**
   * The session's real machine, as its own Envoy plugin reported it (envoy-client's
   * machineID(): `ENVOY_MACHINE_ID`, falling back to the kernel hostname). `machine_id` is
   * always the listener that registered the session and owns its delivery, which may be a
   * single shared listener for every machine; this is the one to show and filter on. The
   * listener falls back to `machine_id` itself for a session whose plugin predates this field,
   * so it is never empty.
   */
  readonly host: string;
  readonly roles: string[];
  /**
   * Wire value from the Envoy registry: an open string list. The known values
   * are `DELIVERY_CAPABILITIES`; unknown strings persist and never match a mode.
   */
  readonly capabilities: string[];
  readonly last_seen: number;
  readonly open_asks: number;
  readonly last_activity: string | null;
}

// Subscriber is a session whose persisted Envoy interests include an issue's
// or unlinked document's topic family, enriched with its live registry state
// when the session is currently connected. Removable is false when the only
// matching topic is broader than this owner (e.g. "notifications.dispatch.>"):
// removing it would silently unsubscribe the session from every other issue
// and document too, so the server refuses to remove it; `via` then names one
// such topic.
export interface Subscriber {
  readonly session_id: string;
  readonly title: string;
  readonly live: boolean;
  readonly last_seen: number;
  readonly topics: readonly string[];
  readonly removable: boolean;
  readonly via?: string;
}

// SubscriptionRemovedEventPayload is the wire payload of subscription.removed:
// a human unsubscribed session_id from topics on this issue or document, and
// this record is both the audit trail and the direct notice routed to that
// session's own agent topic regardless of the issue's route.
export const subscriptionRemovedEventPayloadFields = {
  session_id: z.string(),
  by: ActorSchema,
  topics: z.array(z.string()).readonly(),
  /** True only while Dispatch is retrying the listener-side removal. */
  pending: z.boolean().optional(),
  /** The remove-requested event this completion settles. */
  request_event_id: z.number().int().positive().optional(),
};

export const SubscriptionRemovedEventPayloadSchema = z
  .strictObject(subscriptionRemovedEventPayloadFields)
  .readonly();

export type SubscriptionRemovedEventPayload = z.output<
  typeof SubscriptionRemovedEventPayloadSchema
>;

/** What a reader of the bus makes of a `subscription.*` payload: every field optional, and the
 *  remover read as a frame may carry it. */
export const InboundSubscriptionRemovedEventPayloadSchema = z.object({
  session_id: subscriptionRemovedEventPayloadFields.session_id.optional(),
  by: InboundBySchema.optional(),
  topics: subscriptionRemovedEventPayloadFields.topics.optional(),
  pending: subscriptionRemovedEventPayloadFields.pending,
  request_event_id: subscriptionRemovedEventPayloadFields.request_event_id,
});
