import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import type { EnvoyClient, Interest } from "@legion/envoy-client/transport"
import plugin from "../.claude-plugin/plugin.json" with { type: "json" }
import pkg from "../package.json" with { type: "json" }
import type {
  ChannelForwarderConnection,
  ChannelInboundMessage,
  ChannelTopicSubscription,
} from "../src/channel-forwarder"
import {
  type ChannelNotification,
  type ChannelNotifier,
  type ChannelSessionOptions,
  createChannelDelivery,
  enqueueChannelMessage,
  executeEnvoyTool,
  MCP_SERVER_INFO,
  sanitizeChannelMetadata,
  startChannelSession,
} from "../src/envoy-channel-server"
import {
  roleStateFile,
  SessionIdentity,
  sessionHandoffFile,
  writeSessionHandoff,
} from "../src/session-identity"

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

  constructor(readonly calls: string[] = []) {}

  subscribe(subject: string): ChannelTopicSubscription {
    this.calls.push(`nats.subscribe ${subject}`)
    const queue: Queue = { subject, messages: [], waiter: undefined }
    this.subscriptions.set(subject, queue)
    return {
      unsubscribe: () => {
        this.calls.push(`nats.unsubscribe ${subject}`)
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

type FakeClient = Pick<
  EnvoyClient,
  "subscribe" | "unsubscribe" | "unregisterSession" | "setRole" | "getRole" | "getInterest"
>

function settled(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>()
  setImmediate(resolve)
  return promise
}

async function scratchState(): Promise<string> {
  return mkdtemp(join(tmpdir(), "claude-envoy-state-"))
}

function noInterest(): Interest {
  return {
    session_id: "ses_claude",
    machine_id: "devbox",
    dir: "/tmp",
    topics: [directSubject],
  }
}

/** A client that records every call it sees, in order, into `calls`. */
function recordingClient(calls: string[], overrides: Partial<FakeClient> = {}): FakeClient {
  return {
    subscribe: async (input) => {
      calls.push(`subscribe ${input.sessionID} [${input.capabilities?.join(",") ?? ""}]`)
      return noInterest()
    },
    unsubscribe: async (input) => {
      calls.push(`unsubscribe ${input.sessionID} ${input.topics.join(",")}`)
    },
    unregisterSession: async (sessionID) => {
      calls.push(`unregister ${sessionID}`)
    },
    setRole: async (input) => {
      calls.push(
        `setRole ${input.sessionID} ${input.role}${input.soft ? " soft" : ""}${input.previousSessionID === undefined ? "" : ` previous=${input.previousSessionID}`}`,
      )
      return { claimed: true, interest: noInterest() }
    },
    getRole: async (role) => ({ role, holder: "ses_claude", last_seen: 1 }),
    getInterest: async (sessionID) => {
      calls.push("getInterest")
      return {
        ...noInterest(),
        session_id: sessionID,
        topics: [`notifications.agent.${sessionID}`],
      }
    },
    ...overrides,
  }
}

function sessionOptions(
  identity: SessionIdentity,
  stateDirectory: string,
  extra: Partial<ChannelSessionOptions> = {},
): ChannelSessionOptions {
  return {
    identity,
    connection: new FakeNats(),
    notifier: new FakeNotifier(),
    client: recordingClient([]),
    heartbeatMs: 60_000,
    stateDirectory,
    ...extra,
  }
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
/** A role-lane envelope the listener forwarded to the direct subject and awaits a receipt for. */
const roleForwardRaw = JSON.stringify({
  event_id: "evt-role-7",
  dedupe_key: "envoy.role.forward.role-7",
  source: "agent",
  source_session: "ses_sender",
  topic: "notifications.role.reviewer",
  issued_at: 1_760_000_000_000,
  payload_summary: "Review please",
})

test("emits each Envoy envelope as the exact Claude channel notification", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
  })

  await delivery.enqueue({ subject: directSubject, raw: deliveryRaw })

  expect(notifier.notifications).toEqual([
    {
      method: "notifications/claude/channel",
      params: {
        content:
          'envoy:\n  to: you (ses_…)\n  from: ses_sender\n  at: "2025-10-09T08:53:20Z"\n  id: evt-42\n  urgency: high\n  reply_with:\n    tool: envoy_send\n    args:\n      session_id: ses_sender\n      in_reply_to: evt-42\n      message: ...\n  summary: Use the channel',
        meta: {
          producer: "agent",
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

test("announces a followed ask once as a plain channel notification and never subscribes", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
  })
  const details = { issue: "DSP-3", ask: "ask-3", follows: { ask: "ask-3" } }

  await delivery.announceFollow(details)
  await delivery.announceFollow({ ...details, comment: "c-1" })
  await delivery.announceFollow({ issue: "DSP-3", comment: "c-2" })

  expect(notifier.notifications).toEqual([
    {
      method: "notifications/claude/channel",
      params: {
        content:
          "Following ask ask-3 on DSP-3: its answer and replies reach you directly (dispatch_follow unfollow to stop). For every event on DSP-3: envoy_subscribe notifications.dispatch.issue.DSP-3.>.",
        meta: { producer: "dispatch" },
      },
    },
  ])
})

test("strips unsafe channel meta keys before notifying Claude Code", () => {
  expect(
    sanitizeChannelMetadata({
      producer: "envoy",
      "from-session": "ses_sender",
      "1bad": "dropped",
      event_id: "evt-1",
    }),
  ).toEqual({ producer: "envoy", event_id: "evt-1" })
})

test("enqueues a forwarded role-lane event before publishing its adapter receipt", async () => {
  const nats = new FakeNats()
  const delivery = {
    enqueue: async () => {
      nats.order.push("enqueue")
    },
    announceFollow: async () => undefined,
    inbox: () => [],
  }

  await enqueueChannelMessage(delivery, nats, directSubject, {
    subject: directSubject,
    data: new TextEncoder().encode(roleForwardRaw),
    reply: "_INBOX.receipt",
    envelopeTopic: "notifications.role.reviewer",
  })

  expect(nats.order).toEqual(["enqueue", "receipt"])
})

test("never answers the reply inbox of a JetStream publish to the direct subject", async () => {
  // /v1/messages/send publishes through JetStream; the reply subject on that
  // message is the publisher's acknowledgement inbox, and an empty receipt
  // there fails the publish (`invalid jetstream publish response`) even though
  // the event was delivered. Only the listener's forwarded role lane, which
  // keeps its role topic, waits for a receipt.
  const nats = new FakeNats()
  const delivery = {
    enqueue: async () => {
      nats.order.push("enqueue")
    },
    announceFollow: async () => undefined,
    inbox: () => [],
  }

  await enqueueChannelMessage(delivery, nats, directSubject, {
    subject: directSubject,
    data: new TextEncoder().encode(deliveryRaw),
    reply: "_INBOX.jetstream-ack",
    envelopeTopic: directSubject,
  })

  expect(nats.order).toEqual(["enqueue"])
  expect(nats.published).toEqual([])
})

test("delivers each event id once while acknowledging every forwarded request after enqueue", async () => {
  const nats = new FakeNats()
  const notifier = new FakeNotifier(nats.order)
  const stateDirectory = await scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      notifier,
    }),
  )

  try {
    nats.emit(directSubject, roleForwardRaw, "_INBOX.receipt")
    nats.emit(directSubject, roleForwardRaw, "_INBOX.receipt")
    await settled()

    expect(nats.order).toEqual(["receipt", "notification", "receipt"])
    expect(notifier.notifications).toHaveLength(1)
    expect(nats.published.map(({ subject, data }) => [subject, data.byteLength])).toEqual([
      ["_INBOX.receipt", 0],
      ["_INBOX.receipt", 0],
    ])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("retains only the latest fifty inbound envelope summaries", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
  })

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

test("registers aside capability and soft-reclaims the role persisted for a resumed session id", async () => {
  const stateDirectory = await scratchState()
  const roleFile = roleStateFile(stateDirectory, "ses_claude")
  await mkdir(dirname(roleFile), { recursive: true })
  await writeFile(roleFile, JSON.stringify({ session_id: "ses_claude", role: "reviewer" }))
  const subscribes: unknown[] = []
  const setRoles: unknown[] = []
  const client: FakeClient = recordingClient([], {
    subscribe: async (input) => {
      subscribes.push(input)
      return noInterest()
    },
    setRole: async (input) => {
      setRoles.push(input)
      return { claimed: true, interest: noInterest() }
    },
  })

  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, { client }),
  )

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
      { sessionID: "ses_claude", role: "reviewer", soft: true, previousSessionID: "ses_claude" },
    ])
    expect(JSON.parse(await readFile(roleFile, "utf8"))).toEqual({
      session_id: "ses_claude",
      role: "reviewer",
    })
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("drops local topics named by a human Dispatch subscription removal", async () => {
  const nats = new FakeNats()
  const stateDirectory = await scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
    }),
  )
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
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("the plugin manifest, package, and MCP server all report one version", () => {
  expect(plugin.version).toBe(pkg.version)
  expect(MCP_SERVER_INFO).toEqual({ name: "envoy", version: pkg.version })
})

test("follows the session id its Claude process hands off: new subject first, then re-register, unfollow, and role transfer", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  const identity = new SessionIdentity("ses_old", "/tmp")
  const oldRoleFile = roleStateFile(stateDirectory, "ses_old")
  await mkdir(dirname(oldRoleFile), { recursive: true })
  await writeFile(oldRoleFile, JSON.stringify({ session_id: "ses_old", role: "reviewer" }))
  const handoff = sessionHandoffFile(stateDirectory, 777)
  await writeSessionHandoff(handoff, "ses_old")
  // The heartbeat registers again once the handoff completed, so the second
  // subscribe under the new id marks the whole sequence (role file included) done.
  const transferred = Promise.withResolvers<void>()
  let newRegistrations = 0
  const client = recordingClient(calls, {
    subscribe: async (input) => {
      calls.push(`subscribe ${input.sessionID} [${input.capabilities?.join(",") ?? ""}]`)
      if (input.sessionID === "ses_new") {
        newRegistrations += 1
        if (newRegistrations === 2) transferred.resolve()
      }
      return noInterest()
    },
  })
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 25,
      handoffPid: 777,
    }),
  )

  try {
    calls.length = 0
    await writeSessionHandoff(handoff, "ses_new")
    await transferred.promise

    expect(calls).toEqual([
      "nats.subscribe notifications.agent.ses_new",
      "unregister ses_old",
      "subscribe ses_new [aside]",
      "nats.unsubscribe notifications.agent.ses_old",
      "setRole ses_new reviewer soft previous=ses_old",
      "subscribe ses_new [aside]",
    ])
    expect(identity.id).toBe("ses_new")
    expect(await readFile(roleStateFile(stateDirectory, "ses_new"), "utf8")).toBe(
      `${JSON.stringify({ session_id: "ses_new", role: "reviewer" })}\n`,
    )
    expect(await readdir(join(stateDirectory, "roles"))).toEqual(["ses_new.json"])
    expect(
      await executeEnvoyTool(
        { identity, client: client as EnvoyClient, session },
        "envoy_whoami",
        {},
      ),
    ).toMatchObject({ session_id: "ses_new" })

    calls.length = 0
    await session.shutdown()
    expect(calls).toEqual(["nats.unsubscribe notifications.agent.ses_new", "unregister ses_new"])
    // The handoff file outlives this server: a restart inside the same Claude process reads it.
    expect(await readdir(join(stateDirectory, "sessions"))).toEqual(["777"])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("two channel servers under different Claude processes rebind independently", async () => {
  const stateDirectory = await scratchState()
  const callsA: string[] = []
  const callsB: string[] = []
  const identityA = new SessionIdentity("ses_a", "/tmp")
  const identityB = new SessionIdentity("ses_b", "/tmp")
  await writeSessionHandoff(sessionHandoffFile(stateDirectory, 1001), "ses_a")
  await writeSessionHandoff(sessionHandoffFile(stateDirectory, 1002), "ses_b")
  const rebound = Promise.withResolvers<void>()
  const serverA = await startChannelSession(
    sessionOptions(identityA, stateDirectory, {
      connection: new FakeNats(callsA),
      client: recordingClient(callsA, {
        unregisterSession: async (sessionID) => {
          callsA.push(`unregister ${sessionID}`)
          rebound.resolve()
        },
      }),
      heartbeatMs: 25,
      handoffPid: 1001,
    }),
  )
  let heartbeatsB = 0
  let afterRebind: PromiseWithResolvers<void> | undefined
  const serverB = await startChannelSession(
    sessionOptions(identityB, stateDirectory, {
      connection: new FakeNats(callsB),
      client: recordingClient(callsB, {
        subscribe: async (input) => {
          callsB.push(`subscribe ${input.sessionID}`)
          heartbeatsB += 1
          afterRebind?.resolve()
          return noInterest()
        },
      }),
      heartbeatMs: 25,
      handoffPid: 1002,
    }),
  )

  try {
    callsA.length = 0
    callsB.length = 0
    await writeSessionHandoff(sessionHandoffFile(stateDirectory, 1001), "ses_a2")
    await rebound.promise
    // B must get a heartbeat of its own after A rebound to prove it read its file and stayed.
    afterRebind = Promise.withResolvers<void>()
    await afterRebind.promise

    expect(identityA.id).toBe("ses_a2")
    expect(identityB.id).toBe("ses_b")
    expect(callsA).toContain("unregister ses_a")
    expect(heartbeatsB).toBeGreaterThan(0)
    expect(callsB.filter((call) => call !== "subscribe ses_b")).toEqual([])
  } finally {
    await serverA.shutdown()
    await serverB.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

const rejectedFrameRaw = JSON.stringify({
  event_id: "dispatch-malformed",
  source: "dispatch",
  source_event_id: "1",
  topic: directSubject,
  dedupe_key: "dispatch-malformed",
  issued_at: 1,
  payload_summary: "Can this ship?",
  payload: JSON.stringify({
    event: {
      actor: { id: "alice", kind: "user" },
      issue_key: "CORE-1",
      payload: { body: "Can this ship?", id: "message-malformed" },
      type: "message.created",
    },
    delivery: { attempt: 1, mode: "aside" },
  }),
  trace_id: "dispatch-malformed",
})

test("answers a rejected targeted frame on Dispatch instead of notifying the model", async () => {
  const replies: Array<{
    readonly path: string
    readonly auth: string | null
    readonly body: unknown
  }> = []
  const server = Bun.serve({
    port: 0,
    fetch: async (request) => {
      replies.push({
        path: new URL(request.url).pathname,
        auth: request.headers.get("authorization"),
        body: await request.json(),
      })
      return Response.json({})
    },
  })
  const previous = { ...process.env }
  process.env["DISPATCH_URL"] = `http://127.0.0.1:${server.port}`
  process.env["DISPATCH_TOKEN"] = "reply-token"
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", process.cwd()),
    notifier,
  })

  try {
    await delivery.enqueue({ subject: directSubject, raw: rejectedFrameRaw })

    expect(notifier.notifications).toEqual([])
    expect(replies).toEqual([
      {
        path: "/api/v1/messages/message-malformed/reply",
        auth: "Bearer reply-token",
        body: {
          actor: { kind: "session", id: "ses_claude" },
          attempt: 1,
          error: "Invalid Dispatch targeted delivery frame",
        },
      },
    ])
  } finally {
    server.stop(true)
    process.env = previous
  }
})

test("drops a malformed targeted frame that has no reply address", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
  })

  await delivery.enqueue({
    subject: directSubject,
    raw: JSON.stringify({
      event_id: "dispatch-junk",
      source: "dispatch",
      payload: JSON.stringify({ delivery: { attempt: "one" } }),
    }),
  })

  expect(notifier.notifications).toEqual([])
  expect(delivery.inbox()).toEqual([])
})

test("a resumed server rebuilds the interests its session id already registered and follow reports them as not fresh", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  const subscribes: Array<readonly string[]> = []
  const issueTopic = "notifications.dispatch.issue.DSP-3.>"
  const client = recordingClient([], {
    getInterest: async () => ({
      ...noInterest(),
      topics: [
        directSubject,
        "notifications.dispatch.issue.DSP-3",
        issueTopic,
        "notifications.role.reviewer",
      ],
    }),
    subscribe: async (input) => {
      subscribes.push(input.topics)
      return noInterest()
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
    }),
  )

  try {
    expect(subscribes).toEqual([[directSubject, "notifications.dispatch.issue.DSP-3", issueTopic]])
    expect([...nats.subscriptions.keys()]).toEqual([
      directSubject,
      "notifications.dispatch.issue.DSP-3",
      issueTopic,
    ])
    expect(await session.follow([issueTopic])).toEqual([])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})
