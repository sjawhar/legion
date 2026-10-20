import { unwatchFile, watchFile } from "node:fs"
import { rm } from "node:fs/promises"
import {
  agentSubject,
  type DeliveryCapability,
  ROLE_TOPIC_PREFIX,
  zodSchemaApi,
} from "@legion/contracts"
import {
  type DispatchDelivery,
  expectsLaneReceipt,
  inboundTimestamp,
  postDeliveryReply,
  renderInbound,
} from "@legion/envoy-client/delivery"
import {
  sessionDirectory as dispatchSessionDirectory,
  writeSessionTitle,
} from "@legion/envoy-client/dispatch-session-state"
import { subscriptionRemovedTopics } from "@legion/envoy-client/dispatch-subscribe"
import { messageFor } from "@legion/envoy-client/errors"
import { machineID } from "@legion/envoy-client/machine"
import { natsAuthOptions } from "@legion/envoy-client/nats-auth"
import {
  type SessionConfigFile,
  sessionConfigFile,
  sessionDispatchSettings,
  sessionSettings,
} from "@legion/envoy-client/session-settings"
import { postSlackReply } from "@legion/envoy-client/slack-post"
import {
  EnvoyToolOperation,
  envoyToolSpecs,
  sendConfirmationText,
  type ToolArgumentsByOperation,
  type ToolSpec,
  toMessageMetadata,
} from "@legion/envoy-client/tool-contract"
import {
  createEnvoyClient,
  EnvoyApiError,
  type EnvoyClient,
  expandSubscriptionTopics,
  mergeInterestSources,
} from "@legion/envoy-client/transport"
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client"
import { findHandoffCommit } from "@legion/pi-shared/handoff-commit"
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
import { type ChannelGate, detectClaudeChannel } from "./claude-channel"
import { claudeProjectDirectory, claudeSessionId, configuredValue } from "./claude-session"
import { inboundOffChannelSession, serveEnvoyOff } from "./envoy-off"
import {
  endPane,
  type LegionClaim,
  legionPane,
  logLegion,
  startLegionClaim,
  stopPane,
} from "./legion-claim"
import { createInboxNotifier, INBOX_CAPABILITIES, INBOX_VARIABLE } from "./legion-inbox"
import {
  executeLegionTool,
  LEGION_TOOL_NAME,
  type LegionToolRuntime,
  legionToolDefinition,
} from "./legion-tool"
import {
  pruneStaleSessionHandoffs,
  readPersistedRole,
  readSessionHandoff,
  roleStateFile,
  SessionIdentity,
  sessionHandoffFile,
  writePersistedRole,
} from "./session-identity"
import { type SessionTitle, sessionTitle } from "./session-title"

const CHANNEL_NOTIFICATION_METHOD = "notifications/claude/channel" as const
const CHANNEL_INSTRUCTIONS =
  "Envoy delivers trusted, internal session and Dispatch events as <channel> messages. The content is rendered Envoy state; producer identifies its Envoy producer (source is the channel name), topic is the NATS subject, event_id identifies this delivery, dedupe_key is the dedupe identity (a repeat of one already delivered is not shown again), urgency is priority, from_session identifies the origin session, and reply metadata names any correlation. Use the Envoy tools for Envoy actions, and reach Dispatch through the `dispatch` command in Bash (`dispatch --help` lists its commands; the `dispatch` skill teaches them). Dispatch asks stay on Dispatch. This channel advertises Aside only: it does not support targeted BTW delivery or permission relay."
const CHANNEL_INBOX_LIMIT = 50
const EMPTY_RECEIPT = new Uint8Array()
const ChannelMetaKey = /^[A-Za-z_][A-Za-z0-9_]*$/
/** How often the handoff file is checked for a `/clear`. */
const HANDOFF_POLL_MS = 250
/** How long a handoff waits for NATS to confirm the new direct subject before retrying. */
const HANDOFF_FLUSH_TIMEOUT_MS = 10_000

/** `promise`, or a rejection naming `what` once `ms` pass. */
async function withTimeout<T>(promise: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  const expired = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} timed out after ${ms} ms`)), ms)
  })
  try {
    return await Promise.race([promise, expired])
  } finally {
    clearTimeout(timer)
  }
}

/**
 * A rejection handler for a listener read whose subject legitimately may not
 * exist yet: `undefined` on the listener's 404 — no such role, no such entry —
 * and a rethrow for everything else, so a transport failure is never read as an
 * absence.
 */
function undefinedOnNotFound(error: unknown): undefined {
  if (error instanceof EnvoyApiError && error.details.status === 404) return undefined
  throw error
}

/** The MCP `serverInfo`; the version is the package's, so it is spelled once. */
export const MCP_SERVER_INFO = { name: "envoy", version: packageVersion } as const

/**
 * The delivery modes a session whose deliveries are channel notifications advertises: Claude Code
 * queues a notification for the model's next turn, which is an aside and cannot be a steer.
 */
export const CHANNEL_CAPABILITIES = ["aside"] as const satisfies readonly DeliveryCapability[]

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

/** Why the heartbeat regained this session's role (`ChannelSession.onRoleRegained`). */
export type RoleRegainReason = "reclaimed" | "reregistered"
export type RoleRegainHook = (role: string, reason: RoleRegainReason) => Promise<void>

export interface ChannelInboxEntry {
  readonly event_id: string
  readonly at: string
  readonly from: string
  readonly summary: string
}

export interface ChannelDelivery {
  /**
   * Queue one rendered envelope onto the MCP stdio transport. Resolves whether it went to Claude
   * Code: `false` for a frame skipped, dropped, or answered on Dispatch with an error instead.
   */
  enqueue(input: { readonly subject: string; readonly raw: string }): Promise<boolean>
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
  /** Persist the role held by an explicit envoy_role_set call, or by the Legion claim. */
  rememberRole(role: string): Promise<void>
  /**
   * Sets the one hook the heartbeat runs after it re-establishes this session as its held role's
   * live holder: `"reclaimed"` when the listener had stopped naming this session and a soft claim
   * took the role back, `"reregistered"` on the first healthy tick after a failed registration,
   * when the listener may have answered "no holder" meanwhile. The Legion claim sends its daemon
   * ready again from it. The hook runs detached from the heartbeat; a rejection is logged.
   */
  onRoleRegained(hook: RoleRegainHook): void
  /** Drain NATS, deregister this listener route, and release timer resources. */
  shutdown(): Promise<void>
}

export interface ChannelSessionOptions {
  readonly identity: SessionIdentity
  readonly connection: ChannelForwarderConnection
  readonly notifier: ChannelNotifier
  /**
   * The delivery modes the session advertises to the listener, which Dispatch checks every
   * targeted delivery against: the ones `notifier` honours (`CHANNEL_CAPABILITIES`, or
   * `INBOX_CAPABILITIES` for the Legion bridge's inbox).
   */
  readonly capabilities: readonly DeliveryCapability[]
  readonly client: Pick<
    EnvoyClient,
    "subscribe" | "unsubscribe" | "unregisterSession" | "setRole" | "getRole" | "getInterest"
  >
  readonly heartbeatMs: number
  /**
   * The session's envoy.json (`sessionConfigFile`): a rejected Dispatch frame is answered with the
   * Dispatch URL and token read from it at that moment.
   */
  readonly configFile: SessionConfigFile
  /** How long a handoff waits for NATS to confirm the new direct subject before it retries. */
  readonly handoffFlushTimeoutMs?: number
  /** `${CLAUDE_PLUGIN_DATA}`: role state per session id and the per-process handoff files. */
  readonly stateDirectory: string
  /**
   * The `claude` process this server belongs to. When set, the server polls that
   * process's handoff file and rebinds to the id it names; unset under the QA override.
   */
  readonly handoffPid?: number
  /**
   * The title each registration sends for the current id (`sessionTitle`); omitted, the title is
   * empty and Dispatch names the session by its id.
   */
  readonly title?: SessionTitle
  /**
   * Writes the title where the `dispatch` command reads it to stamp the session's Dispatch writes
   * (`writeSessionTitle` into the session's Dispatch state directory), as pi-envoy does for an Oh
   * My Pi session; omitted, nothing is written.
   */
  readonly stampTitle?: (sessionId: string, title: string) => void
}

// The shared tool contract builds schemas from each host's Zod API. Claude's
// server emits JSON Schema with its own Zod but keeps validation canonical. The
// specs are frozen module constants, so each schema is built once per spec.
const argumentSchemas = new WeakMap<ToolSpec, z.ZodObject<z.ZodRawShape>>()
function argumentsSchema(spec: ToolSpec): z.ZodObject<z.ZodRawShape> {
  let schema = argumentSchemas.get(spec)
  if (schema === undefined) {
    schema = z.object(spec.arguments(zodSchemaApi(z)) as z.ZodRawShape)
    argumentSchemas.set(spec, schema)
  }
  return schema
}

function parseArguments<Operation extends EnvoyToolOperation>(
  spec: ToolSpec & { readonly operation: Operation },
  input: unknown,
): ToolArgumentsByOperation[Operation] {
  return argumentsSchema(spec).parse(input) as ToolArgumentsByOperation[Operation]
}

/** A tool's answer as MCP content: the value as one JSON text block. */
export function mcpResult(value: unknown): {
  readonly content: readonly { readonly type: "text"; readonly text: string }[]
} {
  return { content: [{ type: "text", text: JSON.stringify(value) }] }
}

function projectDirectory(): string {
  return claudeProjectDirectory(
    { CLAUDE_PROJECT_DIR: process.env["CLAUDE_PROJECT_DIR"] },
    process.cwd(),
  )
}

export function channelToolDefinitions() {
  return envoyToolSpecs.map((spec) => ({
    name: spec.name,
    description: spec.description,
    inputSchema: z.toJSONSchema(argumentsSchema(spec)),
  }))
}

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
  configFile: SessionConfigFile,
  delivery: DispatchDelivery,
  result: { readonly error: string },
): Promise<void> {
  const config = sessionDispatchSettings(configFile, process.env, {
    cwd: identity.directory,
  }).dispatch
  if (!config.enabled) throw new Error(config.error ?? config.notice)
  await postDeliveryReply(config, identity.id, delivery, result)
}

/**
 * Render inbound Envoy envelopes through the shared renderer and serialize MCP
 * writes so a receipt can be returned as soon as the notification is queued.
 */
export function createChannelDelivery(input: {
  readonly identity: SessionIdentity
  readonly notifier: ChannelNotifier
  /** The session's envoy.json, which a rejected Dispatch frame's reply reads. */
  readonly configFile: SessionConfigFile
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
      if (rendered.skip) return tail.then(() => false)
      // A targeted Dispatch delivery the shared renderer could not validate is
      // never shown to the model: it is answered on Dispatch when it names a
      // message, and dropped with a log line when it does not.
      if (rendered.rejectedDelivery !== undefined) {
        const rejected = rendered.rejectedDelivery
        process.stderr.write(
          `envoy-channel: rejecting malformed Dispatch targeted delivery ${rejected.id}\n`,
        )
        return postDispatchReply(input.identity, input.configFile, rejected, {
          error: "Invalid Dispatch targeted delivery frame",
        }).then(
          () => false,
          (error: unknown) => {
            process.stderr.write(
              `envoy-channel: could not report the rejected delivery to Dispatch — ${messageFor(error)}\n`,
            )
            return false
          },
        )
      }
      if (rendered.malformedDelivery === true) {
        process.stderr.write(
          "envoy-channel: dropping malformed Dispatch targeted delivery without a reply address\n",
        )
        return tail.then(() => false)
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
      const queued = queue({
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
      return queued.then(() => true)
    },
    inbox() {
      return [...inbox]
    },
  }
}

/**
 * Queues a channel event, then answers a forwarded lane's receipt, and resolves whether Claude
 * Code was handed the event. The receipt proves only this local queue accepted the request; Claude
 * Code does not acknowledge it. Which frames wait for one is `expectsLaneReceipt`'s rule.
 */
export async function enqueueChannelMessage(
  delivery: ChannelDelivery,
  connection: ChannelForwarderConnection,
  directSubject: string,
  message: ChannelInboundMessage,
): Promise<boolean> {
  const queued = message.duplicate
    ? Promise.resolve(false)
    : delivery.enqueue({ subject: message.subject, raw: message.raw })
  const lane = {
    subject: message.subject,
    directSubject,
    envelopeTopic: message.envelopeTopic,
    reply: message.reply,
  }
  if (expectsLaneReceipt(lane)) connection.publish(lane.reply, EMPTY_RECEIPT)
  return queued
}

/**
 * Begin one real NATS consumer before advertising the route to Envoy. The
 * channel server owns direct and followed subjects, so no Monitor or relay is
 * needed between NATS and Claude Code.
 */
export async function startChannelSession(options: ChannelSessionOptions): Promise<ChannelSession> {
  const { identity } = options
  let directSubject = agentSubject(identity.id)
  const delivery = createChannelDelivery({
    identity,
    notifier: options.notifier,
    configFile: options.configFile,
  })
  const userTopics = new Set<string>()
  let heldRole: string | undefined
  let shuttingDown = false
  const handoffFile =
    options.handoffPid === undefined
      ? undefined
      : sessionHandoffFile(options.stateDirectory, options.handoffPid)

  /**
   * Every registry write runs through this chain, one at a time. The listener
   * merges each registration's topics into the entry and never removes one, so a
   * registration sent after a topic's removal or after an id's deregistration
   * writes it back for good. Queued writes run in order, and a registration reads
   * the id and topics when it runs rather than when it was requested.
   */
  let registryWrites: Promise<unknown> = Promise.resolve()
  const serialized = <T>(write: () => Promise<T>): Promise<T> => {
    const result = registryWrites.then(() => write())
    registryWrites = result.catch(() => undefined)
    return result
  }

  const forwarder: ChannelForwarder = createChannelForwarder(options.connection, {
    deliver: async (message) => {
      const removedTopics = subscriptionRemovedTopics(message.raw, identity.id)
      // An empty list would mean "every topic" to both unfollow and the listener.
      if (removedTopics !== undefined && removedTopics.length > 0) {
        for (const topic of removedTopics) userTopics.delete(topic)
        const { dropping, unsubscribing } = dropFromForwarderAndRegistry(removedTopics)
        dropping.catch((error: unknown) => {
          process.stderr.write(
            `envoy-channel: could not drop a removed subscription — ${messageFor(error)}\n`,
          )
        })
        // Dispatch has already removed them from the entry, but a registration in
        // flight at that moment adds them back; removing them again behind it undoes
        // that. A failure here is not retried by name: the heartbeat's own interest
        // reconciliation (below) brings the entry back in line with what this session
        // actually follows, whatever caused the drift.
        unsubscribing.catch((error: unknown) => {
          process.stderr.write(
            `envoy-channel: could not drop a removed subscription from the registry — ${messageFor(error)}; the next heartbeat reconciles it\n`,
          )
        })
      }
      return enqueueChannelMessage(delivery, options.connection, directSubject, message)
    },
  })

  /**
   * Drops `topics` from the forwarder's subscriptions and the registry entry — the two
   * steps every removal here takes once a caller has already taken them out of
   * `userTopics`. `forwarder.unfollow` runs first and removes from `following`
   * before its first await, so no registration queued behind either promise
   * advertises a dropped topic again. Both callers here — `deliver`'s own
   * best-effort removal and the public `unfollow` below — take these same two
   * steps in this order; only whether they wait for them differs.
   */
  function dropFromForwarderAndRegistry(topics: readonly string[]): {
    readonly dropping: Promise<void>
    readonly unsubscribing: Promise<void>
  } {
    return {
      dropping: forwarder.unfollow(topics),
      unsubscribing: serialized(() =>
        options.client.unsubscribe({ sessionID: identity.id, topics }),
      ),
    }
  }

  /** The title last stamped, so a heartbeat stamps nothing new. */
  let stampedTitle: { readonly sessionID: string; readonly title: string } | undefined
  /** Whether a failure to stamp the title has been reported since the last success. */
  let stampFailureReported = false

  /**
   * Advertise the current id and topics; runs only inside `serialized`. The title is also stamped
   * for the `dispatch` command (`options.stampTitle`) whenever it or the id changes.
   */
  const register = async (): Promise<void> => {
    const title = options.title?.(identity.id) ?? ""
    if (
      options.stampTitle !== undefined &&
      title !== "" &&
      (stampedTitle?.sessionID !== identity.id || stampedTitle.title !== title)
    ) {
      try {
        options.stampTitle(identity.id, title)
        stampedTitle = { sessionID: identity.id, title }
        stampFailureReported = false
      } catch (error) {
        // Said once per failure run: every heartbeat retries it.
        if (!stampFailureReported) {
          stampFailureReported = true
          process.stderr.write(
            `envoy-channel: could not write the session title for the dispatch command (${messageFor(error)}); retrying every heartbeat, and the command names this session by its id\n`,
          )
        }
      }
    }
    await options.client.subscribe({
      sessionID: identity.id,
      directory: identity.directory,
      topics: forwarder.topics(),
      port: 0,
      title,
      capabilities: options.capabilities,
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

  let regained: RoleRegainHook | undefined
  /** Runs `regained` off the heartbeat's chain: it is a daemon round trip this side cannot bound,
   * and every registry write queued behind the tick would wait on it. */
  const fireRegained = (role: string, reason: RoleRegainReason): void => {
    const hook = regained
    if (hook === undefined) return
    void Promise.resolve()
      .then(() => hook(role, reason))
      .catch((error: unknown) => {
        process.stderr.write(
          `envoy-channel: role ${role} regain hook failed (${reason}) — ${messageFor(error)}\n`,
        )
      })
  }

  /** `afterOutage`: the last tick's registration failed, so a holder the listener still names
   * may have been answered "no holder" meanwhile. */
  const reassertRole = async (afterOutage: boolean): Promise<void> => {
    if (heldRole === undefined) return
    const role = heldRole
    const holder = await options.client
      .getRole(role)
      .then((info) => info.holder, undefinedOnNotFound)
    if (holder === identity.id) {
      if (afterOutage) fireRegained(role, "reregistered")
      return
    }
    const result = await options.client.setRole({
      sessionID: identity.id,
      role,
      soft: true,
    })
    if (!result.claimed) {
      heldRole = undefined
      await rm(roleStateFile(options.stateDirectory, identity.id), { force: true })
      return
    }
    fireRegained(role, "reclaimed")
  }

  /**
   * The id a handoff moved away from, until its role has been taken back; a
   * handoff whose registration failed leaves it for the next heartbeat.
   */
  let roleTransferFrom: string | undefined
  const transferRole = async (): Promise<void> => {
    if (roleTransferFrom === undefined) return
    await restoreRole(roleTransferFrom)
    roleTransferFrom = undefined
  }

  /**
   * Claude Code mints a new session id on `/clear` while this process keeps the
   * one it was spawned with; the SessionStart hook writes the current id for our
   * shared parent process, and we move every binding to it. The new direct
   * subject is consumed before anything is advertised under the new id, and the
   * old one leaves the topic list before the first registration under it. Runs
   * only inside `serialized`.
   */
  const adoptHandoff = async (): Promise<void> => {
    if (handoffFile === undefined) return
    const next = await readSessionHandoff(handoffFile)
    if (next === undefined || next === identity.id) return
    const previous = identity.id
    const previousSubject = directSubject
    const nextSubject = agentSubject(next)
    forwarder.follow(nextSubject)
    try {
      // Bounded: this runs inside the registry chain, and a flush on a stalled NATS
      // connection never settles, which would hold every later write and shutdown.
      await withTimeout(
        options.connection.flush(),
        options.handoffFlushTimeoutMs ?? HANDOFF_FLUSH_TIMEOUT_MS,
        "flushing NATS",
      )
      await options.client.unregisterSession(previous)
    } catch (error) {
      // The session stays on the old id until a later tick retries, so the new
      // subject leaves the topic list; otherwise the next registration would
      // advertise it under the old id.
      void forwarder.unfollow([nextSubject])
      throw error
    }
    identity.set(next)
    directSubject = nextSubject
    // `unfollow` removes the subject from the topic list before it first awaits.
    // What it then awaits is the drain of deliveries already in flight, and the
    // handoff does not wait for it: a stuck notification would otherwise hold
    // every registry write queued behind this one. The drain never rejects.
    void forwarder.unfollow([previousSubject])
    roleTransferFrom ??= previous
    await register()
    await transferRole()
    process.stderr.write(`envoy: session id changed ${previous} -> ${next}; re-registered\n`)
  }

  /** Set by the first read `syncRegisteredInterests` completes, never cleared. */
  let interestsRead = false

  /**
   * Brings the registry entry and this process's own NATS subscriptions into line
   * with each other. The first successful read adopts every topic the entry lists;
   * every later read subtracts every topic the entry lists that
   * `forwarder.topics()` no longer names.
   *
   * Subtracting is safe only while `forwarder.topics()` is authoritative for this
   * session, so both windows where it is not are checked after the read, together,
   * because both open while the read is in flight. `interestsRead` is false until
   * a read has succeeded, when the forwarder holds only the direct subject while
   * the entry legitimately holds more. `shuttingDown` is set as teardown begins,
   * and `forwarder.close()` empties the topic list in that same synchronous step,
   * so a shutdown landing mid-read would leave an emptied list looking like total
   * drift. Neither is read before the await: the early return at the top only
   * saves a round trip, and guards nothing.
   *
   * The drift filter names the direct subject even though `followed` covers it in
   * every state this can subtract in — it is followed before the first
   * registration, and `adoptHandoff` follows the new subject before assigning it.
   * That redundancy is deliberate, because it is not what the reasoning costs if
   * it is ever wrong again: an empty topic set makes the listener delete the whole
   * entry (`removeInterestTopics` calls `deleteInterest`), where a followed topic
   * slipping through loses one topic.
   *
   * A 404 completes the read: it is the genuinely-new-session case, and the
   * forwarder is already complete. Any other failure leaves `interestsRead` false
   * and the next heartbeat adopts again, so a permanent outage means the entry is
   * never reconciled. That is the correct direction, because nothing undoes a
   * wrong subtraction: `unregisterSession` deletes only the sessions row, so a
   * stripped entry stands until `Registry.Reap` and is what a `--resume` inside
   * that window reads back. The listener refuses the same inference from the other
   * side of the wire — `mergeForUpsert` (packages/envoy/internal/store/kv.go)
   * merges and never removes because a transient read once truncated a live
   * subscription out of durable state.
   *
   * Reading live state each heartbeat, rather than replaying a topic list a
   * failure was recorded against, is what keeps a topic re-followed since a failed
   * removal from being stripped again, and keeps one topic's registry outage from
   * blocking any other: there is no per-topic queue, only this session's own entry.
   *
   * Runs inside `serialized` from the heartbeat, so it never races a concurrent
   * registration; the startup call runs before the first one is queued.
   * Quiet only at startup, where the caller swallows a failed read; from the
   * heartbeat an outage is what the heartbeat's own report exists to surface.
   */
  const syncRegisteredInterests = async (): Promise<void> => {
    if (shuttingDown) return
    const registry = await options.client.getInterest(identity.id).catch(undefinedOnNotFound)
    if (shuttingDown) return
    if (!interestsRead) {
      for (const topic of registry?.topics ?? []) {
        if (topic === directSubject || topic.startsWith(ROLE_TOPIC_PREFIX)) continue
        userTopics.add(topic)
        forwarder.follow(topic)
      }
      interestsRead = true
      return
    }
    if (registry === undefined) return
    const followed = new Set(forwarder.topics())
    const drifted = registry.topics.filter(
      (topic) =>
        topic !== directSubject && !topic.startsWith(ROLE_TOPIC_PREFIX) && !followed.has(topic),
    )
    if (drifted.length === 0) return
    await options.client.unsubscribe({ sessionID: identity.id, topics: drifted })
  }

  // One tick at a time, and at most one pending behind it. `serialized` already
  // orders the registry writes across ticks; what these two flags buy is
  // coalescing — a tick requested while one runs (a handoff file written
  // mid-tick) runs once, straight after — and the single immediate retry a
  // failed tick asks for below.
  let heartbeatInFlight = false
  let heartbeatRequested = false
  let outageReported = false
  // Set only by a failed registration, never by a failed role check after it: the listener answers
  // "no holder" for a session whose registry entry lapsed, and only that warrants a
  // `"reregistered"` regain. Cleared once a whole tick has succeeded.
  let registrationFailed = false
  const heartbeat = async (): Promise<void> => {
    if (shuttingDown || heartbeatInFlight) return
    heartbeatInFlight = true
    try {
      await serialized(async () => {
        await adoptHandoff()
        try {
          await register()
        } catch (error) {
          registrationFailed = true
          throw error
        }
        await reassertRole(registrationFailed)
        await transferRole()
        await syncRegisteredInterests()
      })
      registrationFailed = false
      outageReported = false
    } catch (error) {
      if (outageReported) return
      outageReported = true
      process.stderr.write(
        `envoy-channel: registry heartbeat failed (${messageFor(error)}); retrying every heartbeat\n`,
      )
      // A handoff that unregistered the old id and then failed to register the new
      // one leaves the session unreachable under both until the next attempt: one
      // immediate retry, not the full heartbeat interval, closes that window. Bounded
      // to once per outage: a further failure here hits the `outageReported` guard
      // above before ever reaching this line.
      requestHeartbeat()
    } finally {
      heartbeatInFlight = false
      if (heartbeatRequested) {
        heartbeatRequested = false
        void heartbeat()
      }
    }
  }
  const requestHeartbeat = (): void => {
    if (heartbeatInFlight) heartbeatRequested = true
    else void heartbeat()
  }

  // The NATS subscription precedes registration so the listener never routes
  // a direct delivery to an advertised but deaf channel process.
  forwarder.follow(directSubject)
  // Best-effort: a listener outage must not fail startup. It leaves
  // `interestsRead` false, so the heartbeat retries the adopt and never
  // subtracts against a set this read could not populate.
  await syncRegisteredInterests().catch((error: unknown) => {
    process.stderr.write(
      `envoy-channel: could not read the registered interests (${messageFor(error)}); the next heartbeat retries\n`,
    )
  })
  await options.connection.flush()
  await serialized(async () => {
    await register()
    await restoreRole()
  })
  await pruneStaleSessionHandoffs(options.stateDirectory)
  const heartbeatTimer = setInterval(() => {
    void heartbeat()
  }, options.heartbeatMs)
  // A `/clear` is adopted when the SessionStart hook writes the handoff file, not
  // at the next heartbeat minutes later. The hook replaces the file by rename,
  // which a directory watch reports only as its temporary file, so the file is
  // polled by path; the first tick picks up a file written before polling began.
  if (handoffFile !== undefined) {
    watchFile(handoffFile, { interval: HANDOFF_POLL_MS }, requestHeartbeat)
    requestHeartbeat()
  }

  return {
    delivery,
    topics: () => forwarder.topics(),
    async follow(topics) {
      if (shuttingDown) return []
      const fresh: string[] = []
      for (const topic of expandSubscriptionTopics(topics)) {
        if (topic === directSubject || userTopics.has(topic)) continue
        userTopics.add(topic)
        forwarder.follow(topic)
        fresh.push(topic)
      }
      await options.connection.flush()
      await serialized(register)
      return fresh
    },
    async unfollow(topics) {
      const requested = topics.length === 0 ? [...userTopics] : expandSubscriptionTopics(topics)
      const removed = requested.filter((topic) => userTopics.delete(topic))
      if (removed.length === 0) return []
      const { dropping, unsubscribing } = dropFromForwarderAndRegistry(removed)
      await unsubscribing
      await dropping
      return removed
    },
    onRoleRegained(hook) {
      regained = hook
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
      if (handoffFile !== undefined) unwatchFile(handoffFile, requestHeartbeat)
      await forwarder.close()
      await serialized(() => options.client.unregisterSession(identity.id)).catch(
        (error: unknown) => {
          process.stderr.write(
            `envoy-channel: session deregistration failed — ${messageFor(error)}\n`,
          )
        },
      )
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
        message: sendConfirmationText(result),
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
      const registry = await runtime.client.getInterest(identity.id)
      const interests = mergeInterestSources(registry.topics, runtime.session.topics())
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
        ? sessions.filter((session) => (session.host ?? session.machine_id) === args.machine)
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
    case EnvoyToolOperation.slackPost: {
      const args = parseArguments(spec, input)
      const posted = await postSlackReply({
        channel: args.channel,
        threadTs: args.thread_ts,
        text: args.text,
      })
      return { channel: posted.channel, thread_ts: posted.threadTs, ts: posted.ts }
    }
  }
}

function pluginStateDirectory(): string {
  const pluginData = configuredValue(process.env["CLAUDE_PLUGIN_DATA"])
  if (pluginData === undefined) {
    throw new Error(
      "CLAUDE_PLUGIN_DATA is required to preserve an Envoy role across channel server restarts",
    )
  }
  return pluginData
}

/** Start the stdio MCP server Claude Code invokes for this plugin, on the session's envoy.json. With
 * Envoy off it serves no tool (`serveEnvoyOff`, `./envoy-off`). With Envoy on and inbound messages
 * off (`inbound.off`: no NATS server, or its seed refused), it serves the tools, which call the
 * listener, on an inbound-off session (`inboundOffChannelSession`, `./envoy-off`); the start notice
 * has said why on stderr.
 *
 * In a Legion pane (a boot token on its environment, `legionPane`) it also serves the `legion` tool
 * and boots the pane's claim (`./legion-claim`); a Legion pane whose Envoy is off, or takes no
 * inbound messages, is stopped, since its task could never reach it. When `legion claude-bridge`
 * names its inbox (`LEGION_CLAUDE_INBOX`), deliveries go there as user turns instead of channel
 * notifications (`./legion-inbox`). */
export async function runEnvoyChannelServer(): Promise<void> {
  const directory = projectDirectory()
  const pane = legionPane(process.env)
  // The session's envoy.json, by Claude Code's rule (`$CLAUDE_CONFIG_DIR/envoy.json` when that is
  // set), worked out once as the server starts; Dispatch's URL and token are re-read from it on each
  // use, so a file that moved its server takes effect without a restart.
  const configFile = sessionConfigFile("claude", process.env)
  const { dispatch, envoy, notice } = sessionSettings(configFile, process.env, { cwd: directory })
  // Claude Code logs the server's stderr: why a section is off, once.
  if (notice !== null) process.stderr.write(`envoy-channel: ${notice}\n`)
  const overrideSessionId = process.env["ENVOY_SESSION_ID"]
  // The title every registration sends, and the one the `dispatch` command stamps on this
  // session's Dispatch writes. Registration stamps it; with Envoy or its inbound messages off,
  // nothing registers, so those paths stamp it once themselves.
  const title = sessionTitle(
    {
      ENVOY_SESSION_TITLE: process.env["ENVOY_SESSION_TITLE"],
      TMUX: process.env["TMUX"],
      TMUX_PANE: process.env["TMUX_PANE"],
    },
    directory,
  )
  const stampTitle = (sessionId: string, value: string): void => {
    writeSessionTitle(dispatchSessionDirectory(process.env, sessionId), value)
  }

  // Only the `dispatch` command reads the stamp, so nothing is written while the session starts with
  // Dispatch off, or under the QA override ENVOY_SESSION_ID, whose id that command never reads.
  const stamping = dispatch.enabled && configuredValue(overrideSessionId) === undefined
  /** Stamp the title for the session id `resolveId` names, once, where nothing will register it. */
  const stampOnce = (resolveId: () => string): void => {
    if (!stamping) return
    try {
      const sessionId = resolveId()
      const own = title(sessionId)
      if (own !== "") stampTitle(sessionId, own)
    } catch (error) {
      process.stderr.write(
        `envoy-channel: could not write the session title for the dispatch command — ${messageFor(error)}; the dispatch command's writes carry no session title\n`,
      )
    }
  }
  if (!envoy.enabled) {
    // The `dispatch` command still runs with Envoy off, so it still gets the session's title.
    stampOnce(() =>
      claudeSessionId({
        ENVOY_SESSION_ID: overrideSessionId,
        CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
      }),
    )
    const refusal = `Envoy is off for this session: ${envoy.error ?? envoy.notice}`
    if (pane !== undefined) stopPane(`a Legion pane needs Envoy for its task; ${refusal}`)
    await serveEnvoyOff(MCP_SERVER_INFO, refusal)
    return
  }
  const identity = new SessionIdentity(
    claudeSessionId({
      ENVOY_SESSION_ID: overrideSessionId,
      CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
    }),
    directory,
  )
  const stateDirectory = pluginStateDirectory()
  // The Legion bridge's inbox takes the events itself; otherwise they go to Claude Code's channel.
  const inboxSocket = configuredValue(process.env[INBOX_VARIABLE])
  // Claude Code drops a notification from a channel it did not load without telling this server,
  // so such a session advertises no delivery mode and Dispatch refuses a message to it rather than
  // reporting it delivered. The bridge's inbox needs no channel.
  const channel: ChannelGate =
    inboxSocket === undefined
      ? detectClaudeChannel({
          claudePid: process.ppid,
          bundlePath: process.argv[1] ?? "",
          environment: process.env,
        })
      : { loaded: true }
  if (!channel.loaded) {
    process.stderr.write(
      `envoy-channel: Claude Code did not load this server as a channel: ${channel.why}; the session advertises no delivery mode, so Dispatch refuses messages to it instead of losing them\n`,
    )
  }

  const server = new Server(MCP_SERVER_INFO, {
    capabilities: { tools: {}, experimental: { "claude/channel": {} } },
    instructions: channel.loaded
      ? CHANNEL_INSTRUCTIONS
      : `${CHANNEL_INSTRUCTIONS} Claude Code did not load this channel for this session (${channel.why}), so no Envoy event or Dispatch message reaches you as a <channel> message, and the session advertises no delivery mode; envoy_inbox still lists what arrived.`,
  })
  const client = createEnvoyClient({
    baseUrl: envoy.url,
    fetch: globalThis.fetch,
    token: envoy.token,
  })
  let runtime: ChannelToolRuntime | undefined
  let claim: LegionClaim | undefined
  const daemon = pane === undefined ? undefined : createLegionDaemonClient(pane.daemonUrl)
  const toolDefinitions = [
    ...channelToolDefinitions(),
    ...(daemon === undefined ? [] : [legionToolDefinition()]),
  ]
  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: toolDefinitions }))
  server.setRequestHandler(CallToolRequestSchema, async (request) => {
    if (daemon !== undefined && request.params.name === LEGION_TOOL_NAME) {
      const legionRuntime: LegionToolRuntime = {
        daemon,
        claim: () => claim?.capability(),
        handoffCommit: (phase, role) => findHandoffCommit({ phase, role, env: process.env }),
      }
      return mcpResult(await executeLegionTool(legionRuntime, request.params.arguments))
    }
    if (runtime === undefined) throw new Error("Envoy channel server is still starting")
    return mcpResult(await executeEnvoyTool(runtime, request.params.name, request.params.arguments))
  })
  const notifier = inboxSocket === undefined ? server : createInboxNotifier(inboxSocket)
  // A session whose deliveries are channel notifications Claude Code did not load shows the model
  // nothing, so it advertises no delivery mode at all.
  const capabilities =
    inboxSocket !== undefined ? INBOX_CAPABILITIES : channel.loaded ? CHANNEL_CAPABILITIES : []
  const booting = new AbortController()

  const stopped = Promise.withResolvers<void>()
  let stopping = false
  let session: ChannelSession | undefined
  const stop = (): void => {
    if (stopping) return
    stopping = true
    booting.abort()
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
    const { inbound } = envoy
    if ("off" in inbound) {
      if (pane !== undefined) {
        stopPane(
          `a Legion pane's task reaches it over NATS, and inbound messages are off: ${inbound.off}`,
        )
      }
      // No registration happens here, so the title is stamped directly.
      stampOnce(() => identity.id)
      session = inboundOffChannelSession({ identity, stateDirectory, why: inbound.off })
    } else {
      connection = await connect({
        servers: [...inbound.natsUrls],
        name: `claude-envoy-channel-${identity.id}`,
        ...natsAuthOptions(inbound.natsSeed),
        reconnect: true,
        maxReconnectAttempts: -1,
        reconnectTimeWait: 2_000,
      })
      session = await startChannelSession({
        identity,
        connection,
        notifier,
        capabilities,
        client,
        heartbeatMs: envoy.heartbeatMs,
        configFile,
        stateDirectory,
        title,
        ...(stamping ? { stampTitle } : {}),
        // The QA override names a fixed identity; only a Claude-minted id follows `/clear`.
        ...(configuredValue(overrideSessionId) === undefined ? { handoffPid: process.ppid } : {}),
      })
    }
    runtime = { identity, client, session }
    if (pane !== undefined && daemon !== undefined && !stopping) {
      claim = startLegionClaim({
        pane,
        sessionID: identity.id,
        daemon,
        client,
        session,
        signal: booting.signal,
        log: logLegion,
        stop: endPane,
      })
    }
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
