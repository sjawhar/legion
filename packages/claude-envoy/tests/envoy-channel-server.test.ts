import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join, resolve } from "node:path"
import { sessionConfigFile } from "@legion/envoy-client/session-settings"
import { nkeys } from "nats"
import marketplace from "../../../.claude-plugin/marketplace.json" with { type: "json" }
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
import { FakeNatsServer } from "./fake-nats-server"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

/**
 * A role-lane envelope the listener forwarded to the direct subject and awaits a receipt for,
 * under the key the listener minted for the publish behind the arbiter's forward mark.
 */
const roleForwardRaw = JSON.stringify({
  event_id: "evt-role-7",
  dedupe_key: "envoy.role.forward.publish.0123456789abcdef0123456789abcdef",
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
    configFile: sessionConfigFile("claude", process.env),
  })

  await delivery.enqueue({ subject: directSubject, raw: deliveryRaw })

  expect(notifier.notifications).toEqual([
    {
      method: "notifications/claude/channel",
      params: {
        content:
          'envoy:\n  to: you (ses_claude)\n  from: ses_sender\n  at: "2025-10-09T08:53:20Z"\n  id: evt-42\n  urgency: high\n  reply_with:\n    tool: envoy_send\n    args:\n      session_id: ses_sender\n      in_reply_to: evt-42\n      message: ...\n  summary: Use the channel',
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
      return true
    },
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
      return true
    },
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
    nats.emit(directSubject, listenerSend("third"))
    await settled()

    // Claude never saw the first send, so its key must not turn the second away as a repeat; it
    // did see the second, so the third is one.
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
    configFile: sessionConfigFile("claude", process.env),
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

test("registers no delivery mode for a session whose channel Claude Code did not load", async () => {
  const stateDirectory = await scratchState()
  const subscribes: unknown[] = []
  const client: FakeClient = recordingClient([], {
    subscribe: async (input) => {
      subscribes.push(input)
      return noInterest()
    },
  })

  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      client,
      capabilities: [],
    }),
  )

  try {
    expect(subscribes).toEqual([
      expect.objectContaining({ sessionID: "ses_claude", capabilities: [] }),
    ])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
    await Bun.sleep(5)
  }
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

test("Claude Code versions the plugin by its commit; omp and the MCP server report package.json's", () => {
  // Claude Code updates an installed plugin only when its computed version changes. With no
  // version in plugin.json or the marketplace entry it takes the version of a relative-path plugin
  // from the marketplace commit it installs from, so every merge to main reaches an update; a
  // declared version nothing bumps holds every install at the tree it was first installed from.
  expect(plugin).not.toHaveProperty("version")
  const entries = marketplace.plugins.filter((entry) => entry.name === plugin.name)
  expect(entries).toHaveLength(1)
  expect(entries[0]).not.toHaveProperty("version")
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
    configFile: sessionConfigFile("claude", process.env),
  })

  try {
    // Not handed to Claude Code, so the forwarder releases its claim: Dispatch records the attempt
    // failed, and a same-mode Retry is answered again rather than dropped as a repeat.
    expect(await delivery.enqueue({ subject: directSubject, raw: rejectedFrameRaw })).toBe(false)

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

// The forwarder releases the claim of a frame Claude Code was not handed, so the re-send of a
// Dispatch frame the channel answered with an error is answered again rather than dropped as a
// repeat of its dedupe key.
test("a Dispatch frame the channel answered with an error is answered again when re-sent", async () => {
  const control = JSON.parse(
    rejectedFrameRaw.replaceAll(
      "44444444-4444-4444-8444-444444444444",
      "55555555-5555-4555-8555-555555555555",
    ),
  )
  const replied: string[] = []
  const controlAnswered = Promise.withResolvers<void>()
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      const path = new URL(request.url).pathname
      replied.push(path)
      if (path.includes("55555555")) controlAnswered.resolve()
      return Response.json({})
    },
  })
  const previous = { ...process.env }
  process.env["DISPATCH_URL"] = `http://127.0.0.1:${server.port}`
  process.env["DISPATCH_TOKEN"] = "reply-token"
  const nats = new FakeNats()
  const notifier = new FakeNotifier()
  const stateDirectory = await scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", process.cwd()), stateDirectory, {
      connection: nats,
      notifier,
    }),
  )

  try {
    nats.emit(directSubject, rejectedFrameRaw)
    // The listener mints a fresh event id for the re-send; its dedupe key is the first send's.
    nats.emit(
      directSubject,
      JSON.stringify({ ...JSON.parse(rejectedFrameRaw), event_id: "resend" }),
    )
    // One subscription's frames are delivered in order, so the control's answer comes after
    // whatever the channel did with the re-send.
    nats.emit(
      directSubject,
      JSON.stringify({ ...control, event_id: "control", dedupe_key: "dispatch-control" }),
    )
    await controlAnswered.promise

    expect(notifier.notifications).toEqual([])
    expect(replied.filter((path) => path.includes("44444444"))).toHaveLength(2)
  } finally {
    await session.shutdown()
    server.stop(true)
    process.env = previous
    await rm(stateDirectory, { recursive: true, force: true })
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
    configFile: sessionConfigFile("claude", process.env),
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
    configFile: sessionConfigFile("claude", process.env),
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

const channelEntry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

/**
 * Runs the channel server as Claude Code does, in `env.HOME` with `env` as its whole environment:
 * it initializes over MCP, lists the tools, calls `envoy_whoami`, then each of `calls` (request ids
 * 4 and on), then closes stdin. Returns the exit code, each response by its request id, and stderr.
 */
async function runChannelServer(
  env: Readonly<Record<string, string>> & { readonly HOME: string },
  calls: readonly { readonly name: string; readonly arguments: object }[] = [],
): Promise<{
  readonly exitCode: number
  readonly responses: ReadonlyMap<number, Record<string, unknown>>
  readonly stderr: string
}> {
  const server = Bun.spawn(["bun", channelEntry], {
    cwd: env.HOME,
    env: { PATH: process.env["PATH"] ?? "", ...env },
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  })
  const responses = new Map<number, Record<string, unknown>>()
  const waiters = new Map<number, () => void>()
  // A response, or the server's exit naming the request it never answered.
  const answered = (id: number): Promise<void> => {
    if (responses.has(id)) return Promise.resolve()
    const waiting = Promise.withResolvers<void>()
    waiters.set(id, waiting.resolve)
    return Promise.race([
      waiting.promise,
      server.exited.then((code) => {
        throw new Error(`the channel server exited ${code} before answering request ${id}`)
      }),
    ])
  }
  const reading = (async () => {
    let buffered = ""
    for await (const chunk of server.stdout.pipeThrough(new TextDecoderStream())) {
      buffered += chunk
      for (let newline = buffered.indexOf("\n"); newline !== -1; newline = buffered.indexOf("\n")) {
        const message = JSON.parse(buffered.slice(0, newline)) as Record<string, unknown>
        buffered = buffered.slice(newline + 1)
        const id = message["id"]
        if (typeof id !== "number") continue
        responses.set(id, message)
        waiters.get(id)?.()
      }
    }
  })()
  const send = (message: object) => server.stdin.write(`${JSON.stringify(message)}\n`)
  try {
    send({
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: {
        protocolVersion: "2025-06-18",
        capabilities: {},
        clientInfo: { name: "test", version: "0" },
      },
    })
    await answered(1)
    send({ jsonrpc: "2.0", method: "notifications/initialized" })
    send({ jsonrpc: "2.0", id: 2, method: "tools/list", params: {} })
    send({
      jsonrpc: "2.0",
      id: 3,
      method: "tools/call",
      params: { name: "envoy_whoami", arguments: {} },
    })
    for (const [index, call] of calls.entries()) {
      send({ jsonrpc: "2.0", id: 4 + index, method: "tools/call", params: call })
    }
    await Promise.all([answered(2), answered(3), ...calls.map((_, index) => answered(4 + index))])
    await server.stdin.end()
    const [exitCode, stderr] = await Promise.all([
      server.exited,
      new Response(server.stderr).text(),
      reading,
    ])
    return { exitCode, responses, stderr }
  } finally {
    server.kill("SIGKILL")
  }
}

test("with CLAUDE_CONFIG_DIR holding no envoy.json the server names the unread old file, lists no tool, and exits 0 when stdin ends", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-off-"))
  const configDirectory = join(home, "claude-config")
  const old = join(home, ".config", "opencode", "envoy.json")
  await mkdir(configDirectory)
  await mkdir(dirname(old), { recursive: true })
  await writeFile(
    old,
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: "https://old.dispatch.example",
        token: "old-token",
      },
    }),
  )
  try {
    const { exitCode, responses, stderr } = await runChannelServer({
      HOME: home,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_CODE_SESSION_ID: "ses_off",
      CLAUDE_PLUGIN_DATA: home,
    })
    const file = join(configDirectory, "envoy.json")
    const notice = `${old} is not read for this profile; copy or link it to ${file}, or write that file`

    expect(responses.get(2)?.["result"]).toEqual({ tools: [] })
    expect(responses.get(3)?.["error"]).toMatchObject({
      message: expect.stringContaining(
        `Envoy is off for this session: no envoy.json at ${file}: Envoy is off; ${notice}`,
      ),
    })
    expect(stderr).toContain(
      `envoy-channel: no envoy.json at ${file}: Dispatch and Envoy are off; ${notice}`,
    )
    expect(exitCode).toBe(0)
  } finally {
    await rm(home, { recursive: true, force: true })
  }
}, 20_000)

test("an envoy.json that turns Envoy off opens no NATS connection and calls no listener, whatever the variables name", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-off-file-"))
  const configDirectory = join(home, "claude-config")
  await mkdir(configDirectory)
  await writeFile(
    join(configDirectory, "envoy.json"),
    JSON.stringify({ envoy: { enabled: false } }),
  )
  const broker = new FakeNatsServer()
  const listenerCalls: string[] = []
  const listener = Bun.serve({
    port: 0,
    fetch: (request) => {
      listenerCalls.push(`${request.method} ${new URL(request.url).pathname}`)
      return Response.json({ error: "no call belongs here" }, { status: 500 })
    },
  })
  try {
    const { exitCode, responses } = await runChannelServer({
      HOME: home,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_CODE_SESSION_ID: "ses_off_file",
      CLAUDE_PLUGIN_DATA: home,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      ENVOY_NATS_URL: broker.url,
    })

    expect(responses.get(2)?.["result"]).toEqual({ tools: [] })
    expect(exitCode).toBe(0)
    expect(broker.connections).toBe(0)
    expect(listenerCalls).toEqual([])
  } finally {
    listener.stop(true)
    await broker.stop()
    await rm(home, { recursive: true, force: true })
  }
}, 20_000)

// The listener's bearer and the NATS seed are resolved once and handed to the client and the
// connection: a server that dropped either would leave the session unregistered or signed out.
test("the server calls the listener with the file's envoy.token and signs in to NATS with its seed", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-credentials-"))
  const configDirectory = join(home, "claude-config")
  await mkdir(configDirectory)
  const broker = new FakeNatsServer({ nonce: "fake-nonce" })
  const user = nkeys.createUser()
  const auth: Array<string | null> = []
  const registered = Promise.withResolvers<void>()
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      auth.push(request.headers.get("Authorization"))
      const url = new URL(request.url)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as { session_id: string; topics: string[] }
        registered.resolve()
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
        })
      }
      if (request.method === "DELETE") return new Response(null, { status: 200 })
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  await writeFile(
    join(configDirectory, "envoy.json"),
    JSON.stringify({
      envoy: {
        enabled: true,
        url: `http://127.0.0.1:${listener.port}`,
        natsUrls: [broker.url],
        token: "file-token",
        natsSeed: new TextDecoder().decode(user.getSeed()),
      },
    }),
  )
  const server = Bun.spawn(["bun", channelEntry], {
    cwd: home,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: home,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_CODE_SESSION_ID: "ses_credentials",
      CLAUDE_PLUGIN_DATA: home,
    },
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  })
  try {
    await Promise.all([registered.promise, broker.until(() => broker.connects.length > 0)])
    expect(broker.connects[0]).toMatchObject({ nkey: user.getPublicKey() })
    expect(new Set(auth)).toEqual(new Set(["Bearer file-token"]))
    server.kill("SIGTERM")
    expect(await server.exited).toBe(0)
  } finally {
    server.kill("SIGKILL")
    listener.stop(true)
    await broker.stop()
    await rm(home, { recursive: true, force: true })
  }
}, 20_000)

/** A listener that answers `GET /v1/sessions` with no session and records every call. */
function recordingListener(calls: string[]) {
  return Bun.serve({
    port: 0,
    fetch: (request) => {
      const path = new URL(request.url).pathname
      calls.push(`${request.method} ${path}`)
      if (request.method === "GET" && path === "/v1/sessions") return Response.json([])
      return Response.json({ error: "no call belongs here" }, { status: 500 })
    },
  })
}

// Inbound messages arrive over NATS; the tools call the listener. An Envoy with no NATS server
// keeps its tools, as pi-envoy's does, and registers nothing a delivery would reach deaf.
test("with Envoy on and no NATS server the server serves its listener tools with inbound off, and says so", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-no-nats-"))
  const configDirectory = join(home, "claude-config")
  await mkdir(configDirectory)
  const calls: string[] = []
  const listener = recordingListener(calls)
  await writeFile(
    join(configDirectory, "envoy.json"),
    JSON.stringify({ envoy: { url: `http://127.0.0.1:${listener.port}` } }),
  )
  try {
    const { exitCode, responses, stderr } = await runChannelServer(
      {
        HOME: home,
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_CODE_SESSION_ID: "ses_no_nats",
        CLAUDE_PLUGIN_DATA: home,
      },
      [
        { name: "envoy_sessions", arguments: {} },
        { name: "envoy_subscribe", arguments: { topics: ["notifications.role.reviewer"] } },
      ],
    )

    const listed = responses.get(2)?.["result"] as { tools: { name: string }[] } | undefined
    expect(listed?.tools.map((tool) => tool.name)).toContain("envoy_send")
    expect(JSON.stringify(responses.get(3)?.["result"])).toContain("ses_no_nats")
    expect(responses.get(4)?.["result"]).toEqual({ content: [{ type: "text", text: "[]" }] })
    expect(responses.get(5)?.["error"]).toMatchObject({
      message: expect.stringContaining(
        "inbound messages are off for this session: Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)",
      ),
    })
    expect(stderr).toContain(
      "envoy-channel: Envoy's inbound messages are off: Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL); the Envoy tools still work",
    )
    expect(calls).toEqual(["GET /v1/sessions"])
    expect(exitCode).toBe(0)
  } finally {
    listener.stop(true)
    await rm(home, { recursive: true, force: true })
  }
}, 20_000)

test("a refused NATS seed leaves the listener tools on, opens no NATS connection, and says why", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-seed-refused-"))
  const configDirectory = join(home, "claude-config")
  await mkdir(configDirectory)
  const calls: string[] = []
  const listener = recordingListener(calls)
  const broker = new FakeNatsServer()
  const file = join(configDirectory, "envoy.json")
  await writeFile(
    file,
    JSON.stringify({
      envoy: {
        url: `http://127.0.0.1:${listener.port}`,
        natsUrls: ["nats://file.internal.example:4222"],
        natsSeed: "SUFILESEED",
      },
    }),
  )
  try {
    const { exitCode, responses, stderr } = await runChannelServer(
      {
        HOME: home,
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_CODE_SESSION_ID: "ses_seed_refused",
        CLAUDE_PLUGIN_DATA: home,
        ENVOY_NATS_URL: broker.url,
      },
      [{ name: "envoy_sessions", arguments: {} }],
    )

    expect(responses.get(4)?.["result"]).toEqual({ content: [{ type: "text", text: "[]" }] })
    expect(stderr).toContain(
      `envoy-channel: Envoy's inbound messages are off: ENVOY_NATS_URL names other servers than ${file}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them; the Envoy tools still work`,
    )
    expect(broker.connections).toBe(0)
    expect(calls).toEqual(["GET /v1/sessions"])
    expect(exitCode).toBe(0)
  } finally {
    listener.stop(true)
    await broker.stop()
    await rm(home, { recursive: true, force: true })
  }
}, 20_000)
