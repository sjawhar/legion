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
      if (message.duplicate) return false
      // Real nats.js messages: subject, data, and the envelope topic must all survive.
      subjects.push(message.subject)
      envelopeTopics.push(message.envelopeTopic)
      received.push(new TextDecoder().decode(message.data))
      if (received.length === 2) second.resolve()
      return true
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

test("keeps each valid dedupe identity when a sibling identity field is empty", async () => {
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const accepted: string[] = []
  const sentinel = Promise.withResolvers<void>()
  const forwarder = createChannelForwarder(connection, {
    deliver: async (message) => {
      if (message.duplicate !== true) accepted.push(message.raw)
      if (message.raw.includes("identity sentinel")) sentinel.resolve()
      return message.duplicate !== true
    },
  })
  const frame = (summary: string, identity: Record<string, string>) =>
    JSON.stringify({
      source: "github",
      topic: COMMENT,
      issued_at: 1,
      payload_summary: summary,
      ...identity,
    })

  try {
    forwarder.follow(COMMENT)
    await broker.until(() => broker.subscribed.includes(COMMENT))
    for (const [summary, identity] of [
      ["identity valid event", { event_id: "evt-valid-event", dedupe_key: "" }],
      [
        "identity valid dispatch event",
        { event_id: "evt-valid-dispatch-event", dedupe_key: "", source: "dispatch" },
      ],
      [
        "identity valid key",
        { event_id: "", dedupe_key: "dispatch.valid-key", source: "dispatch" },
      ],
      ["identity empty event", { event_id: "" }],
      ["identity empty fields", { event_id: "", dedupe_key: "", source: "dispatch" }],
      ["identity absent fields", {}],
    ] as const) {
      const raw = frame(summary, identity)
      broker.deliver(COMMENT, raw)
      broker.deliver(COMMENT, raw)
    }
    broker.deliver(COMMENT, frame("identity sentinel", { event_id: "evt-identity-sentinel" }))
    await sentinel.promise

    const count = (summary: string) =>
      accepted.filter((raw) => raw.includes(`"payload_summary":"${summary}"`)).length
    expect({
      validEvent: count("identity valid event"),
      validDispatchEvent: count("identity valid dispatch event"),
      validKey: count("identity valid key"),
      emptyEvent: count("identity empty event"),
      emptyFields: count("identity empty fields"),
      absentFields: count("identity absent fields"),
    }).toEqual({
      validEvent: 1,
      validDispatchEvent: 1,
      validKey: 1,
      emptyEvent: 2,
      emptyFields: 2,
      absentFields: 2,
    })
  } finally {
    await forwarder.close()
    await broker.stop()
  }
})

// A session that follows a pull request's whole thread and its checks holds two subscriptions for
// the checks subject, so one CI settlement arrives twice. Its key does not name its event (a random
// source event id), so only the event id both copies carry says the second is the first again.
test("one publish reaches Claude once through two overlapping subscriptions, whatever its key", async () => {
  const PR = "notifications.github.acme.widgets.pr.7.>"
  const CHECKS = "notifications.github.acme.widgets.pr.7.checks"
  const settlement = (eventId: string, generation: number) =>
    JSON.stringify({
      event_id: eventId,
      source: "github",
      source_event_id: `ci-${eventId}`,
      dedupe_key: `github.checks.acme.widgets.pr.7.0a1b2c3.g${generation}`,
      topic: CHECKS,
      payload_summary: `checks ${eventId}`,
    })
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const handed: string[] = []
  let duplicates = 0
  const all = Promise.withResolvers<void>()
  const forwarder = createChannelForwarder(connection, {
    deliver: async (message) => {
      if (message.duplicate) duplicates++
      else handed.push(message.raw)
      if (handed.length + duplicates === 4) all.resolve()
      return !message.duplicate
    },
  })

  try {
    forwarder.follow(PR)
    forwarder.follow(CHECKS)
    await broker.until(() => broker.subscribed.includes(PR) && broker.subscribed.includes(CHECKS))
    broker.deliver(CHECKS, settlement("evt-1", 1))
    broker.deliver(CHECKS, settlement("evt-2", 2))
    await all.promise

    expect(handed).toEqual([settlement("evt-1", 1), settlement("evt-2", 2)])
    expect(duplicates).toBe(2)
  } finally {
    await forwarder.close()
    await broker.stop()
  }
})

// A frame on a followed topic can carry a key Dispatch recorded without naming it as Dispatch does
// (another producer, or a caller who chose the key). The channel skips or refuses such a frame, and
// that must not unclaim the Dispatch key, or the real Retry under it reaches Claude a second time.
test("a frame the channel did not hand on releases nothing another frame claimed", async () => {
  const key = "agent.ses_a.m1:aside"
  const frame = (eventId: string, source: string) =>
    JSON.stringify({ event_id: eventId, source, dedupe_key: key, topic: COMMENT })
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const seen: Array<{ event: string; duplicate: boolean }> = []
  const settled = Promise.withResolvers<void>()
  const forwarder = createChannelForwarder(connection, {
    deliver: async (message) => {
      const { event_id, source } = JSON.parse(message.raw) as { event_id: string; source: string }
      seen.push({ event: event_id, duplicate: message.duplicate === true })
      if (seen.length === 3) settled.resolve()
      return source === "dispatch" && message.duplicate !== true
    },
  })

  try {
    forwarder.follow(COMMENT)
    await broker.until(() => broker.subscribed.includes(COMMENT))
    broker.deliver(COMMENT, frame("evt-attempt", "dispatch"))
    broker.deliver(COMMENT, frame("evt-borrowed", "agent"))
    broker.deliver(COMMENT, frame("evt-retry", "dispatch"))
    await settled.promise

    expect(seen).toEqual([
      { event: "evt-attempt", duplicate: false },
      { event: "evt-borrowed", duplicate: false },
      { event: "evt-retry", duplicate: true },
    ])
  } finally {
    await forwarder.close()
    await broker.stop()
  }
})

test("closes all NATS subscriptions and the connection during channel shutdown", async () => {
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const forwarder = createChannelForwarder(connection, { deliver: async () => true })

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

test("unfollow drops a topic from topics() before it waits for the drain", async () => {
  const broker = new FakeNatsServer()
  const connection = await connect({ servers: broker.url })
  const forwarder = createChannelForwarder(connection, { deliver: async () => true })

  try {
    forwarder.follow(COMMENT)
    const dropping = forwarder.unfollow([COMMENT])
    expect(forwarder.topics()).toEqual([])
    await dropping
  } finally {
    await forwarder.close()
    await broker.stop()
  }
})
