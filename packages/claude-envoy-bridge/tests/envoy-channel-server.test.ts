import { expect, test } from "bun:test"
import { mkdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import type { EnvoyClient, Interest } from "@legion/envoy-client/transport"
import type {
  ChannelForwarderConnection,
  ChannelInboundMessage,
  ChannelTopicSubscription,
} from "../src/channel-forwarder"
import {
  type ChannelNotification,
  type ChannelNotifier,
  createChannelDelivery,
  enqueueChannelMessage,
  sanitizeChannelMetadata,
  startChannelSession,
} from "../src/envoy-channel-server"

interface Queue {
  readonly subject: string
  readonly messages: ChannelInboundMessage[]
  waiter: PromiseWithResolvers<ChannelInboundMessage | null> | undefined
}

class FakeNats implements ChannelForwarderConnection {
  readonly published: Array<{ readonly subject: string; readonly data: Uint8Array }> = []
  readonly order: string[] = []
  readonly unsubscribed: string[] = []
  readonly subscriptions = new Map<string, Queue>()
  closed = false

  subscribe(subject: string): ChannelTopicSubscription {
    const queue: Queue = { subject, messages: [], waiter: undefined }
    this.subscriptions.set(subject, queue)
    return {
      unsubscribe: () => {
        this.unsubscribed.push(subject)
        this.subscriptions.delete(subject)
        queue.waiter?.resolve(null)
      },
      async *[Symbol.asyncIterator]() {
        for (;;) {
          const message = queue.messages.shift()
          if (message !== undefined) {
            yield message
            continue
          }
          const waiter = Promise.withResolvers<ChannelInboundMessage | null>()
          queue.waiter = waiter
          const next = await waiter.promise
          queue.waiter = undefined
          if (next === null) return
          yield next
        }
      },
    }
  }

  publish(subject: string, data: Uint8Array): void {
    this.order.push("receipt")
    this.published.push({ subject, data })
  }

  flush(): Promise<void> {
    return Promise.resolve()
  }

  isClosed(): boolean {
    return this.closed
  }

  drain(): Promise<void> {
    this.closed = true
    return Promise.resolve()
  }

  close(): Promise<void> {
    this.closed = true
    return Promise.resolve()
  }

  emit(subject: string, raw: string, reply?: string): void {
    const queue = this.subscriptions.get(subject)
    if (queue === undefined) throw new Error(`no subscription for ${subject}`)
    const message = {
      subject,
      data: new TextEncoder().encode(raw),
      ...(reply === undefined ? {} : { reply }),
    }
    if (queue.waiter === undefined) {
      queue.messages.push(message)
      return
    }
    const waiter = queue.waiter
    queue.waiter = undefined
    waiter.resolve(message)
  }
}

class FakeNotifier implements ChannelNotifier {
  readonly notifications: ChannelNotification[] = []

  constructor(readonly order: string[] = []) {}

  notification(notification: ChannelNotification): Promise<void> {
    this.order.push("notification")
    this.notifications.push(notification)
    return Promise.resolve()
  }
}

function settled(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve))
}

const directSubject = "notifications.agent.ses_claude"
const deliveryRaw = JSON.stringify({
  event_id: "evt-42",
  dedupe_key: "delivery-42",
  source: "agent",
  source_session: "ses_sender",
  topic: directSubject,
  issued_at: 1_760_000_000_000,
  urgency: "high",
  payload_summary: "Use the channel",
})

test("emits each Envoy envelope as the exact Claude channel notification", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({ sessionId: "ses_claude", notifier })

  await delivery.enqueue({ subject: directSubject, raw: deliveryRaw })

  expect(notifier.notifications).toEqual([
    {
      method: "notifications/claude/channel",
      params: {
        content:
          'envoy:\n  to: you (ses_…)\n  from: ses_sender\n  at: "2025-10-09T08:53:20Z"\n  id: evt-42\n  urgency: high\n  reply_with: "envoy_send(session_id=\\"ses_sender\\", message=\\"...\\")"\n  summary: Use the channel',
        meta: {
          source: "agent",
          topic: directSubject,
          event_id: "evt-42",
          dedupe_key: "delivery-42",
          urgency: "high",
          from_session: "ses_sender",
        },
      },
    },
  ])
})

test("strips unsafe channel meta keys before notifying Claude Code", () => {
  expect(
    sanitizeChannelMetadata({
      source: "envoy",
      "from-session": "ses_sender",
      "1bad": "dropped",
      event_id: "evt-1",
    }),
  ).toEqual({ source: "envoy", event_id: "evt-1" })
})

test("enqueues a direct event before publishing its adapter receipt", async () => {
  const nats = new FakeNats()
  const delivery = {
    enqueue: async () => {
      nats.order.push("enqueue")
    },
    inbox: () => [],
  }

  await enqueueChannelMessage(delivery, nats, directSubject, {
    subject: directSubject,
    data: new TextEncoder().encode(deliveryRaw),
    reply: "_INBOX.receipt",
  })

  expect(nats.order).toEqual(["enqueue", "receipt"])
})

test("delivers each event id once while acknowledging every direct request after enqueue", async () => {
  const nats = new FakeNats()
  const notifier = new FakeNotifier(nats.order)
  const roleStateFile = join(tmpdir(), `claude-envoy-no-role-${crypto.randomUUID()}`, "role.json")
  const session = await startChannelSession({
    sessionId: "ses_claude",
    directory: "/tmp",
    connection: nats,
    notifier,
    client: {
      subscribe: async () => noInterest(),
      unsubscribe: async () => undefined,
      unregisterSession: async () => undefined,
      setRole: async () => ({ claimed: true, interest: noInterest() }),
      getRole: async () => ({ role: "reviewer", holder: "ses_claude", last_seen: 1 }),
    },
    heartbeatMs: 60_000,
    roleStateFile,
  })

  try {
    nats.emit(directSubject, deliveryRaw, "_INBOX.receipt")
    nats.emit(directSubject, deliveryRaw, "_INBOX.receipt")
    await settled()

    expect(nats.order).toEqual(["receipt", "notification", "receipt"])
    expect(notifier.notifications).toHaveLength(1)
    expect(nats.published.map(({ subject, data }) => [subject, data.byteLength])).toEqual([
      ["_INBOX.receipt", 0],
      ["_INBOX.receipt", 0],
    ])
  } finally {
    await session.shutdown()
  }
})

test("retains only the latest fifty inbound envelope summaries", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({ sessionId: "ses_claude", notifier })

  for (let index = 0; index < 51; index += 1) {
    await delivery.enqueue({
      subject: directSubject,
      raw: JSON.stringify({
        event_id: `evt-${index}`,
        source: "agent",
        source_session: "ses_sender",
        issued_at: index,
        payload_summary: `message ${index}`,
      }),
    })
  }

  expect(delivery.inbox()).toHaveLength(50)
  expect(delivery.inbox()[0]).toEqual({
    event_id: "evt-50",
    at: "1970-01-01T00:00:00Z",
    from: "ses_sender",
    summary: "message 50",
  })
  expect(delivery.inbox()[49]).toEqual({
    event_id: "evt-1",
    at: "1970-01-01T00:00:00Z",
    from: "ses_sender",
    summary: "message 1",
  })
})

function noInterest(): Interest {
  return {
    session_id: "ses_claude",
    machine_id: "devbox",
    dir: "/tmp",
    topics: [directSubject],
  }
}

test("registers aside capability and soft-reclaims its persisted role", async () => {
  const roleStateFile = join(tmpdir(), `claude-envoy-role-${crypto.randomUUID()}`, "role.json")
  await mkdir(dirname(roleStateFile), { recursive: true })
  await writeFile(roleStateFile, JSON.stringify({ session_id: "ses_previous", role: "reviewer" }))
  const nats = new FakeNats()
  const notifier = new FakeNotifier()
  const subscribes: unknown[] = []
  const setRoles: unknown[] = []
  const client: Pick<
    EnvoyClient,
    "subscribe" | "unsubscribe" | "unregisterSession" | "setRole" | "getRole"
  > = {
    subscribe: async (input) => {
      subscribes.push(input)
      return noInterest()
    },
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async (input) => {
      setRoles.push(input)
      return { claimed: true, interest: noInterest() }
    },
    getRole: async () => ({ role: "reviewer", holder: "ses_claude", last_seen: 1 }),
  }

  const session = await startChannelSession({
    sessionId: "ses_claude",
    directory: "/tmp",
    connection: nats,
    notifier,
    client,
    heartbeatMs: 60_000,
    roleStateFile,
  })

  try {
    expect(subscribes).toEqual([
      expect.objectContaining({
        sessionID: "ses_claude",
        topics: [directSubject],
        capabilities: ["aside"],
        selfSubscribed: true,
      }),
    ])
    expect(setRoles).toEqual([
      { sessionID: "ses_claude", role: "reviewer", soft: true, previousSessionID: "ses_previous" },
    ])

    expect(JSON.parse(await readFile(roleStateFile, "utf8"))).toEqual({
      session_id: "ses_claude",
      role: "reviewer",
    })
  } finally {
    await session.shutdown()
    await rm(dirname(roleStateFile), { recursive: true, force: true })
  }
})

test("drops local topics named by a human Dispatch subscription removal", async () => {
  const nats = new FakeNats()
  const notifier = new FakeNotifier()
  const session = await startChannelSession({
    sessionId: "ses_claude",
    directory: "/tmp",
    connection: nats,
    notifier,
    client: {
      subscribe: async () => noInterest(),
      unsubscribe: async () => undefined,
      unregisterSession: async () => undefined,
      setRole: async () => ({ claimed: true, interest: noInterest() }),
      getRole: async () => ({ role: "reviewer", holder: "ses_claude", last_seen: 1 }),
    },
    heartbeatMs: 60_000,
    roleStateFile: join(tmpdir(), `claude-envoy-no-role-${crypto.randomUUID()}`, "role.json"),
  })
  const topic = "notifications.dispatch.issue.DSP-3.>"

  try {
    await session.follow([topic])
    nats.emit(
      directSubject,
      JSON.stringify({
        event_id: "remove-1",
        source: "dispatch",
        payload: JSON.stringify({
          issue_key: "DSP-3",
          type: "subscription.removed",
          actor: { kind: "user", id: "alice" },
          payload: {
            session_id: "ses_claude",
            by: { kind: "user", id: "alice" },
            topics: [topic],
          },
        }),
      }),
    )
    await settled()

    expect(nats.unsubscribed).toEqual([
      "notifications.dispatch.issue.DSP-3",
      "notifications.dispatch.issue.DSP-3.>",
    ])
  } finally {
    await session.shutdown()
  }
})
