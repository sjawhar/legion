import { mkdir, readFile, rename, rm, writeFile } from "node:fs/promises"
import { dirname } from "node:path"
import {
  agentSubject,
  dispatchToolSchema,
  dispatchToolSpecs,
  ROLE_TOPIC_PREFIX,
  zodSchemaApi,
} from "@legion/contracts"
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults"
import {
  type DispatchDelivery,
  inboundTimestamp,
  renderInbound,
} from "@legion/envoy-client/delivery"
import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config"
import { executeDispatchTool } from "@legion/envoy-client/dispatch-execute"
import {
  dispatchSubscriptionTopic,
  dispatchTopicLabel,
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
import { version as packageVersion } from "../package.json" with { type: "json" }
import {
  type ChannelForwarder,
  type ChannelForwarderConnection,
  type ChannelInboundMessage,
  createChannelForwarder,
} from "./channel-forwarder"
import { claudeProjectDirectory, claudeSessionId } from "./claude-session"
import {
  pruneStaleSessionHandoffs,
  readSessionHandoff,
  roleStateFile,
  SessionIdentity,
  sessionHandoffFile,
} from "./session-identity"

const CHANNEL_NOTIFICATION_METHOD = "notifications/claude/channel" as const
const CHANNEL_INBOX_LIMIT = 50
const EMPTY_RECEIPT = new Uint8Array()
const ChannelMetaKey = /^[A-Za-z_][A-Za-z0-9_]*$/
const PersistedRole = z.object({ session_id: z.string().min(1), role: z.string().min(1) })

/** The MCP `serverInfo`; the version is the package's, so it is spelled once. */
export const MCP_SERVER_INFO = { name: "envoy", version: packageVersion } as const

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
  /** Queue a plain Envoy notice (no envelope) for the model, in order with envelopes. */
  announce(content: string): Promise<void>
  /** Latest inbound metadata, most recent first; no envelope body is retained. */
  inbox(): readonly ChannelInboxEntry[]
}

export interface ChannelSession {
  readonly delivery: ChannelDelivery
  /** Every NATS subject this process is consuming right now, direct route included. */
  topics(): readonly string[]
  /** Follow Envoy topics and refresh the registry interest; returns the topics that were new. */
  follow(topics: readonly string[]): Promise<readonly string[]>
  /** Stop following user-selected topics while retaining the direct session route. */
  unfollow(topics: readonly string[]): Promise<readonly string[]>
  /** Persist the role held by an explicit envoy_role_set call. */
  rememberRole(role: string): Promise<void>
  /** Drain NATS, deregister this listener route, and release timer resources. */
  shutdown(): Promise<void>
}

export interface ChannelSessionOptions {
  readonly identity: SessionIdentity
  readonly connection: ChannelForwarderConnection
  readonly notifier: ChannelNotifier
  readonly client: Pick<
    EnvoyClient,
    "subscribe" | "unsubscribe" | "unregisterSession" | "setRole" | "getRole" | "getInterest"
  >
  readonly heartbeatMs: number
  /** `${CLAUDE_PLUGIN_DATA}`: role state per session id and the per-process handoff files. */
  readonly stateDirectory: string
  /**
   * The `claude` process this server belongs to. When set, each heartbeat reads that
   * process's handoff file and rebinds to the id it names; unset under the QA override.
   */
  readonly handoffPid?: number
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
 * Answer a targeted Dispatch delivery this session could not honour, so the
 * sender sees the attempt fail instead of waiting on a reply that never comes.
 */
async function postDispatchReply(
  identity: SessionIdentity,
  delivery: DispatchDelivery,
  result: { readonly error: string },
): Promise<void> {
  const config = currentDispatchConfig(identity.directory)
  if (!config.enabled || config.url === null) {
    throw new Error(config.error ?? "Dispatch is not configured")
  }
  const response = await fetch(`${config.url}/api/v1/messages/${delivery.messageID}/reply`, {
    method: "POST",
    headers: { Authorization: `Bearer ${config.token}`, "Content-Type": "application/json" },
    body: JSON.stringify({
      actor: { kind: "session", id: identity.id },
      attempt: delivery.attempt,
      ...result,
    }),
  })
  if (!response.ok) {
    throw new Error(`Dispatch reply failed: ${response.status} ${await response.text()}`)
  }
}

/**
 * Render inbound Envoy envelopes through the shared renderer and serialize MCP
 * writes so a receipt can be returned as soon as the notification is queued.
 */
export function createChannelDelivery(input: {
  readonly identity: SessionIdentity
  readonly notifier: ChannelNotifier
}): ChannelDelivery {
  const inbox: ChannelInboxEntry[] = []
  let tail = Promise.resolve()

  const queue = (notification: ChannelNotification): Promise<void> => {
    const queued = tail.then(() => input.notifier.notification(notification))
    tail = queued.catch((error: unknown) => {
      process.stderr.write(`envoy-channel: channel notification failed — ${messageFor(error)}\n`)
    })
    return queued
  }

  return {
    enqueue({ subject, raw }) {
      const rendered = renderInbound(raw, input.identity.id, subject)
      if (rendered.skip) return tail
      // A targeted Dispatch delivery the shared renderer could not validate is
      // never shown to the model: it is answered on Dispatch when it names a
      // message, and dropped with a log line when it does not.
      if (rendered.rejectedDelivery !== undefined) {
        const rejected = rendered.rejectedDelivery
        process.stderr.write(
          `envoy-channel: rejecting malformed Dispatch targeted delivery ${rejected.messageID}\n`,
        )
        return postDispatchReply(input.identity, rejected, {
          error: "Invalid Dispatch targeted delivery frame",
        }).catch((error: unknown) => {
          process.stderr.write(
            `envoy-channel: could not report the rejected delivery to Dispatch — ${messageFor(error)}\n`,
          )
        })
      }
      if (rendered.malformedDelivery === true) {
        process.stderr.write(
          "envoy-channel: dropping malformed Dispatch targeted delivery without a reply address\n",
        )
        return tail
      }
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
      return queue({
        method: CHANNEL_NOTIFICATION_METHOD,
        params: {
          content: rendered.content,
          // Claude Code stamps its own `source="<channel name>"` attribute on the
          // <channel> tag, so the Envoy producer travels under `producer`.
          meta: sanitizeChannelMetadata({
            producer: envelope?.source ?? "unknown",
            topic: subject,
            event_id: envelope?.event_id,
            dedupe_key: envelope?.dedupe_key,
            urgency: envelope?.urgency,
            from_session: envelope?.source_session,
            expects_reply: envelope?.expects_reply,
            in_reply_to: envelope?.in_reply_to,
          }),
        },
      })
    },
    announce(content) {
      return queue({
        method: CHANNEL_NOTIFICATION_METHOD,
        params: { content, meta: { producer: "envoy" } },
      })
    },
    inbox() {
      return [...inbox]
    },
  }
}

async function readPersistedRole(
  roleFile: string,
): Promise<z.infer<typeof PersistedRole> | undefined> {
  try {
    return PersistedRole.parse(JSON.parse(await readFile(roleFile, "utf8")))
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") return undefined
    throw error
  }
}

async function writePersistedRole(
  roleFile: string,
  state: z.infer<typeof PersistedRole>,
): Promise<void> {
  await mkdir(dirname(roleFile), { recursive: true, mode: 0o700 })
  const temporary = `${roleFile}.${process.pid}.${crypto.randomUUID()}.tmp`
  await writeFile(temporary, `${JSON.stringify(state)}\n`, { mode: 0o600 })
  await rename(temporary, roleFile)
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
 * Queues a channel event, then answers a forwarded lane's receipt. The receipt
 * proves only this local queue accepted the request; Claude Code does not
 * acknowledge it. Only the listener's forwarded lanes (an envelope that still
 * names its role topic while arriving on the direct subject) wait for one: a
 * plain direct send is a JetStream publish whose reply subject is the
 * publisher's acknowledgement inbox, and an empty receipt there fails that
 * publish (`invalid jetstream publish response`) after the event was delivered.
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
  if (
    message.subject === directSubject &&
    message.reply !== undefined &&
    message.envelopeTopic !== undefined &&
    message.envelopeTopic !== directSubject
  ) {
    connection.publish(message.reply, EMPTY_RECEIPT)
  }
  await queued
}

/**
 * Begin one real NATS consumer before advertising the route to Envoy. The
 * channel server owns direct and followed subjects, so no Monitor or relay is
 * needed between NATS and Claude Code.
 */
export async function startChannelSession(options: ChannelSessionOptions): Promise<ChannelSession> {
  const { identity } = options
  let directSubject = agentSubject(identity.id)
  const delivery = createChannelDelivery({ identity, notifier: options.notifier })
  const userTopics = new Set<string>()
  let heldRole: string | undefined
  let shuttingDown = false
  const handoffFile =
    options.handoffPid === undefined
      ? undefined
      : sessionHandoffFile(options.stateDirectory, options.handoffPid)

  const forwarder: ChannelForwarder = createChannelForwarder(options.connection, {
    deliver: async (message) => {
      const raw = new TextDecoder().decode(message.data)
      const removedTopics = subscriptionRemovedTopics(raw, identity.id)
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
      sessionID: identity.id,
      directory: identity.directory,
      topics: forwarder.topics(),
      port: 0,
      title: "",
      capabilities: ["aside"],
      driving: false,
      selfSubscribed: true,
    })
  }

  /**
   * Take back the role persisted for `previousSessionID` (this id on `--resume`,
   * the pre-`/clear` id on a handoff) with a soft claim naming that predecessor.
   */
  const restoreRole = async (previousSessionID = identity.id): Promise<void> => {
    const previousFile = roleStateFile(options.stateDirectory, previousSessionID)
    const persisted = await readPersistedRole(previousFile)
    if (persisted === undefined) return
    const claim = await options.client.setRole({
      sessionID: identity.id,
      role: persisted.role,
      soft: true,
      previousSessionID: persisted.session_id,
    })
    if (!claim.claimed) {
      await rm(previousFile, { force: true })
      return
    }
    heldRole = persisted.role
    const currentFile = roleStateFile(options.stateDirectory, identity.id)
    if (previousFile !== currentFile) await rm(previousFile, { force: true })
    await writePersistedRole(currentFile, { session_id: identity.id, role: persisted.role })
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
    if (holder === identity.id) return
    const result = await options.client.setRole({
      sessionID: identity.id,
      role: heldRole,
      soft: true,
    })
    if (!result.claimed) {
      heldRole = undefined
      await rm(roleStateFile(options.stateDirectory, identity.id), { force: true })
    }
  }

  /**
   * Claude Code mints a new session id on `/clear` while this process keeps the
   * one it was spawned with; the SessionStart hook writes the current id for our
   * shared parent process, and we move every binding to it. The new direct
   * subject is consumed before anything is advertised under the new id.
   */
  const adoptHandoff = async (): Promise<void> => {
    if (handoffFile === undefined) return
    const next = await readSessionHandoff(handoffFile)
    if (next === undefined || next === identity.id) return
    const previous = identity.id
    const previousSubject = directSubject
    const nextSubject = agentSubject(next)
    forwarder.follow(nextSubject)
    await options.connection.flush()
    await options.client.unregisterSession(previous)
    identity.set(next)
    directSubject = nextSubject
    await register()
    await forwarder.unfollow([previousSubject])
    await restoreRole(previous)
    process.stderr.write(`envoy: session id changed ${previous} -> ${next}; re-registered\n`)
  }

  let outageReported = false
  const heartbeat = async (): Promise<void> => {
    if (shuttingDown) return
    try {
      await adoptHandoff()
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

  /**
   * Registered interests deliver only through this process's own NATS
   * subscriptions, so a resumed or restarted server must rebuild the topics its
   * session id already registered or stay deaf to them (and would re-announce
   * the first Dispatch write on each). Quiet on failure: a brand-new id has no
   * registry entry, and a listener outage must not fail startup.
   */
  const recoverRegisteredInterests = async (): Promise<void> => {
    const registry = await options.client.getInterest(identity.id).catch(() => undefined)
    if (registry === undefined) return
    for (const topic of registry.topics) {
      if (topic === directSubject || topic.startsWith(ROLE_TOPIC_PREFIX)) continue
      userTopics.add(topic)
      forwarder.follow(topic)
    }
  }

  // The NATS subscription precedes registration so the listener never routes
  // a direct delivery to an advertised but deaf channel process.
  forwarder.follow(directSubject)
  await recoverRegisteredInterests()
  await options.connection.flush()
  await register()
  await restoreRole()
  await pruneStaleSessionHandoffs(options.stateDirectory)
  const heartbeatTimer = setInterval(() => {
    void heartbeat()
  }, options.heartbeatMs)

  return {
    delivery,
    topics: () => forwarder.topics(),
    async follow(topics) {
      const fresh: string[] = []
      for (const topic of expandSubscriptionTopics(topics)) {
        if (topic === directSubject || userTopics.has(topic)) continue
        userTopics.add(topic)
        forwarder.follow(topic)
        fresh.push(topic)
      }
      await options.connection.flush()
      await register()
      return fresh
    },
    async unfollow(topics) {
      const requested = topics.length === 0 ? [...userTopics] : expandSubscriptionTopics(topics)
      const removed = requested.filter((topic) => userTopics.delete(topic))
      if (removed.length === 0) return []
      await options.client.unsubscribe({ sessionID: identity.id, topics: removed })
      await forwarder.unfollow(removed)
      return removed
    },
    async rememberRole(role) {
      heldRole = role
      await writePersistedRole(roleStateFile(options.stateDirectory, identity.id), {
        session_id: identity.id,
        role,
      })
    },
    async shutdown() {
      if (shuttingDown) return
      shuttingDown = true
      clearInterval(heartbeatTimer)
      await forwarder.close()
      await options.client.unregisterSession(identity.id).catch((error: unknown) => {
        process.stderr.write(
          `envoy-channel: session deregistration failed — ${messageFor(error)}\n`,
        )
      })
      // The handoff directory stays: Claude Code restarts this server inside the
      // same `claude` process (plugin reload, changed config), and the hook does
      // not rewrite the file until the next SessionStart. Startup pruning removes
      // the directory once its Claude process is gone.
    },
  }
}

export interface ChannelToolRuntime {
  readonly identity: SessionIdentity
  readonly client: EnvoyClient
  readonly session: ChannelSession
}
export async function executeEnvoyTool(
  runtime: ChannelToolRuntime,
  name: string,
  input: unknown,
): Promise<unknown> {
  const { identity } = runtime
  const dispatchSpec = dispatchToolSpecs.find((candidate) => candidate.name === name)
  if (dispatchSpec !== undefined) {
    const config = currentDispatchConfig(identity.directory)
    if (!config.enabled) throw new UnsupportedEnvoyToolError(name)
    const result = await executeDispatchTool({
      tool: dispatchSpec.name,
      args: input as Record<string, unknown>,
      cwd: identity.directory,
      host: "claude",
      sessionId: identity.id,
      config,
      env: process.env,
    })
    const topic = dispatchSubscriptionTopic(result.details)
    if (topic !== null) {
      try {
        // A write must tell the agent it now gets every event on this issue;
        // an already-followed topic is not news and stays silent.
        const fresh = await runtime.session.follow([topic])
        if (fresh.length > 0) {
          await runtime.session.delivery.announce(
            `Subscribed to ${dispatchTopicLabel(topic)} (every event on this issue reaches you; envoy_unsubscribe ${topic} to stop).`,
          )
        }
      } catch (error) {
        const issue =
          typeof result.details["issue"] === "string" ? result.details["issue"] : "the issue"
        process.stderr.write(
          `envoy-channel: ${name} completed for ${issue} but subscribing ${identity.id} to ${topic} failed — ${messageFor(error)}\n`,
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
        sourceSessionID: identity.id,
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
        sourceSessionID: identity.id,
        topic: args.topic,
        message: args.message,
        ...toMessageMetadata(args),
      })
    }
    case EnvoyToolOperation.subscribe: {
      const args = parseArguments(spec, input)
      await runtime.session.follow(args.topics)
      return runtime.client.getInterest(identity.id)
    }
    case EnvoyToolOperation.unsubscribe: {
      const args = parseArguments(spec, input)
      return { removed: await runtime.session.unfollow(args.topics ?? []) }
    }
    case EnvoyToolOperation.listInterests: {
      parseArguments(spec, input)
      // Live NATS subscriptions and the listener's registry can disagree after
      // a reconnect or a human removal; report both, and which side knows.
      const registry = await runtime.client.getInterest(identity.id)
      const live = runtime.session.topics()
      const interests = new Map<string, "registry" | "live" | "both">()
      for (const topic of registry.topics) {
        interests.set(topic, live.includes(topic) ? "both" : "registry")
      }
      for (const topic of live) {
        if (!interests.has(topic)) interests.set(topic, "live")
      }
      return {
        ...registry,
        topics: [...interests.keys()],
        interests: [...interests].map(([topic, source]) => ({ topic, source })),
      }
    }
    case EnvoyToolOperation.whoami:
      parseArguments(spec, input)
      return { session_id: identity.id, machine_id: machineID(), dir: identity.directory }
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
      const result = await runtime.client.setRole({ sessionID: identity.id, role: args.role })
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

function pluginStateDirectory(): string {
  const pluginData = process.env["CLAUDE_PLUGIN_DATA"]
  if (pluginData === undefined || pluginData.trim().length === 0) {
    throw new Error(
      "CLAUDE_PLUGIN_DATA is required to preserve an Envoy role across channel server restarts",
    )
  }
  return pluginData
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
  const overrideSessionId = process.env["ENVOY_SESSION_ID"]
  const identity = new SessionIdentity(
    claudeSessionId({
      ENVOY_SESSION_ID: overrideSessionId,
      CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
    }),
    directory,
  )
  const defaults = envoyDefaultsFromEnvironment(process.env)
  if (defaults.natsUrls.length === 0) {
    throw new Error("ENVOY_NATS_URL is required for the Envoy Claude channel server")
  }
  const stateDirectory = pluginStateDirectory()

  const server = new Server(MCP_SERVER_INFO, {
    capabilities: { tools: {}, experimental: { "claude/channel": {} } },
    instructions:
      "Envoy delivers trusted, internal session and Dispatch events as <channel> messages. The content is rendered Envoy state; producer identifies its Envoy producer (source is the channel name), topic is the NATS subject, event_id is the dedupe identity, urgency is priority, from_session identifies the origin session, and reply metadata names any correlation. Use the shared Envoy and Dispatch tools for actions. Dispatch asks stay on Dispatch. This channel advertises Aside only: it does not support targeted BTW delivery or permission relay.",
  })
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
  // The server shares `claude`'s process group, so a closing terminal or
  // `tmux kill-session` hangs us up directly; deregister instead of dying.
  process.once("SIGHUP", stop)
  await server.connect(new StdioServerTransport())

  let connection: ChannelForwarderConnection | undefined
  try {
    connection = await connect({
      servers: [...defaults.natsUrls],
      name: `claude-envoy-channel-${identity.id}`,
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
    })
    session = await startChannelSession({
      identity,
      connection,
      notifier: server,
      client,
      heartbeatMs: defaults.heartbeatMs,
      stateDirectory,
      // The QA override names a fixed identity; only a Claude-minted id follows `/clear`.
      ...(overrideSessionId === undefined || overrideSessionId.trim().length === 0
        ? { handoffPid: process.ppid }
        : {}),
    })
    runtime = { identity, client, session }
    if (stopping) await session.shutdown()
    await stopped.promise
  } finally {
    if (session === undefined && connection !== undefined) {
      await connection.drain().catch(() => undefined)
      await connection.close().catch(() => undefined)
    }
    // Release stdin so the process can exit once the session is deregistered,
    // even when the parent has not closed its end yet.
    await server.close().catch(() => undefined)
  }
}
