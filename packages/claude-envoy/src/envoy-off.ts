import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import type { ChannelSession } from "./envoy-channel-server"
import { roleStateFile, type SessionIdentity, writePersistedRole } from "./session-identity"

/**
 * The session of a channel server whose Envoy takes no inbound messages, for `why`: no NATS
 * consumer, nothing registered with the listener (which would route a delivery to a deaf process),
 * an empty inbox, and following a topic refused naming `why`. A role an explicit `envoy_role_set`
 * claimed through the listener is still persisted, so a later server with NATS takes it back.
 */
export function inboundOffChannelSession(options: {
  readonly identity: SessionIdentity
  readonly stateDirectory: string
  readonly why: string
}): ChannelSession {
  const { identity, stateDirectory, why } = options
  return {
    delivery: { enqueue: async () => false, inbox: () => [] },
    topics: () => [],
    async follow() {
      throw new Error(`inbound messages are off for this session: ${why}`)
    },
    unfollow: async () => [],
    // No heartbeat runs, so no role is ever regained.
    onRoleRegained: () => undefined,
    async rememberRole(role) {
      await writePersistedRole(roleStateFile(stateDirectory, identity.id), {
        session_id: identity.id,
        role,
      })
    },
    shutdown: async () => undefined,
  }
}

/**
 * The MCP server, as `serverInfo`, of a session whose Envoy is off: no tool, every call refused
 * with `refusal`, and nothing subscribed, until stdin ends or a signal stops it.
 */
export async function serveEnvoyOff(
  serverInfo: { readonly name: string; readonly version: string },
  refusal: string,
): Promise<void> {
  const server = new Server(serverInfo, { capabilities: { tools: {} } })
  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: [] }))
  server.setRequestHandler(CallToolRequestSchema, async () => {
    throw new Error(refusal)
  })
  const stopped = Promise.withResolvers<void>()
  process.stdin.once("end", stopped.resolve)
  for (const signal of ["SIGTERM", "SIGINT", "SIGHUP"] as const) {
    process.once(signal, () => stopped.resolve())
  }
  await server.connect(new StdioServerTransport())
  try {
    await stopped.promise
  } finally {
    await server.close().catch(() => undefined)
  }
}
