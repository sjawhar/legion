import { z } from "zod";

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

const messageArguments = {
  message: z.string(),
  in_reply_to: z.string().optional(),
  supersedes: z.string().optional(),
  urgency: z.enum(["low", "med", "high", "blocking"]).optional(),
  expects_reply: z.enum(["none", "optional", "required"]).optional(),
  expires_at: z.number().int().optional(),
};

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

export type ToolSpec = {
  readonly name: string;
  readonly description: string;
  readonly arguments: z.ZodRawShape;
  readonly operation: EnvoyToolOperation;
  readonly requiresSubscriptionCapability: boolean;
};

export const envoyToolSpecs = [
  {
    name: "envoy_subscribe",
    description: `Subscribe this session to Envoy notification topics. ${TOPIC_GUIDE}`,
    arguments: {
      topics: z.array(z.string()).describe("NATS-style topic patterns to subscribe to."),
    },
    operation: EnvoyToolOperation.subscribe,
    requiresSubscriptionCapability: true,
  },
  {
    name: "envoy_unsubscribe",
    description:
      "Unsubscribe this session from Envoy topics, or remove all current subscriptions if topics are omitted.",
    arguments: { topics: z.array(z.string()).optional() },
    operation: EnvoyToolOperation.unsubscribe,
    requiresSubscriptionCapability: true,
  },
  {
    name: "envoy_list",
    description:
      "List the current Envoy topic subscriptions for this session so you can confirm the exact topic shapes that are active.",
    arguments: {},
    operation: EnvoyToolOperation.listInterests,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_inbox",
    description: "List this Pi session's 50 most recent rendered Envoy deliveries, newest first.",
    arguments: {},
    operation: EnvoyToolOperation.inbox,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_send",
    description: `Send an Envoy agent-to-agent message directly to another session by session ID. ${DELIVERY_CONTRACT}`,
    arguments: {
      session_id: z
        .string()
        .describe("Target session ID; find it with envoy_sessions or envoy_whoami."),
      ...messageArguments,
    },
    operation: EnvoyToolOperation.send,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_publish",
    description: `Publish an Envoy message to any topic. ${DELIVERY_CONTRACT}`,
    arguments: { topic: z.string(), ...messageArguments },
    operation: EnvoyToolOperation.publish,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_role_set",
    description:
      "Set the current session as the holder of a named role. Messages published to notifications.role.<role> route to this session.",
    arguments: { role: z.string() },
    operation: EnvoyToolOperation.setRole,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_role_get",
    description: "Get the live holder of a named Envoy role.",
    arguments: { role: z.string() },
    operation: EnvoyToolOperation.getRole,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_whoami",
    description:
      "Returns this session's Envoy identity: session ID, machine ID, port, and directory.",
    arguments: {},
    operation: EnvoyToolOperation.whoami,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_sessions",
    description:
      "List all live sessions registered with Envoy. Filter by optional machine, directory, or title.",
    arguments: {
      machine: z.string().optional(),
      dir: z.string().optional(),
      title: z.string().optional(),
    },
    operation: EnvoyToolOperation.listSessions,
    requiresSubscriptionCapability: false,
  },
] as const satisfies readonly ToolSpec[];
