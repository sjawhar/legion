import { expect, test } from "bun:test"
import { mkdir, rm, writeFile } from "node:fs/promises"
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
import { readPersistedRole, roleStateFile, SessionIdentity } from "../src/session-identity"
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
    getRole: async () => ({ role: "reviewer", holder: "ses_lost", last_seen: 1 }),
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
    pluginVersion: "source",
  })

  try {
    await reasserted.promise
    expect(setRoles).toEqual([
      { sessionID: "ses_current", role: "reviewer", soft: true, previousSessionID: "ses_current" },
      { sessionID: "ses_current", role: "reviewer", soft: true },
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
      const answer = { role: "legion-claim", holder, last_seen: 1 }
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
    })
  } finally {
    await session.shutdown()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("a soft claim another live session refuses removes the role record, which no kept answer touches", async () => {
  // The one path that removes a role record by hand (`pruneStaleSessionRoles` says so) is driven
  // by `setRole`'s 409 — another live session holds the role — and never by a publish answer.
  // INFRA-839 changed only the publish route, so this path is as it was.
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
    getRole: async () => ({ role: "legion-claim", holder: "ses_newer", last_seen: 1 }),
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
