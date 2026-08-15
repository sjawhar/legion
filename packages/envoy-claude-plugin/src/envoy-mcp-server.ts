import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import ky from "ky"
import { z } from "zod"

import { envoyToolDefinitions } from "./envoy-client"

const ENVOY_URL = "http://127.0.0.1:9020"

const sendSchema = z.object({ target_session: z.string().min(1), message: z.string().min(1) })
const publishSchema = z.object({ topic: z.string().min(1), message: z.string().min(1) })
const subscribeSchema = z.object({ topics: z.array(z.string().min(1)).min(1) })
const unsubscribeSchema = z.object({ topics: z.array(z.string().min(1)).optional() })
const sessionsSchema = z.object({ machine: z.string().min(1).optional() })
const roleSchema = z.object({ role: z.string().min(1) })
const toolNameSchema = z.enum([
  "envoy_send",
  "envoy_publish",
  "envoy_subscribe",
  "envoy_unsubscribe",
  "envoy_list",
  "envoy_whoami",
  "envoy_sessions",
  "envoy_role_set",
])

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

async function envoyPost(path: string, body: unknown): Promise<unknown> {
  return ky
    .post(new URL(path, process.env["ENVOY_URL"] ?? ENVOY_URL), {
      json: body,
      retry: 0,
      timeout: 5_000,
    })
    .json<unknown>()
}

async function envoyGet(path: string): Promise<unknown> {
  return ky
    .get(new URL(path, process.env["ENVOY_URL"] ?? ENVOY_URL), {
      retry: 0,
      timeout: 5_000,
    })
    .json<unknown>()
}

async function executeEnvoyTool(name: string, input: unknown): Promise<unknown> {
  const sessionId = currentSessionId()
  const toolName = toolNameSchema.parse(name)

  switch (toolName) {
    case "envoy_send": {
      const args = sendSchema.parse(input)
      return envoyPost("/v1/messages/send", { source_session: sessionId, ...args })
    }
    case "envoy_publish": {
      const args = publishSchema.parse(input)
      return envoyPost("/v1/messages/publish", { source_session: sessionId, ...args })
    }
    case "envoy_subscribe": {
      const args = subscribeSchema.parse(input)
      return envoyPost("/v1/interests/subscribe", {
        session_id: sessionId,
        dir: process.cwd(),
        topics: args.topics,
        port: 0,
        title: "",
        driving: true,
        self_subscribed: true,
      })
    }
    case "envoy_unsubscribe": {
      const args = unsubscribeSchema.parse(input)
      return envoyPost("/v1/interests/unsubscribe", {
        session_id: sessionId,
        topics: args.topics ?? [],
      })
    }
    case "envoy_list":
      return envoyGet(`/v1/interests/${encodeURIComponent(sessionId)}`)
    case "envoy_whoami":
      return {
        session_id: sessionId,
        machine_id: process.env["HOSTNAME"] ?? "unknown",
        dir: process.cwd(),
      }
    case "envoy_sessions": {
      const args = sessionsSchema.parse(input)
      const sessions = await envoyGet("/v1/sessions")
      if (!args.machine || !Array.isArray(sessions)) {
        return sessions
      }
      return sessions.filter(
        (session): boolean =>
          typeof session === "object" &&
          session !== null &&
          Reflect.get(session, "machine_id") === args.machine,
      )
    }
    case "envoy_role_set": {
      const args = roleSchema.parse(input)
      return envoyPost("/v1/roles/set", { session_id: sessionId, ...args })
    }
    default:
      throw new UnsupportedEnvoyToolError(toolName)
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

  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: envoyToolDefinitions }))
  server.setRequestHandler(CallToolRequestSchema, async (request) =>
    mcpResult(await executeEnvoyTool(request.params.name, request.params.arguments)),
  )

  await server.connect(new StdioServerTransport())
}
