import {
  DispatchEventSchema,
  dispatchDocumentSubject,
  dispatchIssueSubject,
  SubscriptionRemovedEventPayloadSchema,
} from "@legion/contracts";

/**
 * The follow notice a host shows once per ask after a native tool result whose
 * `details.follows.ask` says the calling session now follows that ask (it opened it,
 * replied to it, or asked to follow it). No tool result subscribes the session to the
 * owner; the notice names the `envoy_subscribe` line for whoever wants every event.
 * Returns null for any other result: reads, unfollows, errors, and Envoy tools.
 */
export function dispatchFollowNotice(
  details: unknown
): { readonly ask: string; readonly text: string } | null {
  if (typeof details !== "object" || details === null) return null;
  const { follows, issue, document } = details as {
    follows?: unknown;
    issue?: unknown;
    document?: unknown;
  };
  if (typeof follows !== "object" || follows === null) return null;
  const { ask } = follows as { ask?: unknown };
  if (typeof ask !== "string" || ask === "") return null;
  let owner: { readonly label: string; readonly topic: string };
  if (typeof issue === "string" && issue !== "") {
    owner = { label: issue, topic: dispatchIssueSubject(issue, ">") };
  } else if (typeof document === "string") {
    const [project, slug] = document.split("/", 2);
    if (!project || !slug) return null;
    owner = { label: document, topic: dispatchDocumentSubject(project, slug, ">") };
  } else {
    return null;
  }
  return {
    ask,
    text: `Following ask ${ask} on ${owner.label}: its answer and replies reach you directly (dispatch_follow unfollow to stop). For every event on ${owner.label}: envoy_subscribe ${owner.topic}.`,
  };
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
