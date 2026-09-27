import { rememberBounded } from "@legion/envoy-client/delivery"
import { messageFor } from "@legion/envoy-client/errors"
import { expandSubscriptionTopics } from "@legion/envoy-client/transport"
import { z } from "zod"

/** A message as the broker yields it (a nats.js `Msg` satisfies this). */
export interface ChannelBrokerMessage {
  readonly subject: string
  readonly data: Uint8Array
  readonly reply?: string
}

/** A broker message as the forwarder delivers it: decoded once and classified. */
export interface ChannelInboundMessage extends ChannelBrokerMessage {
  /** `data` decoded as UTF-8 once, at the broker boundary. */
  readonly raw: string
  /** The event already reached the channel queue; direct request-reply still needs its receipt. */
  readonly duplicate?: true
  /**
   * The topic the envelope itself names. It differs from `subject` when the listener forwarded
   * a lane (a role topic) onto the direct subject and is waiting for a receipt.
   */
  readonly envelopeTopic?: string
}

export interface ChannelTopicSubscription extends AsyncIterable<ChannelBrokerMessage> {
  unsubscribe(): void
}

/** The NATS connection surface used by a channel session. */
export interface ChannelForwarderConnection {
  subscribe(subject: string): ChannelTopicSubscription
  publish(subject: string, data: Uint8Array): void
  flush(): Promise<void>
  isClosed(): boolean
  drain(): Promise<void>
  close(): Promise<void>
}

export interface ChannelForwarder {
  /** Start delivering a topic; a topic already followed is left alone. */
  follow(topic: string): void
  /** Stop delivering the given topics, or every topic when the list is empty. */
  unfollow(topics: readonly string[]): Promise<void>
  /** Topics currently delivered, in follow order. */
  topics(): readonly string[]
  /** True once the connection is closed for good. */
  isClosed(): boolean
  /** Stop every subscription and drain the NATS connection; never rejects. */
  close(): Promise<void>
}

export interface ChannelForwarderOptions {
  readonly deliver: (message: ChannelInboundMessage) => Promise<void>
  /** How long close waits for a broker drain before closing outright. */
  readonly drainTimeoutMs?: number
}

interface Following {
  readonly subscription: ChannelTopicSubscription
  readonly done: Promise<void>
}

// Envoy envelopes created today always carry an event id. The dedupe key keeps
// older producer versions from replaying a single logical event into Claude.
const DeliveryIdentity = z.object({
  event_id: z.string().min(1).optional(),
  dedupe_key: z.string().min(1).optional(),
  topic: z.string().min(1).optional(),
})

const decoder = new TextDecoder()
const SEEN_KEYS_LIMIT = 1_000
const DEFAULT_DRAIN_TIMEOUT_MS = 1_000

function deliveryIdentity(raw: string): z.infer<typeof DeliveryIdentity> | undefined {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return undefined
  }
  const identity = DeliveryIdentity.safeParse(parsed)
  return identity.success ? identity.data : undefined
}

function report(what: string, error: unknown): void {
  process.stderr.write(`envoy-channel: ${what} — ${messageFor(error)}\n`)
}

/**
 * One channel process receives both its direct Envoy route and every followed
 * topic. It deduplicates at the broker boundary so an event covered by several
 * patterns still becomes one Claude Code channel notification.
 */
export function createChannelForwarder(
  connection: ChannelForwarderConnection,
  options: ChannelForwarderOptions,
): ChannelForwarder {
  const following = new Map<string, Following>()
  const seen = new Set<string>()
  const drainTimeoutMs = options.drainTimeoutMs ?? DEFAULT_DRAIN_TIMEOUT_MS

  const deliver = async (topic: string, subscription: ChannelTopicSubscription): Promise<void> => {
    try {
      for await (const message of subscription) {
        try {
          const raw = decoder.decode(message.data)
          const identity = deliveryIdentity(raw)
          const key = identity?.event_id ?? identity?.dedupe_key
          const duplicate = key !== undefined && seen.has(key)
          if (key !== undefined && !duplicate) rememberBounded(seen, key, SEEN_KEYS_LIMIT)
          // A nats.js Msg exposes subject/data/reply through prototype getters,
          // which an object spread would silently drop; copy the fields by name.
          await options.deliver({
            subject: message.subject,
            data: message.data,
            raw,
            // nats.js MsgImpl.reply is a getter returning "" when unset; absent means no reply address.
            ...(message.reply ? { reply: message.reply } : {}),
            ...(identity?.topic === undefined ? {} : { envelopeTopic: identity.topic }),
            ...(duplicate ? { duplicate: true } : {}),
          })
        } catch (error) {
          report(`could not deliver a message on ${message.subject}`, error)
        }
      }
    } catch (error) {
      report(`delivery from ${topic} stopped`, error)
    }
  }

  const stop = async (topics: readonly string[]): Promise<void> => {
    const stopping = topics.flatMap((topic) => {
      const entry = following.get(topic)
      if (entry === undefined) return []
      following.delete(topic)
      entry.subscription.unsubscribe()
      return [entry.done]
    })
    await Promise.all(stopping)
  }

  return {
    follow(topic) {
      for (const subject of expandSubscriptionTopics([topic])) {
        if (following.has(subject)) continue
        const subscription = connection.subscribe(subject)
        following.set(subject, { subscription, done: deliver(subject, subscription) })
      }
    },
    unfollow(topics) {
      return stop(topics.length === 0 ? [...following.keys()] : expandSubscriptionTopics(topics))
    },
    topics() {
      return [...following.keys()]
    },
    isClosed() {
      return connection.isClosed()
    },
    async close() {
      await stop([...following.keys()])
      if (connection.isClosed()) return
      const timeout = Promise.withResolvers<void>()
      const timer = setTimeout(timeout.resolve, drainTimeoutMs)
      try {
        await Promise.race([connection.drain(), timeout.promise])
      } catch (error) {
        report("draining the broker connection failed", error)
      }
      clearTimeout(timer)
      if (connection.isClosed()) return
      try {
        await connection.close()
      } catch (error) {
        report("closing the broker connection failed", error)
      }
    },
  }
}
