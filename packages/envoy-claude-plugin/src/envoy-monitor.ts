import { connect, StringCodec } from "nats"

import { formatMonitorEvent } from "./monitor-event"
import { monitorSessionId } from "./monitor-identity"
import { subscriptionTopics } from "./subscription-topics"

const NATS_URL = "nats://envoy-nats:4222"

function terminationSignal(): Promise<void> {
  return new Promise((resolve) => {
    const stop = (): void => {
      process.off("SIGINT", stop)
      process.off("SIGTERM", stop)
      resolve()
    }

    process.once("SIGINT", stop)
    process.once("SIGTERM", stop)
  })
}

export async function runEnvoyMonitor(): Promise<void> {
  const environment = {
    ENVOY_SESSION_ID: process.env["ENVOY_SESSION_ID"],
    CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
  }
  const sessionId = monitorSessionId(environment)
  const topics = subscriptionTopics({
    sessionId,
    additionalTopics: process.env["ENVOY_TOPICS"],
  })
  const connection = await connect({
    servers: process.env["ENVOY_NATS_URL"] ?? NATS_URL,
    name: `claude-envoy-${sessionId}`,
  })
  const codec = StringCodec()
  const subscriptions = topics.map((topic) => connection.subscribe(topic))
  const forwarding = subscriptions.map(async (subscription) => {
    for await (const message of subscription) {
      process.stdout.write(`${formatMonitorEvent(codec.decode(message.data))}\n`)
    }
  })

  process.stdout.write(`[envoy] connected session=${sessionId} topics=${topics.join(",")}\n`)

  try {
    await terminationSignal()
  } finally {
    for (const subscription of subscriptions) {
      subscription.unsubscribe()
    }
    await Promise.all(forwarding)
    await connection.drain()
  }
}
