import { expect, test } from "bun:test"
import { mkdir, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import type { EnvoyClient, Interest } from "@legion/envoy-client/transport"
import type {
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
import { roleStateFile, SessionIdentity } from "../src/session-identity"

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

test("reasserts a restored role after a successful registration heartbeat loses it", async () => {
  const stateDirectory = join(tmpdir(), `claude-envoy-reassert-${crypto.randomUUID()}`)
  const roleFile = roleStateFile(stateDirectory, "ses_current")
  await mkdir(dirname(roleFile), { recursive: true })
  await writeFile(roleFile, JSON.stringify({ session_id: "ses_current", role: "reviewer" }))
  const setRoles: unknown[] = []
  const reasserted = Promise.withResolvers<void>()
  const client: Pick<
    EnvoyClient,
    "subscribe" | "unsubscribe" | "unregisterSession" | "setRole" | "getRole" | "getInterest"
  > = {
    subscribe: async () => interest(),
    getInterest: async () => interest(),
    unsubscribe: async () => undefined,
    unregisterSession: async () => undefined,
    setRole: async (input) => {
      setRoles.push(input)
      if (setRoles.length === 2) reasserted.resolve()
      return { claimed: true, interest: interest() }
    },
    getRole: async () => ({ role: "reviewer", holder: "ses_lost", last_seen: 1 }),
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
  const client: Pick<
    EnvoyClient,
    "subscribe" | "unsubscribe" | "unregisterSession" | "setRole" | "getRole" | "getInterest"
  > = {
    subscribe: async () => interest(),
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
