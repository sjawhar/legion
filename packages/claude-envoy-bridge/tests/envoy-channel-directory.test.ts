import { expect, test } from "bun:test"
import type { EnvoyClient } from "@legion/envoy-client/transport"
import {
  executeEnvoyTool,
  type ChannelSession,
  type ChannelToolRuntime,
} from "../src/envoy-channel-server"

const session: ChannelSession = {
  delivery: { enqueue: async () => undefined, inbox: () => [] },
  follow: async () => undefined,
  unfollow: async () => [],
  rememberRole: async () => undefined,
  shutdown: async () => undefined,
}

test("reports the Claude project directory rather than the package launch directory", async () => {
  const runtime: ChannelToolRuntime = {
    sessionId: "ses_claude",
    directory: "/work/project",
    client: {} as EnvoyClient,
    session,
  }

  const result = await executeEnvoyTool(runtime, "envoy_whoami", {})

  expect(result).toMatchObject({ session_id: "ses_claude", dir: "/work/project" })
})
