import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import type { EnvoyClient, Interest } from "@legion/envoy-client/transport"
import plugin from "../.claude-plugin/plugin.json" with { type: "json" }
import ompPlugin from "../.omp-plugin/plugin.json" with { type: "json" }
import pkg from "../package.json" with { type: "json" }
import type {
  ChannelBrokerMessage,
  ChannelForwarderConnection,
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
  readonly messages: ChannelBrokerMessage[]
  waiter: PromiseWithResolvers<ChannelBrokerMessage | null> | undefined
  /** Set by unsubscribe: the iterator ends at its next pull, even when a message was mid-delivery. */
  closed: boolean
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
    const queue: Queue = { subject, messages: [], waiter: undefined, closed: false }
    this.subscriptions.set(subject, queue)
    return {
      unsubscribe: () => {
        this.calls.push(`nats.unsubscribe ${subject}`)
        this.unsubscribed.push(subject)
        this.subscriptions.delete(subject)
        queue.closed = true
        queue.waiter?.resolve(null)
      },
      async *[Symbol.asyncIterator]() {
        for (;;) {
          if (queue.closed) return
          const message = queue.messages.shift()
          if (message !== undefined) {
            yield message
            continue
          }
          const waiter = Promise.withResolvers<ChannelBrokerMessage | null>()
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

  /** When set, a flush never settles, as on a stalled connection. */
  stallFlush = false

  flush(): Promise<void> {
    return this.stallFlush ? new Promise<void>(() => undefined) : Promise.resolve()
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

/**
 * Polls `condition` every 5 ms until it holds; fails naming `what` after 5 s.
 * Real time on purpose: the awaited conditions are filesystem writes the
 * server's own heartbeat performs, which no fake clock can advance.
 */
async function waitFor(condition: () => Promise<boolean>, what: string): Promise<void> {
  const deadline = Date.now() + 5_000
  while (!(await condition())) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`)
    await Bun.sleep(5)
  }
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
    raw: roleForwardRaw,
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
    raw: deliveryRaw,
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
  expect(ompPlugin.version).toBe(pkg.version)
  expect(MCP_SERVER_INFO).toEqual({ name: "envoy", version: pkg.version })
})

test("the Oh My Pi manifest declares no MCP server", () => {
  // Oh My Pi reads .omp-plugin/plugin.json before .claude-plugin/plugin.json and a manifest
  // mcpServers replaces .mcp.json outright, so an empty map is what keeps omp from launching a
  // channel server that exits without Claude Code's session identity. Dropping the key falls
  // through to .mcp.json and starts it again.
  expect(ompPlugin.mcpServers).toEqual({})
})

test("follows the session id its Claude process hands off: new subject first, the old one dropped before re-registering, then role transfer", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  const identity = new SessionIdentity("ses_old", "/tmp")
  const oldRoleFile = roleStateFile(stateDirectory, "ses_old")
  await mkdir(dirname(oldRoleFile), { recursive: true })
  await writeFile(oldRoleFile, JSON.stringify({ session_id: "ses_old", role: "reviewer" }))
  const handoff = sessionHandoffFile(stateDirectory, 777)
  await writeSessionHandoff(handoff, "ses_old")
  // The tick that adopts the handoff registers once more after it, so the second
  // registration under the new id comes after the role file is in place; the wait
  // below still checks the file itself so a failure names what is missing.
  const newRegistrationTopics: Array<readonly string[]> = []
  const client = recordingClient(calls, {
    subscribe: async (input) => {
      calls.push(`subscribe ${input.sessionID} [${input.capabilities?.join(",") ?? ""}]`)
      if (input.sessionID === "ses_new") newRegistrationTopics.push(input.topics)
      return noInterest()
    },
    // The listener reports this session as the holder once a claim landed, so
    // heartbeats reassert nothing and the recorded calls stay the handoff's own.
    getRole: async (role) => ({ role, holder: identity.id, last_seen: 1 }),
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
    const newRoleFile = roleStateFile(stateDirectory, "ses_new")
    const expectedRole = `${JSON.stringify({ session_id: "ses_new", role: "reviewer" })}\n`
    await waitFor(
      async () =>
        newRegistrationTopics.length >= 2 &&
        (await readFile(newRoleFile, "utf8").catch(() => undefined)) === expectedRole,
      `a second registration of ses_new and ${expectedRole.trim()} in ${newRoleFile}`,
    )

    const reregister = "subscribe ses_new [aside]"
    // A tick that fired before the handoff file landed only re-registered the old id.
    const handoffCalls = calls.filter((call) => call !== "subscribe ses_old [aside]")
    expect(handoffCalls.slice(0, 6)).toEqual([
      "nats.subscribe notifications.agent.ses_new",
      "unregister ses_old",
      "nats.unsubscribe notifications.agent.ses_old",
      reregister,
      "setRole ses_new reviewer soft previous=ses_old",
      reregister,
    ])
    // The listener merges registered topics into the entry and never drops one, so no
    // registration under the new id may carry the old id's direct subject.
    expect(newRegistrationTopics.flat()).not.toContain("notifications.agent.ses_old")
    // Whatever followed is a later tick doing nothing but re-registering.
    expect(handoffCalls.slice(6).filter((call) => call !== reregister)).toEqual([])
    expect(identity.id).toBe("ses_new")
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

test("a heartbeat tick that outlives the interval is never overlapped by the next one", async () => {
  const stateDirectory = await scratchState()
  const identity = new SessionIdentity("ses_claude", "/tmp")
  const events: string[] = []
  const release = Promise.withResolvers<void>()
  let registrations = 0
  const client = recordingClient([], {
    subscribe: async () => {
      registrations += 1
      events.push(`start ${registrations}`)
      // Startup registers once; the first heartbeat tick then stalls until released.
      if (registrations === 2) await release.promise
      events.push(`end ${registrations}`)
      return noInterest()
    },
  })
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, { client, heartbeatMs: 25 }),
  )

  try {
    await waitFor(async () => registrations === 2, "the first heartbeat tick to begin")
    // Real time on purpose: the server's own setInterval is what must not fire
    // a second registration while this one is stalled, so give it several
    // intervals to try.
    await Bun.sleep(100)
    expect(events).toEqual(["start 1", "end 1", "start 2"])

    release.resolve()
    await waitFor(async () => registrations === 3, "the heartbeat to resume after the slow tick")
    expect(events.slice(0, 5)).toEqual(["start 1", "end 1", "start 2", "end 2", "start 3"])
  } finally {
    await session.shutdown()
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
      payload: { body: "Can this ship?", id: "44444444-4444-4444-8444-444444444444" },
      type: "message.created",
    },
    delivery: { attempt: 1, mode: "aside" },
  }),
  trace_id: "dispatch-malformed",
})

const rejectedCommentFrameRaw = JSON.stringify({
  event_id: "dispatch-malformed-comment",
  source: "dispatch",
  source_event_id: "2",
  topic: directSubject,
  dedupe_key: "dispatch-malformed-comment",
  issued_at: 1,
  payload_summary: "Please review this.",
  payload: JSON.stringify({
    event: {
      actor: { id: "alice", kind: "user" },
      issue_key: "CORE-1",
      payload: { body: "Please review this.", id: "comment-payload-1" },
      type: "comment.created",
    },
    delivery: {
      attempt: 1,
      mode: "aside",
      comment_id: "33333333-3333-4333-8333-333333333333",
      target: "session:ses_claude",
    },
  }),
  trace_id: "dispatch-malformed-comment",
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
        path: "/api/v1/messages/44444444-4444-4444-8444-444444444444/reply",
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

test("answers a malformed targeted comment through its supplied comment reply address", async () => {
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
    await delivery.enqueue({ subject: directSubject, raw: rejectedCommentFrameRaw })

    expect(notifier.notifications).toEqual([])
    expect(replies).toEqual([
      {
        path: "/api/v1/comments/33333333-3333-4333-8333-333333333333/reply",
        auth: "Bearer reply-token",
        body: {
          actor: { kind: "session", id: "ses_claude" },
          attempt: 1,
          target: "session:ses_claude",
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

test("a registration in flight when a topic is unsubscribed cannot write it back into the entry", async () => {
  const stateDirectory = await scratchState()
  // The listener merges registered topics into the entry and only an unsubscribe removes one.
  const entry = new Set<string>()
  let held: PromiseWithResolvers<void> | undefined
  let registrationHeld = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      const topics = [...input.topics]
      if (held !== undefined) {
        registrationHeld = true
        await held.promise
      }
      for (const topic of topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, { client }),
  )
  const dropped = "notifications.dispatch.issue.DSP-1"
  const kept = "notifications.dispatch.issue.DSP-9"

  try {
    await session.follow([dropped])
    expect(entry.has(dropped)).toBe(true)
    const release = Promise.withResolvers<void>()
    held = release
    // This registration reads its topics, `dropped` among them, then waits on the listener.
    const following = session.follow([kept])
    await waitFor(async () => registrationHeld, "the registration to reach the listener")
    held = undefined
    const unfollowing = session.unfollow([dropped])
    await settled()
    release.resolve()
    expect(await unfollowing).toEqual([dropped])
    await following
    expect([...entry].sort()).toEqual([directSubject, kept])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a subscription made while a handoff deregisters the old id registers under the new id only", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const identity = new SessionIdentity("ses_old", "/tmp")
  const handoff = sessionHandoffFile(stateDirectory, 778)
  await writeSessionHandoff(handoff, "ses_old")
  const unregistering = Promise.withResolvers<void>()
  let unregisterStarted = false
  const client = recordingClient(calls, {
    unregisterSession: async (sessionID) => {
      calls.push(`unregister ${sessionID}`)
      if (sessionID !== "ses_old") return
      unregisterStarted = true
      await unregistering.promise
    },
    getRole: async (role) => ({ role, holder: identity.id, last_seen: 1 }),
  })
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, { client, heartbeatMs: 25, handoffPid: 778 }),
  )

  try {
    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(async () => unregisterStarted, "the handoff to start deregistering ses_old")
    const following = session.follow(["notifications.dispatch.issue.DSP-9"])
    await settled()
    unregistering.resolve()
    await following
    const afterUnregister = calls.slice(calls.indexOf("unregister ses_old") + 1)
    expect(afterUnregister.filter((call) => call.startsWith("subscribe ses_old"))).toEqual([])
    expect(identity.id).toBe("ses_new")
  } finally {
    unregistering.resolve()
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a delivery still in flight on the old subject does not hold back registration under the new id", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  const gate = Promise.withResolvers<void>()
  let notified = false
  const notifier: ChannelNotifier = {
    notification: async () => {
      notified = true
      await gate.promise
    },
  }
  const identity = new SessionIdentity("ses_old", "/tmp")
  const handoff = sessionHandoffFile(stateDirectory, 779)
  await writeSessionHandoff(handoff, "ses_old")
  const client = recordingClient(calls, {
    getRole: async (role) => ({ role, holder: identity.id, last_seen: 1 }),
  })
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, {
      connection: nats,
      notifier,
      client,
      heartbeatMs: 25,
      handoffPid: 779,
    }),
  )

  try {
    nats.emit("notifications.agent.ses_old", deliveryRaw.replace(directSubject, "notifications.agent.ses_old"))
    await waitFor(async () => notified, "the delivery on the old subject to reach the notifier")
    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(
      async () => calls.includes("subscribe ses_new [aside]"),
      "a registration under ses_new while the old subject's delivery is still in flight",
    )
  } finally {
    gate.resolve()
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a handoff whose first registration under the new id fails takes the role back on a later heartbeat", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const identity = new SessionIdentity("ses_old", "/tmp")
  const oldRoleFile = roleStateFile(stateDirectory, "ses_old")
  await mkdir(dirname(oldRoleFile), { recursive: true })
  await writeFile(oldRoleFile, JSON.stringify({ session_id: "ses_old", role: "reviewer" }))
  const handoff = sessionHandoffFile(stateDirectory, 780)
  await writeSessionHandoff(handoff, "ses_old")
  let listenerDown = false
  const client = recordingClient(calls, {
    subscribe: async (input) => {
      if (input.sessionID === "ses_new" && listenerDown) {
        listenerDown = false
        throw new Error("listener unavailable")
      }
      calls.push(`subscribe ${input.sessionID} [${input.capabilities?.join(",") ?? ""}]`)
      return noInterest()
    },
    getRole: async (role) => ({ role, holder: identity.id, last_seen: 1 }),
  })
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, { client, heartbeatMs: 25, handoffPid: 780 }),
  )

  try {
    listenerDown = true
    await writeSessionHandoff(handoff, "ses_new")
    const newRoleFile = roleStateFile(stateDirectory, "ses_new")
    const expectedRole = `${JSON.stringify({ session_id: "ses_new", role: "reviewer" })}\n`
    await waitFor(
      async () => (await readFile(newRoleFile, "utf8").catch(() => undefined)) === expectedRole,
      `${expectedRole.trim()} in ${newRoleFile}`,
    )
    expect(calls).toContain("setRole ses_new reviewer soft previous=ses_old")
    expect(await readdir(join(stateDirectory, "roles"))).toEqual(["ses_new.json"])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("adopts a handed-off session id when the hook writes it, without waiting for a heartbeat", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const identity = new SessionIdentity("ses_old", "/tmp")
  const handoff = sessionHandoffFile(stateDirectory, 781)
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, {
      client: recordingClient(calls),
      heartbeatMs: 60_000,
      handoffPid: 781,
    }),
  )

  try {
    // The startup registration and the startup tick are done; only the file poll remains.
    await waitFor(
      async () => calls.filter((call) => call === "subscribe ses_old [aside]").length >= 2,
      "the startup registration and the startup tick",
    )
    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(async () => identity.id === "ses_new", "identity to follow the handoff file")
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a registration in flight when Dispatch removes a subscription cannot write the topic back", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  const entry = new Set<string>()
  let held: PromiseWithResolvers<void> | undefined
  let registrationHeld = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      const topics = [...input.topics]
      if (held !== undefined) {
        registrationHeld = true
        await held.promise
      }
      for (const topic of topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
    }),
  )
  const removed = "notifications.dispatch.issue.DSP-3"
  const kept = "notifications.dispatch.issue.DSP-9"

  try {
    await session.follow([removed])
    const release = Promise.withResolvers<void>()
    held = release
    const following = session.follow([kept])
    await waitFor(async () => registrationHeld, "the registration to reach the listener")
    held = undefined
    // Dispatch removes the topic from the entry itself, then tells the session.
    entry.delete(removed)
    nats.emit(
      directSubject,
      JSON.stringify({
        event_id: "remove-2",
        source: "dispatch",
        payload: JSON.stringify({
          issue_key: "DSP-3",
          type: "subscription.removed",
          actor: { kind: "user", id: "alice" },
          payload: {
            session_id: "ses_claude",
            by: { kind: "user", id: "alice" },
            topics: [removed],
          },
        }),
      }),
    )
    await settled()
    release.resolve()
    await following
    await waitFor(async () => !entry.has(removed), `${removed} to stay out of the entry`)
    expect([...entry].sort()).toEqual([directSubject, kept])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a handoff stuck on a stalled NATS flush gives up, so shutdown still deregisters", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  const identity = new SessionIdentity("ses_old", "/tmp")
  const handoff = sessionHandoffFile(stateDirectory, 782)
  await writeSessionHandoff(handoff, "ses_old")
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, {
      connection: nats,
      client: recordingClient(calls),
      heartbeatMs: 25,
      handoffPid: 782,
      handoffFlushTimeoutMs: 50,
    }),
  )

  try {
    nats.stallFlush = true
    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(
      async () => calls.includes("nats.subscribe notifications.agent.ses_new"),
      "the handoff to start",
    )
    await Bun.sleep(150)
    calls.length = 0
    await session.shutdown()
    expect(calls).toContain("unregister ses_old")
    expect(identity.id).toBe("ses_old")
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a handoff that fails before the id switch leaves the new subject out of registrations under the old id", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  const identity = new SessionIdentity("ses_old", "/tmp")
  const handoff = sessionHandoffFile(stateDirectory, 783)
  await writeSessionHandoff(handoff, "ses_old")
  const oldRegistrationTopics: Array<readonly string[]> = []
  let refusedDeregistrations = 0
  const client = recordingClient(calls, {
    subscribe: async (input) => {
      if (input.sessionID === "ses_old") oldRegistrationTopics.push(input.topics)
      return noInterest()
    },
    // Every handoff attempt fails before the id switch.
    unregisterSession: async (sessionID) => {
      if (sessionID !== "ses_old") return
      refusedDeregistrations += 1
      throw new Error("listener unavailable")
    },
  })
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 25,
      handoffPid: 783,
    }),
  )

  try {
    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(async () => refusedDeregistrations >= 2, "two failed handoff attempts")
    await session.follow(["notifications.dispatch.issue.DSP-9"])
    expect(identity.id).toBe("ses_old")
    expect(oldRegistrationTopics.flat()).not.toContain("notifications.agent.ses_new")
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("two handoffs before a registration succeeds move the role from the first id", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const identity = new SessionIdentity("ses_a", "/tmp")
  const roleFile = roleStateFile(stateDirectory, "ses_a")
  await mkdir(dirname(roleFile), { recursive: true })
  await writeFile(roleFile, JSON.stringify({ session_id: "ses_a", role: "reviewer" }))
  const handoff = sessionHandoffFile(stateDirectory, 784)
  await writeSessionHandoff(handoff, "ses_a")
  let refusedB = false
  const client = recordingClient(calls, {
    subscribe: async (input) => {
      if (input.sessionID === "ses_b") {
        refusedB = true
        throw new Error("listener unavailable")
      }
      calls.push(`subscribe ${input.sessionID} [${input.capabilities?.join(",") ?? ""}]`)
      return noInterest()
    },
    getRole: async (role) => ({ role, holder: identity.id, last_seen: 1 }),
  })
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, { client, heartbeatMs: 25, handoffPid: 784 }),
  )

  try {
    await writeSessionHandoff(handoff, "ses_b")
    await waitFor(async () => refusedB, "a failed registration under ses_b")
    await writeSessionHandoff(handoff, "ses_c")
    const cRoleFile = roleStateFile(stateDirectory, "ses_c")
    const expectedRole = `${JSON.stringify({ session_id: "ses_c", role: "reviewer" })}\n`
    await waitFor(
      async () => (await readFile(cRoleFile, "utf8").catch(() => undefined)) === expectedRole,
      `${expectedRole.trim()} in ${cRoleFile}`,
    )
    expect(calls).toContain("setRole ses_c reviewer soft previous=ses_a")
    expect(await readdir(join(stateDirectory, "roles"))).toEqual(["ses_c.json"])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a Dispatch subscription removal that names no topics removes nothing", async () => {
  const stateDirectory = await scratchState()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client: recordingClient(calls),
    }),
  )
  const topic = "notifications.dispatch.issue.DSP-3"

  try {
    await session.follow([topic])
    calls.length = 0
    nats.emit(
      directSubject,
      JSON.stringify({
        event_id: "remove-empty",
        source: "dispatch",
        payload: JSON.stringify({
          issue_key: "DSP-3",
          type: "subscription.removed",
          actor: { kind: "user", id: "alice" },
          payload: { session_id: "ses_claude", by: { kind: "user", id: "alice" }, topics: [] },
        }),
      }),
    )
    await settled()
    expect(calls.filter((call) => call.includes("unsubscribe"))).toEqual([])
    expect(session.topics()).toEqual([directSubject, topic])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})
