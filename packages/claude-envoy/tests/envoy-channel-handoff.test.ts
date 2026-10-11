import { expect, test } from "bun:test"
import { mkdir, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { dirname, join } from "node:path"
import type { EnvoyClient } from "@legion/envoy-client/transport"
import {
  type ChannelNotifier,
  executeEnvoyTool,
  startChannelSession,
} from "../src/envoy-channel-server"
import {
  roleStateFile,
  SessionIdentity,
  sessionHandoffFile,
  writeSessionHandoff,
} from "../src/session-identity"
import {
  deliveryRaw,
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
    const handoffCalls = calls.filter(
      (call) => call !== "subscribe ses_old [aside]" && call !== "getInterest",
    )
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
    expect(callsB.filter((call) => call !== "subscribe ses_b" && call !== "getInterest")).toEqual(
      [],
    )
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
    nats.emit(
      "notifications.agent.ses_old",
      deliveryRaw.replace(directSubject, "notifications.agent.ses_old"),
    )
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
