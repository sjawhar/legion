import { type Stats, unwatchFile, watchFile } from "node:fs"
import { rm } from "node:fs/promises"
import {
  AGENT_STREAM_WATCH_INTERVAL_MS,
  type AgentStreamCommand,
  type AgentStreamControlMessage,
  agentStreamControlSubject,
  agentStreamFramesSubject,
  agentSubject,
  DELIVERY_CAPABILITIES,
  type DeliveryCapability,
  ROLE_TOPIC_PREFIX,
  zodSchemaApi,
} from "@legion/contracts"
import {
  type ConsumerDelivery,
  type ConsumerDeliverySnapshot,
  type ConsumerFrame,
  createConsumerDelivery,
} from "@legion/envoy-client/consumer-delivery"
import {
  createDeliveryDedupe,
  type DispatchDelivery,
  expectsLaneReceipt,
  inboundTimestamp,
  postDeliveryReply,
  type RenderInboundResult,
  renderInbound,
} from "@legion/envoy-client/delivery"
import { activeDispatchSettings } from "@legion/envoy-client/dispatch-config"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { sessionDirectory as dispatchSessionDirectory } from "@legion/envoy-client/dispatch-session-directory"
import { writeSessionTitle } from "@legion/envoy-client/dispatch-session-state"
import { subscriptionRemovedTopics } from "@legion/envoy-client/dispatch-subscribe"
import { messageFor } from "@legion/envoy-client/errors"
import { machineID } from "@legion/envoy-client/machine"
import { natsConnectionOptions } from "@legion/envoy-client/nats-connection"
import {
  closeReplayedRemoval,
  createReplayEngine,
  envoyNoticeText,
  failureNoticeFields,
  type ReplayDeliveryOutcome,
  type ReplayFrame,
  type ReplayNotice,
  reachesPrivateStreamSubject,
  rememberedTopics,
  replayedText,
  replayFollowedSubjects,
  replaySuppressed,
  routeReplayNotice,
} from "@legion/envoy-client/replay"
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
  publishConfirmationText,
  sendConfirmationText,
  sessionsOnMachine,
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
  newClientInstanceID,
  type SubscribeResult,
} from "@legion/envoy-client/transport"
import { AgentStreamPublisher } from "@legion/pi-shared/agent-stream"
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client"
import { findPushedBranchHead, paneHandoffReport } from "@legion/pi-shared/handoff-record"
import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { connect } from "nats"
import { z } from "zod"
import type { ModConfigRow, ModContentBlock, ModEvent } from "../hooks/mod-protocol"
import { name as packageName, version as packageVersion } from "../package.json" with {
  type: "json",
}
import { claudeAgentStreamCommands, createClaudeAgentStream } from "./agent-stream-claude"
import {
  type ChannelForwarder,
  type ChannelForwarderConnection,
  type ChannelInboundMessage,
  type ChannelTopicSubscription,
  createChannelForwarder,
  deliveryIdentity,
} from "./channel-forwarder"
import { claudeProjectDirectory, claudeSessionId, configuredValue } from "./claude-session"
import { inboundOffChannelSession, serveEnvoyOff } from "./envoy-off"
import { installedPluginVersion } from "./installed-build"
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
  createPhaseHookEndpoint,
  PHASE_HOOK_TOOL_NAME,
  paneReminderLog,
  phaseHookToolDefinition,
  phaseStallFile,
} from "./legion-phase-hook"
import {
  executeLegionTool,
  LEGION_TOOL_NAME,
  type LegionToolRuntime,
  legionToolDefinition,
} from "./legion-tool"
import { type ModLink, startModLink } from "./mod-link"
import { type ModSession, modSession } from "./mod-session"
import {
  type ReplayStateFile,
  type ReplayStateWriter,
  readReplayState,
  replayStateFile,
  replayStateWriter,
} from "./replay-state"
import {
  pruneStaleSessionHandoffs,
  pruneStaleSessionRoles,
  readPersistedRole,
  readSessionHandoff,
  roleStateFile,
  SessionIdentity,
  sessionHandoffFile,
  UnparseableRoleRecordError,
  writePersistedRole,
} from "./session-identity"
import {
  pruneStaleSessionTitles,
  readChosenTitleSync,
  sessionTitleFile,
  touchStateFile,
} from "./session-state"
import { type SessionTitle, sessionTitle } from "./session-title"
import {
  createUserTurnRouter,
  type UserTurnRouter,
  type UserTurnRouterOptions,
} from "./user-turn-router"
import {
  pruneStaleSessionTurns,
  readUserTurnState,
  type UserTurnState,
  userTurnState,
  userTurnStateFile,
} from "./user-turn-state"

const CHANNEL_INSTRUCTIONS =
  "Envoy delivers trusted, internal session and Dispatch events into this conversation. Use the Envoy tools for Envoy actions, and reach Dispatch through the `dispatch` command in Bash (`dispatch --help` lists its commands; the `dispatch` skill teaches them). Dispatch asks stay on Dispatch."
const CHANNEL_INBOX_LIMIT = 50
const EMPTY_RECEIPT = new Uint8Array()
const encoder = new TextEncoder()
const decoder = new TextDecoder()
/** How often the handoff file is checked for a `/clear`. */
const HANDOFF_POLL_MS = 250
/** How long a handoff waits for NATS to confirm the new direct subject before retrying. */
const HANDOFF_FLUSH_TIMEOUT_MS = 10_000

/** How long the live view waits for the mod to answer one of its reads (`$.session.model()`,
 *  `$.config.list()`, `$.command.list()`). Each is served out of the session's own memory, so a
 *  wait this long means the mod is gone rather than busy, and the header keeps its last values. */
const AGENT_STREAM_CALL_TIMEOUT_MS = 5_000
/** How long the live view waits before taking its control subject again after the subscription's
 *  iterator ended, which is what a dropped connection looks like from here. */
const AGENT_STREAM_RESUBSCRIBE_MS = 1_000

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
 * absence. These are reads: only the publish route keeps a message it cannot
 * deliver and answers 202 (INFRA-839), and `GET /v1/roles/<role>` still refuses
 * an unheld role, so a 404 here is the absence it always was.
 */
function undefinedOnNotFound(error: unknown): undefined {
  if (error instanceof EnvoyApiError && error.details.status === 404) return undefined
  throw error
}

/** The MCP `serverInfo`; the version is the package's, so it is spelled once. */
export const MCP_SERVER_INFO = { name: "envoy", version: packageVersion } as const

/**
 * What a session takes, which Dispatch checks every targeted delivery against.
 *
 * A terminal session reaches the model only through its mod (`./mod-session`): until one
 * connects no message reaches the model at all, so it advertises nothing and Dispatch refuses a
 * message to it rather than reporting it delivered; with one it takes an Aside, a Send and the
 * side turn a BTW is. A Legion pane's messages go through the bridge's inbox, which needs no mod
 * (`INBOX_CAPABILITIES`), and it gains the BTW when the mod connects.
 */
export function sessionCapabilities(session: {
  readonly pane: boolean
  readonly mod: boolean
}): readonly DeliveryCapability[] {
  if (!session.pane) return session.mod ? DELIVERY_CAPABILITIES : []
  return session.mod ? [...INBOX_CAPABILITIES, "btw"] : INBOX_CAPABILITIES
}

/**
 * One line this server writes into the session: a rendered Envoy delivery, a replayed one, or a
 * notice of Envoy's own. Claude Code took these as `<channel>` messages until INFRA-1026 removed
 * that channel; what writes them now is the session's own conversation, through the mod
 * (`./mod-session`) or, in a Legion pane, the bridge's inbox (`./legion-inbox`).
 */
export interface ChannelNotification {
  readonly content: string
  /** The envelope this line is, where it is one: the id the Legion bridge answers it by. */
  readonly eventId?: string
  /** `content` as Claude Code's own blocks, where a line carries more than its text (a person's
   *  pictures); the text alone is still `content`, for a reader that does not know the field. */
  readonly blocks?: readonly ModContentBlock[]
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
   * Queue one rendered envelope onto the notifier, behind every frame queued before it. Resolves
   * whether it went to Claude Code, once the notifier resolved its write: `false` for a frame
   * skipped, dropped, suppressed, or answered on Dispatch with an error instead (once that reply
   * is posted). `late` is a relay frame delivered before or stored before the current epoch
   * (`ConsumerFrame.late`): a late BTW is suppressed, as a replayed one is.
   */
  enqueue(input: {
    readonly subject: string
    readonly raw: string
    readonly late?: boolean
  }): Promise<boolean>
  /** Latest inbound metadata, most recent first; no envelope body is retained. */
  inbox(): readonly ChannelInboxEntry[]
}

/**
 * The channel delivery of a running session, which also shows what replay recovers
 * (`@legion/envoy-client/replay`), on the same notifier as a live frame: the channel notification,
 * or the Legion bridge's inbox.
 */
export interface ChannelDeliveryPath extends ChannelDelivery {
  /**
   * Shows a stored frame the session missed, rendered as a live one is, with when it was
   * published, and runs nothing: a replay answers nothing on Dispatch, so a targeted delivery the
   * renderer refuses gets no late reply. `suppressed` for a frame the renderer does not show; a
   * rejection is a failure.
   */
  replay(frame: ReplayFrame): Promise<ReplayDeliveryOutcome>
  /** Shows a notice of Envoy's own; a rejection says it was not shown. */
  notice(content: string): Promise<void>
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
  /**
   * The session's advertised delivery modes have changed (`ChannelSessionOptions.capabilities` is
   * read again): register, so Dispatch learns what the session takes now. A terminal session
   * advertises nothing until its mod connects, which is the change this exists for.
   */
  capabilitiesChanged(): void
  /**
   * Takes one of the mod's events, in the order the link yielded them: the router of a person's
   * messages reads them (`./user-turn-router`), and so does the session behind it. The live view
   * owns the link's one event stream and hands each event over.
   */
  observe(event: ModEvent): void
  /** Drain NATS, deregister this listener route, and release timer resources. */
  shutdown(): Promise<void>
}

export interface ChannelSessionOptions {
  readonly identity: SessionIdentity
  readonly connection: ChannelForwarderConnection
  readonly notifier: ChannelNotifier
  /**
   * The delivery modes the session advertises to the listener, which Dispatch checks every
   * targeted delivery against. Read at each registration rather than once, because what the
   * session can take changes while it runs: a terminal session takes nothing until its mod
   * connects, and a Legion pane gains `btw` when it does. The owner says so with
   * `ChannelSession.capabilitiesChanged`.
   */
  readonly capabilities: () => readonly DeliveryCapability[]
  /**
   * The router a person's messages go through (`./user-turn-router`), built for the record of
   * the session id now bound: at startup, and again whenever a `/clear` moves the session to
   * another id, since that record is kept per session id (`./user-turn-state`). Omitted, every
   * frame arrives as the card it renders as.
   */
  readonly router?: (state: UserTurnState) => UserTurnRouter
  readonly client: Pick<
    EnvoyClient,
    | "subscribe"
    | "unsubscribe"
    | "unregisterSession"
    | "setRole"
    | "getRole"
    | "getInterest"
    | "replay"
  >
  readonly heartbeatMs: number
  /**
   * The session's envoy.json (`sessionConfigFile`): a rejected Dispatch frame is answered with the
   * Dispatch URL and token read from it at that moment.
   */
  readonly configFile: SessionConfigFile
  /** How long a handoff waits for NATS to confirm the new direct subject before it retries. */
  readonly handoffFlushTimeoutMs?: number
  /**
   * `${CLAUDE_PLUGIN_DATA}`: role state per session id, the per-process handoff files, and the
   * replay state per session id.
   */
  readonly stateDirectory: string
  /**
   * The channel-server process this session runs in, as a random 128-bit hex id minted once per
   * process (`runEnvoyChannelServer`). Every registration and every deregistration carries it,
   * so the listener can tell this process's registry entry from one another process made under
   * the same session id: a soft role claim against a live predecessor is granted only to the
   * instance that registered it, and a deregistration removes an entry only while it is this
   * instance's.
   */
  readonly clientInstanceID: string
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
  /**
   * The build every registration names (`installedPluginVersion` of the bundle Claude Code runs,
   * or `source` for a bundle outside its plugin cache).
   */
  readonly pluginVersion: string
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

/**
 * Answer a targeted Dispatch delivery, on the sender's behalf: the reply this session has for it,
 * or the error that stopped it, so the sender sees the attempt fail instead of waiting on a reply
 * that never comes.
 */
export async function postDispatchReply(
  identity: SessionIdentity,
  configFile: SessionConfigFile,
  delivery: DispatchDelivery,
  result: { readonly body?: string; readonly error?: string },
): Promise<void> {
  const config = sessionDispatchSettings(configFile, process.env, {
    cwd: identity.directory,
  }).dispatch
  if (!config.enabled) throw new Error(config.error ?? config.notice)
  await postDeliveryReply(config, identity.id, delivery, result)
}

/**
 * Render inbound Envoy envelopes through the shared renderer and hand each one to the session,
 * one at a time, in the order they were queued.
 *
 * A frame a person's own message, command, pick or BTW (`router`, `./user-turn-router`) is taken
 * by the router and nothing is written as a card; everything else is written to the notifier as
 * the rendered card it is, which names its sender. A frame's promise resolves once the session
 * has it: in a Legion pane, once the bridge answered that Claude Code took the message into a
 * step; in a terminal session, once the mod stored the row.
 *
 * Routing runs in the same queue as the writes, so a person's message and the cards around it
 * reach the session in the order they arrived.
 */
export function createChannelDelivery(input: {
  readonly identity: SessionIdentity
  readonly notifier: ChannelNotifier
  /** The session's envoy.json, which a rejected Dispatch frame's reply reads. */
  readonly configFile: SessionConfigFile
  /** What a person's own message, command, pick or BTW becomes; omitted, every frame is a card. */
  readonly router?: () => UserTurnRouter | undefined
}): ChannelDeliveryPath {
  const inbox: ChannelInboxEntry[] = []
  let tail = Promise.resolve()

  /** Runs `work` behind everything queued before it, keeping the frames in order. */
  const queued = <T>(work: () => Promise<T>): Promise<T> => {
    const run = tail.then(work)
    tail = run.then(
      () => undefined,
      (error: unknown) => {
        process.stderr.write(`envoy-channel: a delivery failed — ${messageFor(error)}\n`)
      },
    )
    return run
  }

  const write = (notification: ChannelNotification): Promise<void> =>
    queued(() => input.notifier.notification(notification))

  /** Lists a rendered frame in the inbox. */
  const list = (rendered: RenderInboundResult): void => {
    const envelope = rendered.envelope
    if (envelope === undefined) return
    inbox.unshift({
      event_id: envelope.event_id ?? "unknown",
      at: inboundTimestamp(envelope.issued_at),
      from: envelope.source_session ?? envelope.source,
      summary: envelope.payload_summary ?? "unknown",
    })
    if (inbox.length > CHANNEL_INBOX_LIMIT) inbox.pop()
  }

  /** Lists a rendered frame and writes `content` into the session as the card it is. */
  const show = (rendered: RenderInboundResult, content: string): Promise<void> => {
    list(rendered)
    return write({
      content,
      ...(rendered.envelope?.event_id === undefined ? {} : { eventId: rendered.envelope.event_id }),
    })
  }

  // A replayed or late BTW is never answered: the question was asked while this session was not
  // taking messages, and Dispatch has long since recorded the attempt. Only a live one runs
  // (`./user-turn-router`).
  const passOverBtw = (rendered: RenderInboundResult, how: "replayed" | "late"): boolean => {
    if (rendered.delivery?.mode !== "btw") return false
    process.stderr.write(
      `envoy-channel: passing over ${how} BTW ${rendered.envelope?.event_id ?? "unknown"}: it was asked while this session was not taking messages\n`,
    )
    return true
  }

  return {
    enqueue({ subject, raw, late }) {
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
      if (late === true && passOverBtw(rendered, "late")) return tail.then(() => false)
      return queued(async () => {
        // A message Dispatch accepted as this session's own turn is never also a card. A failure
        // after that acceptance throws, so the frame is not answered and the listener sends it
        // again, rather than a person's message vanishing into a delivery nobody kept.
        const routed = await input.router?.()?.route(rendered)
        if (routed === true) {
          list(rendered)
          return true
        }
        await input.notifier.notification({
          content: rendered.content,
          ...(rendered.envelope?.event_id === undefined
            ? {}
            : { eventId: rendered.envelope.event_id }),
        })
        list(rendered)
        return true
      })
    },
    async replay(frame) {
      const rendered = renderInbound(frame.data, input.identity.id, frame.subject)
      if (replaySuppressed(rendered)) return "suppressed"
      if (passOverBtw(rendered, "replayed")) return "suppressed"
      await show(rendered, replayedText(rendered.content, frame.time))
      return "shown"
    },
    notice(content) {
      return write({ content })
    },
    inbox() {
      return [...inbox]
    },
  }
}

/**
 * Queues a channel event, then answers a forwarded lane's receipt, and resolves whether Claude
 * Code was handed the event. The receipt is answered at once, before the write: it proves only
 * that this process took the request off the bus. Which frames wait for one is
 * `expectsLaneReceipt`'s rule. A duplicate, which includes a core copy of a frame the listener
 * relays (consumer delivery), answers the receipt and queues nothing.
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
 * channel server owns direct and followed subjects, and the listener's relay
 * subject when it delivers the session's consumer, so no Monitor or helper
 * process is needed between NATS and Claude Code.
 */
export async function startChannelSession(options: ChannelSessionOptions): Promise<ChannelSession> {
  const { identity } = options
  let directSubject = agentSubject(identity.id)
  /**
   * What this session does with a person's direct messages, for the id now bound: the record is
   * kept per session id (`./user-turn-state`), so a `/clear` builds another one. Undefined until
   * the record is loaded, and for a session the owner gave no router.
   */
  let router: UserTurnRouter | undefined
  /** The record the router above reads and writes, kept so a shutdown can wait for its writes. */
  let turns: UserTurnState | undefined
  const delivery = createChannelDelivery({
    identity,
    notifier: options.notifier,
    configFile: options.configFile,
    router: () => router,
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

  // The live path's claims, which a replay sweep reads to leave a frame being delivered alone.
  const dedupe = createDeliveryDedupe()
  /**
   * The session id whose replay state the engine holds. Replay does nothing until it is the
   * current id, so the work of one id never lands in another's file across a `/clear`.
   */
  let replayBound: string | undefined

  /**
   * A stored frame is shown as a live one is, and runs nothing. A removal closes a topic only when
   * it was stored after the session started following that topic (`closeReplayedRemoval`).
   *
   * The frame is claimed in the live path's dedupe before it is handed to the write queue, as a
   * live frame is (`createChannelForwarder`, `deliverRelayed`): a live copy that arrives while the
   * channel write is still open finds the claim and is a repeat, rather than being shown beside
   * it. The claim is released when the frame was not shown after all (the renderer suppressed it,
   * the write failed), so a later copy still reaches the session, and settled once it was shown:
   * the dedupe's record outlives the claim, so a copy that arrives before the engine records the
   * frame in its ledger is a repeat too. A frame the live path is delivering now is that path's to
   * show and record, so the sweep leaves the stream's copy for a later one (`queued`); a frame the
   * dedupe recorded and let go of that the ledger does not hold (one shown before a `/clear` moved
   * the engine, say) is handed over with no claim.
   */
  const showReplayed = async (frame: ReplayFrame): Promise<ReplayDeliveryOutcome> => {
    const identityOf = deliveryIdentity(frame.data)
    const claim = dedupe.claim(identityOf)
    if (claim === undefined && identityOf !== undefined && dedupe.pending(identityOf)) {
      return "queued"
    }
    try {
      await closeReplayedRemoval(frame, {
        sessionID: identity.id,
        engine: replay,
        close: (topic) => userTopics.delete(topic),
        unregister: unfollowRemovedTopics,
        followedChanged: () => replay.followedChanged(),
      })
      const outcome = await delivery.replay(frame)
      if (outcome === "shown") claim?.settle()
      else claim?.release()
      return outcome
    } catch (error) {
      claim?.release()
      throw error
    }
  }

  /**
   * Tells the session a notice of the replay engine or of consumer delivery (a gap). That the
   * listener does not replay goes where every start notice of this server goes, as pi-envoy raises
   * it as a warning rather than a card for the model; every other one is a notice card.
   */
  const tellNotice = (notice: ReplayNotice): void => {
    routeReplayNotice(notice, {
      warn: (message) => process.stderr.write(`envoy-channel: ${message}\n`),
      card: (content) => {
        delivery.notice(content).catch((error: unknown) => {
          process.stderr.write(
            `envoy-channel: ${notice.message} (the session could not be told — ${messageFor(error)})\n`,
          )
        })
      },
    })
  }

  /** The writer of the bound id's state file (`replayStateWriter`), set by `restoreReplay`. */
  let stateWriter: ReplayStateWriter | undefined

  /**
   * Shows the session each message the notification stream stored for it while its subscriptions
   * were down, once (`@legion/envoy-client/replay`), with its state in the session id's file.
   */
  const replay = createReplayEngine({
    host: {
      sessionID: () => (shuttingDown || replayBound !== identity.id ? undefined : identity.id),
      // What the session registers (`replayFollowedSubjects`).
      followedSubjects: () => replayFollowedSubjects(directSubject, userTopics),
      deliver: showReplayed,
      showFailureNotice: (notice) => delivery.notice(envoyNoticeText(failureNoticeFields(notice))),
      failureNoticeFailed: (notice, error) => {
        process.stderr.write(
          `envoy-channel: message ${notice.eventId} from ${notice.sender} was not shown, nor was the notice naming it — ${messageFor(error)}\n`,
        )
      },
      notify: tellNotice,
      log: (message, error) => {
        process.stderr.write(
          `envoy-channel: ${message}${error === undefined ? "" : ` — ${messageFor(error)}`}\n`,
        )
      },
      write: (change) => stateWriter?.replay(change.snapshot()),
    },
    transport: options.client,
    dedupe,
  })

  /**
   * Loads the current id's state file: the replay state into the engine, and the consumer delivery
   * snapshot into a new relay for the id, whose subject it subscribes (`openRelay`). Returns the
   * topics it followed that a restore follows again (`rememberedTopics`). A file that cannot be
   * read starts the session's replay over; its next write replaces the file.
   */
  const restoreReplay = async (): Promise<readonly string[]> => {
    const sessionId = identity.id
    const file = replayStateFile(options.stateDirectory, sessionId)
    const snapshot = await readReplayState(file).catch((error: unknown): ReplayStateFile => {
      process.stderr.write(
        `envoy-channel: could not read the replay state (${messageFor(error)}); this session's replay starts over\n`,
      )
      return { ledger: [] }
    })
    replay.restore(snapshot)
    stateWriter = replayStateWriter(file, snapshot)
    openRelay(sessionId, stateWriter, snapshot.consumer)
    replayBound = sessionId
    return rememberedTopics(snapshot.state)
  }

  /**
   * Builds the router of a person's messages for the id now bound, on the record that id's file
   * holds: at startup, and again after a `/clear`, since no attempt of the id left behind may be
   * spent under the new one. A record that cannot be read leaves the session with none, and
   * every frame then arrives as the card it renders as: taking a person's message as their own
   * turn needs the record of which attempts this session has already spent, and without it an
   * attempt could be accepted twice.
   */
  const restoreTurns = async (): Promise<void> => {
    router = undefined
    turns = undefined
    if (options.router === undefined) return
    const file = userTurnStateFile(options.stateDirectory, identity.id)
    const restored = await readUserTurnState(file).catch((error: unknown) => {
      process.stderr.write(
        `envoy-channel: could not read this session's record of the messages it took (${messageFor(error)}); its messages arrive as cards\n`,
      )
      return undefined
    })
    if (restored === undefined) return
    turns = userTurnState(file, restored)
    router = options.router(turns)
  }

  /** A sweep runs on the engine's own chain, never in the registry's. */
  const triggerReplay = (): void => {
    if (!shuttingDown) void replay.sweep()
  }

  /** A human Dispatch subscription removal naming this session drops its topics here too. */
  const takeRemovals = (raw: string): void => {
    const removedTopics = subscriptionRemovedTopics(raw, identity.id)
    // An empty list would mean "every topic" to both unfollow and the listener.
    if (removedTopics !== undefined && removedTopics.length > 0) {
      for (const topic of removedTopics) userTopics.delete(topic)
      void dropRemovedTopics(removedTopics)
    }
  }

  /**
   * Consumer delivery (`@legion/envoy-client/consumer-delivery`). The current id's relay subject
   * is subscribed before any registration under that id, and every registration asks the listener
   * for consumer delivery (the relay's epoch and resume point). Answered `consumer`, the listener
   * relays the session's durable consumer on that subject: its agent subject and every followed
   * topic the stream stores, each frame handed to the channel's write queue in stream order and
   * answered `done` once written (`deliverRelayed`). The core copies of those subjects then answer
   * only a lane's receipt (`relaysCoreTopic`), and the replay engine makes no `/v1/replay` call.
   * Answered `core`, or not at all, the core path runs as before.
   */
  interface Relay {
    readonly sessionID: string
    readonly delivery: ConsumerDelivery
    /** The id's state file, which also holds the relay's snapshot. */
    readonly state: ReplayStateWriter
    subscription: ChannelTopicSubscription | undefined
  }
  let relay: Relay | undefined

  const relaysCoreTopic = (topic: string): boolean =>
    relay?.sessionID === identity.id && relay.delivery.relays(topic)

  /**
   * Hands one relay frame to the channel's write queue, and answers it once Claude Code has it:
   * `done` once the notifier resolved the write (`createChannelDelivery`) and the ledger recorded
   * the frame, at once for a frame skipped, suppressed (a late BTW), rejected (once the reply is
   * posted) or already shown. A write that fails throws, which the relay answers `nak`, so the
   * listener sends the frame again (from its third delivery on, `done` with a failure notice). The
   * frame stops blocking the next one once it is queued: the write queue keeps the stream's order,
   * and no frame waits on another's write longer than the queue makes it.
   */
  const deliverRelayed = async (owner: Relay, frame: ConsumerFrame): Promise<void> => {
    // The session moved to another id (a `/clear`) or is shutting down: whoever takes it next
    // gets the frame again.
    if (shuttingDown || relay !== owner || owner.sessionID !== identity.id) {
      frame.nak()
      return
    }
    takeRemovals(frame.data)
    const identityOf = deliveryIdentity(frame.data)
    const claim = dedupe.claim(identityOf)
    // A copy of a frame shown or being shown (another subject, a core frame of an unstored topic).
    if (claim === undefined) {
      frame.done()
      return
    }
    let shown: boolean
    try {
      const queued = delivery.enqueue({ subject: frame.subject, raw: frame.data, late: frame.late })
      frame.release()
      shown = await queued
    } catch (error) {
      claim.release()
      throw error
    }
    if (!shown) {
      claim.release()
      frame.done()
      return
    }
    // Recorded before the claim settles and before `done`, as the core path records it.
    if (identityOf !== undefined) await replay.recordDelivered(identityOf)
    claim.settle()
    frame.done()
    owner.delivery.persist()
  }

  /**
   * A new relay for session `sessionID`, its snapshot `restored` (before any `setFollowed`), its
   * subject subscribed at once; another id's relay is closed first. Its epoch begins as it is
   * made, on this process's one NATS connection; a reconnect begins another (`renewRelayEpoch`).
   * Every later change of its snapshot goes into the id's state file: after a frame is written,
   * after the followed topics change, and after a registration answer moves the stream.
   */
  const openRelay = (
    sessionID: string,
    state: ReplayStateWriter,
    restored: ConsumerDeliverySnapshot | undefined,
  ): void => {
    closeRelay()
    const consumer: ConsumerDelivery = createConsumerDelivery({
      sessionID,
      ledger: replay,
      host: {
        deliver: (frame) => deliverRelayed(owner, frame),
        gap: tellNotice,
        log: (message, error) => {
          process.stderr.write(
            `envoy-channel: ${message}${error === undefined ? "" : ` — ${messageFor(error)}`}\n`,
          )
        },
      },
      publish: (reply, body) => options.connection.publish(reply, encoder.encode(body)),
      persist: (snapshot) => {
        if (relay !== owner) return false
        state.consumer(snapshot).catch((error: unknown) => {
          process.stderr.write(
            `envoy-channel: could not persist the consumer delivery state — ${messageFor(error)}\n`,
          )
        })
        return true
      },
    })
    if (restored !== undefined) consumer.restore(restored)
    const owner: Relay = {
      sessionID,
      delivery: consumer,
      state,
      subscription: undefined,
    }
    relay = owner
    const subscription = options.connection.subscribe(consumer.subject)
    owner.subscription = subscription
    void pumpRelay(owner, subscription)
  }

  /** Each relay message goes to the relay, which answers `received` at once and queues it. */
  const pumpRelay = async (owner: Relay, subscription: ChannelTopicSubscription): Promise<void> => {
    try {
      for await (const message of subscription) {
        owner.delivery.receive(message)
      }
    } catch (error) {
      process.stderr.write(
        `envoy-channel: the relay subscription of ${owner.sessionID} stopped — ${messageFor(error)}\n`,
      )
    }
  }

  /**
   * The relay's session left (a `/clear`) or shuts down: its unsettled frames are answered `nak`,
   * the replay engine leaves consumer mode, its subject is unsubscribed, and the core path runs
   * until a registration answers otherwise.
   */
  function closeRelay(): void {
    const owner = relay
    if (owner === undefined) return
    relay = undefined
    owner.delivery.close()
    owner.subscription?.unsubscribe()
    owner.subscription = undefined
  }

  /**
   * nats.js reconnected: its subscriptions are the same objects, but the listener must restart the
   * relay at its ack floor, so a new epoch is registered at once.
   */
  const renewRelayEpoch = (): void => {
    if (shuttingDown || relay === undefined) return
    relay.delivery.beginEpoch()
    serialized(register).catch((error: unknown) => {
      process.stderr.write(
        `envoy-channel: could not register again after a reconnect (${messageFor(error)}); the next heartbeat retries\n`,
      )
    })
  }

  /**
   * What a registration's answer says of consumer delivery, for the id it registered with the
   * topics it carried, and the relay it asked for. One that asked nothing (no relay open) is
   * answered `core` by the listener, and nothing is relayed: the core path is on already, since
   * `closeRelay` is the one place a relay is dropped and `delivery.close()` left consumer mode
   * there. An answer for an id this process has left, or for a relay since closed, changes
   * nothing.
   */
  const takeDeliveryAnswer = (
    sessionID: string,
    topics: readonly string[],
    asked: Relay | undefined,
    result: SubscribeResult,
  ): void => {
    if (sessionID !== identity.id || relay !== asked) return
    asked?.delivery.answered(result.delivery, topics)
  }

  const forwarder: ChannelForwarder = createChannelForwarder(options.connection, {
    dedupe,
    ledger: replay,
    relayed: relaysCoreTopic,
    failed: (message, topic, error, receivedAt) => {
      // A frame no sweep will show again gets its notice at once: a role forward, already
      // acknowledged, one on a subscription the replay does not read (refused, not started, or
      // the listener's replay failing), or one received before its subject's cursor started.
      // The stream's copy of any other waits for a sweep.
      const envelope = renderInbound(message.raw, identity.id, message.subject).envelope
      if (envelope !== undefined) void replay.liveFailed(topic, envelope, error, receivedAt)
    },
    deliver: async (message) => {
      takeRemovals(message.raw)
      return enqueueChannelMessage(delivery, options.connection, directSubject, message)
    },
  })

  /**
   * A human Dispatch subscription removal, best-effort: drops the topics, which the caller has
   * taken out of `userTopics`, and resolves once the replay state no longer follows them.
   */
  function dropRemovedTopics(topics: readonly string[]): Promise<void> {
    unfollowRemovedTopics(topics)
    return replay.followedChanged()
  }

  /**
   * Starts dropping removed topics from the forwarder and the registry entry, each failure
   * reported here: the drop half of `dropRemovedTopics`, and what a replayed removal takes out of
   * the registry (`closeReplayedRemoval`).
   */
  function unfollowRemovedTopics(topics: readonly string[]): void {
    const { dropping, unsubscribing } = dropFromForwarderAndRegistry(topics)
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

  /**
   * Drops `topics` from the forwarder's subscriptions and the registry entry — the two
   * steps every removal here takes once a caller has already taken them out of
   * `userTopics`. `forwarder.unfollow` runs first and removes from `following`
   * before its first await, so no registration queued behind either promise
   * advertises a dropped topic again, and the relay stops taking them at once. Both
   * callers here — `dropRemovedTopics`, a removal's best-effort drop, and the public
   * `unfollow` below — take these same two steps in this order; only whether they
   * wait for them differs.
   */
  function dropFromForwarderAndRegistry(topics: readonly string[]): {
    readonly dropping: Promise<void>
    readonly unsubscribing: Promise<void>
  } {
    const dropping = forwarder.unfollow(topics)
    // The relay takes the topics the session follows now, before a registration carries them.
    if (relay?.sessionID === identity.id) relay.delivery.setFollowed(registeredTopics())
    return {
      dropping,
      unsubscribing: serialized(() =>
        options.client.unsubscribe({ sessionID: identity.id, topics }),
      ),
    }
  }

  /**
   * The topics a registration carries: every subject followed, the direct one included, but a
   * followed topic reaching another session's agent subject (a wildcard over them, or another
   * session's by name), which the listener refuses to store.
   */
  function registeredTopics(): string[] {
    return forwarder.topics().filter((topic) => !reachesPrivateStreamSubject(directSubject, topic))
  }

  /** The title last stamped, so a heartbeat stamps nothing new. */
  let stampedTitle: { readonly sessionID: string; readonly title: string } | undefined
  /** Whether a failure to stamp the title has been reported since the last success. */
  let stampFailureReported = false

  /**
   * Advertise the current id and topics; runs only inside `serialized`. The title is also stamped
   * for the `dispatch` command (`options.stampTitle`) whenever it or the id changes. With the id's
   * relay open, the registration asks for consumer delivery (`delivery`: the relay's epoch and
   * resume point), the relay taking the topics it carries as followed first; its answer says
   * whether the listener relays them (`takeDeliveryAnswer`).
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
    const sessionID = identity.id
    const topics = registeredTopics()
    const asking = relay?.sessionID === identity.id ? relay : undefined
    asking?.delivery.setFollowed(topics)
    const result = await options.client.subscribe({
      sessionID,
      directory: identity.directory,
      topics,
      port: 0,
      title,
      capabilities: options.capabilities(),
      driving: false,
      selfSubscribed: true,
      clientInstanceID: options.clientInstanceID,
      plugin: { name: packageName, version: options.pluginVersion },
      ...(asking === undefined ? {} : { delivery: asking.delivery.registration() }),
    })
    takeDeliveryAnswer(sessionID, topics, asking, result)
  }

  /**
   * Take back the role persisted for `previousSessionID` (this id on `--resume`,
   * the pre-`/clear` id on a handoff) with a soft claim naming that predecessor. The listener
   * grants such a claim only when that predecessor has no live registry entry, when its entry
   * carries this process's own client instance id, or when either side carries none; a
   * predecessor still live under another client instance keeps the role.
   *
   * Which is why a refusal naming the predecessor itself keeps the record: it belongs to that
   * live holder, another process sharing this state directory, and deleting it would cost that
   * process its only way back to the role. A refusal naming any other session says the
   * predecessor demonstrably no longer holds it, so its record goes — the one path that removes
   * a role record by hand.
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
      if (claim.holder !== persisted.session_id) await rm(previousFile, { force: true })
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

  /** Whether a failure to refresh the role record has been reported since the last success. */
  let roleRefreshFailureReported = false
  /**
   * Marks the role record of a holder the heartbeat has just confirmed, so its mtime is the last
   * time this session was seen holding the role, which is what `pruneStaleSessionRoles`'s age
   * rule takes it for: nothing else touches the record of a session that keeps its role, and
   * without this the sweep expires a live holder's only way back to it.
   *
   * A timestamp touch and not a rewrite: the record's content has not changed, and an atomic
   * rewrite would leave a temporary file beside it in `roles/` every two minutes. Best-effort,
   * because a state directory that cannot be written must not cost the session its heartbeat;
   * the record it could not touch is still there to restore from until the sweep's age.
   */
  const refreshRoleRecord = async (): Promise<void> => {
    const roleFile = roleStateFile(options.stateDirectory, identity.id)
    try {
      await touchStateFile(roleFile)
      roleRefreshFailureReported = false
    } catch (error) {
      // Said once per failure run: every heartbeat retries it.
      if (!roleRefreshFailureReported) {
        roleRefreshFailureReported = true
        process.stderr.write(
          `envoy-channel: could not refresh the role record ${roleFile} (${messageFor(error)}); retrying every heartbeat\n`,
        )
      }
    }
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
      await refreshRoleRecord()
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
  /** Whether a failure to take back the queued role has been reported since the last success. */
  let roleTransferFailureReported = false
  /**
   * Best-effort, unlike `syncRegisteredInterests` behind it on the heartbeat. A failed transfer
   * keeps its own retry, `roleTransferFrom`, which clears only once `restoreRole` resolves; a
   * throw would add nothing to that and would end the tick before the interests reconciliation,
   * on every tick for as long as the failure lasts — a resumed session would never adopt its
   * subscriptions and a registry-side unsubscribe that failed would never be cleared. The
   * session works without its role meanwhile; it does not work without its interests, whose read
   * has no queue of its own and needs the failed tick, its report and its retry.
   */
  const transferRole = async (): Promise<void> => {
    const from = roleTransferFrom
    if (from === undefined) return
    try {
      await restoreRole(from)
      roleTransferFrom = undefined
      roleTransferFailureReported = false
    } catch (error) {
      // Said once per failure run: every heartbeat retries it.
      if (!roleTransferFailureReported) {
        roleTransferFailureReported = true
        process.stderr.write(
          `envoy-channel: could not take back the role held by ${from} (${messageFor(error)}); retrying every heartbeat\n`,
        )
      }
    }
  }

  /**
   * Claude Code mints a new session id on `/clear` while this process keeps the
   * one it was spawned with; the SessionStart hook writes the current id for our
   * shared parent process, and we move every binding to it. The new direct
   * subject is consumed before anything is advertised under the new id, and the
   * old one leaves the topic list before the first registration under it. Runs
   * only inside `serialized`.
   *
   * The deregistration of the old id carries this process's client instance id, so the listener
   * removes that entry only while it is still this instance's: a later process that resumed the
   * old session id and registered it keeps its registration, and this handoff takes nothing
   * from it.
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
      await options.client.unregisterSession({
        sessionID: previous,
        clientInstanceID: options.clientInstanceID,
      })
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
    // The new id's replay state: none after a `/clear`, which starts afresh as a new session
    // does, so nothing published before it existed comes back. Until it is loaded, replay does
    // nothing, and the old id's sweep in flight writes nothing more. Loading it closes the old
    // id's relay (its unanswered frames `nak`) and subscribes the new id's relay subject.
    await restoreReplay()
    // The new id takes a record of its own: an attempt the old id handled is the old id's, and
    // the rows of a conversation a `/clear` ended name nothing in the new one.
    await restoreTurns()
    roleTransferFrom ??= previous
    // The new relay subject reaches the server before the registration that asks for it. Not
    // fatal: a relayed frame nobody answers pauses the relay, which sends again after its backoff
    // and at the registration below.
    await withTimeout(
      options.connection.flush(),
      options.handoffFlushTimeoutMs ?? HANDOFF_FLUSH_TIMEOUT_MS,
      "flushing NATS",
    ).catch((error: unknown) => {
      process.stderr.write(
        `envoy-channel: could not confirm the relay subscription of ${next} — ${messageFor(error)}\n`,
      )
    })
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
      // Every tick also sweeps the stream, so a missed trigger is caught up, and a listener that
      // does not replay yet keeps being asked.
      triggerReplay()
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

  // The session's replay state comes first, so a live repeat of a frame it was shown before is
  // dropped against it. The topics it followed come back with it: a session the listener reaped
  // (ten minutes after it stopped) still follows them, and its sweep still reads them. Its relay
  // comes with it too, restored and subscribed before the first registration asks for it.
  const remembered = await restoreReplay()
  // What this session has already done with the messages Dispatch sent it, before any frame can
  // arrive: an attempt a process before this one spent must not be spent again.
  await restoreTurns()
  // The NATS subscription precedes registration so the listener never routes
  // a direct delivery to an advertised but deaf channel process.
  forwarder.follow(directSubject)
  for (const topic of remembered) {
    userTopics.add(topic)
    forwarder.follow(topic)
  }
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
    // Best-effort, as the interests read above is, and for a sharper reason: registration has
    // already happened, so a throw here takes down a channel the registry is advertising and
    // leaves the Agents page a row that never beats again.
    //
    // A failure is queued as a transfer from this session's own id, so the heartbeat's
    // `transferRole` runs `restoreRole` again on every tick until it resolves. That covers every
    // failure but one: a listener answer that is not JSON or misses the schema, an unreadable
    // role file, any transient error. The one is a role file whose bytes cannot be parsed, which
    // only `readPersistedRole` can tell apart: no later read of the same bytes succeeds, so it is
    // not queued, and it costs this session its role and nothing more.
    await restoreRole().catch((error: unknown) => {
      if (error instanceof UnparseableRoleRecordError) {
        process.stderr.write(
          `envoy-channel: ${messageFor(error)}; the session starts without a role\n`,
        )
        return
      }
      roleTransferFrom = identity.id
      // This is the first failure of the run `transferRole` reports, so it says nothing more
      // until the retry has succeeded once.
      roleTransferFailureReported = true
      process.stderr.write(
        `envoy-channel: could not restore the role (${messageFor(error)}); the next heartbeat retries\n`,
      )
    })
  })
  // The first sweep, once the listener knows what the session follows: what was stored while it
  // was away (a resume included), or, for a new session, its cursors' start at the stream's end.
  triggerReplay()
  // Housekeeping of the plugin's own state directories, each best-effort for the reason the
  // interests read above is: the heartbeat timer below is not armed yet, so a sweep that threw
  // here would take the session's whole channel down over a dead pid's handoff, a cosmetic title
  // or a role the session can restore anyway. Everything else this state adds degrades the same
  // way — the reader swallows a failed read, the hook always exits 0 — and a directory this run
  // could not sweep is swept by the next session that starts.
  for (const prune of [
    () => pruneStaleSessionHandoffs(options.stateDirectory),
    () => pruneStaleSessionTitles(options.stateDirectory),
    () => pruneStaleSessionTurns(options.stateDirectory),
    // The role sweep is told whose session this is: see `pruneStaleSessionRoles`.
    () => pruneStaleSessionRoles(options.stateDirectory, identity.id),
  ]) {
    await prune().catch((error: unknown) => {
      process.stderr.write(
        `envoy-channel: could not prune stale session state (${messageFor(error)})\n`,
      )
    })
  }
  const heartbeatTimer = setInterval(() => {
    void heartbeat()
  }, options.heartbeatMs)
  // A `/clear` is adopted when the SessionStart hook writes the handoff file, not
  // at the next heartbeat minutes later. The hook replaces the file by rename,
  // which a directory watch reports only as its temporary file, so the file is
  // polled by path; the first tick picks up a file written before polling began.
  //
  // `watchFile` also calls the listener, with every stat field zeroed, when the file
  // is missing as polling begins and when it is removed. A missing file names no id
  // to adopt, and the request below already covers startup: a tick there would be a
  // second one for the same moment, and in an outage one more attempt than the
  // bounded retry allows.
  const handoffFileChanged = (current: Stats): void => {
    if (current.ino !== 0) requestHeartbeat()
  }
  if (handoffFile !== undefined) {
    watchFile(handoffFile, { interval: HANDOFF_POLL_MS }, handoffFileChanged)
    requestHeartbeat()
  }
  // nats.js resubscribes by itself after a reconnect; what was published while it was away is in
  // the stream: the relay restarts at its ack floor on a new epoch's registration, and the core
  // path's sweep reads it. The status events end when the connection closes.
  void (async () => {
    try {
      for await (const status of options.connection.status()) {
        if (status.type !== "reconnect") continue
        renewRelayEpoch()
        triggerReplay()
      }
    } catch (error) {
      process.stderr.write(
        `envoy-channel: the NATS connection's status reader failed — ${messageFor(error)}\n`,
      )
    }
  })()

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
      if (fresh.length > 0) {
        // A new follow's cursor starts where the stream ends at the next sweep: no history.
        await replay.followedChanged()
        triggerReplay()
      }
      return fresh
    },
    async unfollow(topics) {
      const requested = topics.length === 0 ? [...userTopics] : expandSubscriptionTopics(topics)
      const removed = requested.filter((topic) => userTopics.delete(topic))
      if (removed.length === 0) return []
      const { dropping, unsubscribing } = dropFromForwarderAndRegistry(removed)
      // Recorded at once, so a restart does not follow the topics again from the replay state.
      await replay.followedChanged()
      await unsubscribing
      await dropping
      return removed
    },
    onRoleRegained(hook) {
      regained = hook
    },
    capabilitiesChanged() {
      if (shuttingDown) return
      // The same path a reconnect takes: one more registration, behind whatever is queued.
      serialized(register).catch((error: unknown) => {
        process.stderr.write(
          `envoy-channel: could not advertise this session's delivery modes (${messageFor(error)}); the next heartbeat retries\n`,
        )
      })
    },
    observe(event) {
      router?.observe(event)
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
      if (handoffFile !== undefined) unwatchFile(handoffFile, handoffFileChanged)
      // The relay's unanswered frames `nak`, so the listener sends them to whoever resumes.
      closeRelay()
      await forwarder.close()
      await serialized(() =>
        options.client.unregisterSession({
          sessionID: identity.id,
          clientInstanceID: options.clientInstanceID,
        }),
      ).catch((error: unknown) => {
        process.stderr.write(
          `envoy-channel: session deregistration failed — ${messageFor(error)}\n`,
        )
      })
      // What was written meanwhile (a resume point, a ledger record, a message taken) lands
      // before the process goes.
      await stateWriter?.idle()
      await turns?.idle()
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
        ...(args.idempotency_key === undefined ? {} : { idempotencyKey: args.idempotency_key }),
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
      const result = await runtime.client.publish({
        source: "agent",
        sourceSessionID: identity.id,
        topic: args.topic,
        message: args.message,
        ...toMessageMetadata(args),
      })
      // A kept answer is a success: the listener holds the message for the role's next holder.
      // The line says so, in the words the Oh My Pi plugin uses (`publishConfirmationText`).
      return { message: publishConfirmationText(result), ...result }
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
      return sessionsOnMachine(sessions, args.machine)
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

export interface AgentStreamOptions {
  /** The link to this session's Claude Code mod (`./mod-link`): its events are the conversation,
   *  and its `$` calls are what the header's two frames are built from. */
  readonly link: ModLink
  /** The session's own NATS connection: the frames go out on it, and the control subject is
   *  taken on it. The session's NATS user may already publish and subscribe to both. */
  readonly connection: ChannelForwarderConnection
  /** The session id the Agents page knows this session by, which both subjects name. */
  readonly sessionID: string
  /**
   * Whether the stream follows the session id the mod reports. Claude Code mints a new one on
   * `/clear` and the channel rebinds to it (`adoptHandoff`), so the two are the same id; under
   * the `ENVOY_SESSION_ID` override the session registers a fixed id of its own and the Claude
   * id names nothing Dispatch knows, so the stream stays where it started.
   */
  readonly followsSessionId: boolean
  readonly now?: () => number
  /** Where a failure that must not stop the session is written; the server's stderr by default. */
  readonly warn?: (text: string) => void
  /**
   * Takes every event this stream reads, before the stream records it. The link's events have one
   * consumer and this is it, so whatever else needs them - the router of a person's messages, and
   * the turn tracking behind it - is handed them here.
   */
  readonly observe?: (event: ModEvent) => void
  /** The Dispatch message a row of the conversation is, which tags that row so the Agents page
   *  shows a person's own message once rather than beside its stored copy. */
  readonly tagFor?: (row: string) => string | undefined
}

export interface AgentStreamRunner {
  /** Stop answering the control subject. The mod's events end with the link itself. */
  readonly stop: () => void
  /** Read the session's model and levels again and publish them: a pick has just changed one,
   *  and the header must show it before the reply naming it reaches the page. */
  readonly refresh: () => void
}

/**
 * Publish this session's conversation for the Agents page's live view (INFRA-1026).
 *
 * The mod's events become messages on the shared publisher (`./agent-stream-claude`), and this
 * answers the viewer's control subject the way every Oh My Pi session does (`packages/pi-envoy`,
 * `pumpAgentStreamControl`): a message that is neither `replay` nor `watch` is ignored, the
 * replay answer is taken before the message arms anything, so a bare replay from a client that
 * never attached a viewer gets an empty history, and every message runs inside a guard, since
 * one throw out of the iterator would leave the session deaf until the resubscribe.
 *
 * What differs from an Oh My Pi session is where the header's values come from. That host reads
 * its model, its levels and its commands out of its own memory; here they are three calls on the
 * mod, over a socket, which a session with no mod never answers. So a refresh is asked for and
 * not waited on: a control message is answered from what the mod said last, and a changed answer
 * reaches the viewer as its own frame a moment later. A session whose mod never connects
 * publishes no settings and no commands, and its conversation stays empty.
 */
export function startAgentStream(options: AgentStreamOptions): AgentStreamRunner {
  const { connection, link } = options
  const now = options.now ?? Date.now
  const warn =
    options.warn ?? ((text: string): void => void process.stderr.write(`envoy-channel: ${text}\n`))
  const publisher = new AgentStreamPublisher({
    now,
    publish: (subject, payload) => connection.publish(subject, encoder.encode(payload)),
  })
  const stream = createClaudeAgentStream({
    now,
    publisher,
    ...(options.tagFor === undefined ? {} : { tagFor: options.tagFor }),
  })
  /** The id both subjects name now; it moves with the mod's own id after a `/clear`. */
  let session = options.sessionID
  let subscription: ChannelTopicSubscription | undefined
  let stopped = false
  /** What the mod last said of the session's model, its `/config` rows and its commands. They
   *  survive a session id change: the process and its model do not change with the id. */
  let model: string | undefined
  let configRows: readonly ModConfigRow[] | undefined
  let listedCommands: readonly AgentStreamCommand[] | undefined
  let readingSettings = false
  let readingCommands = false
  /** Whether a viewer has ever armed this session, and when a watch last refreshed the commands:
   *  both as the Oh My Pi pump keeps them. */
  let viewerAttached = false
  let commandsRefreshedOnWatchAt = Number.NEGATIVE_INFINITY
  /** Whether the mod has said anything at all, which is when its reads are first worth making. */
  let primed = false

  const framesSubject = (): string => agentStreamFramesSubject(session)

  const publishSettings = (): void => {
    publisher.setSettings(framesSubject(), stream.settings(model, configRows))
  }

  const publishCommands = (): void => {
    if (listedCommands !== undefined) publisher.setCommands(framesSubject(), listedCommands)
  }

  /** Read the session's model and its `/config` rows, and publish what they say. One read at a
   *  time: a watch arrives every ten seconds and the answers rarely change. */
  const refreshSettings = (): void => {
    if (stopped || readingSettings || !link.connected()) return
    readingSettings = true
    void Promise.all([
      link.request("session.model", undefined, { timeoutMs: AGENT_STREAM_CALL_TIMEOUT_MS }),
      link.request("config.list", undefined, { timeoutMs: AGENT_STREAM_CALL_TIMEOUT_MS }),
    ])
      .then(([answeredModel, rows]) => {
        model = answeredModel
        configRows = rows
        if (!stopped) publishSettings()
      })
      .catch((error: unknown) => {
        warn(`the live view could not read this session's model — ${messageFor(error)}`)
      })
      .finally(() => {
        readingSettings = false
      })
  }

  const refreshCommands = (): void => {
    if (stopped || readingCommands || !link.connected()) return
    readingCommands = true
    void link
      .request("command.list", undefined, { timeoutMs: AGENT_STREAM_CALL_TIMEOUT_MS })
      .then((commands) => {
        listedCommands = claudeAgentStreamCommands(commands)
        if (!stopped) publishCommands()
      })
      .catch((error: unknown) => {
        warn(`the live view could not read this session's commands — ${messageFor(error)}`)
      })
      .finally(() => {
        readingCommands = false
      })
  }

  const pump = async (taken: ChannelTopicSubscription, forSession: string): Promise<void> => {
    try {
      for await (const message of taken) {
        // Any client on the bus can publish here. A `null` payload parses cleanly and then
        // throws on `.type`, and a reply over the server's max_payload throws out of the
        // publish; either would end the iterator and take the session deaf with it.
        try {
          const parsed: unknown = JSON.parse(decoder.decode(message.data))
          if (typeof parsed !== "object" || parsed === null) continue
          const request = parsed as AgentStreamControlMessage
          if (request.type !== "replay" && request.type !== "watch") continue
          refreshSettings()
          if (
            request.type === "replay" ||
            !viewerAttached ||
            now() - commandsRefreshedOnWatchAt >= AGENT_STREAM_WATCH_INTERVAL_MS
          ) {
            refreshCommands()
            if (request.type === "watch" && viewerAttached) commandsRefreshedOnWatchAt = now()
          }
          // Taken before this message arms anything, so a bare replay from a client that never
          // attached a viewer gets an empty history rather than the ring.
          const reply = message.reply
          const answer =
            request.type === "replay" && reply !== undefined
              ? JSON.stringify(publisher.replay(forSession))
              : undefined
          const arming = !publisher.watched
          publisher.noteViewer()
          viewerAttached = true
          if (arming) publisher.publishState(agentStreamFramesSubject(forSession))
          if (answer === undefined || reply === undefined) continue
          connection.publish(reply, encoder.encode(answer))
        } catch (error) {
          warn(`a live view control message failed — ${messageFor(error)}`)
        }
      }
    } catch {
      // A dead iterator is a dropped connection, taken again below.
    }
    if (subscription === taken) subscription = undefined
    if (stopped || session !== forSession) return
    const retry = setTimeout(() => {
      if (stopped || session !== forSession) return
      subscribe()
    }, AGENT_STREAM_RESUBSCRIBE_MS)
    retry.unref()
  }

  function subscribe(): void {
    if (stopped) return
    subscription?.unsubscribe()
    const taken = connection.subscribe(agentStreamControlSubject(session))
    subscription = taken
    void pump(taken, session)
  }

  // A `/clear` ends the conversation and the process goes on under a new id: the ring holds the
  // previous one, and a viewer opening the new id must not be served it.
  link.onSessionId((next: string) => {
    if (stopped || !options.followsSessionId || next === session) return
    session = next
    publisher.reset()
    stream.reset()
    viewerAttached = false
    commandsRefreshedOnWatchAt = Number.NEGATIVE_INFINITY
    // `reset` dropped the two header frames with the conversation; what the mod already said
    // still holds, so they are put back under the new id rather than read again.
    publishSettings()
    publishCommands()
    subscribe()
  })

  const reading = (async (): Promise<void> => {
    for await (const event of link.events) {
      if (!primed) {
        primed = true
        refreshSettings()
        refreshCommands()
      }
      if (event.kind === "model") model = event.model
      // Before the stream records it: an event can be the row a person's message turned out to
      // be, and that row is tagged with the message it is.
      options.observe?.(event)
      stream.record(framesSubject(), event)
      // A step is the one event that can change the thinking level, and the model event is the
      // mod's own report of a model change: neither is worth a read of the mod to notice.
      if (event.kind === "step" || event.kind === "model") publishSettings()
    }
  })()
  void reading.catch((error: unknown) => {
    warn(`the live view stopped reading this session's events — ${messageFor(error)}`)
  })

  subscribe()

  return {
    refresh(): void {
      refreshSettings()
    },
    stop(): void {
      if (stopped) return
      stopped = true
      subscription?.unsubscribe()
      subscription = undefined
    },
  }
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
  // `CLAUDE_PLUGIN_DATA` is read here, rather than through `pluginStateDirectory` below, because
  // that throws when it is unset and runs only after the Envoy-off return: a session with Envoy
  // off and no plugin data must still serve.
  const pluginData = configuredValue(process.env["CLAUDE_PLUGIN_DATA"])
  const title = await sessionTitle(
    {
      ENVOY_SESSION_TITLE: process.env["ENVOY_SESSION_TITLE"],
      TMUX: process.env["TMUX"],
      TMUX_PANE: process.env["TMUX_PANE"],
    },
    directory,
    {
      chosen: (sessionId) =>
        pluginData === undefined
          ? undefined
          : readChosenTitleSync(sessionTitleFile(pluginData, sessionId)),
    },
  )
  const stampTitle = (sessionId: string, value: string): void => {
    writeSessionTitle(dispatchSessionDirectory(process.env, sessionId), value)
  }

  // Only the `dispatch` command reads the stamp, so nothing is written while the session starts with
  // Dispatch off. Nothing is written under the QA override ENVOY_SESSION_ID either, although that
  // command acts as the override's id when it is set (`runDispatchCli` in envoy-client): a QA
  // session's Dispatch writes carry no session title unless its shell sets DISPATCH_SESSION_TITLE.
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
  // In a Legion pane a person's message goes to the bridge's inbox, which makes it a user turn;
  // in a terminal session everything this server writes goes through the mod.
  const inboxSocket = configuredValue(process.env[INBOX_VARIABLE])
  // The plugin's own bundle, which Claude Code runs from its plugin cache when installed.
  const bundlePath = process.argv[1] ?? ""
  // One connection to the bridge carries every line this server writes into the session: each
  // Envoy delivery, and the reminders and notices the phase endpoint writes.
  const inbox = inboxSocket === undefined ? undefined : createInboxNotifier(inboxSocket)

  const server = new Server(MCP_SERVER_INFO, {
    capabilities: { tools: {} },
    instructions: CHANNEL_INSTRUCTIONS,
  })
  const client = createEnvoyClient({
    baseUrl: envoy.url,
    fetch: globalThis.fetch,
    token: envoy.token,
  })
  // INFRA-404: this process, as the listener knows it. Minted once here and sent on every
  // registration and every deregistration, so the listener can tell the entry this process made
  // from one another process made under the same session id: a soft role claim against a live
  // predecessor goes only to the instance that registered it, and a deregistration removes only
  // this instance's entry. One per process, not per session id: this server drives one session
  // at a time, and a restart is a new instance with a new id.
  const clientInstanceID = newClientInstanceID()
  let runtime: ChannelToolRuntime | undefined
  let claim: LegionClaim | undefined
  const daemon = pane === undefined ? undefined : createLegionDaemonClient(pane.daemonUrl)
  // INFRA-220: the phase reminder's Claude Code host (`./legion-phase-hook`), which Claude Code's
  // own hooks drive through the `legion_phase_hook` tool. It is started here, beside the claim it
  // gates on, and restores this session's record as it starts, before any hook can be answered.
  const phaseLog = paneReminderLog(logLegion)
  // The claim's boot, for the endpoint's resume reminder and its recreated-workspace notice: both
  // need the claim, and the claim starts further down, once the channel session exists.
  const claimBooted = Promise.withResolvers<void>()
  const phaseHook =
    pane === undefined || daemon === undefined
      ? undefined
      : createPhaseHookEndpoint({
          stateFile: phaseStallFile(pane.configDir, identity.id),
          claim: () => claim?.capability(),
          booted: claimBooted.promise,
          readState: (signal) => daemon.state({ signal }),
          inbox: inbox === undefined ? undefined : (text) => inbox.notification({ content: text }),
          publish: client.publish,
          env: process.env,
          log: phaseLog,
        })
  const toolDefinitions = [
    ...channelToolDefinitions(),
    ...(daemon === undefined ? [] : [legionToolDefinition(), phaseHookToolDefinition()]),
  ]
  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: toolDefinitions }))
  server.setRequestHandler(CallToolRequestSchema, async (request, extra) => {
    if (phaseHook !== undefined && request.params.name === PHASE_HOOK_TOOL_NAME) {
      return mcpResult(await phaseHook.handle(request.params.arguments))
    }
    if (daemon !== undefined && request.params.name === LEGION_TOOL_NAME) {
      const legionRuntime: LegionToolRuntime = {
        daemon,
        claim: () => claim?.capability(),
        // The pane's own environment and PATH's jj, and the Dispatch this session's envoy.json
        // names, which the pane's `dispatch handoff-write` wrote the record to.
        handoffReport: paneHandoffReport(configFile, process.env, directory),
        branchHead: () => findPushedBranchHead({ env: process.env }),
      }
      // The request's signal aborts on the client's notifications/cancelled, which ends a wait
      // for a restarting daemon.
      const answer = await executeLegionTool(legionRuntime, request.params.arguments, extra.signal)
      // A call that returned without throwing: a completion, and a backward move, which hands the
      // issue to another phase. Either way this session's phase is no longer open, and the
      // reminder must stop — the same two operations the Oh My Pi host closes its phase on
      // (`createLegionTool` in `packages/pi-legion/src/tools.ts`). `ready_for_update` ends no
      // phase: it answers the pod's update task, and the worker then waits for its process to be
      // replaced, so the `Stop` of the turn it answered in blocks nothing (`updateAnswered`,
      // `./legion-phase-hook`), as the Oh My Pi host's `onUpdateAnswered` does.
      const op = request.params.arguments?.["op"]
      if (op === "handoff_complete" || op === "request_backward_move") phaseHook?.phaseClosed()
      else if (op === "ready_for_update") phaseHook?.updateAnswered()
      return mcpResult(answer)
    }
    if (runtime === undefined) throw new Error("Envoy channel server is still starting")
    return mcpResult(await executeEnvoyTool(runtime, request.params.name, request.params.arguments))
  })
  // INFRA-1026: the link to this session's Claude Code mod, bound below once the MCP transport is
  // up. It is this process's own socket, closed with the server. `linked` resolves with it for
  // the live view, which also needs the NATS connection this function opens further down.
  let modLink: ModLink | undefined
  const linked = Promise.withResolvers<ModLink>()
  // The Agents page's live view of this session, once both the link and the connection are up.
  let agentStream: AgentStreamRunner | undefined
  // The session itself, as this server drives it (`./mod-session`). The link binds after this
  // server starts and can fail to bind at all, so every call goes through the holder above: a
  // session with no mod answers none of them, which is what "no mod" means here.
  const claude: ModSession = modSession({
    link: {
      request: (call, args, requestOptions) => {
        const link = modLink
        if (link === undefined) {
          return Promise.reject(new Error("this session has no link to Claude Code"))
        }
        return link.request(call, args, requestOptions)
      },
      connected: () => modLink?.connected() === true,
    },
    warn: (text) => process.stderr.write(`envoy-channel: ${text}\n`),
  })
  // Every line this server writes into the session: the bridge's inbox in a Legion pane, and the
  // mod's own `session.append` in a terminal session, where the model reads the row at its next
  // request. The endpoint is told every line written for a delivery: the `UserPromptSubmit` that
  // carries one back is that delivery's turn, not a fresh assignment, and this server, which
  // writes them all, is the only place that knows.
  const writer: ChannelNotifier = inbox ?? {
    notification: async ({ content, blocks }) => {
      await claude.show(blocks ?? [{ type: "text", text: content }])
    },
  }
  const notifier: ChannelNotifier =
    phaseHook === undefined
      ? writer
      : {
          notification: async (notification) => {
            phaseHook.deliveryWritten(notification.content)
            await writer.notification(notification)
          },
        }
  const capabilities = (): readonly DeliveryCapability[] =>
    sessionCapabilities({ pane: inboxSocket !== undefined, mod: modLink?.connected() === true })
  const booting = new AbortController()

  const stopped = Promise.withResolvers<void>()
  let stopping = false
  let session: ChannelSession | undefined
  const stop = (): void => {
    if (stopping) return
    stopping = true
    booting.abort()
    // A reminder waiting on a 15-minute WAITING wait holds no shutting-down server open.
    phaseHook?.stop()
    void modLink?.close().catch(() => undefined)
    agentStream?.stop()
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
  // The mod (`../hooks/mod.ts`) probes this socket from its own `session.start` and gives up
  // after about half a minute, so it is bound as early as the transport allows. Started, not
  // awaited: the transport is already reading stdin, and an await here would let the client's
  // first tool call arrive before the session below exists. A session whose link cannot bind
  // runs exactly as it does today, the mod finding nothing and staying silent.
  void startModLink({
    // The id Claude Code gave this process, which is what the mod reads as `$.session.id()`;
    // the ENVOY_SESSION_ID override names an Envoy identity, never a Claude Code session.
    sessionId: claudeSessionId({ CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"] }),
    env: process.env,
  }).then(
    (link) => {
      modLink = link
      // The server was already stopping when the bind finished: nothing will read this link.
      if (stopping) void link.close().catch(() => undefined)
      else linked.resolve(link)
    },
    (error: unknown) => {
      process.stderr.write(
        `envoy-channel: no link to this session's Claude Code mod — ${messageFor(error)}; the session runs on without one\n`,
      )
    },
  )

  /** The record of the session id now bound, which the live view reads a row's tag from. */
  let turns: UserTurnState | undefined
  /**
   * The router of a person's messages, built again for each session id the session binds
   * (`ChannelSessionOptions.router`). Everything it needs that does not move with the id - the
   * Claude Code session, Dispatch, where a pane's message goes - is this process's.
   */
  const router = (state: UserTurnState): UserTurnRouter => {
    turns = state
    const options: UserTurnRouterOptions = {
      dispatch: (ms) => {
        const active = activeDispatchSettings(
          sessionDispatchSettings(configFile, process.env, { cwd: directory }).dispatch,
        )
        return active === null
          ? undefined
          : new DispatchClient(active.url, active.token, fetch, AbortSignal.timeout(ms))
      },
      refreshSettings: () => agentStream?.refresh(),
      reply: (delivery, result) => postDispatchReply(identity, configFile, delivery, result),
      session: claude,
      sessionId: () => identity.id,
      state,
      warn: (text) => process.stderr.write(`envoy-channel: ${text}\n`),
      // A pane's person message is a line of the bridge's inbox, which makes it a user turn;
      // the phase endpoint is told of it as it is told of every line this server writes.
      ...(inboxSocket === undefined ? {} : { inbox: (input) => notifier.notification(input) }),
    }
    return createUserTurnRouter(options)
  }

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
      connection = await connect(
        natsConnectionOptions({
          servers: inbound.natsUrls,
          name: `claude-envoy-channel-${identity.id}`,
          seed: inbound.natsSeed,
        }),
      )
      session = await startChannelSession({
        identity,
        connection,
        notifier,
        capabilities,
        router,
        client,
        heartbeatMs: envoy.heartbeatMs,
        configFile,
        stateDirectory,
        clientInstanceID,
        title,
        pluginVersion: installedPluginVersion(bundlePath) ?? "source",
        ...(stamping ? { stampTitle } : {}),
        // The QA override names a fixed identity; only a Claude-minted id follows `/clear`.
        ...(configuredValue(overrideSessionId) === undefined ? { handoffPid: process.ppid } : {}),
      })
      // INFRA-1026: the Agents page's live view, once this session's mod has a link to bind to.
      // It needs the bus as well as the link, so it starts here rather than beside the bind; a
      // session whose link never binds never starts one, and runs as it did before.
      const bus = connection
      const live = session
      void linked.promise.then((link) => {
        if (stopping) return
        agentStream = startAgentStream({
          connection: bus,
          // Only a Claude-minted id follows `/clear`; the override names a fixed identity.
          followsSessionId: configuredValue(overrideSessionId) === undefined,
          link,
          sessionID: identity.id,
          // The link's events have one consumer, and the router needs them too.
          observe: (event) => live.observe(event),
          tagFor: (row) => turns?.messageOf(row),
        })
        // The session takes messages once the mod is there, and says so: a terminal session
        // advertises nothing until then, and a pane gains the side turn a BTW is.
        link.onSessionId(() => live.capabilitiesChanged())
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
      // What `claimSession.bootstrap` returning is to the Oh My Pi host: the point the phase
      // endpoint may read the claim, arm its resume reminder and tell the worker its workspace
      // was recreated.
      void claim.booted.then(claimBooted.resolve)
    }
    if (stopping) await session.shutdown()
    await stopped.promise
  } finally {
    await modLink?.close().catch(() => undefined)
    if (session === undefined && connection !== undefined) {
      await connection.drain().catch(() => undefined)
      await connection.close().catch(() => undefined)
    }
    // Release stdin so the process can exit once the session is deregistered,
    // even when the parent has not closed its end yet.
    await server.close().catch(() => undefined)
  }
}
