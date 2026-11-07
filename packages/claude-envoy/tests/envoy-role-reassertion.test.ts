import { expect, test } from "bun:test"
import { mkdir, rm, stat, utimes, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { EnvoyApiError, type Interest, type SubscribeResult } from "@legion/envoy-client/transport"
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
  startChannelSession,
} from "../src/envoy-channel-server"
import {
  pruneStaleSessionRoles,
  readPersistedRole,
  roleStateFile,
  SessionIdentity,
} from "../src/session-identity"
import { STATE_FILE_MAX_AGE_MS } from "../src/session-state"
import { emptyReplay, type FakeClient, waitFor } from "./channel-session-harness"

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

/** The client instance of the channel server under test, and of a second `claude` process that
 *  resumed its session id: the pair the listener tells apart. */
const INSTANCE = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
const OTHER_INSTANCE = "f0e1d2c3b4a596876543210fedcba987"

test("reasserts a restored role after a successful registration heartbeat loses it", async () => {
  const stateDirectory = join(tmpdir(), `claude-envoy-reassert-${crypto.randomUUID()}`)
  const roleFile = roleStateFile(stateDirectory, "ses_current")
  await mkdir(dirname(roleFile), { recursive: true })
  await writeFile(roleFile, JSON.stringify({ session_id: "ses_current", role: "reviewer" }))
  const setRoles: unknown[] = []
  const reasserted = Promise.withResolvers<void>()
  const client: FakeClient = {
    subscribe: async () => registered(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async (input) => {
      setRoles.push(input)
      if (setRoles.length === 2) reasserted.resolve()
      return { claimed: true, interest: interest() }
    },
    getRole: async () => ({ role: "reviewer", holder: "ses_lost", last_seen: 1, state: "live" }),
    replay: emptyReplay,
  }
  const session = await startChannelSession({
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

  try {
    await reasserted.promise
    expect(setRoles).toEqual([
      {
        sessionID: "ses_current",
        role: "reviewer",
        soft: true,
        previousSessionID: "ses_current",
        clientInstanceID: INSTANCE,
      },
      { sessionID: "ses_current", role: "reviewer", soft: true, clientInstanceID: INSTANCE },
    ])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("runs the regain hook once a heartbeat's soft claim takes the held role back, and not while the listener still names this session", async () => {
  const stateDirectory = join(tmpdir(), `claude-envoy-regain-${crypto.randomUUID()}`)
  let holder = "ses_current"
  const regains: string[] = []
  const regained = Promise.withResolvers<void>()
  const client: FakeClient = {
    subscribe: async () => registered(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async () => {
      holder = "ses_current"
      return { claimed: true, interest: interest() }
    },
    getRole: async () => {
      // Healthy ticks see this session; the listener then loses the claim once.
      const answer = { role: "legion-claim", holder, last_seen: 1, state: "live" } as const
      if (holder === "ses_current" && regains.length === 0) holder = "ses_lost"
      return answer
    },
    replay: emptyReplay,
  }
  const session = await startChannelSession({
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
  session.onRoleRegained(async (role, reason) => {
    regains.push(`${role} ${reason}`)
    regained.resolve()
  })

  try {
    await session.rememberRole("legion-claim")
    await regained.promise
    expect(regains).toEqual(["legion-claim reclaimed"])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("the role read's 404 still means no live holder: the kept lane changed only the publish route", async () => {
  // INFRA-839 gave a role publish a kept lane, so POST /v1/messages/publish answers 202 where it
  // used to answer 404. GET /v1/roles/<role> is untouched and still answers 404, with the
  // role-holder body, when no live session holds the role. The heartbeat's re-assertion reads
  // that route (`undefinedOnNotFound`), so it stays as it is.
  const stateDirectory = join(tmpdir(), `claude-envoy-role-404-${crypto.randomUUID()}`)
  const regains: string[] = []
  const regained = Promise.withResolvers<void>()
  const client: FakeClient = {
    subscribe: async () => registered(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async () => ({ claimed: true, interest: interest() }),
    getRole: async () => {
      throw new EnvoyApiError({
        method: "GET",
        url: "http://listener.test/v1/roles/legion-claim",
        status: 404,
        responseBody: JSON.stringify({
          error: "no holder for role legion-claim",
          reason: "unclaimed",
        }),
      })
    },
    replay: emptyReplay,
  }
  const session = await startChannelSession({
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
  session.onRoleRegained(async (role, reason) => {
    regains.push(`${role} ${reason}`)
    regained.resolve()
  })

  try {
    await session.rememberRole("legion-claim")
    await regained.promise
    expect(regains).toEqual(["legion-claim reclaimed"])
    // The role is this session's again, so its record stays where `claude --resume` finds it.
    expect(await readPersistedRole(roleStateFile(stateDirectory, "ses_current"))).toEqual({
      session_id: "ses_current",
      role: "legion-claim",
      client_instance_id: INSTANCE,
    })
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a soft claim a different live session refuses removes the role record, which no kept answer touches", async () => {
  // The one path that removes a role record by hand (`pruneStaleSessionRoles` says so) is driven
  // by `setRole`'s 409 naming a session other than this one — that session holds the role, and
  // this one demonstrably does not — and never by a publish answer. INFRA-839 changed only the
  // publish route, so this path is as it was.
  const stateDirectory = join(tmpdir(), `claude-envoy-role-refused-${crypto.randomUUID()}`)
  const removed = Promise.withResolvers<void>()
  const client: FakeClient = {
    subscribe: async () => registered(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async () => {
      removed.resolve()
      return { claimed: false, holder: "ses_newer" }
    },
    getRole: async () => ({
      role: "legion-claim",
      holder: "ses_newer",
      last_seen: 1,
      state: "live" as const,
    }),
    replay: emptyReplay,
  }
  const session = await startChannelSession({
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
  const regains: string[] = []
  session.onRoleRegained(async (role, reason) => {
    regains.push(`${role} ${reason}`)
  })

  try {
    await session.rememberRole("legion-claim")
    await removed.promise
    await waitFor(
      async () =>
        (await readPersistedRole(roleStateFile(stateDirectory, "ses_current"))) === undefined,
      "the refused role's record to be removed",
    )
    expect(regains).toEqual([])
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a refusal naming this session under another process keeps the record, asks again, and takes the role back once that claim lapses", async () => {
  // The incident (dispatch://INFRA-783): a second `claude` resumed this session's id, took the
  // role under its own client instance and exited. The listener's 409 names this very session,
  // because the standing claim is on this session id; the role is still not this process's, and
  // the record is its only way back to it. So the record stays, every heartbeat asks again, and
  // the claim lands as soon as nobody renews the stale one.
  const stateDirectory = join(tmpdir(), `claude-envoy-role-same-id-${crypto.randomUUID()}`)
  const roleFile = roleStateFile(stateDirectory, "ses_current")
  const claims: unknown[] = []
  let staleClaimLive = true
  const client: FakeClient = {
    subscribe: async () => registered(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async (input) => {
      claims.push(input)
      if (staleClaimLive) return { claimed: false, holder: "ses_current" }
      return { claimed: true, interest: interest() }
    },
    // The lookup names this session, under the process that claimed it: the other `claude`.
    getRole: async () => ({
      role: "legion-claim",
      holder: "ses_current",
      last_seen: 1,
      state: "live" as const,
      client_instance_id: OTHER_INSTANCE,
    }),
    replay: emptyReplay,
  }
  const session = await startChannelSession({
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
  const regains: string[] = []
  const regained = Promise.withResolvers<void>()
  session.onRoleRegained(async (role, reason) => {
    regains.push(`${role} ${reason}`)
    regained.resolve()
  })

  try {
    await session.rememberRole("legion-claim")
    await waitFor(
      async () => claims.length >= 2,
      "the refused claim to be asked again on a later heartbeat",
      () => `${claims.length} claims so far`,
    )
    expect(regains).toEqual([])
    // Kept throughout: deleting it here is what left the lane with no way back to its role.
    expect(await readPersistedRole(roleFile)).toEqual({
      session_id: "ses_current",
      role: "legion-claim",
      client_instance_id: INSTANCE,
    })

    staleClaimLive = false

    await regained.promise
    expect(regains).toEqual(["legion-claim reclaimed"])
    expect(await readPersistedRole(roleFile)).toEqual({
      session_id: "ses_current",
      role: "legion-claim",
      client_instance_id: INSTANCE,
    })
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a lookup marked handed back makes the heartbeat claim the role again and keeps the record", async () => {
  // dispatch://INFRA-783: a `claude` built before a release named its process hands this role
  // back without naming one, so the listener cannot free this live process's claim and marks it
  // instead. Nothing renews a marked claim, so without reading the mark this process would go
  // on believing it holds the role and lose it a live window and up to one reap interval
  // later. The mark is read, the very next heartbeat claims again, and the listener grants a
  // claim from the process its claim names, which clears the mark. The record is kept
  // throughout: removing it is what a refusal naming another session does, and nothing here
  // says another session has the role.
  const stateDirectory = join(tmpdir(), `claude-envoy-role-handed-back-${crypto.randomUUID()}`)
  const roleFile = roleStateFile(stateDirectory, "ses_current")
  const claims: unknown[] = []
  let handedBack = true
  const client: FakeClient = {
    subscribe: async () => registered(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async (input) => {
      claims.push(input)
      // The fresh claim clears the mark, as the listener's does.
      handedBack = false
      return { claimed: true, interest: interest() }
    },
    // This session and this very process — and the hand-back mark, which is why it is no hold.
    getRole: async () => ({
      role: "legion-claim",
      holder: "ses_current",
      last_seen: 1,
      state: "live" as const,
      client_instance_id: INSTANCE,
      ...(handedBack ? { handed_back: true } : {}),
    }),
    replay: emptyReplay,
  }
  const session = await startChannelSession({
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
  const regains: string[] = []
  session.onRoleRegained(async (role, reason) => {
    regains.push(`${role} ${reason}`)
  })

  try {
    await session.rememberRole("legion-claim")
    await waitFor(
      async () => regains.length >= 1,
      "the heartbeat to claim the marked role again and be granted it",
      () => `${claims.length} claims so far; regains [${regains.join(", ")}]`,
    )
    expect(regains).toEqual(["legion-claim reclaimed"])
    // One claim: the mark it cleared leaves the later heartbeats confirming the hold.
    expect(claims).toEqual([
      { sessionID: "ses_current", role: "legion-claim", soft: true, clientInstanceID: INSTANCE },
    ])
    expect(await readPersistedRole(roleFile)).toEqual({
      session_id: "ses_current",
      role: "legion-claim",
      client_instance_id: INSTANCE,
    })
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("the age sweep does not reach the record of a process still pressing its refused claim", async () => {
  // A claim refused by the same session id under another process is pressed every heartbeat, so
  // the record it is pressed for must stay readable however long the refusal lasts: the retry
  // marks it seen, as a confirmed holder's heartbeat does, and the thirty-day age never reaches
  // it.
  const stateDirectory = join(tmpdir(), `claude-envoy-role-pressed-${crypto.randomUUID()}`)
  const roleFile = roleStateFile(stateDirectory, "ses_current")
  const claims: unknown[] = []
  const client: FakeClient = {
    subscribe: async () => registered(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async (input) => {
      claims.push(input)
      return { claimed: false, holder: "ses_current" }
    },
    getRole: async () => ({
      role: "legion-claim",
      holder: "ses_current",
      last_seen: 1,
      state: "live" as const,
      client_instance_id: OTHER_INSTANCE,
    }),
    replay: emptyReplay,
  }
  const session = await startChannelSession({
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

  try {
    await session.rememberRole("legion-claim")
    await waitFor(async () => claims.length >= 1, "the first refusal")
    // Aged as a record would be whose role nothing confirmed for thirty days.
    const past = new Date(Date.now() - STATE_FILE_MAX_AGE_MS - 60_000)
    await utimes(roleFile, past, past)
    await waitFor(
      async () => (await stat(roleFile)).mtimeMs > Date.now() - STATE_FILE_MAX_AGE_MS,
      `a heartbeat marking ${roleFile} seen while the claim is refused`,
    )
    await session.shutdown()
    // Swept as the next session to start, not as this one: the sweep never drops its own record.
    await pruneStaleSessionRoles(stateDirectory, "ses_other")

    expect(await readPersistedRole(roleFile)).toEqual({
      session_id: "ses_current",
      role: "legion-claim",
      client_instance_id: INSTANCE,
    })
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})
