import {
  AGENT_TOPIC_PREFIX,
  DISPATCH_TOPIC_PREFIX,
  GHOSTWISPR_TOPIC_PREFIX,
  LEGION_NOTICE_TOPIC_PREFIX,
  ROLE_TOPIC_PREFIX,
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

/**
 * Whether nothing would ever be delivered to a session on `topic`, so the listener refuses the
 * whole registration naming it by name (`bus.ReachesNoSession` in packages/envoy/internal/bus,
 * applied by POST /v1/interests/subscribe): a `notifications.` topic no stream subject captures
 * and that is no role lane. A session takes its messages from its own consumer, which holds what
 * the stream stored, so such an interest would be stored and deliver nothing. A role lane still
 * travels over core NATS and is registered by its holder (POST /v1/roles/set).
 */
export function streamDeliversNothing(topic: string): boolean {
  return (
    topic.startsWith("notifications.") &&
    !topic.startsWith(ROLE_TOPIC_PREFIX) &&
    !streamStores(topic)
  );
}

/**
 * The stream subjects whose topics carry one agent session's private payload: each session's
 * inbox, the Legion daemon's notices for a project's controller, a failed delivery's report on a
 * session's subject (a session's own lane included), and a failed delivery on a team topic, which
 * carries the undelivered payload and the session it was for as the agent lane does. This is the
 * listener's own list, `privateStreamSubjects` in packages/envoy/internal/bus/stream.go.
 */
const PRIVATE_STREAM_SUBJECTS = [
  `${AGENT_TOPIC_PREFIX}>`,
  `${LEGION_NOTICE_TOPIC_PREFIX}>`,
  `${EXCEPTION_TOPIC_PREFIX}${AGENT_TOPIC_PREFIX}>`,
  `${EXCEPTION_TOPIC_PREFIX}notifications.team.>`,
];

/** What a controller notice lane ends with; every other subject of that family names an issue. */
const CONTROLLER_NOTICE_SUFFIX = ".controller";

/**
 * The Legion role token whose holder `topic`'s notices are for, or undefined when `topic` is no
 * controller notice lane: project `<p>`'s lane is `notifications.legion.<p>.controller` and its
 * role token is `legion-<p>-controller`, which is `controllerToken(p)` for every project token
 * the daemon mints. Both are read here from the one project token the lane names, as the
 * listener's `bus.ControllerNoticeRole` reads them, so only a literal lane names a role: a
 * wildcard token, an empty project, a longer subject, and the issue notices of the same family
 * (`notifications.legion.<p>.<issue key>`) name none.
 */
export function controllerNoticeRole(topic: string): string | undefined {
  if (!topic.startsWith(LEGION_NOTICE_TOPIC_PREFIX) || !topic.endsWith(CONTROLLER_NOTICE_SUFFIX)) {
    return undefined;
  }
  const project = topic.slice(
    LEGION_NOTICE_TOPIC_PREFIX.length,
    topic.length - CONTROLLER_NOTICE_SUFFIX.length
  );
  if (project === "" || /[.*>]/.test(project)) return undefined;
  return `legion-${project}-controller`;
}

/** Whether some subject matches both patterns, under NATS wildcard rules. */
export function subjectsOverlap(a: string, b: string): boolean {
  const aTokens = a.split(".");
  const bTokens = b.split(".");
  for (let index = 0; index < aTokens.length && index < bTokens.length; index += 1) {
    const aToken = aTokens[index];
    const bToken = bTokens[index];
    if (aToken === ">" || bToken === ">") return true;
    if (aToken !== "*" && bToken !== "*" && aToken !== bToken) return false;
  }
  return aTokens.length === bTokens.length;
}

/**
 * Whether a topic reaches a private stream subject that is not this session's: it overlaps one
 * under NATS wildcard rules, so another session's agent subject, a failed-delivery lane
 * (`notifications.envoy.exceptions.notifications.agent.<id>`, the session's own included, and the
 * team lane beside it), another project's controller lane, and every wildcard reaching any of
 * them (`notifications.agent.*`, `notifications.>`, `notifications.*.<x>`,
 * `notifications.envoy.>`) count. Two literal subjects are this session's own: `ownAgentSubject`,
 * and the controller notice lane of a project whose controller role `roles` names
 * (`controllerNoticeRole`), which its holder registers because a session receives what its own
 * consumer delivers and nothing else. This mirrors the listener's rule,
 * `ReachesPrivateStreamSubject` in packages/envoy/internal/bus/stream.go: the listener stores no
 * such interest, so a host registers none.
 */
export function reachesPrivateStreamSubject(
  ownAgentSubject: string,
  topic: string,
  roles: readonly string[] = []
): boolean {
  if (!PRIVATE_STREAM_SUBJECTS.some((subject) => subjectsOverlap(topic, subject))) return false;
  const role = controllerNoticeRole(topic);
  if (role !== undefined) return !roles.includes(role);
  // A session id holding `*` or `>` makes a wildcard of its "own" agent subject, no session's alone.
  return topic !== ownAgentSubject || /[*>]/.test(ownAgentSubject);
}

/**
 * The topics a registration carries, for every host that builds one: the session's own agent
 * subject, which its consumer is always filtered on, and every topic it follows that the
 * listener will store for it. Two are left out, and a host that followed one still follows it:
 * it just never goes to the listener.
 *
 * A topic reaching a private stream subject that is not this session's
 * (`reachesPrivateStreamSubject`) is left out because the listener leaves it out of the stored
 * row itself, answering a warning.
 *
 * A topic nothing would ever be delivered on (`streamDeliversNothing`) is left out because the
 * listener refuses the *whole* registration naming one, `400`, so a single such topic would
 * leave the session with no consumer at all, at every heartbeat, for as long as anything still
 * names it. A host cannot refuse it where a topic is first followed and be done: a topic also
 * comes back from the listener's own stored row and from the transcript the host restores
 * (INFRA-771 review), and a row written by an earlier build holds `notifications.team`, the bare
 * base that build expanded `notifications.team.>` to, which this build's stream captures
 * nothing of. Filtering here is the one place every registration passes through.
 */
export function registrationTopics(
  ownAgentSubject: string,
  followed: Iterable<string>,
  roles: readonly string[] = []
): string[] {
  return [
    ...new Set([
      ownAgentSubject,
      ...[...followed].filter(
        (topic) =>
          !reachesPrivateStreamSubject(ownAgentSubject, topic, roles) &&
          !streamDeliversNothing(topic)
      ),
    ]),
  ];
}

/**
 * The subjects a restore follows again, which is what the ledger persists: the session's own
 * agent subject, then every topic it follows but a role's and one reaching a private stream
 * subject. A role's topic — a role lane, or a subject the host holds only while it holds a role
 * (`heldForRole`) — is the role claim's to open, so a process that restores this state without
 * the role follows none of it; another session's agent subject (a fork's parent, until the host
 * closes it), a failed-delivery lane, a controller notice lane, or a wildcard reaching one of
 * them, is not this session's to follow, and the listener would refuse it
 * (`reachesPrivateStreamSubject`).
 */
export function replayFollowedSubjects(
  ownAgentSubject: string,
  topics: Iterable<string>,
  heldForRole: (topic: string) => boolean = () => false
): string[] {
  const followed = [...topics].filter(
    (topic) =>
      !topic.startsWith(ROLE_TOPIC_PREFIX) &&
      !reachesPrivateStreamSubject(ownAgentSubject, topic) &&
      !heldForRole(topic)
  );
  return [...new Set([ownAgentSubject, ...followed])];
}

/**
 * The topics a persisted state says the session followed that a restore follows again, so a
 * session back after the listener reaped its interests still follows them and takes what its
 * consumer kept for them: every subject of the state but agent subjects (the host opens its own,
 * and another's is not its to follow) and role lanes (held through a role's claim). An unfollowed
 * topic is absent from the newest state, so it stays unfollowed.
 */
export function rememberedTopics(topics: readonly string[] | undefined): string[] {
  return (topics ?? []).filter(
    (topic) => !topic.startsWith(AGENT_TOPIC_PREFIX) && !topic.startsWith(ROLE_TOPIC_PREFIX)
  );
}
