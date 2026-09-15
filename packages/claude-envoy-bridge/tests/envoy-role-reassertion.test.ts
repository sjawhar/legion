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
import type { ChannelNotification, ChannelNotifier } from "../src/envoy-channel-server"
import { startChannelSession } from "../src/envoy-channel-server"
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
    client,
    heartbeatMs: 25,
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
