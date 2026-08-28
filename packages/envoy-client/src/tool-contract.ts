import { z } from "zod";

export const EnvoyToolOperation = {
  subscribe: "subscribe",
  unsubscribe: "unsubscribe",
  listInterests: "listInterests",
  send: "send",
  publish: "publish",
  setRole: "setRole",
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
    description:
      "Subscribe this session to Envoy notification topics. GitHub topics are resource-scoped: notifications.github.<owner>.<repo>.pr.<number>, notifications.github.<owner>.<repo>.issue.<number>.comment, etc. Use NATS wildcards for broad subscriptions: notifications.github.<owner>.<repo>.pr.> (all PR events). Other topics: notifications.agent.<session_id>, notifications.slack.<team_id>.<channel_id>.message, notifications.slack.<team_id>.<channel_id>.mention. Use this when a session should RECEIVE future events.",
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
    name: "envoy_send",
    description:
      "Send an Envoy agent-to-agent message directly to another session by session ID. Use this for coordination or to notify a known controller or worker session.",
    arguments: {
      session_id: z
        .string()
        .describe("Target session ID (ses_…); find it with envoy_sessions or envoy_whoami."),
      message: z.string(),
    },
    operation: EnvoyToolOperation.send,
    requiresSubscriptionCapability: false,
  },
  {
    name: "envoy_publish",
    description:
      "Publish an Envoy message to any topic. Use for broadcast to named topics like notifications.role.legion-controller, team channels, or custom routing.",
    arguments: { topic: z.string(), message: z.string() },
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
      "List all live sessions registered with Envoy. Use the optional machine filter to show only sessions on a specific host.",
    arguments: { machine: z.string().optional() },
    operation: EnvoyToolOperation.listSessions,
    requiresSubscriptionCapability: false,
  },
] as const satisfies readonly ToolSpec[];
