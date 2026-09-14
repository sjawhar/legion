import { expect, test } from "bun:test"
import { connect } from "nats"
import { createChannelForwarder } from "../src/channel-forwarder"
import { FakeNatsServer } from "./fake-nats-server"

const THREAD = "notifications.github.acme-org.example-repo.issue.3.>"
const COMMENT = "notifications.github.acme-org.example-repo.issue.3.comment"

function envelope(eventId: string): string {
  return JSON.stringify({
    event_id: eventId,
    dedupe_key: `delivery-${eventId}`,
    source: "github",
    topic: THREAD,
    payload_summary: `event ${eventId}`,
  })
}

test("delivers each event id once even when concrete and wildcard subscriptions overlap", async () => {
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const received: string[] = []
  const subjects: string[] = []
  const envelopeTopics: Array<string | undefined> = []
  const second = Promise.withResolvers<void>()
  const forwarder = createChannelForwarder(connection, {
    deliver: async (message) => {
      if (message.duplicate) return
      // Real nats.js messages: subject, data, and the envelope topic must all survive.
      subjects.push(message.subject)
      envelopeTopics.push(message.envelopeTopic)
      received.push(new TextDecoder().decode(message.data))
      if (received.length === 2) second.resolve()
    },
  })

  try {
    forwarder.follow(THREAD)
    await broker.until(() => broker.subscribed.includes(THREAD))
    broker.deliver(COMMENT, envelope("evt-1"))
    broker.deliver(COMMENT, envelope("evt-1"))
    broker.deliver(COMMENT, envelope("evt-2"))
    await second.promise

    expect(received).toEqual([envelope("evt-1"), envelope("evt-2")])
    expect(subjects).toEqual([COMMENT, COMMENT])
    expect(envelopeTopics).toEqual([THREAD, THREAD])
  } finally {
    await forwarder.close()
    await broker.stop()
  }
})

test("closes all NATS subscriptions and the connection during channel shutdown", async () => {
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const forwarder = createChannelForwarder(connection, { deliver: async () => undefined })

  try {
    forwarder.follow(THREAD)
    await broker.until(() => broker.subscribed.includes(THREAD))
    await forwarder.close()

    expect(broker.unsubscribed).toEqual([
      "notifications.github.acme-org.example-repo.issue.3",
      THREAD,
    ])
    expect(connection.isClosed()).toBe(true)
  } finally {
    await broker.stop()
  }
})
