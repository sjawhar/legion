// Delivery for the topics a Claude session subscribes to. The Envoy listener
// pushes nothing to a self-subscribed session with no serve port — it expects
// the session to consume NATS itself — and the monitor consumes only
// notifications.agent.<session-id>, fixed at spawn. The MCP server, which is
// long-lived, therefore subscribes NATS to each followed topic and republishes
// every envelope, byte for byte, on the session's agent subject, where the
// monitor already renders it as a native peer message.

import { agentSubject } from "@legion/contracts"

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
  drain(): Promise<void>
}

export interface ThreadForwarder {
  /** Start forwarding a topic; a topic already followed is left alone. */
  follow(topic: string): void
  /** Stop forwarding the given topics, or every topic when the list is empty. */
  unfollow(topics: readonly string[]): Promise<void>
  /** Topics currently forwarded, in follow order. */
  topics(): readonly string[]
  /** Stop everything and drain the connection. */
  close(): Promise<void>
}

interface Following {
  readonly subscription: TopicSubscription
  readonly done: Promise<void>
}

export function createThreadForwarder(
  connection: ForwarderConnection,
  sessionId: string,
): ThreadForwarder {
  const inbox = agentSubject(sessionId)
  const following = new Map<string, Following>()

  const forward = async (subscription: TopicSubscription): Promise<void> => {
    for await (const message of subscription) {
      // A pattern that covers the inbox itself would otherwise echo forever.
      if (message.subject === inbox) continue
      connection.publish(inbox, message.data)
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
      following.set(topic, { subscription, done: forward(subscription) })
    },
    unfollow(topics) {
      return stop(topics.length === 0 ? [...following.keys()] : topics)
    },
    topics() {
      return [...following.keys()]
    },
    async close() {
      await stop([...following.keys()])
      await connection.drain()
    },
  }
}
