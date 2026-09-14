import { expect, test } from "bun:test"
import type { EnvoyClient } from "@legion/envoy-client/transport"
import {
  type ChannelSession,
  type ChannelToolRuntime,
  executeEnvoyTool,
} from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"

const session: ChannelSession = {
  delivery: { enqueue: async () => undefined, announce: async () => undefined, inbox: () => [] },
  topics: () => [],
  follow: async () => [],
  unfollow: async () => [],
  rememberRole: async () => undefined,
  shutdown: async () => undefined,
}

test("reports the Claude project directory rather than the package launch directory", async () => {
  const runtime: ChannelToolRuntime = {
    identity: new SessionIdentity("ses_claude", "/work/project"),
    client: {} as EnvoyClient,
    session,
  }

  const result = await executeEnvoyTool(runtime, "envoy_whoami", {})

  expect(result).toMatchObject({ session_id: "ses_claude", dir: "/work/project" })
})
