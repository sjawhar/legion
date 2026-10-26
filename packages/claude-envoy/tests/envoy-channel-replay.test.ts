import { describe, expect, setSystemTime, spyOn, test } from "bun:test"
import { createHash } from "node:crypto"
import { mkdir, open, readdir, readFile, rm, stat, utimes } from "node:fs/promises"
import { createServer, type Socket } from "node:net"
import { join } from "node:path"
import { agentSubject } from "@legion/contracts"
import { REPLAY_LEDGER_TTL_MS, type ReplaySnapshot } from "@legion/envoy-client/replay"
import { createEnvoyClient, type EnvoyClient } from "@legion/envoy-client/transport"
import {
  CHANNEL_CAPABILITIES,
  type ChannelNotifier,
  type ChannelSessionOptions,
  startChannelSession,
} from "../src/envoy-channel-server"
import { createInboxNotifier, INBOX_CAPABILITIES } from "../src/legion-inbox"
import { replayStateFile } from "../src/replay-state"
import { SessionIdentity, sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import { FakeNats, scratchState, sessionOptions, waitFor } from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const SESSION = "ses_replay"
const ISSUE = "DSP-7"
const ISSUE_TOPIC = `notifications.dispatch.issue.${ISSUE}.>`
const ISSUE_BASE = `notifications.dispatch.issue.${ISSUE}`
const ISSUE_SUBJECT = `notifications.dispatch.issue.${ISSUE}.comment`
const HOUR_MS = 60 * 60 * 1_000
const DAY_MS = 24 * HOUR_MS

interface StoredMessage {
  readonly seq: number
  readonly subject: string
  readonly time: string
  readonly data: string
}

interface ReplayRequest {
  readonly session_id: string
  readonly subjects: readonly { readonly subject: string; readonly after?: number }[]
}

// NATS subject matching: `*` is one token, `>` one or more trailing tokens.
function subjectMatches(pattern: string, subject: string): boolean {
  const want = pattern.split(".")
  const got = subject.split(".")
  for (const [index, token] of want.entries()) {
    if (token === ">") return got.length > index
    if (index >= got.length || (token !== "*" && token !== got[index])) return false
  }
  return want.length === got.length
}

/**
 * The Envoy listener as a channel session reaches it: `ENVOY_NOTIFICATIONS` as its replay route
 * reads it (stored messages with their sequence, subject and time, the first sequence retention
 * left, the stream's creation time), and its interest registry, which the route's eligibility
 * reads. The stream keeps agent and Dispatch subjects only, as the deployed one keeps no team
 * topic. `client` is the real transport over this model.
 */
class StreamModel {
  readonly created = "2026-10-20T00:00:00Z"
  firstSeq = 1
  lastSeq = 0
  messages: StoredMessage[] = []
  /** False is a listener without the route, which answers 404. */
  served = true
  replays = 0
  readonly interests = new Map<string, Set<string>>()
  readonly client: EnvoyClient = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: (input, init) => this.fetch(String(input), init),
  })

  store(subject: string, data: string, time = new Date().toISOString()): number {
    this.lastSeq += 1
    this.messages.push({ seq: this.lastSeq, subject, time, data })
    return this.lastSeq
  }

  /** Retention drops every message up to `seq`. */
  expireThrough(seq: number): void {
    this.messages = this.messages.filter((message) => message.seq > seq)
    this.firstSeq = seq + 1
  }

  /** The route's rules: the watermark, a cursor started without `after`, the eligibility. */
  answer(request: ReplayRequest) {
    const watermark = this.lastSeq
    const registered = this.interests.get(request.session_id) ?? new Set<string>()
    return {
      stream: { created: this.created, first_seq: this.firstSeq, last_seq: watermark },
      subjects: request.subjects.map(({ subject, after }) => {
        const refused =
          subject !== agentSubject(request.session_id) && !registered.has(subject)
            ? "not_followed"
            : !subject.startsWith("notifications.agent.") &&
                !subject.startsWith("notifications.dispatch.")
              ? "not_retained"
              : undefined
        if (refused !== undefined) {
          return { subject, messages: [], next_after: after ?? 0, more: false, refused }
        }
        const messages =
          after === undefined
            ? []
            : this.messages.filter(
                (message) =>
                  message.seq > after &&
                  message.seq <= watermark &&
                  subjectMatches(subject, message.subject),
              )
        return { subject, messages, next_after: watermark, more: false }
      }),
    }
  }

  private async fetch(input: string, init: RequestInit | undefined): Promise<Response> {
    const url = new URL(input)
    const method = init?.method ?? "GET"
    const body = init?.body === undefined ? {} : JSON.parse(String(init.body))
    if (url.pathname === "/v1/replay") {
      this.replays += 1
      if (!this.served) return Response.json({ error: "not found" }, { status: 404 })
      return Response.json(this.answer(body as ReplayRequest))
    }
    if (url.pathname === "/v1/interests/subscribe") {
      const { session_id, topics } = body as { session_id: string; topics: string[] }
      const registered = this.interests.get(session_id) ?? new Set<string>()
      for (const topic of topics) registered.add(topic)
      this.interests.set(session_id, registered)
      return Response.json({ session_id, machine_id: "test", dir: "/tmp", topics: [...registered] })
    }
    if (url.pathname === "/v1/interests/unsubscribe") {
      const { session_id, topics } = body as { session_id: string; topics: string[] }
      for (const topic of topics) this.interests.get(session_id)?.delete(topic)
      return Response.json({})
    }
    if (method === "GET" && url.pathname.startsWith("/v1/interests/")) {
      const id = decodeURIComponent(url.pathname.slice("/v1/interests/".length))
      const registered = this.interests.get(id)
      if (registered === undefined || registered.size === 0) {
        return Response.json({ error: "no such interest" }, { status: 404 })
      }
      return Response.json({
        session_id: id,
        machine_id: "test",
        dir: "/tmp",
        topics: [...registered],
      })
    }
    if (method === "DELETE") return new Response(null, { status: 200 })
    return Response.json({ error: "not found" }, { status: 404 })
  }
}

/** What the session was shown: the event a card names (its `event_id` meta, or inbox line id). */
interface Shown {
  readonly eventId: string | undefined
  readonly content: string
}

/** Where a session's deliveries go, as `startChannelSession` sees it, and what reached it. */
interface Sink {
  readonly notifier: ChannelNotifier
  readonly capabilities: ChannelSessionOptions["capabilities"]
  readonly shown: Shown[]
  close(): void
}

/** Claude Code's channel; `refuse` makes a write fail, as a closed MCP transport does. */
function channelSink(refuse: (shown: Shown) => boolean = () => false): Sink {
  const shown: Shown[] = []
  return {
    shown,
    capabilities: CHANNEL_CAPABILITIES,
    notifier: {
      notification: async (notification) => {
        const entry = {
          eventId: notification.params.meta["event_id"],
          content: notification.params.content,
        }
        if (refuse(entry)) throw new Error("the MCP transport refused the write")
        shown.push(entry)
      },
    },
    close: () => undefined,
  }
}

/** `legion claude-bridge`'s inbox socket, which answers every line it queued. */
async function inboxSink(directory: string): Promise<Sink> {
  const shown: Shown[] = []
  const sockets = new Set<Socket>()
  const path = join(directory, "inbox.sock")
  const server = createServer((socket: Socket) => {
    sockets.add(socket)
    let buffered = ""
    socket.setEncoding("utf8")
    socket.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const line = JSON.parse(buffered.slice(0, end)) as { id: string; content: string }
        buffered = buffered.slice(end + 1)
        shown.push({ eventId: line.id, content: line.content })
        socket.write(`${JSON.stringify({ id: line.id, ok: true })}\n`)
      }
    })
  })
  await new Promise<void>((listening) => server.listen(path, listening))
  return {
    shown,
    capabilities: INBOX_CAPABILITIES,
    notifier: createInboxNotifier(path),
    close: () => {
      for (const socket of sockets) socket.destroy()
      server.close()
    },
  }
}

const NOTICE = "message not shown"
const cards = (sink: Sink, eventId: string) =>
  sink.shown.filter((shown) => shown.eventId === eventId && !shown.content.includes(NOTICE))
const notices = (sink: Sink, eventId: string) =>
  sink.shown.filter((shown) => shown.content.includes(NOTICE) && shown.content.includes(eventId))

function minted(label: string): string {
  return createHash("sha256").update(label).digest("hex").slice(0, 32)
}

interface Frame {
  readonly raw: string
  readonly eventId: string
}

/** A peer session's message to `sessionID`, under a dedupe key minted once for it. */
function peerMessage(sessionID: string, label: string): Frame {
  const eventId = `evt-${label}`
  return {
    eventId,
    raw: JSON.stringify({
      event_id: eventId,
      source: "agent",
      source_event_id: `source-${label}`,
      source_session: "ses_peer",
      topic: agentSubject(sessionID),
      dedupe_key: `agent.ses_peer.${minted(label)}`,
      issued_at: 1,
      payload_summary: `peer message ${label}`,
      trace_id: `trace-${label}`,
    }),
  }
}

/** Dispatch's outbox copy of event `id` on the issue topic. */
function issueEvent(id: number): Frame {
  const eventId = `dispatch-${id}`
  return {
    eventId,
    raw: JSON.stringify({
      event_id: eventId,
      source: "dispatch",
      source_event_id: String(id),
      topic: ISSUE_SUBJECT,
      dedupe_key: eventId,
      issued_at: 1,
      payload_summary: `${ISSUE} moved (${id})`,
      trace_id: `trace-dispatch-${id}`,
      payload: JSON.stringify({
        issue_key: ISSUE,
        type: "issue.updated",
        actor: { kind: "user", id: "alice" },
        notify: true,
        payload: { status: "in_progress" },
      }),
    }),
  }
}

/** The listener's role arbiter forwarding a role lane's message onto the holder's subject. */
function roleForward(label: string): Frame {
  const eventId = `evt-${label}`
  return {
    eventId,
    raw: JSON.stringify({
      event_id: eventId,
      source: "agent",
      source_event_id: `source-${label}`,
      source_session: "ses_lead",
      topic: "notifications.role.reviewer",
      dedupe_key: `envoy.role.forward.publish.${minted(label)}`,
      issued_at: 1,
      payload_summary: `role work ${label}`,
      trace_id: `trace-${label}`,
    }),
  }
}

async function replayState(directory: string, sessionID: string): Promise<ReplaySnapshot> {
  return JSON.parse(await readFile(replayStateFile(directory, sessionID), "utf8")) as ReplaySnapshot
}

/** Whether the session's state file holds a started cursor for `subject`. */
async function cursorStarted(directory: string, sessionID: string, subject: string) {
  try {
    return (await replayState(directory, sessionID)).state?.subjects[subject]?.after !== undefined
  } catch {
    return false
  }
}

/**
 * Starts a channel session on `model` and `sink` with its state in `directory`, and resolves once
 * its first sweep has asked the listener and the agent subject's cursor is on file.
 */
async function boot(
  model: StreamModel,
  sink: Sink,
  directory: string,
  options: { readonly sessionID?: string; readonly heartbeatMs?: number; handoffPid?: number } = {},
) {
  const nats = new FakeNats()
  const identity = new SessionIdentity(options.sessionID ?? SESSION, "/tmp")
  const before = model.replays
  const session = await startChannelSession(
    sessionOptions(identity, directory, {
      connection: nats,
      notifier: sink.notifier,
      capabilities: sink.capabilities,
      client: model.client,
      heartbeatMs: options.heartbeatMs ?? 60_000,
      ...(options.handoffPid === undefined ? {} : { handoffPid: options.handoffPid }),
    }),
  )
  await waitFor(
    async () =>
      model.replays > before &&
      (await cursorStarted(directory, identity.id, agentSubject(identity.id))),
    "the session's first sweep",
  )
  return {
    nats,
    identity,
    session,
    /**
     * nats.js reports a reconnect; resolves once the sweep it starts has asked the listener.
     * Sweeps run one at a time, so every sweep started before it has ended by then.
     */
    reconnect: async () => {
      const asked = model.replays
      nats.emitStatus("reconnect")
      await waitFor(async () => model.replays > asked, "a reconnect sweep")
    },
    /** Follows `topic` with the tool's path; its cursor starts at the stream's end. */
    follow: async (topic: string) => {
      await session.follow([topic])
      await waitFor(() => cursorStarted(directory, identity.id, topic), `a cursor for ${topic}`)
    },
  }
}

const SUBJECTS = [
  {
    name: "its own topic",
    follow: undefined,
    subject: agentSubject(SESSION),
    subscription: agentSubject(SESSION),
    frame: (id: number) => peerMessage(SESSION, `own-${id}`),
  },
  {
    name: "a followed issue topic",
    follow: ISSUE_TOPIC,
    subject: ISSUE_SUBJECT,
    subscription: ISSUE_TOPIC,
    frame: issueEvent,
  },
] as const

const SINKS = [
  { name: "the channel notification", make: async (_directory: string) => channelSink() },
  { name: "the Legion bridge inbox", make: inboxSink },
] as const

for (const target of SUBJECTS) {
  for (const route of SINKS) {
    describe(`a frame on ${target.name} stored while the subscription was down, through ${route.name}`, () => {
      test("is shown once after nats.js reconnects, and still once after more sweeps and the live copy", async () => {
        const directory = await scratchState()
        const model = new StreamModel()
        // Something else is stored first, so cursors start past the stream's beginning.
        model.store("notifications.agent.ses_other", peerMessage("ses_other", "unrelated").raw)
        const sink = await route.make(directory)
        const running = await boot(model, sink, directory)
        try {
          if (target.follow !== undefined) await running.follow(target.follow)
          const frame = target.frame(1)
          // Published while the subscription was down: the stream keeps it, nothing is pushed.
          model.store(target.subject, frame.raw)

          await running.reconnect()
          await waitFor(async () => cards(sink, frame.eventId).length === 1, "the missed frame")
          expect(cards(sink, frame.eventId)[0]?.content).toContain(
            "while this session's connection was down",
          )
          await running.reconnect()
          await running.reconnect()
          expect(cards(sink, frame.eventId)).toHaveLength(1)

          // The live copy turns up after all; the frame behind it on the same subscription says
          // when the copy was handled.
          const next = target.frame(2)
          running.nats.emitOn(target.subscription, target.subject, frame.raw)
          running.nats.emitOn(target.subscription, target.subject, next.raw)
          await waitFor(async () => cards(sink, next.eventId).length === 1, "the next live frame")
          expect(cards(sink, frame.eventId)).toHaveLength(1)
        } finally {
          await running.session.shutdown()
          sink.close()
          await rm(directory, { recursive: true, force: true })
        }
      })

      test("is shown once by a channel started again on the same session id, and still once after more heartbeats and the live copy", async () => {
        const directory = await scratchState()
        const model = new StreamModel()
        model.store("notifications.agent.ses_other", peerMessage("ses_other", "unrelated").raw)
        const sink = await route.make(directory)
        const first = await boot(model, sink, directory)
        if (target.follow !== undefined) await first.follow(target.follow)
        await first.session.shutdown()
        const frame = target.frame(3)
        model.store(target.subject, frame.raw)

        const second = await boot(model, sink, directory, { heartbeatMs: 20 })
        try {
          await waitFor(async () => cards(sink, frame.eventId).length === 1, "the missed frame")
          // The third sweep from now asks only once the two before it have ended.
          const sweeps = model.replays
          await waitFor(async () => model.replays >= sweeps + 3, "three more heartbeat sweeps")
          expect(cards(sink, frame.eventId)).toHaveLength(1)

          const next = target.frame(4)
          second.nats.emitOn(target.subscription, target.subject, frame.raw)
          second.nats.emitOn(target.subscription, target.subject, next.raw)
          await waitFor(async () => cards(sink, next.eventId).length === 1, "the next live frame")
          expect(cards(sink, frame.eventId)).toHaveLength(1)
        } finally {
          await second.session.shutdown()
          sink.close()
          await rm(directory, { recursive: true, force: true })
        }
      })
    })
  }
}

test("a channel started on the same session id 31 days after its state was written shows a frame from an hour before its start once, and still follows its issue topic with the registry empty", async () => {
  const startAt = Date.now()
  const directory = await scratchState()
  const model = new StreamModel()
  const sink = channelSink()
  try {
    setSystemTime(new Date(startAt - 31 * DAY_MS))
    const first = await boot(model, sink, directory)
    await first.follow(ISSUE_TOPIC)
    const shownThen = issueEvent(1)
    model.store(ISSUE_SUBJECT, shownThen.raw)
    await first.reconnect()
    await waitFor(async () => cards(sink, shownThen.eventId).length === 1, "the first frame")
    await first.session.shutdown()
    // Published while the session was away and expired since: the stream lost it.
    model.store(ISSUE_SUBJECT, issueEvent(2).raw)
    model.expireThrough(model.lastSeq)
    const file = replayStateFile(directory, SESSION)
    const written = new Date(startAt - 31 * DAY_MS)
    await utimes(file, written, written)

    setSystemTime()
    // The listener reaped the session's interests long ago.
    model.interests.clear()
    const missed = issueEvent(3)
    model.store(ISSUE_SUBJECT, missed.raw, new Date(startAt - HOUR_MS).toISOString())

    const second = await boot(model, sink, directory, { heartbeatMs: 20 })
    try {
      await waitFor(async () => cards(sink, missed.eventId).length === 1, "the missed frame")
      const sweeps = model.replays
      await waitFor(async () => model.replays >= sweeps + 3, "three more heartbeat sweeps")
      expect(cards(sink, missed.eventId)).toHaveLength(1)
      expect(cards(sink, shownThen.eventId)).toHaveLength(1)
      expect(cards(sink, missed.eventId)[0]?.content).toContain(
        new Date(startAt - HOUR_MS).toISOString(),
      )
      // Told once that the stream no longer holds everything published while it was away.
      expect(
        sink.shown.filter((shown) => shown.content.includes("messages may be missing")),
      ).toHaveLength(1)
      expect(second.session.topics()).toContain(ISSUE_TOPIC)
      expect([...(model.interests.get(SESSION) ?? [])]).toContain(ISSUE_TOPIC)
      // The ledger entry recorded 31 days ago is gone from the file its next write replaced.
      const { ledger } = await replayState(directory, SESSION)
      expect(ledger.every((entry) => Date.now() - entry.at < REPLAY_LEDGER_TTL_MS)).toBe(true)
      expect(ledger.map((entry) => entry.event_id)).toContain(missed.eventId)
    } finally {
      await second.session.shutdown()
    }
  } finally {
    setSystemTime()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a stored frame the channel fails to show is shown by the next sweep, once", async () => {
  const directory = await scratchState()
  const model = new StreamModel()
  const frame = peerMessage(SESSION, "flaky")
  let attempts = 0
  const sink = channelSink((shown) => shown.eventId === frame.eventId && ++attempts === 1)
  const running = await boot(model, sink, directory)
  try {
    model.store(agentSubject(SESSION), frame.raw)
    await running.reconnect()
    await waitFor(async () => attempts === 1, "the first attempt to fail")
    expect(cards(sink, frame.eventId)).toHaveLength(0)
    await running.reconnect()
    await waitFor(async () => cards(sink, frame.eventId).length === 1, "the retried frame")
    await running.reconnect()
    expect(cards(sink, frame.eventId)).toHaveLength(1)
    expect(notices(sink, frame.eventId)).toHaveLength(0)
  } finally {
    await running.session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a stored frame the channel never takes is named in one notice on its third failure, and not again after more sweeps or a restart", async () => {
  const directory = await scratchState()
  const model = new StreamModel()
  const frame = peerMessage(SESSION, "refused")
  let attempts = 0
  // Every card naming the frame fails; the notice naming it carries no event id of its own.
  const sink = channelSink((shown) => {
    if (shown.eventId !== frame.eventId) return false
    attempts += 1
    return true
  })
  const first = await boot(model, sink, directory)
  try {
    model.store(agentSubject(SESSION), frame.raw)
    await first.reconnect()
    await waitFor(async () => attempts === 1, "the first failure")
    await first.reconnect()
    await waitFor(async () => attempts === 2, "the second failure")
    expect(notices(sink, frame.eventId)).toHaveLength(0)
    await first.reconnect()
    await waitFor(async () => notices(sink, frame.eventId).length === 1, "the failure notice")
    expect(attempts).toBe(3)
    expect(notices(sink, frame.eventId)[0]?.content).toContain("envoy_inbox")
    await first.reconnect()
    await first.reconnect()
    expect(notices(sink, frame.eventId)).toHaveLength(1)
  } finally {
    await first.session.shutdown()
  }
  const second = await boot(model, sink, directory)
  try {
    await second.reconnect()
    expect(notices(sink, frame.eventId)).toHaveLength(1)
    expect(cards(sink, frame.eventId)).toHaveLength(0)
  } finally {
    await second.session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a live frame the channel fails to take waits for the next sweep, which shows it once", async () => {
  const directory = await scratchState()
  const model = new StreamModel()
  const frame = peerMessage(SESSION, "live-flaky")
  let attempts = 0
  const sink = channelSink((shown) => shown.eventId === frame.eventId && ++attempts === 1)
  const running = await boot(model, sink, directory)
  try {
    // The stream keeps what is published live on the agent subject.
    model.store(agentSubject(SESSION), frame.raw)
    running.nats.emit(agentSubject(SESSION), frame.raw)
    await waitFor(async () => attempts === 1, "the live delivery to fail")
    await running.reconnect()
    await waitFor(async () => cards(sink, frame.eventId).length === 1, "the replayed frame")
    await running.reconnect()
    expect(cards(sink, frame.eventId)).toHaveLength(1)
    // The delivery path is one queue: a notice sent for the live failure came before the card.
    expect(notices(sink, frame.eventId)).toHaveLength(0)
  } finally {
    await running.session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a role forward the channel fails to take after its receipt gets its notice at once and is never replayed", async () => {
  const directory = await scratchState()
  const model = new StreamModel()
  const frame = roleForward("role-lost")
  let attempts = 0
  const sink = channelSink((shown) => shown.eventId === frame.eventId && ++attempts === 1)
  const running = await boot(model, sink, directory)
  try {
    // The stream captures the agent subject, so it stores the forward too.
    model.store(agentSubject(SESSION), frame.raw)
    running.nats.emit(agentSubject(SESSION), frame.raw, "_INBOX.receipt")
    await waitFor(async () => notices(sink, frame.eventId).length === 1, "the failure notice")
    expect(running.nats.published.map(({ subject }) => subject)).toEqual(["_INBOX.receipt"])
    await running.reconnect()
    await running.reconnect()
    expect(cards(sink, frame.eventId)).toHaveLength(0)
    expect(notices(sink, frame.eventId)).toHaveLength(1)
  } finally {
    await running.session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a listener without the replay route is said once, replays nothing, and live frames still arrive", async () => {
  const directory = await scratchState()
  const model = new StreamModel()
  model.served = false
  const sink = channelSink()
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  const nats = new FakeNats()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: sink.notifier,
      client: model.client,
      heartbeatMs: 20,
    }),
  )
  try {
    const stored = peerMessage(SESSION, "unserved")
    model.store(agentSubject(SESSION), stored.raw)
    await waitFor(async () => model.replays >= 4, "four sweeps")
    expect(written.filter((line) => line.includes("does not serve replay"))).toHaveLength(1)
    expect(sink.shown).toEqual([])

    const live = peerMessage(SESSION, "live")
    nats.emit(agentSubject(SESSION), live.raw)
    await waitFor(async () => cards(sink, live.eventId).length === 1, "the live frame")
    expect(cards(sink, stored.eventId)).toHaveLength(0)
  } finally {
    await session.shutdown()
    stderr.mockRestore()
    await rm(directory, { recursive: true, force: true })
  }
})

test("the replay state file is private to its user and replaced whole by each write", async () => {
  const directory = await scratchState()
  const model = new StreamModel()
  const sink = channelSink()
  const running = await boot(model, sink, directory)
  try {
    const file = replayStateFile(directory, SESSION)
    expect((await stat(file)).mode & 0o777).toBe(0o600)
    const firstText = await readFile(file, "utf8")
    // The file as the first write left it, held open across the next write.
    const held = await open(file, "r")
    try {
      await running.follow(ISSUE_TOPIC)
      expect((await stat(file)).mode & 0o777).toBe(0o600)
      // A rename put a new file in its place; nothing was written into the old one.
      expect(await readFile(file, "utf8")).not.toBe(firstText)
      expect(await held.readFile("utf8")).toBe(firstText)
    } finally {
      await held.close()
    }
    expect((await readdir(directory)).filter((name) => name.endsWith(".tmp"))).toEqual([])
    expect(Object.keys((await replayState(directory, SESSION)).state?.subjects ?? {})).toEqual([
      agentSubject(SESSION),
      ISSUE_BASE,
      ISSUE_TOPIC,
    ])
  } finally {
    await running.session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a state file that cannot be written is logged, and what the session was shown stays shown", async () => {
  const directory = await scratchState()
  // A directory where the file goes: every rename onto it fails.
  await mkdir(replayStateFile(directory, SESSION))
  const model = new StreamModel()
  const sink = channelSink()
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  const nats = new FakeNats()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: sink.notifier,
      client: model.client,
    }),
  )
  try {
    await waitFor(async () => model.replays >= 1, "the first sweep")
    const frame = peerMessage(SESSION, "unpersisted")
    model.store(agentSubject(SESSION), frame.raw)
    const reconnect = async () => {
      const asked = model.replays
      nats.emitStatus("reconnect")
      await waitFor(async () => model.replays > asked, "a reconnect sweep")
    }
    await reconnect()
    await waitFor(async () => cards(sink, frame.eventId).length === 1, "the missed frame")
    await reconnect()
    await reconnect()
    expect(cards(sink, frame.eventId)).toHaveLength(1)
    expect(written.some((line) => line.includes("could not persist the replay state"))).toBe(true)
  } finally {
    await session.shutdown()
    stderr.mockRestore()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a /clear handoff to a new session id starts a fresh replay state in a file of its own", async () => {
  const directory = await scratchState()
  const model = new StreamModel()
  const sink = channelSink()
  const handoff = sessionHandoffFile(directory, 931)
  await writeSessionHandoff(handoff, "ses_old")
  const running = await boot(model, sink, directory, { sessionID: "ses_old", handoffPid: 931 })
  try {
    // Published to the new id before it existed: a new session gets no history.
    const before = peerMessage("ses_new", "before")
    model.store(agentSubject("ses_new"), before.raw)

    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(
      () => cursorStarted(directory, "ses_new", agentSubject("ses_new")),
      "the new id's first sweep",
    )
    expect(running.identity.id).toBe("ses_new")
    expect(cards(sink, before.eventId)).toHaveLength(0)
    expect(Object.keys((await replayState(directory, "ses_new")).state?.subjects ?? {})).toEqual([
      agentSubject("ses_new"),
    ])
    // The old id's file stays its own: nothing of the new id went into it.
    expect(Object.keys((await replayState(directory, "ses_old")).state?.subjects ?? {})).toEqual([
      agentSubject("ses_old"),
    ])

    const missed = peerMessage("ses_new", "missed")
    model.store(agentSubject("ses_new"), missed.raw)
    await running.reconnect()
    await waitFor(async () => cards(sink, missed.eventId).length === 1, "the missed frame")
  } finally {
    await running.session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})
