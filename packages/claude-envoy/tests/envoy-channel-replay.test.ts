/**
 * What a channel session keeps of its deliveries, end to end over its relay: the record in the
 * session id's state file (`src/replay-state.ts`), the topics it follows again at start, the
 * registry entry a stored removal closes, and the notice naming a frame it never took. The
 * ledger, the notice's wording and the relay's own rules are
 * `@legion/envoy-client/delivery-ledger` and `/consumer-delivery`, tested there; here they are
 * wired to this server.
 */
import { expect, spyOn, test } from "bun:test"
import { mkdir, open, readdir, readFile, rm, stat } from "node:fs/promises"
import { agentSubject } from "@legion/contracts"
import type { Interest, SubscribeResult } from "@legion/envoy-client/transport"
import { startChannelSession } from "../src/envoy-channel-server"
import { type ReplayStateFile, replayStateFile, writeReplayState } from "../src/replay-state"
import { SessionIdentity, sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import {
  type FakeClient,
  FakeNats,
  noInterest,
  recordingClient,
  scratchState,
  sessionOptions,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const SESSION = "ses_replay"
const DIRECT = agentSubject(SESSION)
const ISSUE = "DSP-7"
const ISSUE_TOPIC = `notifications.dispatch.issue.${ISSUE}.>`
const ISSUE_BASE = `notifications.dispatch.issue.${ISSUE}`

/** What the session was shown: the event a card names, and its text. */
interface Shown {
  readonly eventId: string | undefined
  readonly content: string
}

/** Claude Code's channel; `refuse` makes a write fail, as a closed MCP transport does. */
function channelSink(refuse: (shown: Shown) => boolean = () => false) {
  const shown: Shown[] = []
  return {
    shown,
    notifier: {
      notification: async (notification: {
        params: { content: string; meta: Record<string, string> }
      }) => {
        const entry = {
          eventId: notification.params.meta["event_id"],
          content: notification.params.content,
        }
        if (refuse(entry)) throw new Error("the MCP transport refused the write")
        shown.push(entry)
      },
    },
  }
}

const NOTICE = "message not shown"
const cards = (shown: readonly Shown[], eventId: string) =>
  shown.filter((entry) => entry.eventId === eventId && !entry.content.includes(NOTICE))
const notices = (shown: readonly Shown[], eventId: string) =>
  shown.filter((entry) => entry.content.includes(NOTICE) && entry.content.includes(eventId))

/** A peer session's message to `sessionID`. */
function peerMessage(sessionID: string, label: string): { raw: string; eventId: string } {
  const eventId = `evt-${label}`
  return {
    eventId,
    raw: JSON.stringify({
      event_id: eventId,
      source: "agent",
      source_session: "ses_peer",
      topic: agentSubject(sessionID),
      dedupe_key: `agent.ses_peer.${label}`,
      issued_at: 1,
      payload_summary: `peer message ${label}`,
    }),
  }
}

/** A person's removal of `sessionID`'s subscription to `topic`, as Dispatch publishes it. */
function removalFrame(sessionID: string, topic: string): string {
  return JSON.stringify({
    event_id: "dispatch-remove-1",
    source: "dispatch",
    source_event_id: "remove-1",
    topic: agentSubject(sessionID),
    dedupe_key: "dispatch-remove-1",
    issued_at: 1,
    payload_summary: `${ISSUE} subscription removed`,
    payload: JSON.stringify({
      issue_key: ISSUE,
      type: "subscription.removed",
      actor: { kind: "user", id: "alice" },
      payload: { session_id: sessionID, by: { kind: "user", id: "alice" }, topics: [topic] },
    }),
  })
}

/**
 * The listener's interest registry, as a session's registrations reach it: a registration merges
 * its topics into the entry and never removes one, and every one it accepts is answered with the
 * session's own consumer.
 */
function registryClient(): {
  readonly client: FakeClient
  topicsOf(sessionID: string): string[]
} {
  const entries = new Map<string, Set<string>>()
  const entry = (sessionID: string): Set<string> => {
    const held = entries.get(sessionID) ?? new Set<string>()
    entries.set(sessionID, held)
    return held
  }
  const client = recordingClient([], {
    subscribe: async (input): Promise<SubscribeResult> => {
      for (const topic of input.topics) entry(input.sessionID).add(topic)
      return { ...noInterest(input.sessionID), topics: [...entry(input.sessionID)] }
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry(input.sessionID).delete(topic)
    },
    getInterest: async (sessionID): Promise<Interest> => ({
      session_id: sessionID,
      machine_id: "devbox",
      dir: "/tmp",
      topics: [...entry(sessionID)],
    }),
  })
  return { client, topicsOf: (sessionID) => [...entry(sessionID)] }
}

async function stateOf(directory: string, sessionID: string): Promise<ReplayStateFile> {
  return JSON.parse(
    await readFile(replayStateFile(directory, sessionID), "utf8"),
  ) as ReplayStateFile
}

test("a frame the channel never takes is named in one notice on its third delivery, and never shown after a restart", async () => {
  const directory = await scratchState()
  const frame = peerMessage(SESSION, "refused")
  let attempts = 0
  // Every card naming the frame fails; the notice naming it carries no event id of its own.
  const sink = channelSink((shown) => {
    if (shown.eventId !== frame.eventId) return false
    attempts += 1
    return true
  })
  const nats = new FakeNats()
  const first = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: sink.notifier,
      client: registryClient().client,
    }),
  )
  try {
    // The listener delivers the frame again after each `nak`, counting its deliveries.
    for (const delivered of [1, 2]) {
      const reply = `_INBOX.try-${delivered}`
      nats.emitRelay(SESSION, { subject: DIRECT, seq: 9, raw: frame.raw, reply, delivered })
      await waitFor(async () => nats.repliesOn(reply).includes("nak"), `the ${delivered}. nak`)
      expect(notices(sink.shown, frame.eventId)).toHaveLength(0)
    }
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 9,
      raw: frame.raw,
      reply: "_INBOX.try-3",
      delivered: 3,
    })
    await waitFor(
      async () => notices(sink.shown, frame.eventId).length === 1,
      "the failure notice",
      () => `attempts ${attempts}`,
    )
    // Given up on: the listener is told so, rather than sending it forever.
    expect(nats.repliesOn("_INBOX.try-3")).toEqual(["received", "done"])
    expect(attempts).toBe(3)
    expect(notices(sink.shown, frame.eventId)[0]?.content).toContain("envoy_inbox")
    expect(cards(sink.shown, frame.eventId)).toHaveLength(0)
  } finally {
    await first.shutdown()
  }

  const laterNats = new FakeNats()
  const second = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: laterNats,
      notifier: sink.notifier,
      client: registryClient().client,
    }),
  )
  try {
    // The notice recorded the frame, so the relay's next copy of it is passed over in silence.
    laterNats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 9,
      raw: frame.raw,
      reply: "_INBOX.after",
      delivered: 4,
    })
    await waitFor(async () => laterNats.repliesOn("_INBOX.after").includes("done"), "done")
    expect(notices(sink.shown, frame.eventId)).toHaveLength(1)
    expect(cards(sink.shown, frame.eventId)).toHaveLength(0)
  } finally {
    await second.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a session the listener reaped follows its remembered topics again and registers them", async () => {
  const directory = await scratchState()
  // The state a stopped session left: it followed the issue topic, and the listener has since
  // reaped its interests, so the registry holds nothing for it.
  await writeReplayState(replayStateFile(directory, SESSION), {
    ledger: [],
    state: { topics: [DIRECT, ISSUE_BASE, ISSUE_TOPIC] },
  })
  const registry = registryClient()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: new FakeNats(),
      client: registry.client,
    }),
  )
  try {
    expect(session.topics()).toEqual(expect.arrayContaining([DIRECT, ISSUE_BASE, ISSUE_TOPIC]))
    expect(registry.topicsOf(SESSION)).toEqual(
      expect.arrayContaining([DIRECT, ISSUE_BASE, ISSUE_TOPIC]),
    )
  } finally {
    await session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a removal the relay delivers after a restart re-registered the topic takes it out of the registry, so a later restart does not follow it", async () => {
  const directory = await scratchState()
  const registry = registryClient()
  const start = async (connection: FakeNats) =>
    startChannelSession(
      sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
        connection,
        client: registry.client,
      }),
    )

  const firstNats = new FakeNats()
  const first = await start(firstNats)
  await first.follow([ISSUE_TOPIC])
  await first.shutdown()
  expect(registry.topicsOf(SESSION)).toContain(ISSUE_TOPIC)

  // The restart follows the topic again from its state and registers it; the removal a person
  // made while the session was stopped is the first frame its consumer relays.
  const secondNats = new FakeNats()
  const second = await start(secondNats)
  try {
    expect(second.topics()).toContain(ISSUE_TOPIC)
    secondNats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 11,
      raw: removalFrame(SESSION, ISSUE_TOPIC),
      reply: "_INBOX.removal",
    })
    await waitFor(
      async () =>
        !second.topics().includes(ISSUE_TOPIC) && !registry.topicsOf(SESSION).includes(ISSUE_TOPIC),
      "the relayed removal",
    )
  } finally {
    await second.shutdown()
  }

  const third = await start(new FakeNats())
  try {
    expect(third.topics()).not.toContain(ISSUE_TOPIC)
    expect(registry.topicsOf(SESSION)).not.toContain(ISSUE_TOPIC)
  } finally {
    await third.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a followed topic reaching a private stream subject is followed and never registered", async () => {
  const directory = await scratchState()
  const registry = registryClient()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: new FakeNats(),
      client: registry.client,
    }),
  )
  const refused = [
    "notifications.*.x",
    "notifications.agent.*",
    agentSubject("ses_other"),
    `notifications.envoy.exceptions.${DIRECT}`,
    "notifications.legion.ACME.controller",
  ]
  try {
    await session.follow([...refused, ISSUE_TOPIC])

    expect(session.topics()).toEqual(expect.arrayContaining([...refused, ISSUE_TOPIC]))
    const registered = registry.topicsOf(SESSION)
    expect(registered).toEqual(expect.arrayContaining([DIRECT, ISSUE_TOPIC]))
    for (const topic of refused) expect(registered).not.toContain(topic)
  } finally {
    await session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("the delivery state file is private to its user and replaced whole by each write", async () => {
  const directory = await scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: new FakeNats(),
      client: registryClient().client,
    }),
  )
  const file = replayStateFile(directory, SESSION)
  try {
    // Nothing is written until the session has something to record: the first follow stamps it.
    await session.follow([ISSUE_TOPIC])
    await waitFor(async () => (await stat(file).catch(() => undefined)) !== undefined, "the file")
    expect((await stat(file)).mode & 0o777).toBe(0o600)
    const firstText = await readFile(file, "utf8")
    // The file as the first write left it, held open across the next write.
    const held = await open(file, "r")
    try {
      await session.follow(["notifications.team.deploy"])
      await waitFor(async () => (await readFile(file, "utf8")) !== firstText, "the next write")
      expect((await stat(file)).mode & 0o777).toBe(0o600)
      // A rename put a new file in its place; nothing was written into the old one.
      expect(await held.readFile("utf8")).toBe(firstText)
    } finally {
      await held.close()
    }
    expect((await readdir(directory)).filter((name) => name.endsWith(".tmp"))).toEqual([])
    const state = await stateOf(directory, SESSION)
    expect(state.state?.topics).toEqual([
      DIRECT,
      ISSUE_BASE,
      ISSUE_TOPIC,
      "notifications.team.deploy",
    ])
    expect(Object.keys(state.consumer?.follows ?? {})).toEqual([
      ISSUE_BASE,
      ISSUE_TOPIC,
      "notifications.team.deploy",
    ])
  } finally {
    await session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a state file that cannot be written is logged, and what the session was shown stays shown", async () => {
  const directory = await scratchState()
  // A directory where the file goes: every rename onto it fails.
  await mkdir(replayStateFile(directory, SESSION))
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
      client: registryClient().client,
    }),
  )
  try {
    const frame = peerMessage(SESSION, "unpersisted")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 4, raw: frame.raw, reply: "_INBOX.first" })
    await waitFor(async () => cards(sink.shown, frame.eventId).length === 1, "the frame")
    await waitFor(
      async () => written.some((line) => line.includes("could not persist")),
      "the failed write",
    )
    // The in-memory ledger still holds it, so the relay's next copy is not shown again.
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 4,
      raw: frame.raw,
      reply: "_INBOX.again",
      delivered: 2,
    })
    await waitFor(async () => nats.repliesOn("_INBOX.again").includes("done"), "done")
    expect(cards(sink.shown, frame.eventId)).toHaveLength(1)
  } finally {
    await session.shutdown()
    stderr.mockRestore()
    await rm(directory, { recursive: true, force: true })
  }
})

test("a /clear handoff to a new session id starts a fresh delivery state in a file of its own", async () => {
  const directory = await scratchState()
  const registry = registryClient()
  const handoff = sessionHandoffFile(directory, 931)
  await writeSessionHandoff(handoff, "ses_old")
  const sink = channelSink()
  const nats = new FakeNats()
  const identity = new SessionIdentity("ses_old", "/tmp")
  const session = await startChannelSession(
    sessionOptions(identity, directory, {
      connection: nats,
      notifier: sink.notifier,
      client: registry.client,
      heartbeatMs: 20,
      handoffPid: 931,
    }),
  )
  try {
    await session.follow([ISSUE_TOPIC])
    const before = peerMessage("ses_old", "before")
    nats.emitRelay("ses_old", {
      subject: agentSubject("ses_old"),
      seq: 1,
      raw: before.raw,
      reply: "_INBOX.old",
    })
    await waitFor(async () => cards(sink.shown, before.eventId).length === 1, "the old id's frame")
    await waitFor(
      async () => (await stateOf(directory, "ses_old")).ledger.length === 1,
      "the old id's record",
    )

    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(
      async () => nats.subscriptions.has("notifications.envoy.deliver.ses_new"),
      "the new id's relay subscription",
    )

    // The new id's relay subject is the only one this process holds; the old id's is gone.
    expect([...nats.subscriptions.keys()]).toEqual(["notifications.envoy.deliver.ses_new"])
    await waitFor(
      async () =>
        (await stat(replayStateFile(directory, "ses_new")).catch(() => undefined)) !== undefined,
      "the new id's state file",
    )
    // A new id starts afresh, in a file of its own: nothing the id before it was shown comes
    // back, and the id before it keeps its own record whole.
    expect((await stateOf(directory, "ses_new")).ledger).toEqual([])
    const old = await stateOf(directory, "ses_old")
    expect(old.ledger.map((entry) => entry.event_id)).toEqual([before.eventId])
    expect(old.state?.topics).toEqual(
      expect.arrayContaining([agentSubject("ses_old"), ISSUE_TOPIC]),
    )

    const taken = peerMessage("ses_new", "taken")
    nats.emitRelay("ses_new", {
      subject: agentSubject("ses_new"),
      seq: 2,
      raw: taken.raw,
      reply: "_INBOX.new",
    })
    await waitFor(async () => cards(sink.shown, taken.eventId).length === 1, "the new id's frame")
  } finally {
    await session.shutdown()
    await rm(directory, { recursive: true, force: true })
  }
})
