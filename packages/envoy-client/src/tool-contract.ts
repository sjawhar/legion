import {
  DELIVERY_DUPLICATE_WINDOW_MS,
  EnvelopeSchema,
  type SchemaApi,
  type SchemaNode,
  SLACK_POST_TEXT_MAX,
  type ToolArgumentsShape,
  zodSchemaApi,
} from "@legion/contracts";
import { z } from "zod";
import { formatZodIssues, ToolInputError } from "./tool-input-errors";
import type { MessageMetadataInput, SessionInfo, SessionState } from "./transport";

const DELIVERY_CONTRACT =
  "Delivery is at-least-once, possibly out of order across topics; use id for dedupe and at for freshness.";

/**
 * How long the notification stream keeps a message a session has not read, which is how long a
 * message for a stopped session waits for it: the stream takes its own max age from this number
 * (`packages/envoy/internal/bus/stream.go`), so the four say one number.
 */
const STORED_MESSAGE_HOURS = DELIVERY_DUPLICATE_WINDOW_MS / (60 * 60 * 1000);

const SEND_STATES =
  "The answer says what the target was when Envoy stored the message: connected means it is " +
  "running, stopped means it is not and Envoy keeps the message " +
  `${STORED_MESSAGE_HOURS} hours, so it arrives if that session runs again in that time.`;

const PUBLISH_COUNTS =
  "The answer counts who gets it: recipients is every session other than you that the message " +
  `goes to, one that is stopped and gets it when it runs again within ${STORED_MESSAGE_HOURS} hours included, and ` +
  "recipients_connected is how many of those are running now. A listener that counts neither " +
  "sends neither, and a role topic names its holder instead.";

const STORED_DELIVERY =
  "Stored topics (agent, dispatch, github, slack, legion, ghostwispr, whatsapp; a role message " +
  "reaches its holder as a stored copy on the holder's agent topic) reach each subscribed session " +
  "once from the durable consumer its listener relays to it, also one that was away or restarted. " +
  "Team and other ad-hoc topics are live only.";

const TOPIC_GUIDE =
  "Topic guide (all are under notifications.): agent.<session_id> (subscribe: own inbox); role.<role> " +
  "(publish-to; holders claim via envoy_role_set). > matches one or more trailing tokens and does not " +
  "match the base subject. Envoy registers the concrete base when you subscribe to <subject>.>, so " +
  "the recommended default remains github.<owner>.<repo>.pr.<n>.>. <owner> and <repo> are one token " +
  "each, with every dot in the name written _ (sjawhar/.github is github.sjawhar._github; " +
  "acme/site.io is github.acme.site_io), and a topic spelled with the dot receives nothing. " +
  "Default PR subscription: " +
  "github.<owner>.<repo>.pr.<n>.> (it receives the quiet PR family): pr.<n> (lifecycle: " +
  "opened/synchronize/closed; closed carries merged, merge_commit_sha, merged_by, head_sha), " +
  "pr.<n>.comment, pr.<n>.review, pr.<n>.mention, pr.<n>.checks (one settlement event per commit whose checks settle, the head or not, naming its sha: " +
  'passed/failed/cancelled/skipped with failing names and URLs; re-fires with superseded_settlement: "true" ' +
  "when new runs appear for the same commit). Other GitHub: issue.<n>, issue.<n>.comment, " +
  "issue.<n>.mention, mention, push.branch.<name>, push.tag.<name>, workflow.<file>.<action> (only " +
  "runs without an associated PR); slack.<team>.<channel>.message|mention and " +
  "slack.<team>.<channel>.thread.<ts>.message|mention; ghostwispr.<session>.<kind>; " +
  "whatsapp.<phone>.<jid>.<kind>; envoy.exceptions.<original-topic>.";

export type { SchemaApi, ToolArgumentsShape } from "@legion/contracts";

const URGENCY_VALUES = EnvelopeSchema.shape.urgency.unwrap().options as unknown as readonly [
  "low",
  "med",
  "high",
  "blocking",
];
const EXPECTS_REPLY_VALUES = EnvelopeSchema.shape.expects_reply.unwrap()
  .options as unknown as readonly ["none", "optional", "required"];

/** Message metadata arguments shared by envoy_send and envoy_publish, built on the host's Zod. */
export function messageMetadataShape<Element extends SchemaNode<Element>>(
  schema: SchemaApi<Element>
): ToolArgumentsShape {
  return {
    in_reply_to: schema.string().optional(),
    supersedes: schema.string().optional(),
    urgency: schema
      .enum(URGENCY_VALUES)
      .describe(
        "low | med | high | blocking: how soon the recipient should act, shown on its card. It does not deliver the message sooner or ahead of others."
      )
      .optional(),
    expects_reply: schema
      .enum(EXPECTS_REPLY_VALUES)
      .describe("none | optional | required")
      .optional(),
    expires_at: schema.number({ int: true }).optional(),
  };
}

export const MessageMetadataSchema = z.object(
  messageMetadataShape(zodSchemaApi(z)) as z.ZodRawShape
);

export type MessageMetadataArguments = {
  readonly in_reply_to?: string;
  readonly supersedes?: string;
  readonly urgency?: (typeof URGENCY_VALUES)[number];
  readonly expects_reply?: (typeof EXPECTS_REPLY_VALUES)[number];
  readonly expires_at?: number;
};

/** Converts validated tool-wire metadata into the Envoy client's camel-case input. */
export function toMessageMetadata(args: MessageMetadataArguments): MessageMetadataInput {
  return {
    ...(args.in_reply_to === undefined ? {} : { inReplyTo: args.in_reply_to }),
    ...(args.supersedes === undefined ? {} : { supersedes: args.supersedes }),
    ...(args.urgency === undefined ? {} : { urgency: args.urgency }),
    ...(args.expects_reply === undefined ? {} : { expectsReply: args.expects_reply }),
    ...(args.expires_at === undefined ? {} : { expiresAt: args.expires_at }),
  };
}

function messageArguments<Element extends SchemaNode<Element>>(
  schema: SchemaApi<Element>
): ToolArgumentsShape {
  return { message: schema.string(), ...messageMetadataShape(schema) };
}

export const EnvoyToolOperation = {
  subscribe: "subscribe",
  unsubscribe: "unsubscribe",
  listInterests: "listInterests",
  inbox: "inbox",
  send: "send",
  publish: "publish",
  setRole: "setRole",
  getRole: "getRole",
  whoami: "whoami",
  listSessions: "listSessions",
  slackPost: "slackPost",
} as const;

export type EnvoyToolOperation = (typeof EnvoyToolOperation)[keyof typeof EnvoyToolOperation];

/**
 * What each operation's arguments parse to. The shapes above validate on the host's Zod and
 * come back untyped, so this is the typed view a host reads after `parse`; keep it in step
 * with the builders.
 */
export interface ToolArgumentsByOperation {
  readonly subscribe: { readonly topics: readonly string[] };
  readonly unsubscribe: { readonly topics?: readonly string[] };
  readonly listInterests: Record<string, never>;
  readonly inbox: Record<string, never>;
  readonly send: MessageMetadataArguments & {
    readonly session_id: string;
    readonly message: string;
    readonly idempotency_key?: string;
  };
  readonly publish: MessageMetadataArguments & { readonly topic: string; readonly message: string };
  readonly setRole: { readonly role: string };
  readonly getRole: { readonly role: string };
  readonly whoami: Record<string, never>;
  readonly listSessions: {
    readonly machine?: string;
    readonly dir?: string;
    readonly title?: string;
  };
  readonly slackPost: {
    readonly channel: string;
    readonly thread_ts: string;
    readonly text: string;
  };
}

export type ToolSpec = {
  readonly name: string;
  readonly description: string;
  readonly arguments: <Element extends SchemaNode<Element>>(
    schema: SchemaApi<Element>
  ) => ToolArgumentsShape;
  readonly operation: EnvoyToolOperation;
};

export const envoyToolSpecs = [
  {
    name: "envoy_subscribe",
    description: `Subscribe this session to Envoy notification topics. ${STORED_DELIVERY} ${TOPIC_GUIDE}`,
    arguments: (schema) => ({
      topics: schema.array(schema.string()).describe("NATS-style topic patterns to subscribe to."),
    }),
    operation: EnvoyToolOperation.subscribe,
  },
  {
    name: "envoy_unsubscribe",
    description:
      "Unsubscribe this session from Envoy topics, or remove all current subscriptions if topics are omitted. A subscription another plugin holds for this session's role stays, and the result lists it as kept.",
    arguments: (schema) => ({ topics: schema.array(schema.string()).optional() }),
    operation: EnvoyToolOperation.unsubscribe,
  },
  {
    name: "envoy_list",
    description:
      "List the current Envoy topic subscriptions for this session so you can confirm the exact topic shapes that are active.",
    arguments: () => ({}),
    operation: EnvoyToolOperation.listInterests,
  },
  {
    name: "envoy_inbox",
    description: "List this Pi session's 50 most recent rendered Envoy deliveries, newest first.",
    arguments: () => ({}),
    operation: EnvoyToolOperation.inbox,
  },
  {
    name: "envoy_send",
    description: `Send an Envoy agent-to-agent message directly to another session by session ID. ${DELIVERY_CONTRACT} ${SEND_STATES} A send that is not confirmed delivered errs naming its idempotency_key; send the same message again with that idempotency_key within ${STORED_MESSAGE_HOURS} hours, and Envoy drops it as a repeat if the first copy arrived.`,
    arguments: (schema) => ({
      session_id: schema
        .string()
        .describe("Target session ID; find it with envoy_sessions or envoy_whoami."),
      ...messageArguments(schema),
      idempotency_key: schema
        .string()
        .describe(
          "Only to send again a message whose envoy_send was not confirmed delivered: the idempotency_key that error named. Omit it for a new message."
        )
        .optional(),
    }),
    operation: EnvoyToolOperation.send,
  },
  {
    name: "envoy_publish",
    description: `Publish an Envoy message to a notifications. topic. ${STORED_DELIVERY} A role or built-in topic is delivered as its guide says; a role message errs when its holder did not confirm it within the listener's window. A team or ad-hoc topic (such as notifications.team.deploy-squad.all) reaches every session subscribed to it when you publish, and is not kept for a session that subscribes later. The listener confirms it with each other session that holds its own NATS connection and registered the topic, and errs naming each session that did not confirm; a session that stopped without unregistering stays registered for up to 5 minutes and is named in that time, and a named session is not a failed delivery to the other sessions. A session that holds its own NATS connection receives such a live topic directly, yours included; a session the listener delivers to over a serve port is not confirmed and receives it through its machine's listener when that listener subscribes to every topic (ENVOY_SUBSCRIBE_ALL_TOPICS=1; the shared hub's listener does not). ${PUBLISH_COUNTS} ${DELIVERY_CONTRACT}`,
    arguments: (schema) => ({ topic: schema.string(), ...messageArguments(schema) }),
    operation: EnvoyToolOperation.publish,
  },
  {
    name: "envoy_role_set",
    description:
      "Set the current session as the holder of a named role. Messages published to notifications.role.<role> route to this session. Moving to a different role is refused while another plugin holds subscriptions that last only as long as this session keeps its current role.",
    arguments: (schema) => ({ role: schema.string() }),
    operation: EnvoyToolOperation.setRole,
  },
  {
    name: "envoy_role_get",
    description: "Get the live holder of a named Envoy role.",
    arguments: (schema) => ({ role: schema.string() }),
    operation: EnvoyToolOperation.getRole,
  },
  {
    name: "envoy_whoami",
    description:
      "Returns this session's Envoy identity: session ID, machine ID, port, and directory. session_id is the address a reply reaches. Where a host runs a task subagent inside its parent's process, such a subagent registers no Envoy session of its own, so its session_id is the parent session that spawned it and the result says so.",
    arguments: () => ({}),
    operation: EnvoyToolOperation.whoami,
  },
  {
    name: "envoy_sessions",
    description:
      "List all live sessions registered with Envoy. Filter by optional machine, directory, or title.",
    arguments: (schema) => ({
      machine: schema.string().optional(),
      dir: schema.string().optional(),
      title: schema.string().optional(),
    }),
    operation: EnvoyToolOperation.listSessions,
  },
  {
    name: "slack_post",
    description:
      "Reply in a Slack thread as the Legion Slack app, with the bot token the secrets broker grants this session (LEGION_SLACK_BOT_TOKEN); the session never holds it. channel is the Slack channel id (a mention's channel_id); thread_ts is the thread's root ts: the mention's thread_ts, or its own ts when it has none. Answers the new message's ts. A refusal carries Slack's own error code (not_in_channel: the app is not in that channel) or the broker's state (waiting for approval, denied). Sent once and never retried: after 'Slack did not answer' the reply may already be in the thread, so do not post it again.",
    arguments: (schema) => ({
      channel: schema.string({ min: 1 }).describe("Slack channel id, such as C0123ABCD."),
      thread_ts: schema
        .string({ min: 1 })
        .describe("The thread's root ts, which keeps the reply in the mention's thread."),
      text: schema
        .string({ min: 1, max: SLACK_POST_TEXT_MAX })
        .describe(`The reply, in Slack mrkdwn, at most ${SLACK_POST_TEXT_MAX} characters.`),
    }),
    operation: EnvoyToolOperation.slackPost,
  },
] as const satisfies readonly ToolSpec[];

/**
 * When a stopped session was last seen, from `last_seen` (unix milliseconds), as a UTC instant to
 * the second: `2026-11-02T09:15:00Z`. An instant rather than "4 minutes ago", so the line means
 * the same whenever it is read again, in a transcript a day later included, and a test pins it
 * without a clock.
 */
function lastSeenText(lastSeen: number): string {
  return `${new Date(lastSeen).toISOString().slice(0, 19)}Z`;
}

/**
 * What a send says when the listener already held it under this target and idempotency key: the
 * resend stored nothing, so one copy is there and no second one.
 */
const STORED_ONCE =
  "an earlier attempt under the same idempotency_key was already stored, so this one stored no " +
  "second copy";

/**
 * The envoy_send confirmation line both hosts show the model.
 *
 * A connected target is sent to. A stopped one is queued: Envoy stored the message on that
 * session's topic and it arrives if the session runs again within the stream's
 * {@link STORED_MESSAGE_HOURS} hours. The queued line says `queued`, never `sent`, because
 * nothing read the message and a model told "sent" waits for an answer that is not coming.
 *
 * A `duplicate` answer is a resend of a message the listener already stored under the same
 * idempotency key. It stored nothing this time, so this answer's event id names no stored copy
 * and the line leaves the id out; the target's state still chooses the verb, because a stopped
 * target's one stored copy is waiting rather than delivered.
 *
 * `confirmed` is false only for a listener too old to echo the recipient, which is also too old
 * to answer a state, so the unconfirmed note belongs to the connected line alone.
 */
export function sendConfirmationText(result: {
  readonly envelope: { readonly event_id: string };
  readonly recipient: string;
  readonly confirmed: boolean;
  readonly state: SessionState;
  readonly lastSeen?: number;
  readonly duplicate: boolean;
}): string {
  if (result.state === "stopped") {
    const seen =
      result.lastSeen === undefined ? "" : ` (last seen ${lastSeenText(result.lastSeen)})`;
    const waits = `the session is stopped${seen}; if it runs again within ${STORED_MESSAGE_HOURS} hours, it gets the message`;
    if (result.duplicate) {
      return `queued once for ${result.recipient}: ${STORED_ONCE}; ${waits}`;
    }
    return `queued ${result.envelope.event_id} for ${result.recipient}: ${waits}`;
  }
  if (result.duplicate) {
    return `sent to ${result.recipient} once: ${STORED_ONCE}`;
  }
  return `sent ${result.envelope.event_id} to ${result.recipient}, which is connected${result.confirmed ? "" : " (recipient unconfirmed by listener)"}`;
}

/**
 * The envoy_publish confirmation line both hosts show the model, so one text is written once
 * rather than once per host.
 *
 * A role topic's answer names its holder and carries no count, so it reads as it always did. A
 * count of zero is the answer that matters most — nobody subscribes, so the message reaches
 * nobody — and a listener that counts nothing (a role topic, or one whose NATS user cannot read
 * the known-session bucket) sends no counts at all, which reads as the plain line every release
 * wrote before the counts existed.
 */
export function publishConfirmationText(result: {
  readonly envelope: { readonly event_id: string; readonly topic: string };
  readonly holder?: string;
  readonly recipients?: number;
  readonly recipientsConnected?: number;
}): string {
  const published = `published ${result.envelope.event_id}`;
  if (result.holder !== undefined) return `${published}; holder ${result.holder}`;
  if (result.recipients === undefined) return published;
  if (result.recipients === 0) {
    return `${published}; no other session subscribes to ${result.envelope.topic}, so no other session gets it`;
  }
  // One recipient takes the singular: "to 1 other session (1 running now)". A model reading
  // "1 other sessions" reads a bug in the sender before it reads the count.
  const sessions =
    result.recipients === 1 ? "1 other session" : `${result.recipients} other sessions`;
  const running =
    result.recipientsConnected === undefined ? "" : ` (${result.recipientsConnected} running now)`;
  return `${published} to ${sessions}${running}`;
}

/**
 * The sessions an `envoy_sessions` call's `machine` argument names, which both hosts filter for
 * themselves: the listener's `/v1/sessions` takes no machine. A session answers for two machines:
 * `host`, where its process runs, and `machine_id`, the listener it registered through. A filter
 * naming either one is that machine's filter, so a hub's id (`envoy-hub`, `gke`) lists the
 * sessions registered through it. No filter — the argument absent or empty — lists them all.
 */
export function sessionsOnMachine(
  sessions: readonly SessionInfo[],
  machine: string | undefined
): readonly SessionInfo[] {
  if (machine === undefined || machine === "") return sessions;
  return sessions.filter((session) => session.host === machine || session.machine_id === machine);
}

// The specs are frozen module constants, so each operation's schema is built once and shared;
// parsing never mutates it.
const envoyToolSchemas = new Map<EnvoyToolOperation, z.ZodType>();

/**
 * The idempotency key envoy_send takes: the transport's `crypto.randomUUID()`. The listener stores
 * a send once per target and key only for a key of this shape (`MINTED_DEDUPE_KEY_PATTERN`), so a
 * resend under any other key would be a second delivery.
 */
const SEND_IDEMPOTENCY_KEY = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * Validates an Envoy tool call on real zod in one pass and returns its typed arguments,
 * or throws a `ToolInputError` naming every problem. Hosts register the tools with
 * lenient argument validation, so this is the single place a bad call is refused.
 */
export function parseEnvoyToolArguments<Operation extends EnvoyToolOperation>(
  operation: Operation,
  parameters: Record<string, unknown>
): ToolArgumentsByOperation[Operation] {
  const spec = envoyToolSpecs.find((candidate) => candidate.operation === operation);
  if (spec === undefined) throw new Error(`missing Envoy tool specification for ${operation}`);
  let schema = envoyToolSchemas.get(operation);
  if (schema === undefined) {
    schema = z.object(spec.arguments(zodSchemaApi(z)) as z.ZodRawShape).strict();
    envoyToolSchemas.set(operation, schema);
  }
  const parsed = schema.safeParse(parameters, { reportInput: true });
  if (!parsed.success) {
    throw new ToolInputError(spec.name, formatZodIssues(parsed.error.issues, schema, spec.name));
  }
  const { data } = parsed;
  if (
    typeof data === "object" &&
    data !== null &&
    "idempotency_key" in data &&
    typeof data.idempotency_key === "string" &&
    !SEND_IDEMPOTENCY_KEY.test(data.idempotency_key)
  ) {
    throw new ToolInputError(spec.name, [
      `idempotency_key must be the key an unconfirmed send named, a lower-case UUID; got ${JSON.stringify(data.idempotency_key)}`,
    ]);
  }
  return data as ToolArgumentsByOperation[Operation];
}
