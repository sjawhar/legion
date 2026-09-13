import { mkdir, readFile, rename, rm, writeFile } from "node:fs/promises"
import { dirname } from "node:path"
import {
  agentSubject,
  dispatchToolSchema,
  dispatchToolSpecs,
  zodSchemaApi,
} from "@legion/contracts"
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults"
import { inboundTimestamp, renderInbound } from "@legion/envoy-client/delivery"
import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config"
import { executeDispatchTool } from "@legion/envoy-client/dispatch-execute"
import {
  dispatchSubscriptionTopic,
  subscriptionRemovedTopics,
} from "@legion/envoy-client/dispatch-subscribe"
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
} from "@legion/envoy-client/transport"
import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { connect } from "nats"
import { z } from "zod"
import {
  type ChannelForwarder,
  type ChannelForwarderConnection,
  type ChannelInboundMessage,
  createChannelForwarder,
} from "./channel-forwarder"
import { claudeProjectDirectory, claudeSessionId } from "./claude-session"

const CHANNEL_NOTIFICATION_METHOD = "notifications/claude/channel" as const
const CHANNEL_INBOX_LIMIT = 50
const EMPTY_RECEIPT = new Uint8Array()
const ChannelMetaKey = /^[A-Za-z_][A-Za-z0-9_]*$/
const PersistedRole = z.object({ session_id: z.string().min(1), role: z.string().min(1) })

export interface ChannelNotification {
  readonly method: typeof CHANNEL_NOTIFICATION_METHOD
  readonly params: {
    readonly content: string
    readonly meta: Readonly<Record<string, string>>
  }
}

export interface ChannelNotifier {
  notification(notification: ChannelNotification): Promise<void>
}

export interface ChannelInboxEntry {
  readonly event_id: string
  readonly at: string
  readonly from: string
  readonly summary: string
}

export interface ChannelDelivery {
  /** Queue one rendered envelope onto the MCP stdio transport. */
  enqueue(input: { readonly subject: string; readonly raw: string }): Promise<void>
  /** Latest inbound metadata, most recent first; no envelope body is retained. */
  inbox(): readonly ChannelInboxEntry[]
}

export interface ChannelSession {
  readonly delivery: ChannelDelivery
  /** Follow an Envoy topic and refresh its registry interest after NATS is listening. */
  follow(topics: readonly string[]): Promise<void>
  /** Stop following user-selected topics while retaining the direct session route. */
  unfollow(topics: readonly string[]): Promise<readonly string[]>
  /** Persist the role held by an explicit envoy_role_set call. */
  rememberRole(role: string): Promise<void>
  /** Drain NATS, deregister this listener route, and release timer resources. */
  shutdown(): Promise<void>
}

export interface ChannelSessionOptions {
  readonly sessionId: string
  readonly directory: string
  readonly connection: ChannelForwarderConnection
  readonly notifier: ChannelNotifier
  readonly client: Pick<
    EnvoyClient,
    "subscribe" | "unsubscribe" | "unregisterSession" | "setRole" | "getRole"
  >
  readonly heartbeatMs: number
  readonly roleStateFile: string
}

// The shared tool contract builds schemas from each host's Zod API. Claude's
// server emits JSON Schema with its own Zod but keeps validation canonical.
function argumentsSchema(spec: ToolSpec): z.ZodObject<z.ZodRawShape> {
  return z.object(spec.arguments(zodSchemaApi(z)) as z.ZodRawShape)
}

function parseArguments<Operation extends EnvoyToolOperation>(
  spec: ToolSpec & { readonly operation: Operation },
  input: unknown,
): ToolArgumentsByOperation[Operation] {
  return argumentsSchema(spec).parse(input) as ToolArgumentsByOperation[Operation]
}

function mcpResult(value: unknown): {
  readonly content: readonly [{ readonly type: "text"; readonly text: string }]
} {
  return { content: [{ type: "text", text: JSON.stringify(value) }] }
}

function projectDirectory(): string {
  return claudeProjectDirectory(
    { CLAUDE_PROJECT_DIR: process.env["CLAUDE_PROJECT_DIR"] },
    process.cwd(),
  )
}

function currentDispatchConfig(directory = projectDirectory()) {
  return resolveDispatchConfig(process.env, { cwd: directory })
}

export function channelToolDefinitions(dispatchEnabled: boolean) {
  return [
    ...envoyToolSpecs.map((spec) => ({
      name: spec.name,
      description: spec.description,
      inputSchema: z.toJSONSchema(argumentsSchema(spec)),
    })),
    ...(dispatchEnabled
      ? dispatchToolSpecs.map((spec) => ({
          name: spec.name,
          description: spec.description,
          inputSchema: z.toJSONSchema(dispatchToolSchema(spec, zodSchemaApi(z))),
        }))
      : []),
  ]
}

/** The current process's MCP declarations; Dispatch availability is set at server startup. */
export const envoyChannelToolDefinitions = channelToolDefinitions(currentDispatchConfig().enabled)

class UnsupportedEnvoyToolError extends Error {
  readonly name = "UnsupportedEnvoyToolError"

  constructor(readonly toolName: string) {
    super(`Unsupported Envoy tool: ${toolName}`)
  }
}

/** Claude Code silently drops invalid keys, so remove them before writing a notification. */
export function sanitizeChannelMetadata(
  metadata: Readonly<Record<string, string | undefined>>,
): Record<string, string> {
  const sanitized: Record<string, string> = {}
  for (const [key, value] of Object.entries(metadata)) {
    if (value !== undefined && ChannelMetaKey.test(key)) sanitized[key] = value
  }
  return sanitized
}

/**
 * Render inbound Envoy envelopes through the shared renderer and serialize MCP
 * writes so a receipt can be returned as soon as the notification is queued.
 */
export function createChannelDelivery(input: {
  readonly sessionId: string
  readonly notifier: ChannelNotifier
}): ChannelDelivery {
  const inbox: ChannelInboxEntry[] = []
  let tail = Promise.resolve()

  return {
    enqueue({ subject, raw }) {
      const rendered = renderInbound(raw, input.sessionId, subject)
      if (rendered.skip) return tail
      const envelope = rendered.envelope
      if (envelope !== undefined) {
        inbox.unshift({
          event_id: envelope.event_id ?? "unknown",
          at: inboundTimestamp(envelope.issued_at),
          from: envelope.source_session ?? envelope.source,
          summary: envelope.payload_summary ?? "unknown",
        })
        if (inbox.length > CHANNEL_INBOX_LIMIT) inbox.pop()
      }
      const notification: ChannelNotification = {
        method: CHANNEL_NOTIFICATION_METHOD,
        params: {
          content: rendered.content,
          meta: sanitizeChannelMetadata({
            source: envelope?.source ?? "unknown",
            topic: subject,
            event_id: envelope?.event_id,
            dedupe_key: envelope?.dedupe_key,
            urgency: envelope?.urgency,
            from_session: envelope?.source_session,
            expects_reply: envelope?.expects_reply,
            in_reply_to: envelope?.in_reply_to,
          }),
        },
      }
      const queued = tail.then(() => input.notifier.notification(notification))
      tail = queued.catch((error: unknown) => {
        process.stderr.write(`envoy-channel: channel notification failed — ${messageFor(error)}\n`)
      })
      return queued
    },
    inbox() {
      return [...inbox]
    },
  }
}

async function readPersistedRole(
  roleStateFile: string,
): Promise<z.infer<typeof PersistedRole> | undefined> {
  try {
    return PersistedRole.parse(JSON.parse(await readFile(roleStateFile, "utf8")))
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") return undefined
    throw error
  }
}

async function writePersistedRole(
  roleStateFile: string,
  state: z.infer<typeof PersistedRole>,
): Promise<void> {
  await mkdir(dirname(roleStateFile), { recursive: true, mode: 0o700 })
  const temporary = `${roleStateFile}.${process.pid}.${crypto.randomUUID()}.tmp`
  await writeFile(temporary, `${JSON.stringify(state)}\n`, { mode: 0o600 })
  await rename(temporary, roleStateFile)
}

async function clearPersistedRole(roleStateFile: string): Promise<void> {
  await rm(roleStateFile, { force: true })
}

function isNotFound(error: unknown): boolean {
  return (
    error instanceof Error &&
    "details" in error &&
    typeof error.details === "object" &&
    error.details !== null &&
    "status" in error.details &&
    error.details.status === 404
  )
}

/**
 * Begin one real NATS consumer before advertising the route to Envoy. The
 * channel server owns direct and followed subjects, so no Monitor or relay is
 * needed between NATS and Claude Code.
 */
/**
 * Queues a channel event before receipt. The receipt proves only this local
 * queue accepted a direct Envoy request; Claude Code does not acknowledge it.
 */
export async function enqueueChannelMessage(
  delivery: ChannelDelivery,
  connection: ChannelForwarderConnection,
  directSubject: string,
  message: ChannelInboundMessage,
): Promise<void> {
  const queued = message.duplicate
    ? Promise.resolve()
    : delivery.enqueue({
        subject: message.subject,
        raw: new TextDecoder().decode(message.data),
      })
  if (message.subject === directSubject && message.reply !== undefined) {
    connection.publish(message.reply, EMPTY_RECEIPT)
  }
  await queued
}

export async function startChannelSession(options: ChannelSessionOptions): Promise<ChannelSession> {
  const directSubject = agentSubject(options.sessionId)
  const delivery = createChannelDelivery({
    sessionId: options.sessionId,
    notifier: options.notifier,
  })
  const userTopics = new Set<string>()
  let heldRole: string | undefined
  let shuttingDown = false

  const forwarder: ChannelForwarder = createChannelForwarder(options.connection, {
    deliver: async (message) => {
      const raw = new TextDecoder().decode(message.data)
      const removedTopics = subscriptionRemovedTopics(raw, options.sessionId)
      if (removedTopics !== undefined) {
        for (const topic of removedTopics) userTopics.delete(topic)
        void forwarder.unfollow(removedTopics).catch((error: unknown) => {
          process.stderr.write(
            `envoy-channel: could not drop a removed subscription — ${messageFor(error)}\n`,
          )
        })
      }
      await enqueueChannelMessage(delivery, options.connection, directSubject, message)
    },
  })

  const register = async (): Promise<void> => {
    await options.client.subscribe({
      sessionID: options.sessionId,
      directory: options.directory,
      topics: forwarder.topics(),
      port: 0,
      title: "",
      capabilities: ["aside"],
      driving: false,
      selfSubscribed: true,
    })
  }

  const restoreRole = async (): Promise<void> => {
    const persisted = await readPersistedRole(options.roleStateFile)
    if (persisted === undefined) return
    const claim = await options.client.setRole({
      sessionID: options.sessionId,
      role: persisted.role,
      soft: true,
      previousSessionID: persisted.session_id,
    })
    if (!claim.claimed) {
      await clearPersistedRole(options.roleStateFile)
      return
    }
    heldRole = persisted.role
    await writePersistedRole(options.roleStateFile, {
      session_id: options.sessionId,
      role: persisted.role,
    })
  }

  const reassertRole = async (): Promise<void> => {
    if (heldRole === undefined) return
    const holder = await options.client.getRole(heldRole).then(
      (role) => role.holder,
      (error: unknown) => {
        if (isNotFound(error)) return undefined
        throw error
      },
    )
    if (holder === options.sessionId) return
    const result = await options.client.setRole({
      sessionID: options.sessionId,
      role: heldRole,
      soft: true,
    })
    if (!result.claimed) {
      heldRole = undefined
      await clearPersistedRole(options.roleStateFile)
    }
  }

  let outageReported = false
  const heartbeat = async (): Promise<void> => {
    if (shuttingDown) return
    try {
      await register()
      outageReported = false
      await reassertRole()
    } catch (error) {
      if (outageReported) return
      outageReported = true
      process.stderr.write(
        `envoy-channel: registry heartbeat failed (${messageFor(error)}); retrying every heartbeat\n`,
      )
    }
  }

  // The NATS subscription precedes registration so the listener never routes
  // a direct delivery to an advertised but deaf channel process.
  forwarder.follow(directSubject)
  await options.connection.flush()
  await register()
  await restoreRole()
  const heartbeatTimer = setInterval(() => {
    void heartbeat()
  }, options.heartbeatMs)

  return {
    delivery,
    async follow(topics) {
      const expanded = expandSubscriptionTopics(topics)
      for (const topic of expanded) {
        if (topic === directSubject) continue
        userTopics.add(topic)
        forwarder.follow(topic)
      }
      await options.connection.flush()
      await register()
    },
    async unfollow(topics) {
      const requested = topics.length === 0 ? [...userTopics] : expandSubscriptionTopics(topics)
      const removed = requested.filter((topic) => userTopics.delete(topic))
      if (removed.length === 0) return []
      await options.client.unsubscribe({ sessionID: options.sessionId, topics: removed })
      await forwarder.unfollow(removed)
      return removed
    },
    async rememberRole(role) {
      heldRole = role
      await writePersistedRole(options.roleStateFile, { session_id: options.sessionId, role })
    },
    async shutdown() {
      if (shuttingDown) return
      shuttingDown = true
      clearInterval(heartbeatTimer)
      await forwarder.close()
      await options.client.unregisterSession(options.sessionId).catch((error: unknown) => {
        process.stderr.write(
          `envoy-channel: session deregistration failed — ${messageFor(error)}\n`,
        )
      })
    },
  }
}

export interface ChannelToolRuntime {
  readonly sessionId: string
  readonly directory: string
  readonly client: EnvoyClient
  readonly session: ChannelSession
}
export async function executeEnvoyTool(
  runtime: ChannelToolRuntime,
  name: string,
  input: unknown,
): Promise<unknown> {
  const dispatchSpec = dispatchToolSpecs.find((candidate) => candidate.name === name)
  if (dispatchSpec !== undefined) {
    const config = currentDispatchConfig(runtime.directory)
    if (!config.enabled) throw new UnsupportedEnvoyToolError(name)
    const result = await executeDispatchTool({
      tool: dispatchSpec.name,
      args: input as Record<string, unknown>,
      cwd: runtime.directory,
      host: "claude",
      sessionId: runtime.sessionId,
      config,
      env: process.env,
    })
    const topic = dispatchSubscriptionTopic(result.details)
    if (topic !== null) {
      try {
        await runtime.session.follow([topic])
      } catch (error) {
        const issue =
          typeof result.details["issue"] === "string" ? result.details["issue"] : "the issue"
        process.stderr.write(
          `envoy-channel: ${name} completed for ${issue} but subscribing ${runtime.sessionId} to ${topic} failed — ${messageFor(error)}\n`,
        )
      }
    }
    return result
  }

  const spec = envoyToolSpecs.find((candidate) => candidate.name === name)
  if (spec === undefined) throw new UnsupportedEnvoyToolError(name)
  switch (spec.operation) {
    case EnvoyToolOperation.send: {
      const args = parseArguments(spec, input)
      const result = await runtime.client.send({
        source: "agent",
        sourceSessionID: runtime.sessionId,
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
      return runtime.client.publish({
        source: "agent",
        sourceSessionID: runtime.sessionId,
        topic: args.topic,
        message: args.message,
        ...toMessageMetadata(args),
      })
    }
    case EnvoyToolOperation.subscribe: {
      const args = parseArguments(spec, input)
      await runtime.session.follow(args.topics)
      return runtime.client.getInterest(runtime.sessionId)
    }
    case EnvoyToolOperation.unsubscribe: {
      const args = parseArguments(spec, input)
      return { removed: await runtime.session.unfollow(args.topics ?? []) }
    }
    case EnvoyToolOperation.listInterests:
      parseArguments(spec, input)
      return runtime.client.getInterest(runtime.sessionId)
    case EnvoyToolOperation.whoami:
      parseArguments(spec, input)
      return { session_id: runtime.sessionId, machine_id: machineID(), dir: runtime.directory }
    case EnvoyToolOperation.listSessions: {
      const args = parseArguments(spec, input)
      const sessions = await runtime.client.listSessions({
        ...(args.dir === undefined ? {} : { directory: args.dir }),
        ...(args.title === undefined ? {} : { title: args.title }),
      })
      return args.machine
        ? sessions.filter((session) => session.machine_id === args.machine)
        : sessions
    }
    case EnvoyToolOperation.setRole: {
      const args = parseArguments(spec, input)
      const result = await runtime.client.setRole({ sessionID: runtime.sessionId, role: args.role })
      if (!result.claimed) throw new Error(`role ${args.role} is held by ${result.holder}`)
      await runtime.session.rememberRole(args.role)
      return result.interest
    }
    case EnvoyToolOperation.getRole: {
      const args = parseArguments(spec, input)
      return runtime.client.getRole(args.role)
    }
    case EnvoyToolOperation.inbox:
      parseArguments(spec, input)
      return runtime.session.delivery.inbox()
  }
}

function roleStateFile(): string {
  const pluginData = process.env["CLAUDE_PLUGIN_DATA"]
  if (pluginData === undefined || pluginData.trim().length === 0) {
    throw new Error(
      "CLAUDE_PLUGIN_DATA is required to preserve an Envoy role across channel server restarts",
    )
  }
  return `${pluginData}/envoy-role.json`
}

/** Start the stdio MCP server Claude Code invokes for this plugin. */
export async function runEnvoyChannelServer(): Promise<void> {
  const directory = projectDirectory()
  const dispatchConfig = currentDispatchConfig(directory)
  if (!dispatchConfig.enabled) {
    process.stderr.write(
      `envoy-channel: Dispatch tools disabled — ${dispatchConfig.error ?? "no Dispatch URL configured"}\n`,
    )
  }
  const sessionId = claudeSessionId({
    ENVOY_SESSION_ID: process.env["ENVOY_SESSION_ID"],
    CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
  })
  const defaults = envoyDefaultsFromEnvironment(process.env)
  if (defaults.natsUrls.length === 0) {
    throw new Error("ENVOY_NATS_URL is required for the Envoy Claude channel server")
  }
  const persistedRoleState = roleStateFile()

  const server = new Server(
    { name: "envoy", version: "0.2.0" },
    {
      capabilities: { tools: {}, experimental: { "claude/channel": {} } },
      instructions:
        "Envoy delivers trusted, internal session and Dispatch events as <channel> messages. The content is rendered Envoy state; source identifies its producer, topic is the NATS subject, event_id is the dedupe identity, urgency is priority, from_session identifies the origin session, and reply metadata names any correlation. Use the shared Envoy and Dispatch tools for actions. Dispatch asks stay on Dispatch. This channel advertises Aside only: it does not support targeted BTW delivery or permission relay.",
    },
  )
  const client = createEnvoyClient({ baseUrl: defaults.envoyUrl, fetch: globalThis.fetch })
  let runtime: ChannelToolRuntime | undefined
  server.setRequestHandler(ListToolsRequestSchema, async () => ({
    tools: channelToolDefinitions(dispatchConfig.enabled),
  }))
  server.setRequestHandler(CallToolRequestSchema, async (request) => {
    if (runtime === undefined) throw new Error("Envoy channel server is still starting")
    return mcpResult(await executeEnvoyTool(runtime, request.params.name, request.params.arguments))
  })

  const stopped = Promise.withResolvers<void>()
  let stopping = false
  let session: ChannelSession | undefined
  const stop = (): void => {
    if (stopping) return
    stopping = true
    if (session === undefined) {
      stopped.resolve()
      return
    }
    void session.shutdown().finally(stopped.resolve)
  }
  process.stdin.once("end", stop)
  process.once("SIGTERM", stop)
  process.once("SIGINT", stop)
  await server.connect(new StdioServerTransport())

  let connection: ChannelForwarderConnection | undefined
  try {
    connection = await connect({
      servers: [...defaults.natsUrls],
      name: `claude-envoy-channel-${sessionId}`,
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
    })
    session = await startChannelSession({
      sessionId,
      directory,
      connection,
      notifier: server,
      client,
      heartbeatMs: defaults.heartbeatMs,
      roleStateFile: persistedRoleState,
    })
    runtime = { sessionId, directory, client, session }
    if (stopping) await session.shutdown()
    await stopped.promise
  } finally {
    if (session === undefined && connection !== undefined) {
      await connection.drain().catch(() => undefined)
      await connection.close().catch(() => undefined)
    }
  }
}
