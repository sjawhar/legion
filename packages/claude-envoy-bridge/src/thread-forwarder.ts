// Delivery for the topics a Claude session subscribes to. The Envoy listener
// pushes nothing to a self-subscribed session with no serve port — it expects
// the session to consume NATS itself — and the monitor consumes only
// notifications.agent.<session-id>, fixed at spawn. The MCP server, which is
// long-lived, therefore subscribes NATS to each followed topic and republishes
// every envelope, byte for byte, on the session's agent subject, where the
// monitor already renders it as a native peer message.
//
// The forwarder has the same properties as pi-envoy's pump (extensions/envoy.ts):
// an envelope is delivered once by its dedupe_key however many followed
// patterns match it, a failure on one message or one subscription never ends
// delivery for the rest, and shutdown never hangs on a dead broker.

import { agentSubject } from "@legion/contracts"
import { z } from "zod"

export interface ForwardedMessage {
  readonly subject: string
  readonly data: Uint8Array
}

export interface TopicSubscription extends AsyncIterable<ForwardedMessage> {
  unsubscribe(): void
}

/** The slice of a NATS connection the forwarder needs; `nats`'s NatsConnection satisfies it. */
export interface ForwarderConnection {
  subscribe(subject: string): TopicSubscription
  publish(subject: string, data: Uint8Array): void
  /** True once nats.js has given up on the connection for good; it reconnects through outages itself. */
  isClosed(): boolean
  drain(): Promise<void>
  close(): Promise<void>
}

export interface ThreadForwarder {
  /** Start forwarding a topic; a topic already followed is left alone. */
  follow(topic: string): void
  /** Stop forwarding the given topics, or every topic when the list is empty. */
  unfollow(topics: readonly string[]): Promise<void>
  /** Topics currently forwarded, in follow order. */
  topics(): readonly string[]
  /** True once the connection is closed for good: nothing followed here is delivered any more. */
  isClosed(): boolean
  /** Stop everything and drain the connection; never rejects. */
  close(): Promise<void>
}

export interface ThreadForwarderOptions {
  /** How long `close()` waits for the drain before closing the connection outright. */
  readonly drainTimeoutMs?: number
}

interface Following {
  readonly subscription: TopicSubscription
  readonly done: Promise<void>
}

// Every Envoy envelope carries a dedupe_key (event_id is the fallback). A
// message with neither cannot be told apart from its own relay, so it is
// never forwarded.
const DedupeIdentity = z.object({
  dedupe_key: z.string().min(1).optional(),
  event_id: z.string().min(1).optional(),
})

const decoder = new TextDecoder()

function dedupeKey(data: Uint8Array): string | undefined {
  let parsed: unknown
  try {
    parsed = JSON.parse(decoder.decode(data))
  } catch {
    return undefined
  }
  const identity = DedupeIdentity.safeParse(parsed)
  if (!identity.success) return undefined
  return identity.data.dedupe_key ?? identity.data.event_id
}

/** The seen-set bound pi-envoy uses; the oldest key is evicted first. */
const SEEN_KEYS_LIMIT = 1000

const DEFAULT_DRAIN_TIMEOUT_MS = 1_000

function report(what: string, error: unknown): void {
  process.stderr.write(
    `envoy-mcp: ${what} — ${error instanceof Error ? error.message : String(error)}\n`,
  )
}

export function createThreadForwarder(
  connection: ForwarderConnection,
  sessionId: string,
  options: ThreadForwarderOptions = {},
): ThreadForwarder {
  const inbox = agentSubject(sessionId)
  const following = new Map<string, Following>()
  const seen = new Set<string>()
  const drainTimeoutMs = options.drainTimeoutMs ?? DEFAULT_DRAIN_TIMEOUT_MS

  const remember = (key: string): void => {
    seen.add(key)
    if (seen.size > SEEN_KEYS_LIMIT) {
      const oldest = seen.values().next()
      if (!oldest.done) seen.delete(oldest.value)
    }
  }

  const forward = async (topic: string, subscription: TopicSubscription): Promise<void> => {
    try {
      for await (const message of subscription) {
        try {
          const key = dedupeKey(message.data)
          if (key === undefined) {
            process.stderr.write(
              `envoy-mcp: dropped a message on ${message.subject}: no dedupe_key or event_id\n`,
            )
            continue
          }
          // An envelope is republished at most once, whichever followed pattern
          // delivers it first — so two patterns covering one subject deliver it
          // once. An envelope that arrives on the inbox itself is remembered but
          // never republished: a pattern covering the inbox would otherwise
          // echo forever, and two sessions whose patterns cover each other's
          // inbox would relay the same envelope back and forth.
          if (seen.has(key)) continue
          remember(key)
          if (message.subject === inbox) continue
          connection.publish(inbox, message.data)
        } catch (error) {
          // One message that cannot be republished must not end the topic's delivery.
          report(`could not forward a message on ${message.subject} to ${inbox}`, error)
        }
      }
    } catch (error) {
      // The subscription itself failed (e.g. a permissions violation on the
      // subject); the other topics keep flowing and close() still resolves.
      report(`forwarding ${topic} stopped`, error)
    }
  }

  const stop = async (topics: readonly string[]): Promise<void> => {
    const stopping = topics.flatMap((topic) => {
      const entry = following.get(topic)
      if (!entry) return []
      following.delete(topic)
      entry.subscription.unsubscribe()
      return [entry.done]
    })
    await Promise.all(stopping)
  }

  return {
    follow(topic) {
      if (following.has(topic)) return
      const subscription = connection.subscribe(topic)
      following.set(topic, { subscription, done: forward(topic, subscription) })
    },
    unfollow(topics) {
      return stop(topics.length === 0 ? [...following.keys()] : topics)
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
      // Bound the drain, as pi-envoy does on shutdown: while the broker is
      // unreachable the drain's flush cannot complete — it gives up, without
      // closing, only when the next reconnect attempt fails — and the flush
      // is best-effort anyway. close() then ends nats.js's reconnect loop,
      // which would otherwise keep this process alive.
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
