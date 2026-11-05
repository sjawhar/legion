// The Agents page's live view of one Claude Code session (INFRA-1026): the mod's events as the
// conversation, the two header frames read off the mod's own `$` calls, and the control subject a
// viewer takes this session on. It is driven by the channel server (`./envoy-channel-server`),
// which gives it the link and the session's NATS connection, and it reaches nothing of that
// server's: a subject is a string, and every collaborator arrives as an option.
import {
  AGENT_STREAM_WATCH_INTERVAL_MS,
  type AgentStreamCommand,
  type AgentStreamControlMessage,
  agentStreamControlSubject,
  agentStreamFramesSubject,
} from "@legion/contracts"
import { messageFor } from "@legion/envoy-client/errors"
import { AgentStreamPublisher } from "@legion/pi-shared/agent-stream"
import type { ModConfigRow, ModEvent } from "../hooks/mod-protocol"
import { claudeAgentStreamCommands, createClaudeAgentStream } from "./agent-stream-claude"
import type { ChannelForwarderConnection, ChannelTopicSubscription } from "./channel-forwarder"
import type { ModLink } from "./mod-link"

const encoder = new TextEncoder()
const decoder = new TextDecoder()

/** How long the live view waits for the mod to answer one of its reads (`$.session.model()`,
 *  `$.config.list()`, `$.command.list()`). Each is served out of the session's own memory, so a
 *  wait this long means the mod is gone rather than busy, and the header keeps its last values. */
const AGENT_STREAM_CALL_TIMEOUT_MS = 5_000
/** How long the live view waits before taking its control subject again after the subscription's
 *  iterator ended, which is what a dropped connection looks like from here. */
const AGENT_STREAM_RESUBSCRIBE_MS = 1_000

export interface AgentStreamOptions {
  /** The link to this session's Claude Code mod (`./mod-link`): its events are the conversation,
   *  and its `$` calls are what the header's two frames are built from. */
  readonly link: ModLink
  /** The session's own NATS connection: the frames go out on it, and the control subject is
   *  taken on it. The session's NATS user may already publish and subscribe to both. */
  readonly connection: ChannelForwarderConnection
  /** The session id the Agents page knows this session by, which both subjects name. */
  readonly sessionId: string
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
  let session = options.sessionId
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
