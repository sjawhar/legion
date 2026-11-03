import {
  EnvelopeSchema,
  type SchemaApi,
  type SchemaNode,
  SLACK_POST_TEXT_MAX,
  type ToolArgumentsShape,
  zodSchemaApi,
} from "@legion/contracts";
import { z } from "zod";
import { formatZodIssues, ToolInputError } from "./tool-input-errors";
import {
  type MessageMetadataInput,
  type SessionInfo,
  type SessionState,
  UNRESPONSIVE_RULE,
} from "./transport";

// The rule itself lives beside the states it is about (`transport.ts`), so the tool
// descriptions, the send confirmation and a refusal's message all say it in one wording.

const SESSION_STATE_GUIDE =
  "A session is live, unresponsive or gone, by how long ago it last registered: live within 5 " +
  "minutes, unresponsive after that and within an hour, gone past the hour or with no record at " +
  `all. ${UNRESPONSIVE_RULE} A session named with no state is live.`;

const DELIVERY_CONTRACT =
  "Delivery is at-least-once, possibly out of order across topics; use id for dedupe and at for freshness.";

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
    description: `Send an Envoy agent-to-agent message directly to another session by session ID. The confirmation names the recipient's state. A message to an unresponsive session is stored and reaches it when it answers again, so an unresponsive recipient is not a failed send and no reason to send to someone else instead. ${SESSION_STATE_GUIDE} ${DELIVERY_CONTRACT} A send that is not confirmed delivered errs naming its idempotency_key; send the same message again with that idempotency_key within 72 hours, and Envoy drops it as a repeat if the first copy arrived.`,
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
    description: `Publish an Envoy message to a notifications. topic. ${STORED_DELIVERY} A role or built-in topic is delivered as its guide says; a role message errs when its holder did not confirm it within the listener's window. A team or ad-hoc topic (such as notifications.team.deploy-squad.all) reaches every session subscribed to it when you publish, and is not kept for a session that subscribes later. The listener confirms it with each other session that holds its own NATS connection and registered the topic, and errs naming each session that did not confirm; a session that stopped without unregistering stays registered until Envoy finds it gone and is named until then. A named session is not a failed delivery to the other sessions. A session that is only unresponsive is named as no failure at all. The listener stores that session's copy, asks it for no receipt, and names the session under unresponsive. The accepted publish and the error both name it there. That session takes the message when it answers again. A session that holds its own NATS connection receives such a live topic directly, yours included. ${SESSION_STATE_GUIDE} ${DELIVERY_CONTRACT}`,
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
    description: `Get the holder of a named Envoy role, with its holder_state and the holder's last_seen. A holder that is only unresponsive still holds the role: nothing a lookup does releases its claim, and only a gone holder leaves the role unheld. ${SESSION_STATE_GUIDE}`,
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
      "List the Envoy sessions that are live or unresponsive; a gone session is listed by neither. " +
      "Filter by optional machine, directory, or title. Each row carries its state and its " +
      `last_seen, the epoch milliseconds of its last registration. ${SESSION_STATE_GUIDE}`,
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

/** The envoy_send confirmation line both hosts show the model, naming the recipient's state and,
 *  for one that stopped answering, since when (`lastSeen`, the listener's record of its last
 *  registration): a send to an unresponsive session is stored for it, not a delivery that
 *  failed, and how long it has been silent is what an agent judges the wait by. */
export function sendConfirmationText(result: {
  readonly envelope: { readonly event_id: string };
  readonly recipient: string;
  readonly recipientState: SessionState;
  readonly confirmed: boolean;
  readonly lastSeen?: number;
  readonly duplicate: boolean;
}): string {
  if (result.duplicate) {
    return `sent to ${result.recipient} once: an earlier attempt under the same idempotency_key was already stored, so this one stored no second copy`;
  }
  const since =
    result.lastSeen === undefined ? "" : ` since ${new Date(result.lastSeen).toISOString()}`;
  const state =
    result.recipientState === "unresponsive"
      ? `unresponsive${since}, so it is stored until that session answers; ${UNRESPONSIVE_RULE}`
      : result.recipientState;
  return `sent ${result.envelope.event_id} to ${result.recipient} (${state})${result.confirmed ? "" : " (recipient unconfirmed by listener)"}`;
}

/** The envoy_publish confirmation line both hosts show the model. The listener accepts a topic
 *  send whose subscriber stopped answering: it stores that session's copy, asks it for no
 *  receipt, and names the session under `unresponsive`. So the publish is no failure, and that
 *  peer has not taken the message. This line is the only place the agent that published hears
 *  it, which is why it names every such session and says the rule. A role publish names its
 *  holder and the state the listener resolved the holder in. */
export function publishConfirmationText(result: {
  readonly envelope: { readonly event_id: string };
  readonly holder?: string;
  readonly holderState?: SessionState;
  readonly unresponsive: readonly string[];
}): string {
  const sentences = [`published ${result.envelope.event_id}.`];
  if (result.holder !== undefined) {
    const state = result.holderState === undefined ? "" : ` (${result.holderState})`;
    sentences.push(`Its role holder is ${result.holder}${state}.`);
  }
  if (result.unresponsive.length > 0) {
    const one = result.unresponsive.length === 1;
    sentences.push(
      `Envoy stored it for ${result.unresponsive.join(", ")}.`,
      `${one ? "That session" : "Those sessions"} stopped answering, so ${one ? "it has" : "they have"} not taken it yet.`,
      UNRESPONSIVE_RULE
    );
  }
  return sentences.join(" ");
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
