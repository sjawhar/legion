import {
  EnvelopeSchema,
  type SchemaApi,
  type SchemaNode,
  type ToolArgumentsShape,
  zodSchemaApi,
} from "@legion/contracts";
import { z } from "zod";
import type { MessageMetadataInput } from "./transport";

const DELIVERY_CONTRACT =
  "Delivery is at-least-once, possibly out of order across topics; use id for dedupe and at for freshness.";

const TOPIC_GUIDE =
  "Topic guide (all are under notifications.): agent.<session_id> (subscribe: own inbox); role.<role> " +
  "(publish-to; holders claim via envoy_role_set). > matches one or more trailing tokens and does not " +
  "match the base subject. Envoy registers the concrete base when you subscribe to <subject>.>, so " +
  "the recommended default remains github.<owner>.<repo>.pr.<n>.>. Default PR subscription: " +
  "github.<owner>.<repo>.pr.<n>.> (it receives the quiet PR family): pr.<n> (lifecycle: " +
  "opened/synchronize/closed; closed carries merged, merge_commit_sha, merged_by, head_sha), " +
  "pr.<n>.comment, pr.<n>.review, pr.<n>.mention, pr.<n>.checks (one head-checks settlement event: " +
  'passed/failed/cancelled/skipped with failing names and URLs; re-fires with superseded_settlement: "true" ' +
  "when new runs appear for the same head). Other GitHub: issue.<n>, issue.<n>.comment, " +
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
    urgency: schema.enum(URGENCY_VALUES).optional(),
    expects_reply: schema.enum(EXPECTS_REPLY_VALUES).optional(),
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
}

export type ToolSpec = {
  readonly name: string;
  readonly description: string;
  readonly arguments: <Element extends SchemaNode<Element>>(
    schema: SchemaApi<Element>
  ) => ToolArgumentsShape;
  readonly operation: EnvoyToolOperation;
  readonly requiresSubscriptionCapability: boolean;
};

export const envoyToolSpecs = [
  {
    name: "envoy_subscribe",
    description: `Subscribe this session to Envoy notification topics. ${TOPIC_GUIDE}`,
    arguments: (schema) => ({
      topics: schema.array(schema.string()).describe("NATS-style topic patterns to subscribe to."),
    }),
    operation: EnvoyToolOperation.subscribe,
    requiresSubscriptionCapability: true,
  },
  {
    name: "envoy_unsubscribe",
    description:
      "Unsubscribe this session from Envoy topics, or remove all current subscriptions if topics are omitted.",
    arguments: (schema) => ({ topics: schema.array(schema.string()).optional() }),
    operation: EnvoyToolOperation.unsubscribe,
    requiresSubscriptionCapability: true,
  },
  {
    name: "envoy_list",
    description:
      "List the current Envoy topic subscriptions for this session so you can confirm the exact topic shapes that are active.",
    arguments: () => ({}),
    operation: EnvoyToolOperation.listInterests,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_inbox",
    description: "List this Pi session's 50 most recent rendered Envoy deliveries, newest first.",
    arguments: () => ({}),
    operation: EnvoyToolOperation.inbox,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_send",
    description: `Send an Envoy agent-to-agent message directly to another session by session ID. ${DELIVERY_CONTRACT}`,
    arguments: (schema) => ({
      session_id: schema
        .string()
        .describe("Target session ID; find it with envoy_sessions or envoy_whoami."),
      ...messageArguments(schema),
    }),
    operation: EnvoyToolOperation.send,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_publish",
    description: `Publish an Envoy message to any topic. ${DELIVERY_CONTRACT}`,
    arguments: (schema) => ({ topic: schema.string(), ...messageArguments(schema) }),
    operation: EnvoyToolOperation.publish,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_role_set",
    description:
      "Set the current session as the holder of a named role. Messages published to notifications.role.<role> route to this session.",
    arguments: (schema) => ({ role: schema.string() }),
    operation: EnvoyToolOperation.setRole,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_role_get",
    description: "Get the live holder of a named Envoy role.",
    arguments: (schema) => ({ role: schema.string() }),
    operation: EnvoyToolOperation.getRole,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_whoami",
    description:
      "Returns this session's Envoy identity: session ID, machine ID, port, and directory.",
    arguments: () => ({}),
    operation: EnvoyToolOperation.whoami,
    requiresSubscriptionCapability: false,
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
    requiresSubscriptionCapability: false,
  },
] as const satisfies readonly ToolSpec[];
