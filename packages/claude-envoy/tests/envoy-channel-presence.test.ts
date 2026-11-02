// The listener's connection check (INFRA-626). When someone sends to a session, the listener
// makes one core NATS request on `notifications.envoy.presence.<session id>`, and that session's
// own plugin answers it from its own connection: a reply naming the session is connected, no
// reply and the no-responders status a server gives a subject nothing subscribes to are stopped.
// NATS here is this package's `FakeNats`, the connection every channel-session test drives; the
// no-responders status itself is the server's own behaviour, proved in packages/envoy.
import { expect, test } from "bun:test"
import { rm } from "node:fs/promises"
import type { SubscribeInput } from "@legion/envoy-client/transport"
import { startChannelSession } from "../src/envoy-channel-server"
import { SessionIdentity, sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
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

const CLIENT_INSTANCE = "0123456789abcdef0123456789abcdef"

function presenceSubject(sessionID: string): string {
  return `notifications.envoy.presence.${sessionID}`
}

/**
 * The listener's one request on `sessionID`'s presence subject, answered with what the plugin
 * published on the reply inbox, or `undefined` when nothing is subscribed there or nothing
 * answered — which is the no-responders a real server gives the listener.
 */
async function ask(nats: FakeNats, sessionID: string, inbox: string): Promise<unknown> {
  const subject = presenceSubject(sessionID)
  if (!nats.subscriptions.has(subject)) return undefined
  nats.emit(subject, "", inbox)
  await settled()
  const [reply] = nats.repliesOn(inbox)
  return reply === undefined ? undefined : JSON.parse(reply)
}

test("subscribes exactly its own presence subject, answers it from memory, and declares the check", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  const registrations: SubscribeInput[] = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input)
      return noInterest()
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      client,
      clientInstanceID: CLIENT_INSTANCE,
      connection: nats,
    }),
  )

  try {
    // Every NATS subject this session holds: one presence subject, its own, so a plugin that
    // subscribed to the wildcard or to another session's fails here.
    expect(
      [...nats.subscriptions.keys()].filter((subject) => subject.includes("presence")),
    ).toEqual([presenceSubject("ses_claude")])
    // And it is no registered interest: it reaches neither the topic list nor a registration.
    expect(session.topics()).not.toContain(presenceSubject("ses_claude"))
    expect(registrations[0]?.topics).not.toContain(presenceSubject("ses_claude"))
    expect(registrations[0]?.presence).toBe(true)

    // The reply the listener decodes (`presenceReply`): this session, and the client instance
    // that registered it.
    expect(await ask(nats, "ses_claude", "_INBOX.presence.1")).toEqual({
      session_id: "ses_claude",
      client_instance_id: CLIENT_INSTANCE,
    })
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("declares no check when the server refuses the subscription, and registers all the same", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  nats.refusedSubjects.add(presenceSubject("ses_claude"))
  const registrations: SubscribeInput[] = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input)
      return noInterest()
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      client,
      clientInstanceID: CLIENT_INSTANCE,
      connection: nats,
    }),
  )

  try {
    // The session registered, and said nothing about a check it cannot answer: the listener
    // keeps calling it connected rather than probing a subject nothing listens on.
    expect(registrations).toHaveLength(1)
    expect(registrations[0]?.sessionID).toBe("ses_claude")
    expect(registrations[0]?.presence).toBeUndefined()
    expect(await ask(nats, "ses_claude", "_INBOX.presence.refused")).toBeUndefined()
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("moves the subscription on a /clear handoff: the new id answers and the old one does not", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  const registrations: SubscribeInput[] = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input)
      return noInterest()
    },
  })
  const handoff = sessionHandoffFile(stateDirectory, 781)
  await writeSessionHandoff(handoff, "ses_old")
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_old", "/tmp"), stateDirectory, {
      client,
      clientInstanceID: CLIENT_INSTANCE,
      connection: nats,
      handoffPid: 781,
      heartbeatMs: 25,
    }),
  )

  try {
    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(
      async () => registrations.some((registration) => registration.sessionID === "ses_new"),
      "a registration under the new session id",
    )

    expect(nats.unsubscribed).toContain(presenceSubject("ses_old"))
    expect(nats.subscriptions.has(presenceSubject("ses_old"))).toBe(false)
    expect(await ask(nats, "ses_new", "_INBOX.presence.new")).toEqual({
      session_id: "ses_new",
      client_instance_id: CLIENT_INSTANCE,
    })
    expect(await ask(nats, "ses_old", "_INBOX.presence.old")).toBeUndefined()
    expect(
      registrations.filter((registration) => registration.sessionID === "ses_new").at(-1)?.presence,
    ).toBe(true)
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("answers nothing once the session has gone, which is the no-responders a server then gives", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      clientInstanceID: CLIENT_INSTANCE,
      connection: nats,
    }),
  )

  try {
    expect(await ask(nats, "ses_claude", "_INBOX.presence.live")).toMatchObject({
      session_id: "ses_claude",
    })

    await session.shutdown()

    expect(nats.unsubscribed).toContain(presenceSubject("ses_claude"))
    expect(await ask(nats, "ses_claude", "_INBOX.presence.gone")).toBeUndefined()
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})
