import { expect, test } from "bun:test"
import { mkdir, readFile, rm, writeFile } from "node:fs/promises"
import { dirname } from "node:path"
import plugin from "../.claude-plugin/plugin.json" with { type: "json" }
import ompPlugin from "../.omp-plugin/plugin.json" with { type: "json" }
import pkg from "../package.json" with { type: "json" }
import {
  createChannelDelivery,
  enqueueChannelMessage,
  MCP_SERVER_INFO,
  sanitizeChannelMetadata,
  startChannelSession,
} from "../src/envoy-channel-server"
import { roleStateFile, SessionIdentity } from "../src/session-identity"
import {
  deliveryRaw,
  directSubject,
  type FakeClient,
  FakeNats,
  FakeNotifier,
  noInterest,
  recordingClient,
  scratchState,
  sessionOptions,
  settled,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

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

test("delivers each dedupe key once while acknowledging every forwarded request after enqueue", async () => {
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

/**
 * One Dispatch message as the listener publishes it for one send: `/v1/messages/send` mints a
 * fresh `event_id`, `source_event_id` and `trace_id` for every request and derives `dedupe_key`
 * from the idempotency key Dispatch passes, which is the same for an attempt and its same-mode
 * Retry (`packages/envoy/cmd/listener/api.go` messageEnvelope, sendHandler).
 */
function listenerSend(send: string): string {
  return JSON.stringify({
    event_id: `evt-${send}`,
    source: "dispatch",
    source_event_id: `src-${send}`,
    topic: directSubject,
    dedupe_key: "agent.ses_claude.33333333-3333-4333-8333-333333333333:aside",
    issued_at: 1_760_000_000_000,
    payload_summary: "Run the migration",
    payload: JSON.stringify({
      event: {
        actor: { id: "alice", kind: "user" },
        issue_key: "CORE-1",
        payload: {
          author: { id: "alice", kind: "user" },
          body: "Run the migration",
          created_at: "2026-09-30T00:00:00Z",
          deliveries: [],
          id: "33333333-3333-4333-8333-333333333333",
          in_reply_to: null,
          issue_key: "CORE-1",
          target: "session:ses_claude",
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode: "aside" },
    }),
    trace_id: `trace-${send}`,
  })
}

test("a Dispatch Retry the listener re-sends under the same dedupe key reaches Claude once", async () => {
  const nats = new FakeNats()
  const notifier = new FakeNotifier()
  const stateDirectory = await scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      notifier,
    }),
  )

  try {
    nats.emit(directSubject, listenerSend("attempt"))
    nats.emit(directSubject, listenerSend("retry"))
    await settled()

    expect(notifier.notifications).toHaveLength(1)
    expect(notifier.notifications[0]?.params.meta["event_id"]).toBe("evt-attempt")
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a send whose notification failed is delivered when it is sent again", async () => {
  const nats = new FakeNats()
  const delivered: string[] = []
  let failNext = true
  const stateDirectory = await scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      notifier: {
        notification: async (notification) => {
          if (failNext) {
            failNext = false
            throw new Error("the MCP transport refused the write")
          }
          delivered.push(String(notification.params.meta["event_id"]))
        },
      },
    }),
  )

  try {
    nats.emit(directSubject, listenerSend("attempt"))
    await settled()
    nats.emit(directSubject, listenerSend("retry"))
    await settled()

    // Claude never saw the first send, so its key must not turn the second away as a repeat.
    expect(delivered).toEqual(["evt-retry"])
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
