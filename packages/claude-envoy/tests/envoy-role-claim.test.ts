import { expect, jest, test } from "bun:test"
import { mkdir, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import type { Interest, SubscribeResult } from "@legion/envoy-client/transport"
import type {
  ChannelConnectionStatus,
  ChannelForwarderConnection,
  ChannelInboundMessage,
  ChannelTopicSubscription,
} from "../src/channel-forwarder"
import {
  CHANNEL_CAPABILITIES,
  type ChannelNotification,
  type ChannelNotifier,
  type ChannelSession,
  startChannelSession,
} from "../src/envoy-channel-server"
import {
  readPersistedRole,
  roleStateFile,
  SessionIdentity,
  sessionHandoffFile,
  writePersistedRole,
  writeSessionHandoff,
} from "../src/session-identity"
import {
  emptyReplay,
  type FakeClient,
  noInterest,
  recordingClient,
  scratchState,
  sessionOptions,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

class IdleNats implements ChannelForwarderConnection {
  subscribe(): ChannelTopicSubscription {
    return {
      unsubscribe: () => undefined,
      async *[Symbol.asyncIterator](): AsyncGenerator<ChannelInboundMessage> {},
    }
  }

  publish(): void {}

  flush(): Promise<void> {
    return Promise.resolve()
  }

  isClosed(): boolean {
    return false
  }

  drain(): Promise<void> {
    return Promise.resolve()
  }

  close(): Promise<void> {
    return Promise.resolve()
  }

  async *status(): AsyncGenerator<ChannelConnectionStatus> {}
}

const notifier: ChannelNotifier = {
  notification(_notification: ChannelNotification): Promise<void> {
    return Promise.resolve()
  },
}

function interest(): Interest {
  return {
    session_id: "ses_current",
    machine_id: "devbox",
    dir: "/tmp",
    topics: ["notifications.agent.ses_current"],
  }
}

/** The listener's answer to a registration: the interest, and the core delivery mode. */
function registered(): SubscribeResult {
  return { ...interest(), delivery: { mode: "core", reason: "not_answered" } }
}

/** The client instance of the channel server under test: one `claude` process's. */
const INSTANCE = "a1b2c3d4e5f60718293a4b5c6d7e8f90"

test("reasserts a role claimed after channel startup on the next heartbeat", async () => {
  jest.useFakeTimers()
  const stateDirectory = join(tmpdir(), `claude-envoy-claim-${crypto.randomUUID()}`)
  const reasserted = Promise.withResolvers<void>()
  const setRoles: unknown[] = []
  const client: FakeClient = {
    subscribe: async () => registered(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async (input) => {
      setRoles.push(input)
      reasserted.resolve()
      return { claimed: true, interest: interest() }
    },
    getRole: async () => ({ role: "reviewer", holder: "ses_lost", last_seen: 1, state: "live" }),
    replay: emptyReplay,
  }
  let session: ChannelSession | undefined

  try {
    session = await startChannelSession({
      identity: new SessionIdentity("ses_current", "/tmp"),
      connection: new IdleNats(),
      notifier,
      capabilities: CHANNEL_CAPABILITIES,
      client,
      heartbeatMs: 25,
      configFile: { path: join(stateDirectory, "envoy.json"), error: null },
      stateDirectory,
      clientInstanceID: INSTANCE,
      pluginVersion: "source",
    })

    await session.rememberRole("reviewer")
    jest.advanceTimersByTime(25)
    jest.useRealTimers()
    await reasserted.promise
    expect(setRoles).toEqual([
      { sessionID: "ses_current", role: "reviewer", soft: true, clientInstanceID: INSTANCE },
    ])
  } finally {
    await session?.shutdown()
    jest.useRealTimers()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("the record a remembered role writes names this process, so another one tells it from its own", async () => {
  const stateDirectory = join(tmpdir(), `claude-envoy-remember-${crypto.randomUUID()}`)
  const client: FakeClient = {
    subscribe: async () => registered(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async () => ({ claimed: true, interest: interest() }),
    getRole: async () => ({
      role: "reviewer",
      holder: "ses_current",
      last_seen: 1,
      state: "live",
      client_instance_id: INSTANCE,
    }),
    replay: emptyReplay,
  }
  const session = await startChannelSession({
    identity: new SessionIdentity("ses_current", "/tmp"),
    connection: new IdleNats(),
    notifier,
    capabilities: CHANNEL_CAPABILITIES,
    client,
    heartbeatMs: 60_000,
    configFile: { path: join(stateDirectory, "envoy.json"), error: null },
    stateDirectory,
    clientInstanceID: INSTANCE,
    pluginVersion: "source",
  })

  try {
    await session.rememberRole("reviewer")

    expect(await readPersistedRole(roleStateFile(stateDirectory, "ses_current"))).toEqual({
      session_id: "ses_current",
      role: "reviewer",
      client_instance_id: INSTANCE,
    })
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

/** A second `claude` process sharing this state directory: the one that resumed a live session's
 *  id in dispatch://INFRA-783. */
const OTHER_INSTANCE = "f0e1d2c3b4a596876543210fedcba987"

test("a record another process wrote is left where it is while this process holds a role", async () => {
  // Two processes, one state directory: the second resumed this session id and wrote its own
  // record over the file, which is keyed by session id alone. This process holds a role of its
  // own, so the record it finds for its predecessor is not a role to take back — pressing it
  // would ask the listener for the other process's role and throw this one's away.
  const stateDirectory = await scratchState()
  const identity = new SessionIdentity("ses_old", "/tmp")
  const handoff = sessionHandoffFile(stateDirectory, 786)
  await writeSessionHandoff(handoff, "ses_old")
  const registrations: string[] = []
  const claimedRoles: string[] = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input.sessionID)
      return noInterest()
    },
    setRole: async (input) => {
      claimedRoles.push(input.role)
      return { claimed: true, interest: noInterest() }
    },
    getRole: async (role) => ({ role, holder: identity.id, last_seen: 1, state: "live" }),
  })
  const options = sessionOptions(identity, stateDirectory, {
    client,
    heartbeatMs: 25,
    handoffPid: 786,
  })
  const session = await startChannelSession(options)

  try {
    await session.rememberRole("reviewer")
    const otherRecord = {
      session_id: "ses_old",
      role: "architect",
      client_instance_id: OTHER_INSTANCE,
    }
    await writePersistedRole(roleStateFile(stateDirectory, "ses_old"), otherRecord)

    await writeSessionHandoff(handoff, "ses_new")
    // Three registrations under the new id: the handoff's own, and two heartbeats after it, each
    // of which runs the transfer again.
    await waitFor(
      async () => registrations.filter((id) => id === "ses_new").length >= 3,
      "three registrations under the handed-off id",
    )

    expect(claimedRoles).not.toContain("architect")
    expect(await readPersistedRole(roleStateFile(stateDirectory, "ses_old"))).toEqual(otherRecord)
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a record another process wrote is pressed while this process holds no role", async () => {
  // The same record, found by a process with no role to lose: it asks, and the listener decides
  // between the two processes. Granted, the role and its record are this process's.
  const stateDirectory = await scratchState()
  const identity = new SessionIdentity("ses_old", "/tmp")
  const handoff = sessionHandoffFile(stateDirectory, 787)
  await writeSessionHandoff(handoff, "ses_old")
  const setRoles: unknown[] = []
  const client = recordingClient([], {
    setRole: async (input) => {
      setRoles.push(input)
      return { claimed: true, interest: noInterest() }
    },
    getRole: async (role) => ({ role, holder: identity.id, last_seen: 1, state: "live" }),
  })
  const options = sessionOptions(identity, stateDirectory, {
    client,
    heartbeatMs: 25,
    handoffPid: 787,
  })
  const session = await startChannelSession(options)

  try {
    await writePersistedRole(roleStateFile(stateDirectory, "ses_old"), {
      session_id: "ses_old",
      role: "architect",
      client_instance_id: OTHER_INSTANCE,
    })

    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(
      async () => (await readPersistedRole(roleStateFile(stateDirectory, "ses_new"))) !== undefined,
      "the role record of the handed-off id",
    )

    expect(setRoles).toEqual([
      {
        sessionID: "ses_new",
        role: "architect",
        soft: true,
        previousSessionID: "ses_old",
        clientInstanceID: options.clientInstanceID,
      },
    ])
    expect(await readPersistedRole(roleStateFile(stateDirectory, "ses_new"))).toEqual({
      session_id: "ses_new",
      role: "architect",
      client_instance_id: options.clientInstanceID,
    })
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("every role claim and every release carries that same instance id, not one of its own", async () => {
  const stateDirectory = await scratchState()
  const identity = new SessionIdentity("ses_claude", "/tmp")
  const roleFile = roleStateFile(stateDirectory, "ses_claude")
  await mkdir(dirname(roleFile), { recursive: true })
  await writeFile(roleFile, JSON.stringify({ session_id: "ses_claude", role: "reviewer" }))
  /** What each call carried: the instance id optional, as the client's own API has it, so a call
   * that sent none is caught here rather than typed away. */
  const subscribes: Array<{ readonly clientInstanceID?: string | undefined }> = []
  const claims: Array<{ readonly role: string; readonly clientInstanceID?: string | undefined }> =
    []
  const releases: Array<{
    readonly sessionID: string
    readonly topics: readonly string[]
    readonly clientInstanceID?: string | undefined
  }> = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      subscribes.push(input)
      return noInterest()
    },
    setRole: async (input) => {
      claims.push(input)
      return { claimed: true, interest: noInterest() }
    },
    unsubscribe: async (input) => {
      releases.push(input)
    },
  })
  const session = await startChannelSession(
    sessionOptions(identity, stateDirectory, { client, heartbeatMs: 60_000 }),
  )

  try {
    const roleTopic = "notifications.role.reviewer"
    await session.follow([roleTopic])
    await session.unfollow([roleTopic])

    // Read back from the registration rather than named here: the process mints it once, and
    // what matters is that the claim and the release say that same process (dispatch://INFRA-783),
    // since the listener frees a role claim only for the process that made it.
    const instance = subscribes[0]?.clientInstanceID
    expect(instance).toMatch(/^[0-9a-f]{32}$/)
    expect(claims.map(({ role }) => role)).toEqual(["reviewer"])
    expect(claims.filter((claim) => claim.clientInstanceID === instance)).toHaveLength(
      claims.length,
    )
    expect(releases).toEqual([
      { sessionID: "ses_claude", topics: [roleTopic], clientInstanceID: instance },
    ])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})
