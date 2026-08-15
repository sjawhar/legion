import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults"
import { EnvoyToolOperation, envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { createEnvoyClient } from "@legion/envoy-client/transport"
import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { z } from "zod"

export const envoyMcpToolDefinitions = envoyToolSpecs.map((spec) => ({
  name: spec.name,
  description: spec.description,
  inputSchema: z.toJSONSchema(z.object(spec.arguments)),
}))

class MissingClaudeSessionError extends Error {
  readonly name = "MissingClaudeSessionError"

  constructor() {
    super("CLAUDE_CODE_SESSION_ID is required for Envoy MCP tools")
  }
}

class UnsupportedEnvoyToolError extends Error {
  readonly name = "UnsupportedEnvoyToolError"

  constructor(readonly toolName: string) {
    super(`Unsupported Envoy tool: ${toolName}`)
  }
}

function currentSessionId(): string {
  const sessionId = process.env["CLAUDE_CODE_SESSION_ID"]
  if (!sessionId) {
    throw new MissingClaudeSessionError()
  }
  return sessionId
}

function mcpResult(value: unknown): {
  readonly content: readonly [{ readonly type: "text"; readonly text: string }]
} {
  return { content: [{ type: "text", text: JSON.stringify(value) }] }
}

export async function executeEnvoyTool(name: string, input: unknown): Promise<unknown> {
  const sessionId = currentSessionId()
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
        targetSessionID: args.target_session,
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
        machine_id: process.env["HOSTNAME"] ?? "unknown",
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
        "Use Envoy tools for cross-session messaging and topic subscriptions. Envoy messages arrive as native Claude Code peer messages.",
    },
  )

  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: envoyMcpToolDefinitions }))
  server.setRequestHandler(CallToolRequestSchema, async (request) =>
    mcpResult(await executeEnvoyTool(request.params.name, request.params.arguments)),
  )

  await server.connect(new StdioServerTransport())
}
