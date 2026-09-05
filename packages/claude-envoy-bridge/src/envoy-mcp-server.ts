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
import { machineID } from "@legion/envoy-client/machine"
import { EnvoyToolOperation, envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { createEnvoyClient } from "@legion/envoy-client/transport"
import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { z } from "zod"
import { monitorSessionId } from "./monitor-identity"

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

export async function executeEnvoyTool(name: string, input: unknown): Promise<unknown> {
  // The identity the monitor subscribes under, so replies and threads name one session.
  const sessionId = monitorSessionId({
    ENVOY_SESSION_ID: process.env["ENVOY_SESSION_ID"],
    CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
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
    return callDispatch({ serviceUrl: dispatchConfig.url, getToken: ghTokenGetter(cwd) }, prepared)
  }
  const spec = envoyToolSpecs.find((candidate) => candidate.name === name)
  if (!spec) {
    throw new UnsupportedEnvoyToolError(name)
  }
  const client = createEnvoyClient({
    baseUrl: envoyDefaultsFromEnvironment(process.env).envoyUrl,
    fetch: globalThis.fetch,
  })

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
      return client.subscribe({
        sessionID: sessionId,
        directory: process.cwd(),
        topics: args.topics,
        port: 0,
        title: "",
        driving: true,
        selfSubscribed: true,
      })
    }
    case EnvoyToolOperation.unsubscribe: {
      const args = z.object(spec.arguments).parse(input)
      await client.unsubscribe({ sessionID: sessionId, topics: args.topics ?? [] })
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

  await server.connect(new StdioServerTransport())
}
