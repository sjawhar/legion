import { expect, test } from "bun:test"
import { rm } from "node:fs/promises"
import { createServer, type Socket } from "node:net"
import { join } from "node:path"
import { agentSubject } from "@legion/contracts"
import {
  type DeliveryRequest,
  EnvoyApiError,
  type SubscribeInput,
  type SubscribeResult,
} from "@legion/envoy-client/transport"
import {
  type ChannelNotification,
  type ChannelNotifier,
  startChannelSession,
} from "../src/envoy-channel-server"
import { createInboxNotifier, INBOX_CAPABILITIES } from "../src/legion-inbox"
import { SessionIdentity } from "../src/session-identity"
import {
  FakeNats,
  noInterest,
  recordingClient,
  scratchState,
  sessionOptions,
  settled,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const SESSION = "ses_claude"
const DIRECT = agentSubject(SESSION)
const STREAM = "stream-1"

const CONSUMER = {
  mode: "consumer",
  subject: `notifications.envoy.deliver.${SESSION}`,
  stream_id: STREAM,
  gap: false,
} as const

/** A peer session's message to this session, `label` naming its event. */
function peerMessage(
  label: string,
  topic: string = DIRECT,
): { readonly raw: string; readonly id: string } {
  const id = `evt-${label}`
  return {
    id,
    raw: JSON.stringify({
      event_id: id,
      source: "agent",
      source_session: "ses_peer",
      topic,
      dedupe_key: `delivery-${label}`,
      issued_at: 1_760_000_000_000,
      payload_summary: `peer message ${label}`,
    }),
  }
}

/**
 * The listener, as the session's registrations reach it: `answer` decides what each one is
 * answered, or throws the refusal it is refused with.
 */
function listener(answer: (input: SubscribeInput) => SubscribeResult) {
  const registrations: SubscribeInput[] = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input)
      return answer(input)
    },
  })
  return { client, registrations }
}

/** A notifier whose writes resolve only when the test lets them. */
function heldNotifier() {
  const written: ChannelNotification[] = []
  const pending: PromiseWithResolvers<void>[] = []
  const notifier: ChannelNotifier = {
    notification(notification) {
      written.push(notification)
      const held = Promise.withResolvers<void>()
      pending.push(held)
      return held.promise
    },
  }
  return { notifier, written, pending }
}

const delivery = (input: SubscribeInput | undefined): DeliveryRequest | undefined => input?.delivery

test("channel mode answers a relay frame received at once and done only once the notification write resolves", async () => {
  const directory = await scratchState()
  const nats = new FakeNats()
  const held = heldNotifier()
  const { client, registrations } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: held.notifier,
      client,
    }),
  )
  try {
    expect(delivery(registrations[0])).toMatchObject({ consumer: true })
    const message = peerMessage("one")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 7, raw: message.raw, reply: "_INBOX.one" })
    await waitFor(async () => held.written.length === 1, "the notification write")
    // `received` went out before the write began, and nothing more while it is unresolved.
    expect(nats.repliesOn("_INBOX.one")).toEqual(["received"])
    expect(held.written[0]?.params.meta["event_id"]).toBe(message.id)
    await settled()
    expect(nats.repliesOn("_INBOX.one")).toEqual(["received"])

    held.pending[0]?.resolve()
    await waitFor(async () => nats.repliesOn("_INBOX.one").length === 2, "done")
    expect(nats.repliesOn("_INBOX.one")).toEqual(["received", "done"])
  } finally {
    for (const write of held.pending) write.resolve()
    await session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

/** `legion claude-bridge`'s inbox, which answers each line once the test lets it. */
async function heldInbox(directory: string) {
  const lines: { readonly id: string; answer(ok: boolean): void }[] = []
  const sockets = new Set<Socket>()
  const path = join(directory, "inbox.sock")
  const server = createServer((socket: Socket) => {
    sockets.add(socket)
    let buffered = ""
    socket.setEncoding("utf8")
    socket.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const line = JSON.parse(buffered.slice(0, end)) as { id: string }
        buffered = buffered.slice(end + 1)
        lines.push({
          id: line.id,
          answer: (ok) =>
            socket.write(
              `${JSON.stringify(ok ? { id: line.id, ok: true } : { id: line.id, error: "Claude Code has exited" })}\n`,
            ),
        })
      }
    })
  })
  await new Promise<void>((listening) => server.listen(path, listening))
  return {
    lines,
    notifier: createInboxNotifier(path),
    close() {
      for (const socket of sockets) socket.destroy()
      server.close()
    },
  }
}

test("bridge mode answers done once the inbox says Claude Code took the message, and nak on an inbox error", async () => {
  const directory = await scratchState()
  const inbox = await heldInbox(directory)
  const nats = new FakeNats()
  const { client } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: inbox.notifier,
      capabilities: INBOX_CAPABILITIES,
      client,
    }),
  )
  try {
    const taken = peerMessage("taken")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 1, raw: taken.raw, reply: "_INBOX.taken" })
    await waitFor(async () => inbox.lines.length === 1, "the inbox line")
    await settled()
    expect(nats.repliesOn("_INBOX.taken")).toEqual(["received"])
    inbox.lines[0]?.answer(true)
    await waitFor(async () => nats.repliesOn("_INBOX.taken").length === 2, "done")
    expect(nats.repliesOn("_INBOX.taken")).toEqual(["received", "done"])

    const refused = peerMessage("refused")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 2, raw: refused.raw, reply: "_INBOX.refused" })
    await waitFor(async () => inbox.lines.length === 2, "the second inbox line")
    inbox.lines[1]?.answer(false)
    await waitFor(async () => nats.repliesOn("_INBOX.refused").length === 2, "nak")
    expect(nats.repliesOn("_INBOX.refused")).toEqual(["received", "nak"])

    // The listener sends it again, and this time the bridge takes it: it is shown, not dropped.
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 2,
      raw: refused.raw,
      reply: "_INBOX.again",
      delivered: 2,
    })
    await waitFor(async () => inbox.lines.length === 3, "the redelivered inbox line")
    expect(inbox.lines[2]?.id).toBe(refused.id)
    inbox.lines[2]?.answer(true)
    await waitFor(async () => nats.repliesOn("_INBOX.again").length === 2, "done again")
    expect(nats.repliesOn("_INBOX.again")).toEqual(["received", "done"])
  } finally {
    await session.shutdown()
    inbox.close()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a frame written before a restart is answered done unshown after it, and the resume point is registered", async () => {
  const directory = await scratchState()
  const message = peerMessage("once")
  const first = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const firstNats = new FakeNats()
  const firstNotifier = heldNotifier()
  const before = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: firstNats,
      notifier: firstNotifier.notifier,
      client: first.client,
    }),
  )
  try {
    firstNats.emitRelay(SESSION, { subject: DIRECT, seq: 5, raw: message.raw, reply: "_INBOX.a" })
    await waitFor(async () => firstNotifier.written.length === 1, "the first write")
    firstNotifier.pending[0]?.resolve()
    await waitFor(async () => firstNats.repliesOn("_INBOX.a").includes("done"), "done")
  } finally {
    await before.shutdown()
  }

  const second = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const secondNats = new FakeNats()
  const secondNotifier = heldNotifier()
  const after = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: secondNats,
      notifier: secondNotifier.notifier,
      client: second.client,
    }),
  )
  try {
    // The resume point persisted with the state file seeds the listener's consumer.
    expect(delivery(second.registrations[0])?.resume).toEqual({ stream_id: STREAM, seq: 5 })
    // The listener lost the `done` and sends the frame again: the ledger knows it was shown.
    secondNats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 5,
      raw: message.raw,
      reply: "_INBOX.b",
      delivered: 2,
    })
    await waitFor(async () => secondNats.repliesOn("_INBOX.b").includes("done"), "done")
    expect(secondNats.repliesOn("_INBOX.b")).toEqual(["received", "done"])
    expect(secondNotifier.written).toEqual([])
  } finally {
    for (const write of secondNotifier.pending) write.resolve()
    await after.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

// A refusal is the whole of this session's inbound: nothing else reaches it, so it must say so
// and keep asking rather than run on quietly deaf. The listener refuses by name, and
// `parseSubscribeAnswer` refuses an answer naming no consumer the same way, so there is nothing
// left to fall back to.
for (const refusal of [
  { status: 503, reason: "no_grant" },
  { status: 409, reason: "not_requested" },
] as const) {
  test(`a registration refused ${refusal.status} ${refusal.reason} leaves the session running with one notice naming it, and the next heartbeat asks again`, async () => {
    const directory = await scratchState()
    const nats = new FakeNats()
    const notifier = heldNotifier()
    let refusing = true
    const { client, registrations } = listener(() => {
      if (!refusing) return { ...noInterest(), delivery: CONSUMER }
      throw new EnvoyApiError({
        method: "POST",
        url: "http://127.0.0.1:4000/v1/interests/subscribe",
        status: refusal.status,
        responseBody: JSON.stringify({
          error: "the listener could not give this session a consumer",
          reason: refusal.reason,
        }),
      })
    })
    const session = await startChannelSession(
      sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
        connection: nats,
        notifier: notifier.notifier,
        client,
        heartbeatMs: 10,
      }),
    )
    const notices = () =>
      notifier.written.filter((written) => written.params.meta["producer"] === "envoy")
    try {
      // The session runs on, and its relay subject stays open, so the consumer a later
      // registration is given has somewhere to land.
      expect([...nats.subscriptions.keys()]).toEqual([CONSUMER.subject])
      await waitFor(async () => notices().length === 1, "the notice")
      expect(notices()[0]?.params.content).toContain(refusal.reason)
      expect(notices()[0]?.params.content).toContain("Envoy inbound unavailable")

      // Every heartbeat asks again, and the session is told once, not once per tick.
      const asked = registrations.length
      await waitFor(async () => registrations.length >= asked + 2, "two more registrations")
      expect(notices()).toHaveLength(1)

      // Accepted at last: what the stream kept for the session arrives on its relay.
      refusing = false
      const accepted = registrations.length
      await waitFor(async () => registrations.length > accepted, "the accepted registration")
      const message = peerMessage("after-refusal")
      nats.emitRelay(SESSION, {
        subject: DIRECT,
        seq: 12,
        raw: message.raw,
        reply: "_INBOX.after-refusal",
      })
      // Every write is let through as it comes: the notice, then the frame behind it.
      await waitFor(async () => {
        for (const write of notifier.pending) write.resolve()
        return notifier.written.some((written) => written.params.meta["event_id"] === message.id)
      }, "the frame after the refusal")
    } finally {
      for (const write of notifier.pending) write.resolve()
      await session.shutdown()
      await rm(directory, { recursive: true, force: true })
    }
  })
}

/** Dispatch's BTW to this session: a side question, which this session never advertises. */
function btwFrame(label: string): { readonly raw: string; readonly id: string } {
  const id = `dispatch-btw-${label}`
  return {
    id,
    raw: JSON.stringify({
      event_id: id,
      dedupe_key: id,
      source: "dispatch",
      source_event_id: "1",
      topic: DIRECT,
      issued_at: 1,
      payload_summary: `BTW ${label}`,
      payload: JSON.stringify({
        event: {
          actor: { id: "alice", kind: "user" },
          issue_key: "CORE-1",
          payload: {
            author: { id: "alice", kind: "user" },
            body: `BTW ${label}`,
            created_at: "2026-09-12T00:00:00Z",
            deliveries: [],
            id: "11111111-1111-4111-8111-111111111111",
            in_reply_to: null,
            issue_key: "CORE-1",
            target: `session:${SESSION}`,
          },
          type: "message.created",
        },
        delivery: { attempt: 1, mode: "btw" },
      }),
    }),
  }
}

test("a late BTW is passed over and answered done, and the frame after it is shown", async () => {
  const directory = await scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  const { client } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
    }),
  )
  try {
    const btw = btwFrame("late")
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 3,
      raw: btw.raw,
      reply: "_INBOX.btw",
      delivered: 2,
    })
    const after = peerMessage("after-btw")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 4, raw: after.raw, reply: "_INBOX.after" })
    await waitFor(async () => notifier.written.length === 1, "the frame after the BTW")
    expect(notifier.written[0]?.params.meta["event_id"]).toBe(after.id)
    expect(nats.repliesOn("_INBOX.btw")).toEqual(["received", "done"])
  } finally {
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

// The acceptance line of dispatch://INFRA-771: no Envoy session receives from a plain NATS
// subscription. The whole subscription set is asserted, never the presence of the relay subject,
// so a core subscription added back on any followed subject fails here. Oh My Pi's plugin holds
// one more, its agent-stream control subject; this server has no agent stream, so the relay
// subject is the whole set.
test("holds exactly its relay subject after registration, and no other subscription", async () => {
  const directory = await scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  const { client, registrations } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
      heartbeatMs: 10,
    }),
  )
  try {
    await session.follow(["notifications.team.deploy", "notifications.dispatch.issue.DSP-7.>"])
    // Heartbeats ran meanwhile, each registering what the session follows.
    const ticks = registrations.length
    await waitFor(async () => registrations.length >= ticks + 2, "two more heartbeats")
    await settled()

    expect(registrations.at(-1)?.topics).toEqual(
      expect.arrayContaining(["notifications.team.deploy", "notifications.dispatch.issue.DSP-7.>"]),
    )
    expect([...nats.subscriptions.keys()]).toEqual([CONSUMER.subject])
    expect(nats.unsubscribed).toEqual([])
    // Nothing was published either: a session answers no receipt, and the only bodies on the bus
    // are a relay frame's own replies, of which there have been none.
    expect(nats.published).toEqual([])
    expect(notifier.written).toEqual([])
  } finally {
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a reconnect begins a new epoch and registers it at once", async () => {
  const directory = await scratchState()
  const nats = new FakeNats()
  const { client, registrations } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      client,
    }),
  )
  try {
    const firstEpoch = delivery(registrations[0])?.epoch
    expect(firstEpoch).toBeString()
    const before = registrations.length
    nats.emitStatus("reconnect")
    await waitFor(async () => registrations.length > before, "a registration after the reconnect")
    const renewed = delivery(registrations.at(-1))?.epoch
    expect(renewed).toBeString()
    expect(renewed).not.toBe(firstEpoch)
  } finally {
    await session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a gap the registration reports is told once, through the notice path", async () => {
  const directory = await scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  let gap = true
  const { client } = listener(() => {
    const answer = { ...noInterest(), delivery: { ...CONSUMER, gap } }
    gap = false
    return answer
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
    }),
  )
  try {
    // The first frame after the restart carries the same gap.
    const message = peerMessage("after-gap")
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 40,
      raw: message.raw,
      reply: "_INBOX.gap",
      gap: true,
    })
    // Every write is let through as it comes: the notice, then the frame behind it.
    await waitFor(async () => {
      for (const write of notifier.pending) write.resolve()
      return nats.repliesOn("_INBOX.gap").includes("done")
    }, "done")
    expect(notifier.written.map((written) => written.params.meta["event_id"])).toEqual([
      undefined,
      message.id,
    ])
    const notices = notifier.written.filter(
      (written) => written.params.meta["producer"] === "envoy",
    )
    expect(notices).toHaveLength(1)
    expect(notices[0]?.params.content).toContain("72 hours")
  } finally {
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})
