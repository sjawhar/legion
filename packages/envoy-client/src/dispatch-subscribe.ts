import { DispatchEventSchema, SubscriptionRemovedEventPayloadSchema } from "@legion/contracts";
import { commandLine } from "./dispatch-command";
import { documentTopicOf, issueTopic, type OwnerTopic } from "./dispatch-owner";

/**
 * The ask a result's `details.follows.ask` says the calling session now follows (it opened it,
 * replied to it, or asked to follow it), with its owner; null for any other result: reads,
 * unfollows and errors. The `dispatch` command prints `followNoticeText` once per such ask.
 */
export function followedAsk(
  details: unknown
): { readonly ask: string; readonly owner: OwnerTopic } | null {
  if (typeof details !== "object" || details === null) return null;
  const { follows, issue, document } = details as {
    follows?: unknown;
    issue?: unknown;
    document?: unknown;
  };
  if (typeof follows !== "object" || follows === null) return null;
  const { ask } = follows as { ask?: unknown };
  if (typeof ask !== "string" || ask === "") return null;
  if (typeof issue === "string" && issue !== "") return { ask, owner: issueTopic(issue) };
  if (typeof document !== "string") return null;
  const [project, slug] = document.split("/", 2);
  if (!project || !slug) return null;
  return { ask, owner: { label: document, topic: documentTopicOf(project, slug).topic } };
}

/**
 * The follow notice for `followed`. No result subscribes the session to the owner; the notice
 * names the `envoy_subscribe` line for whoever wants every event when `topics`, the session's
 * subscriptions, hold none that covers the owner (`ownerSubscribeLine`).
 */
export function followNoticeText(
  { ask, owner }: { readonly ask: string; readonly owner: OwnerTopic },
  topics: readonly string[] | null
): string {
  const subscribe = ownerSubscribeLine(owner, topics);
  return `Following ask ${ask} on ${owner.label}: its answer and replies reach you directly (${commandLine("dispatch_follow", { ask, action: "unfollow" })} to stop).${subscribe === null ? "" : ` ${subscribe}.`}`;
}

/**
 * `For every event on <owner>: envoy_subscribe <topic>`, the line that subscribes a session to
 * every event on `owner`, when `topics`, the session's subscriptions, hold none that covers it.
 * Null when one does, and when `topics` is null: nothing then says what the session receives.
 */
export function ownerSubscribeLine(
  owner: OwnerTopic,
  topics: readonly string[] | null | undefined
): string | null {
  if (topics == null || topics.some((topic) => subscriptionCovers(topic, owner))) return null;
  return `For every event on ${owner.label}: envoy_subscribe ${owner.topic}`;
}

/**
 * Whether a subscription to `topic` receives every event on `owner`, under the listener's
 * wildcard rule (`routing.Match`): its tokens match the owner's subject token for token (`*`
 * matching any one), then end in `>`. An event type can span tokens (`ask.answered`), so a
 * topic that names a token past the owner's subject, `*` included, receives only some of them.
 */
export function subscriptionCovers(topic: string, owner: OwnerTopic): boolean {
  const base = owner.topic.slice(0, -".>".length).split(".");
  for (const [index, part] of topic.split(".").entries()) {
    if (part === ">") return true;
    if (index >= base.length || (part !== "*" && part !== base[index])) return false;
  }
  return false;
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
