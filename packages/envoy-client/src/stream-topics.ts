import {
  AGENT_TOPIC_PREFIX,
  DISPATCH_TOPIC_PREFIX,
  GHOSTWISPR_TOPIC_PREFIX,
  LEGION_NOTICE_TOPIC_PREFIX,
} from "@legion/contracts";

/** The topic a failed delivery's report goes to is this prefix followed by the original topic. */
export const EXCEPTION_TOPIC_PREFIX = "notifications.envoy.exceptions.";

/**
 * The literal prefixes of the subjects the notification stream (`ENVOY_NOTIFICATIONS`) stores, as
 * the listener's binary compiles them (`streamSubjects` in packages/envoy/internal/bus/stream.go,
 * each a prefix followed by `>`). A session's consumer relays its agent subject and every followed
 * topic under one of them; a topic under none is one the listener's publish and subscribe routes
 * refuse by name, since nothing would store it.
 */
const STREAM_SUBJECT_PREFIXES = [
  AGENT_TOPIC_PREFIX,
  DISPATCH_TOPIC_PREFIX,
  "notifications.github.",
  "notifications.slack.",
  LEGION_NOTICE_TOPIC_PREFIX,
  GHOSTWISPR_TOPIC_PREFIX,
  "notifications.whatsapp.",
  "notifications.team.",
  `${EXCEPTION_TOPIC_PREFIX}${AGENT_TOPIC_PREFIX}`,
  `${EXCEPTION_TOPIC_PREFIX}notifications.team.`,
] as const;

/**
 * Whether the stream stores a publish to `topic`, read as the listener's `bus.StreamCaptures`
 * reads it: the topic extends one of the stream's prefixes by at least one token. A `*` or `>` in
 * the topic is read literally, so `notifications.dispatch.issue.X.>` is stored.
 */
export function streamStores(topic: string): boolean {
  return STREAM_SUBJECT_PREFIXES.some(
    (prefix) => topic.length > prefix.length && topic.startsWith(prefix)
  );
}
