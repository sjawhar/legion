import { expect, test } from "bun:test"
import { mkdir, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import type { EnvoyClient, Interest } from "@legion/envoy-client/transport"
import type { ChannelNotification, ChannelNotifier } from "../src/envoy-channel-server"
import { startChannelSession } from "../src/envoy-channel-server"
import type {
  ChannelForwarderConnection,
  ChannelInboundMessage,
  ChannelTopicSubscription,
} from "../src/channel-forwarder"

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
  const roleStateFile = join(tmpdir(), `claude-envoy-reassert-${crypto.randomUUID()}`, "role.json")
  await mkdir(dirname(roleStateFile), { recursive: true })
  await writeFile(roleStateFile, JSON.stringify({ session_id: "ses_previous", role: "reviewer" }))
  const setRoles: unknown[] = []
  const reasserted = Promise.withResolvers<void>()
  const client: Pick<
    EnvoyClient,
    "subscribe" | "unsubscribe" | "unregisterSession" | "setRole" | "getRole"
  > = {
    subscribe: async () => interest(),
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
    sessionId: "ses_current",
    directory: "/tmp",
    connection: new IdleNats(),
    notifier,
    client,
    heartbeatMs: 25,
    roleStateFile,
  })

  try {
    await reasserted.promise
    expect(setRoles).toEqual([
      { sessionID: "ses_current", role: "reviewer", soft: true, previousSessionID: "ses_previous" },
      { sessionID: "ses_current", role: "reviewer", soft: true },
    ])
  } finally {
    await session.shutdown()
    await rm(dirname(roleStateFile), { recursive: true, force: true })
  }
})
