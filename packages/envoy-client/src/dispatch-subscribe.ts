import {
  DISPATCH_DOCUMENT_TOPIC_PREFIX,
  DISPATCH_ISSUE_TOPIC_PREFIX,
  DispatchEventSchema,
  SubscriptionRemovedEventPayloadSchema,
} from "@legion/contracts";

/**
 * Extract the Dispatch subscription topic from a native tool result.
 * Hosts call this after any tool result and subscribe only when Dispatch
 * explicitly returned one of its issue topics.
 */
export function dispatchSubscriptionTopic(details: unknown): string | null {
  if (typeof details !== "object" || details === null || !("topic" in details)) return null;
  const { topic } = details;
  return typeof topic === "string" && topic.startsWith("notifications.dispatch.") ? topic : null;
}

/**
 * Renders a Dispatch subscription topic as the short label a human reads:
 * the issue key, or "<project>/<slug>" for an unlinked document. Falls back
 * to the topic itself for a shape neither prefix matches.
 */
export function dispatchTopicLabel(topic: string): string {
  if (topic.startsWith(DISPATCH_ISSUE_TOPIC_PREFIX)) {
    const [key] = topic.slice(DISPATCH_ISSUE_TOPIC_PREFIX.length).split(".", 2);
    if (key !== undefined && key !== "") return key;
  }
  if (topic.startsWith(DISPATCH_DOCUMENT_TOPIC_PREFIX)) {
    const [project, slug] = topic.slice(DISPATCH_DOCUMENT_TOPIC_PREFIX.length).split(".", 3);
    if (project !== undefined && project !== "" && slug !== undefined && slug !== "") {
      return `${project}/${slug}`;
    }
  }
  return topic;
}

/**
 * Extracts the topics a subscription.removed notice says a human removed
 * from sessionID's own interests, when raw is a dispatch envelope carrying
 * that event and sessionID is the one it names. The event also reaches the
 * issue's own topic (every other subscriber, not just the removed session),
 * so this returns undefined for a removal naming a different session — a
 * host uses this to drop its own now-stale local NATS subscription for
 * those topics so the dead-connection recovery path (envoy.ts) does not
 * resurrect what a human just removed, and must not act on a removal that
 * was not its own. Returns undefined for anything else too: a parse
 * failure, a non-dispatch envelope, or a different event type.
 */
export function subscriptionRemovedTopics(
  raw: string,
  sessionID: string
): readonly string[] | undefined {
  let envelope: unknown;
  try {
    envelope = JSON.parse(raw);
  } catch {
    return undefined;
  }
  if (typeof envelope !== "object" || envelope === null) return undefined;
  const { source, payload } = envelope as { source?: unknown; payload?: unknown };
  if (source !== "dispatch" || typeof payload !== "string") return undefined;

  let event: unknown;
  try {
    event = JSON.parse(payload);
  } catch {
    return undefined;
  }
  const parsedEvent = DispatchEventSchema.safeParse(event);
  if (!parsedEvent.success || parsedEvent.data.type !== "subscription.removed") return undefined;
  const parsedPayload = SubscriptionRemovedEventPayloadSchema.safeParse(parsedEvent.data.payload);
  if (!parsedPayload.success || parsedPayload.data.session_id !== sessionID) return undefined;
  return parsedPayload.data.topics ?? [];
}
