import { expect, jest, test } from "bun:test"
import { rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
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
import { readPersistedRole, roleStateFile, SessionIdentity } from "../src/session-identity"
import { emptyReplay, type FakeClient } from "./channel-session-harness"

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
