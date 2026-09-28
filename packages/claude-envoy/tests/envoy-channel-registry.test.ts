import { expect, test } from "bun:test"
import { rm } from "node:fs/promises"
import { startChannelSession } from "../src/envoy-channel-server"
import { SessionIdentity, sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import {
  directSubject,
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

test("a registry removal that fails is reconciled by the next heartbeat, without stripping a topic re-followed since", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  const entry = new Set<string>([directSubject])
  let failNextUnsubscribe = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      for (const topic of input.topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      if (failNextUnsubscribe) {
        failNextUnsubscribe = false
        throw new Error("registry unavailable")
      }
      for (const topic of input.topics) entry.delete(topic)
    },
    getInterest: async (sessionID) => ({
      ...noInterest(),
      session_id: sessionID,
      topics: [...entry],
    }),
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 20,
    }),
  )
  const staysRemoved = "notifications.dispatch.issue.DSP-3"
  const reFollowed = "notifications.dispatch.issue.DSP-9"

  try {
    await session.follow([staysRemoved, reFollowed])
    failNextUnsubscribe = true
    nats.emit(
      directSubject,
      JSON.stringify({
        event_id: "remove-reconcile",
        source: "dispatch",
        payload: JSON.stringify({
          issue_key: "DSP-3",
          type: "subscription.removed",
          actor: { kind: "user", id: "alice" },
          payload: {
            session_id: "ses_claude",
            by: { kind: "user", id: "alice" },
            topics: [staysRemoved, reFollowed],
          },
        }),
      }),
    )
    await waitFor(async () => !failNextUnsubscribe, "the removal's own registry write to fail once")
    // Both are already out of local state; re-follow one before any heartbeat
    // reconciles. A stale retry replaying the failure-time topic list would strip
    // it right back out; reconciliation, driven by live forwarder.topics(), must not.
    await session.follow([reFollowed])
    await waitFor(
      async () => !entry.has(staysRemoved),
      "the heartbeat to reconcile the stale entry",
    )
    expect([...entry].sort()).toEqual([directSubject, reFollowed])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a persistent reconciliation failure retries once immediately, then waits for the next heartbeat, rather than hot-looping", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  let getInterestAttempts = 0
  const client = recordingClient([], {
    getInterest: async () => {
      getInterestAttempts++
      throw new Error("listener degraded")
    },
  })
  // A handoff file naming the session's own current id triggers one heartbeat at
  // startup (adoptHandoff no-ops, `next === identity.id`) without waiting out the
  // 10 s interval below, which otherwise contributes no tick in this test's window.
  const handoff = sessionHandoffFile(stateDirectory, 991)
  await writeSessionHandoff(handoff, "ses_claude")
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 10_000,
      handoffPid: 991,
    }),
  )

  try {
    // One at startup (`syncRegisteredInterests`, swallowed there), one from the
    // heartbeat the handoff poke triggers, one bounded immediate retry (N2) — then
    // the outage guard must hold with no interval tick due for 10 s.
    await waitFor(
      async () => getInterestAttempts >= 3,
      "the startup read, the first reconciliation attempt, and its one bounded retry",
    )
    // A real wait, not a guessed one: proving the guard holds needs to observe a
    // window in which nothing further happens, and nothing else here produces an
    // event to wait for instead — a hot loop would have run thousands of times
    // over 150 ms (measured: ~9,000 in 300 ms without this fix).
    await Bun.sleep(150)
    expect(getInterestAttempts).toBe(3)
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a registry read that fails at startup does not let the heartbeat delete the registered interests", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  const alreadyRegistered = "notifications.dispatch.issue.DSP-5"
  const entry = new Set<string>([directSubject, alreadyRegistered])
  let reads = 0
  const client = recordingClient([], {
    subscribe: async (input) => {
      for (const topic of input.topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
    getInterest: async (sessionID) => {
      reads++
      // The listener is down for exactly the startup read — a redeploy landing on
      // a plugin reload — and healthy from then on.
      if (reads === 1) throw new Error("listener unavailable")
      return { ...noInterest(), session_id: sessionID, topics: [...entry] }
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 20,
    }),
  )

  try {
    await waitFor(async () => reads >= 4, "several heartbeats after the failed startup read")
    // A read the session never completed is not evidence that the entry drifted.
    // Nothing restores a topic deleted from it: `unregisterSession` leaves the
    // interests entry behind, so the stripped one is what the next resume reads.
    expect([...entry].sort()).toEqual([directSubject, alreadyRegistered])
    // Adopting it is also what stops the session being deaf to it until a restart.
    expect(session.topics()).toContain(alreadyRegistered)
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a shutdown landing before the heartbeat's registry read leaves the registry entry alone", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  const alreadyRegistered = "notifications.dispatch.issue.DSP-1"
  const entry = new Set<string>([directSubject, alreadyRegistered])
  const release = Promise.withResolvers<void>()
  let holdRegistration = false
  let registrationHeld = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      const topics = [...input.topics]
      if (holdRegistration) {
        registrationHeld = true
        await release.promise
      }
      for (const topic of topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
    getInterest: async (sessionID) => ({
      ...noInterest(),
      session_id: sessionID,
      topics: [...entry],
    }),
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 20,
    }),
  )

  try {
    expect(session.topics()).toContain(alreadyRegistered)
    holdRegistration = true
    await waitFor(async () => registrationHeld, "a heartbeat to reach the listener")
    holdRegistration = false
    // The tick is held in `register()`, so shutdown lands before the sync is
    // entered at all: this pins the window the entry check covers. The window
    // after the read is the test below.
    const shuttingDown = session.shutdown()
    await waitFor(async () => session.topics().length === 0, "the forwarder to close")
    release.resolve()
    await shuttingDown
    expect([...entry].sort()).toEqual([directSubject, alreadyRegistered])
  } finally {
    release.resolve()
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a shutdown landing inside the heartbeat's registry read leaves the registry entry alone", async () => {
  const stateDirectory = await scratchState()
  const nats = new FakeNats()
  const alreadyRegistered = "notifications.dispatch.issue.DSP-2"
  const entry = new Set<string>([directSubject, alreadyRegistered])
  const release = Promise.withResolvers<void>()
  let holdRead = false
  let readHeld = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      for (const topic of input.topics) entry.add(topic)
      return noInterest()
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry.delete(topic)
    },
    getInterest: async (sessionID) => {
      if (holdRead) {
        readHeld = true
        await release.promise
      }
      return { ...noInterest(), session_id: sessionID, topics: [...entry] }
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      client,
      heartbeatMs: 20,
    }),
  )

  try {
    expect(session.topics()).toContain(alreadyRegistered)
    holdRead = true
    await waitFor(async () => readHeld, "a heartbeat to reach the registry read")
    holdRead = false
    // The tick is past the entry check and suspended on the read when shutdown
    // empties the forwarder, so a check evaluated before the await passes going in
    // and finds an empty topic list coming out — every topic drift, the direct
    // subject with them. The real listener does not merely empty the entry: an
    // empty topic set makes `removeInterestTopics` delete it (internal/store/
    // kv.go), and the deregistration behind it removes only the sessions row, so a
    // `--resume` before `Registry.Reap` reads nothing back.
    const shuttingDown = session.shutdown()
    await waitFor(async () => session.topics().length === 0, "the forwarder to close")
    release.resolve()
    await shuttingDown
    expect([...entry].sort()).toEqual([directSubject, alreadyRegistered])
  } finally {
    release.resolve()
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
