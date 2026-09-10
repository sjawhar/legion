import { dispatchToolSpecs, zodSchemaApi } from "@legion/contracts"
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults"
import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config"
import { executeDispatchTool } from "@legion/envoy-client/dispatch-execute"
import { dispatchSubscriptionTopic } from "@legion/envoy-client/dispatch-subscribe"
import { messageFor } from "@legion/envoy-client/errors"
import { machineID } from "@legion/envoy-client/machine"
import {
  EnvoyToolOperation,
  envoyToolSpecs,
  type ToolArgumentsByOperation,
  type ToolSpec,
  toMessageMetadata,
} from "@legion/envoy-client/tool-contract"
import {
  createEnvoyClient,
  type EnvoyClient,
  expandSubscriptionTopics,
  type Interest,
} from "@legion/envoy-client/transport"
import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { connect } from "nats"
import { z } from "zod"
import { monitorSessionId } from "./monitor-identity"
import { createThreadForwarder, type ThreadForwarder } from "./thread-forwarder"

// The shared tool contract builds argument shapes on the caller's Zod so each host registers
// schemas its runtime recognises; this bridge validates and emits JSON Schema with its own.
function argumentsSchema(spec: ToolSpec): z.ZodObject<z.ZodRawShape> {
  return z.object(spec.arguments(zodSchemaApi(z)) as z.ZodRawShape)
}

function parseArguments<Operation extends EnvoyToolOperation>(
  spec: ToolSpec & { readonly operation: Operation },
  input: unknown,
): ToolArgumentsByOperation[Operation] {
  return argumentsSchema(spec).parse(input) as ToolArgumentsByOperation[Operation]
}

// Claude Code has no native tool API, so its MCP server exposes every native
// Dispatch operation when the shared configuration resolves. Claude exposes no
// session title; the monitor session identity is attached to each request.
const dispatchConfig = resolveDispatchConfig(process.env, { cwd: process.cwd() })
if (!dispatchConfig.enabled) {
  process.stderr.write(
    `envoy-mcp: Dispatch tools disabled — ${dispatchConfig.error ?? "no Dispatch URL configured"}\n`,
  )
}

const dispatchToolDefinitions = dispatchToolSpecs.map((spec) => ({
  name: spec.name,
  description: spec.description,
  inputSchema: z.toJSONSchema(z.object(spec.arguments(zodSchemaApi(z)) as z.ZodRawShape)),
}))

export const envoyMcpToolDefinitions = [
  ...envoyToolSpecs
    .filter((spec) => spec.operation !== EnvoyToolOperation.inbox)
    .map((spec) => ({
      name: spec.name,
      description: spec.description,
      inputSchema: z.toJSONSchema(argumentsSchema(spec)),
    })),
  ...(dispatchConfig.enabled ? dispatchToolDefinitions : []),
]

class UnsupportedEnvoyToolError extends Error {
  readonly name = "UnsupportedEnvoyToolError"

  constructor(readonly toolName: string) {
    super(`Unsupported Envoy tool: ${toolName}`)
  }
}

function mcpResult(value: unknown): {
  readonly content: readonly [{ readonly type: "text"; readonly text: string }]
} {
  return { content: [{ type: "text", text: JSON.stringify(value) }] }
}

// One NATS connection per server process, opened by the first topic a tool
// follows, with pi-envoy's connect options: nats.js rides out broker outages
// on its own and re-subscribes when the broker returns. A connection it has
// given up on for good is replaced by the next follow, carrying every followed
// topic over. Manual envoy_subscribe first establishes that forwarding leg and
// rejects rather than recording an undeliverable interest. Dispatch auto-
// subscription remains best-effort and reports its forwarding gap on stderr.
let forwarder: Promise<ThreadForwarder | null> | undefined
let shuttingDown = false

async function openThreadForwarder(sessionId: string): Promise<ThreadForwarder | null> {
  const natsUrl = process.env["ENVOY_NATS_URL"]
  if (natsUrl === undefined || natsUrl.trim().length === 0) {
    process.stderr.write(
      "envoy-mcp: ENVOY_NATS_URL is not set; messages on subscribed topics will not reach this session\n",
    )
    return null
  }
  try {
    const connection = await connect({
      servers: natsUrl,
      name: `claude-envoy-mcp-${sessionId}`,
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
    })
    return createThreadForwarder(connection, sessionId)
  } catch (error) {
    // Not cached: the next tool call tries the broker again.
    forwarder = undefined
    process.stderr.write(
      `envoy-mcp: cannot reach ${natsUrl}; messages on subscribed topics will not reach this session — ${messageFor(error)}\n`,
    )
    return null
  }
}

async function followTopics(sessionId: string, topics: readonly string[]): Promise<boolean> {
  if (shuttingDown) return false
  try {
    forwarder ??= openThreadForwarder(sessionId)
    const attempt = forwarder
    let active = await attempt
    let pending = topics
    if (active?.isClosed()) {
      // The first caller to notice the closed connection replaces it; the
      // topics it carried come along, since the registry still lists them.
      pending = [...active.topics(), ...topics]
      process.stderr.write(
        `envoy-mcp: the broker connection closed; reopening it for ${pending.length} topic(s)\n`,
      )
      if (forwarder === attempt) forwarder = undefined
      forwarder ??= openThreadForwarder(sessionId)
      active = await forwarder
    }
    if (active === null) {
      if (forwarder === attempt) forwarder = undefined
      return false
    }
    if (shuttingDown) return false
    for (const topic of pending) active.follow(topic)
    return true
  } catch (error) {
    process.stderr.write(
      `envoy-mcp: cannot forward ${topics.join(", ")} to ${sessionId} — ${messageFor(error)}\n`,
    )
    return false
  }
}

// Claude Code ends the session by closing stdin. The stdio transport does not
// watch for that, and an open NATS socket would otherwise keep this process
// alive after the session is gone. Only the broker leg is torn down here: a
// response still in flight is written to stdout as before, and the process
// exits once nothing else is pending. A reopen in flight may replace
// `forwarder` while the one captured here is awaited; that connection is
// closed too, so none outlives the session.
export async function shutdownForwarder(): Promise<void> {
  shuttingDown = true
  for (let current = forwarder; current !== undefined; ) {
    const active = await current
    await active?.close()
    current = forwarder === current ? undefined : forwarder
  }
}

/** Record the interest with Envoy, then carry the topics' envelopes to the session's agent subject. */
async function subscribeAndFollow(
  client: EnvoyClient,
  sessionId: string,
  topics: readonly string[],
  requireForwarder = false,
): Promise<Interest> {
  const expandedTopics = expandSubscriptionTopics(topics)
  if (requireForwarder && !(await followTopics(sessionId, expandedTopics))) {
    throw new Error("envoy_subscribe requires a live ENVOY_NATS_URL forwarder")
  }
  const interest = await client.subscribe({
    sessionID: sessionId,
    directory: process.cwd(),
    topics: expandedTopics,
    port: 0,
    title: "",
    driving: true,
    selfSubscribed: true,
  })
  if (!requireForwarder) await followTopics(sessionId, expandedTopics)
  return interest
}

export async function executeEnvoyTool(name: string, input: unknown): Promise<unknown> {
  // The identity the monitor subscribes under, so replies and threads name one session.
  const sessionId = monitorSessionId({
    ENVOY_SESSION_ID: process.env["ENVOY_SESSION_ID"],
    CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
  })
  const client = createEnvoyClient({
    baseUrl: envoyDefaultsFromEnvironment(process.env).envoyUrl,
    fetch: globalThis.fetch,
  })
  const dispatchSpec = dispatchToolSpecs.find((candidate) => candidate.name === name)
  if (dispatchSpec !== undefined) {
    if (!dispatchConfig.enabled) throw new UnsupportedEnvoyToolError(name)
    const result = await executeDispatchTool({
      tool: dispatchSpec.name,
      args: input as Record<string, unknown>,
      cwd: process.cwd(),
      host: "claude",
      sessionId,
      config: dispatchConfig,
      env: process.env,
    })
    // The registry interest tells Envoy this session is listening; the
    // forwarder carries matching envelopes to its agent subject. A forwarding
    // failure is logged because the Dispatch operation itself succeeded.
    const topic = dispatchSubscriptionTopic(result.details)
    if (topic !== null) {
      try {
        await subscribeAndFollow(client, sessionId, [topic])
      } catch (error) {
        const issue =
          typeof result.details["issue"] === "string" ? result.details["issue"] : "the issue"
        process.stderr.write(
          `envoy-mcp: ${name} completed for ${issue} but subscribing ${sessionId} to ${topic} failed — ${messageFor(error)}\n`,
        )
      }
    }
    return result
  }
  const spec = envoyToolSpecs.find((candidate) => candidate.name === name)
  if (!spec) {
    throw new UnsupportedEnvoyToolError(name)
  }

  switch (spec.operation) {
    case EnvoyToolOperation.send: {
      const args = parseArguments(spec, input)
      const result = await client.send({
        source: "agent",
        sourceSessionID: sessionId,
        targetSessionID: args.session_id,
        message: args.message,
        ...toMessageMetadata(args),
      })
      return {
        message: `sent ${result.envelope.event_id} to ${result.recipient}${result.confirmed ? "" : " (recipient unconfirmed by listener)"}`,
        event_id: result.envelope.event_id,
        recipient: result.recipient,
        confirmed: result.confirmed,
      }
    }
    case EnvoyToolOperation.publish: {
      const args = parseArguments(spec, input)
      return client.publish({
        source: "agent",
        sourceSessionID: sessionId,
        topic: args.topic,
        message: args.message,
        ...toMessageMetadata(args),
      })
    }
    case EnvoyToolOperation.subscribe: {
      const args = parseArguments(spec, input)
      return subscribeAndFollow(client, sessionId, args.topics, true)
    }
    case EnvoyToolOperation.unsubscribe: {
      const args = parseArguments(spec, input)
      const topics = expandSubscriptionTopics(args.topics ?? [])
      await client.unsubscribe({ sessionID: sessionId, topics })
      const active = forwarder === undefined ? null : await forwarder
      // An empty list means everything; name what was actually being forwarded.
      const removed = topics.length === 0 ? (active?.topics() ?? []) : topics
      await active?.unfollow(topics)
      return { removed }
    }
    case EnvoyToolOperation.listInterests:
      parseArguments(spec, input)
      return client.getInterest(sessionId)
    case EnvoyToolOperation.whoami:
      parseArguments(spec, input)
      return {
        session_id: sessionId,
        machine_id: machineID(),
        dir: process.cwd(),
      }
    case EnvoyToolOperation.listSessions: {
      const args = parseArguments(spec, input)
      const sessions = await client.listSessions({
        ...(args.dir === undefined ? {} : { directory: args.dir }),
        ...(args.title === undefined ? {} : { title: args.title }),
      })
      if (!args.machine) {
        return sessions
      }
      return sessions.filter((session) => session.machine_id === args.machine)
    }
    case EnvoyToolOperation.setRole: {
      const args = parseArguments(spec, input)
      const result = await client.setRole({ sessionID: sessionId, role: args.role })
      // A hard claim always lands; the discriminant exists for soft claims.
      if (!result.claimed) throw new Error(`role ${args.role} is held by ${result.holder}`)
      return result.interest
    }
    case EnvoyToolOperation.getRole: {
      const args = parseArguments(spec, input)
      return client.getRole(args.role)
    }
    default:
      throw new UnsupportedEnvoyToolError(name)
  }
}

export async function runEnvoyMcpServer(): Promise<void> {
  const server = new Server(
    { name: "envoy", version: "0.1.0" },
    {
      capabilities: { tools: {} },
      instructions:
        "Use Envoy tools for cross-session messaging and topic subscriptions, and the native Dispatch tools to create issues, ask questions, comment, edit documents, upload artifacts, and read Dispatch state. Envoy messages arrive as native Claude Code peer messages.",
    },
  )

  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: envoyMcpToolDefinitions }))
  server.setRequestHandler(CallToolRequestSchema, async (request) =>
    mcpResult(await executeEnvoyTool(request.params.name, request.params.arguments)),
  )

  process.stdin.once("end", () => {
    void shutdownForwarder()
  })

  await server.connect(new StdioServerTransport())
}
