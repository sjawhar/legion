import type { RelayHeaders } from "@legion/envoy-client/consumer-delivery"
import { DedupeIdentitySchema } from "@legion/envoy-client/delivery"
import { messageFor } from "@legion/envoy-client/errors"
import { expandSubscriptionTopics } from "@legion/envoy-client/transport"
import { z } from "zod"

/** A message as the broker yields it (a nats.js `Msg` satisfies this). */
export interface ChannelBrokerMessage {
  readonly subject: string
  readonly data: Uint8Array
  /**
   * The frame's reply subject, which only a relay frame carries anything useful in: the relay's
   * own answer token (`RelayMessage`, consumer delivery).
   */
  readonly reply?: string
  /** The message's NATS headers; only a relay frame (consumer delivery) carries any read here. */
  readonly headers?: RelayHeaders
}

export interface ChannelTopicSubscription extends AsyncIterable<ChannelBrokerMessage> {
  unsubscribe(): void
}

/** A nats.js connection status event; only its `type` is read. */
export interface ChannelConnectionStatus {
  readonly type: string
}

/** The NATS connection surface used by a channel session. */
export interface ChannelForwarderConnection {
  subscribe(subject: string): ChannelTopicSubscription
  publish(subject: string, data: Uint8Array): void
  flush(): Promise<void>
  isClosed(): boolean
  drain(): Promise<void>
  close(): Promise<void>
  /** The connection's status events (nats.js `status()`), which end when it closes. */
  status(): AsyncIterable<ChannelConnectionStatus>
}

/**
 * The topics this session follows, and the one broker connection it holds. Nothing is subscribed
 * for a topic: the listener relays every followed subject from the session's own durable consumer
 * (`ConsumerDelivery`), so a topic is followed by registering it, and the only subscription the
 * process holds is that consumer's relay subject.
 */
export interface ChannelForwarder {
  /** Start following a topic; one already followed is left alone. */
  follow(topic: string): void
  /** Stop following the given topics, or every topic when the list is empty. */
  unfollow(topics: readonly string[]): void
  /** Topics currently followed, in follow order. */
  topics(): readonly string[]
  /** True once the connection is closed for good. */
  isClosed(): boolean
  /** Drain the NATS connection; never rejects. */
  close(): Promise<void>
}

export interface ChannelForwarderOptions {
  /** How long close waits for a broker drain before closing outright. */
  readonly drainTimeoutMs?: number
}

// A delivery's identity fields parse independently: a field the shared rule refuses identifies
// nothing, and its valid sibling can still recognise a repeat. `topic` follows the same rule
// before it is exposed as the envelope topic.
const DeliveryIdentity = DedupeIdentitySchema.extend({
  event_id: DedupeIdentitySchema.shape.event_id.catch(undefined),
  dedupe_key: DedupeIdentitySchema.shape.dedupe_key.catch(undefined),
  source: DedupeIdentitySchema.shape.source.catch(undefined),
  source_event_id: DedupeIdentitySchema.shape.source_event_id.catch(undefined),
  topic: z.string().min(1).optional().catch(undefined),
})

const DEFAULT_DRAIN_TIMEOUT_MS = 1_000

/** A frame's identity fields, each kept only when valid; undefined for a frame that is not JSON. */
export function deliveryIdentity(raw: string): z.infer<typeof DeliveryIdentity> | undefined {
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
 * One channel process follows its direct Envoy route and every topic a human or a tool added to
 * it. Each is a concrete subject the registration carries (`expandSubscriptionTopics`), and the
 * listener relays all of them from the session's own consumer, so nothing here subscribes: the
 * set is what a registration advertises and what the relay is filtered on.
 */
export function createChannelForwarder(
  connection: ChannelForwarderConnection,
  options: ChannelForwarderOptions = {},
): ChannelForwarder {
  const following = new Set<string>()
  const drainTimeoutMs = options.drainTimeoutMs ?? DEFAULT_DRAIN_TIMEOUT_MS

  return {
    follow(topic) {
      for (const subject of expandSubscriptionTopics([topic])) following.add(subject)
    },
    unfollow(topics) {
      if (topics.length === 0) {
        following.clear()
        return
      }
      for (const subject of expandSubscriptionTopics(topics)) following.delete(subject)
    },
    topics() {
      return [...following]
    },
    isClosed() {
      return connection.isClosed()
    },
    async close() {
      following.clear()
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
