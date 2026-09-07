import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults"
import { executeDispatch } from "@legion/envoy-client/dispatch-call"
import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config"
import {
  DISPATCH_TOOL_DESCRIPTION,
  DISPATCH_TOOL_JSON_SCHEMA,
  DISPATCH_TOOL_NAME,
  parseDispatchCall,
} from "@legion/envoy-client/dispatch-contract"
import { dispatchSubscriptionTopic } from "@legion/envoy-client/dispatch-subscribe"
import { messageFor } from "@legion/envoy-client/errors"
import { machineID } from "@legion/envoy-client/machine"
import { EnvoyToolOperation, envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import {
  createEnvoyClient,
  type EnvoyClient,
  expandSubscriptionTopics,
  type Interest,
  type MessageMetadataInput,
} from "@legion/envoy-client/transport"
import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { connect } from "nats"
import { z } from "zod"
import { monitorSessionId } from "./monitor-identity"
import { createThreadForwarder, type ThreadForwarder } from "./thread-forwarder"

// Claude Code has no native tool API, so dispatch is a tool of this MCP
// server; it runs with the session identity the monitor uses, which is what
// the thread records (Claude exposes no title). Present only where dispatch
// is enabled; an invalid envoy.json is reported on stderr and the tool omitted.
const dispatchConfig = resolveDispatchConfig(process.env, { cwd: process.cwd() })
if (dispatchConfig.error !== null) {
  process.stderr.write(`envoy-mcp: dispatch tool disabled — ${dispatchConfig.error}\n`)
}

const dispatchToolDefinition = {
  name: DISPATCH_TOOL_NAME,
  description: DISPATCH_TOOL_DESCRIPTION,
  inputSchema: DISPATCH_TOOL_JSON_SCHEMA,
}

export const envoyMcpToolDefinitions = [
  ...envoyToolSpecs
    .filter((spec) => spec.operation !== EnvoyToolOperation.inbox)
    .map((spec) => ({
      name: spec.name,
      description: spec.description,
      inputSchema: z.toJSONSchema(z.object(spec.arguments)),
    })),
  ...(dispatchConfig.url === null ? [] : [dispatchToolDefinition]),
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

function messageMetadataFor(input: {
  readonly in_reply_to?: string | undefined
  readonly supersedes?: string | undefined
  readonly urgency?: "low" | "med" | "high" | "blocking" | undefined
  readonly expects_reply?: "none" | "optional" | "required" | undefined
  readonly expires_at?: number | undefined
}): MessageMetadataInput {
  return {
    ...(input.in_reply_to === undefined ? {} : { inReplyTo: input.in_reply_to }),
    ...(input.supersedes === undefined ? {} : { supersedes: input.supersedes }),
    ...(input.urgency === undefined ? {} : { urgency: input.urgency }),
    ...(input.expects_reply === undefined ? {} : { expectsReply: input.expects_reply }),
    ...(input.expires_at === undefined ? {} : { expiresAt: input.expires_at }),
  }
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
  if (requireForwarder && !(await followTopics(sessionId, topics))) {
    throw new Error("envoy_subscribe requires a live ENVOY_NATS_URL forwarder")
  }
  const interest = await client.subscribe({
    sessionID: sessionId,
    directory: process.cwd(),
    topics,
    port: 0,
    title: "",
    driving: true,
    selfSubscribed: true,
  })
  if (!requireForwarder) await followTopics(sessionId, topics)
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
  if (name === DISPATCH_TOOL_NAME) {
    if (dispatchConfig.url === null) throw new UnsupportedEnvoyToolError(name)
    const result = await executeDispatch({
      call: parseDispatchCall(input),
      cwd: process.cwd(),
      host: "claude",
      sessionId,
      serviceUrl: dispatchConfig.url,
    })
    // The human answers on the GitHub issue. The registry interest tells Envoy
    // this session is listening; the forwarder is what actually carries the
    // thread's envelopes to the session's agent subject, where the monitor
    // renders them. The thread exists either way, so a failure on this leg is
    // reported on stderr, not surfaced as a failed dispatch.
    const topic = dispatchSubscriptionTopic(name, JSON.stringify(result))
    if (topic !== null) {
      try {
        await subscribeAndFollow(client, sessionId, [topic])
      } catch (error) {
        process.stderr.write(
          `envoy-mcp: dispatch opened ${result.url} but subscribing ${sessionId} to ${topic} failed — ${messageFor(error)}\n`,
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
      const args = z.object(spec.arguments).parse(input)
      const result = await client.send({
        source: "agent",
        sourceSessionID: sessionId,
        targetSessionID: args.session_id,
        message: args.message,
        ...messageMetadataFor(args),
      })
      return {
        message: `sent ${result.envelope.event_id} to ${result.recipient}${result.confirmed ? "" : " (recipient unconfirmed by listener)"}`,
        event_id: result.envelope.event_id,
        recipient: result.recipient,
        confirmed: result.confirmed,
      }
    }
    case EnvoyToolOperation.publish: {
      const args = z.object(spec.arguments).parse(input)
      return client.publish({
        source: "agent",
        sourceSessionID: sessionId,
        topic: args.topic,
        message: args.message,
        ...messageMetadataFor(args),
      })
    }
    case EnvoyToolOperation.subscribe: {
      const args = z.object(spec.arguments).parse(input)
      return subscribeAndFollow(client, sessionId, args.topics, true)
    }
    case EnvoyToolOperation.unsubscribe: {
      const args = z.object(spec.arguments).parse(input)
      const topics = expandSubscriptionTopics(args.topics ?? [])
      await client.unsubscribe({ sessionID: sessionId, topics })
      const active = forwarder === undefined ? null : await forwarder
      // An empty list means everything; name what was actually being forwarded.
      const removed = topics.length === 0 ? (active?.topics() ?? []) : topics
      await active?.unfollow(topics)
      return { removed }
    }
    case EnvoyToolOperation.listInterests:
      z.object(spec.arguments).parse(input)
      return client.getInterest(sessionId)
    case EnvoyToolOperation.whoami:
      z.object(spec.arguments).parse(input)
      return {
        session_id: sessionId,
        machine_id: machineID(),
        dir: process.cwd(),
      }
    case EnvoyToolOperation.listSessions: {
      const args = z.object(spec.arguments).parse(input)
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
      const args = z.object(spec.arguments).parse(input)
      return client.setRole({ sessionID: sessionId, role: args.role })
    }
    case EnvoyToolOperation.getRole: {
      const args = z.object(spec.arguments).parse(input)
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
        "Use Envoy tools for cross-session messaging and topic subscriptions, and dispatch to raise or continue a durable question thread for the human. Envoy messages arrive as native Claude Code peer messages.",
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
