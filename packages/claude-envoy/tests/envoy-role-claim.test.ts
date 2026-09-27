import { expect, jest, test } from "bun:test"
import { rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import type { EnvoyClient, Interest } from "@legion/envoy-client/transport"
import type {
  ChannelForwarderConnection,
  ChannelInboundMessage,
  ChannelTopicSubscription,
} from "../src/channel-forwarder"
import type {
  ChannelNotification,
  ChannelNotifier,
  ChannelSession,
} from "../src/envoy-channel-server"
import { startChannelSession } from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"

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

test("reasserts a role claimed after channel startup on the next heartbeat", async () => {
  jest.useFakeTimers()
  const stateDirectory = join(tmpdir(), `claude-envoy-claim-${crypto.randomUUID()}`)
  const reasserted = Promise.withResolvers<void>()
  const setRoles: unknown[] = []
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
      reasserted.resolve()
      return { claimed: true, interest: interest() }
    },
    getRole: async () => ({ role: "reviewer", holder: "ses_lost", last_seen: 1 }),
  }
  let session: ChannelSession | undefined

  try {
    session = await startChannelSession({
      identity: new SessionIdentity("ses_current", "/tmp"),
      connection: new IdleNats(),
      notifier,
      client,
      heartbeatMs: 25,
      stateDirectory,
    })

    await session.rememberRole("reviewer")
    jest.advanceTimersByTime(25)
    jest.useRealTimers()
    await reasserted.promise
    expect(setRoles).toEqual([{ sessionID: "ses_current", role: "reviewer", soft: true }])
  } finally {
    await session?.shutdown()
    jest.useRealTimers()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})
