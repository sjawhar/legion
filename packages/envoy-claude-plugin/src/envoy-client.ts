import ky from "ky"

type JsonSchema = {
  readonly type: string
  readonly description?: string | undefined
  readonly properties?: Readonly<Record<string, JsonSchema>> | undefined
  readonly required?: readonly string[] | undefined
  readonly items?: JsonSchema | undefined
}

export type EnvoyToolDefinition = {
  readonly name: string
  readonly description: string
  readonly inputSchema: JsonSchema
}

const stringSchema = { type: "string" } as const satisfies JsonSchema
const topicsSchema = {
  type: "array",
  items: stringSchema,
} as const satisfies JsonSchema

export const envoyToolDefinitions = [
  {
    name: "envoy_send",
    description: "Send a direct message to an Envoy session.",
    inputSchema: {
      type: "object",
      properties: { target_session: stringSchema, message: stringSchema },
      required: ["target_session", "message"],
    },
  },
  {
    name: "envoy_publish",
    description: "Publish a message to an Envoy topic.",
    inputSchema: {
      type: "object",
      properties: { topic: stringSchema, message: stringSchema },
      required: ["topic", "message"],
    },
  },
  {
    name: "envoy_subscribe",
    description: "Subscribe this session to Envoy topics.",
    inputSchema: {
      type: "object",
      properties: { topics: topicsSchema },
      required: ["topics"],
    },
  },
  {
    name: "envoy_unsubscribe",
    description: "Unsubscribe this session from Envoy topics.",
    inputSchema: { type: "object", properties: { topics: topicsSchema } },
  },
  {
    name: "envoy_list",
    description: "List this session's Envoy topic subscriptions.",
    inputSchema: { type: "object" },
  },
  {
    name: "envoy_whoami",
    description: "Show this session's Envoy identity.",
    inputSchema: { type: "object" },
  },
  {
    name: "envoy_sessions",
    description: "List live Envoy sessions, optionally for one machine.",
    inputSchema: { type: "object", properties: { machine: stringSchema } },
  },
  {
    name: "envoy_role_set",
    description: "Claim an Envoy role for this session.",
    inputSchema: {
      type: "object",
      properties: { role: stringSchema },
      required: ["role"],
    },
  },
] as const satisfies readonly EnvoyToolDefinition[]

export type SendEnvoyMessageOptions = {
  readonly baseUrl: string
  readonly targetSession: string
  readonly message: string
}

export async function sendEnvoyMessage(options: SendEnvoyMessageOptions): Promise<void> {
  await ky.post(new URL("/v1/messages/send", options.baseUrl), {
    json: {
      target_session: options.targetSession,
      message: options.message,
    },
    retry: 0,
    timeout: 5_000,
  })
}
