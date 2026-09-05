import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults"
import { prepareDispatchCall } from "@legion/envoy-client/dispatch-call"
import { callDispatch, ghTokenGetter } from "@legion/envoy-client/dispatch-client"
import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config"
import {
  DISPATCH_TOOL_DESCRIPTION,
  DISPATCH_TOOL_NAME,
  dispatchToolShape,
  parseDispatchCall,
} from "@legion/envoy-client/dispatch-contract"
import { defaultExec } from "@legion/envoy-client/dispatch-cwd"
import { dispatchSubscriptionTopic } from "@legion/envoy-client/dispatch-subscribe"
import { machineID } from "@legion/envoy-client/machine"
import { EnvoyToolOperation, envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { createEnvoyClient } from "@legion/envoy-client/transport"
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
  inputSchema: z.toJSONSchema(z.object(dispatchToolShape)),
}

export const envoyMcpToolDefinitions = [
  ...envoyToolSpecs.map((spec) => ({
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

// One NATS connection per server process, opened by the first topic a tool
// follows, with the monitor's connect options. Without a broker address the
// registry interest is still recorded and the gap reported once on stderr;
// no tool call fails for it.
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
    const connection = await connect({ servers: natsUrl, name: `claude-envoy-mcp-${sessionId}` })
    return createThreadForwarder(connection, sessionId)
  } catch (error) {
    // Not cached: the next tool call tries the broker again.
    forwarder = undefined
    process.stderr.write(
      `envoy-mcp: cannot reach ${natsUrl}; messages on subscribed topics will not reach this session — ${error instanceof Error ? error.message : String(error)}\n`,
    )
    return null
  }
}

async function followTopics(sessionId: string, topics: readonly string[]): Promise<void> {
  if (shuttingDown) return
  forwarder ??= openThreadForwarder(sessionId)
  const active = await forwarder
  if (active === null || shuttingDown) return
  for (const topic of topics) active.follow(topic)
}

// Claude Code ends the session by closing stdin. The stdio transport does not
// watch for that, and an open NATS socket would otherwise keep this process
// alive after the session is gone. Only the broker leg is torn down here: a
// response still in flight is written to stdout as before, and the process
// exits once nothing else is pending.
async function shutdownForwarder(): Promise<void> {
  shuttingDown = true
  const active = await forwarder
  await active?.close()
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
    const cwd = process.cwd()
    const prepared = await prepareDispatchCall({
      call: parseDispatchCall(input),
      cwd,
      host: "claude",
      sessionId,
      env: process.env,
      exec: defaultExec,
    })
    const result = await callDispatch(
      { serviceUrl: dispatchConfig.url, getToken: ghTokenGetter(cwd) },
      prepared,
    )
    // The human answers on the GitHub issue. The registry interest tells Envoy
    // this session is listening; the forwarder is what actually carries the
    // thread's envelopes to the session's agent subject, where the monitor
    // renders them. The thread exists either way, so a failure on this leg is
    // reported on stderr, not surfaced as a failed dispatch.
    const topic = dispatchSubscriptionTopic(name, JSON.stringify(result))
    if (topic !== null) {
      try {
        await client.subscribe({
          sessionID: sessionId,
          directory: cwd,
          topics: [topic],
          port: 0,
          title: "",
          driving: true,
          selfSubscribed: true,
        })
        await followTopics(sessionId, [topic])
      } catch (error) {
        process.stderr.write(
          `envoy-mcp: dispatch opened ${result.url} but subscribing ${sessionId} to ${topic} failed — ${error instanceof Error ? error.message : String(error)}\n`,
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
      return client.send({
        sourceSessionID: sessionId,
        targetSessionID: args.session_id,
        message: args.message,
      })
    }
    case EnvoyToolOperation.publish: {
      const args = z.object(spec.arguments).parse(input)
      return client.publish({
        sourceSessionID: sessionId,
        topic: args.topic,
        message: args.message,
      })
    }
    case EnvoyToolOperation.subscribe: {
      const args = z.object(spec.arguments).parse(input)
      const interest = await client.subscribe({
        sessionID: sessionId,
        directory: process.cwd(),
        topics: args.topics,
        port: 0,
        title: "",
        driving: true,
        selfSubscribed: true,
      })
      await followTopics(sessionId, args.topics)
      return interest
    }
    case EnvoyToolOperation.unsubscribe: {
      const args = z.object(spec.arguments).parse(input)
      await client.unsubscribe({ sessionID: sessionId, topics: args.topics ?? [] })
      if (forwarder) await (await forwarder)?.unfollow(args.topics ?? [])
      return undefined
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
      const sessions = await client.listSessions()
      if (!args.machine) {
        return sessions
      }
      return sessions.filter((session) => session.machine_id === args.machine)
    }
    case EnvoyToolOperation.setRole: {
      const args = z.object(spec.arguments).parse(input)
      return client.setRole({ sessionID: sessionId, role: args.role })
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
