import { expect, test } from "bun:test"
import { dispatchToolSpecs } from "@legion/contracts"
import { createEnvoyClient, type EnvoyClient } from "@legion/envoy-client/transport"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import {
  channelToolDefinitions,
  executeEnvoyTool,
  type ChannelSession,
  type ChannelToolRuntime,
} from "../src/envoy-channel-server"

function sessionWith(followed: string[]): ChannelSession {
  return {
    delivery: { enqueue: async () => undefined, inbox: () => [] },
    follow: async (topics) => {
      followed.push(...topics)
    },
    unfollow: async () => [],
    rememberRole: async () => undefined,
    shutdown: async () => undefined,
  }
}

test("declares every shared Envoy tool including the bounded inbox and shared Dispatch tools", () => {
  const definitions = channelToolDefinitions(true)

  expect(definitions.map(({ name }) => name)).toEqual([
    ...envoyToolSpecs.map(({ name }) => name),
    ...dispatchToolSpecs.map(({ name }) => name),
  ])
  expect(definitions.map(({ name }) => name)).toContain("envoy_inbox")
})

test("sends Envoy messages through the shared transport", async () => {
  let body = ""
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async (_request, init) => {
      body = String(init?.body)
      return Response.json({
        event_id: "evt-sent",
        source: "agent",
        source_event_id: "agent.ses_claude.evt-sent",
        source_session: "ses_claude",
        topic: "notifications.agent.ses_receiver",
        dedupe_key: "agent.ses_receiver.evt-sent",
        issued_at: 1,
        payload_summary: "ready",
        trace_id: "trace-sent",
        recipient: "ses_receiver",
      })
    },
  })
  const followed: string[] = []
  const runtime: ChannelToolRuntime = {
    sessionId: "ses_claude",
    directory: process.cwd(),
    client,
    session: sessionWith(followed),
  }

  const result = await executeEnvoyTool(runtime, "envoy_send", {
    session_id: "ses_receiver",
    message: "ready",
  })

  expect(JSON.parse(body)).toMatchObject({
    source: "agent",
    source_session: "ses_claude",
    target_session: "ses_receiver",
    message: "ready",
  })
  expect(result).toEqual({
    message: "sent evt-sent to ses_receiver",
    event_id: "evt-sent",
    recipient: "ses_receiver",
    confirmed: true,
  })
})

test("keeps Dispatch asks on Dispatch and follows their returned event topic", async () => {
  const server = Bun.serve({
    port: 0,
    fetch: async (request) => {
      expect(new URL(request.url).pathname).toBe("/api/v1/issues/DSP-3/asks")
      expect(await request.json()).toMatchObject({
        question: "Approve the channel?",
        actor: { kind: "session", id: "ses_claude", origin: { host: "claude" } },
      })
      return Response.json({
        id: "ask-3",
        issue_key: "DSP-3",
        author: { kind: "session", id: "ses_claude" },
        question: "Approve the channel?",
        options: [],
        multiple: false,
        custom: true,
        urgency: "med",
        anchor: null,
        state: "open",
        answer: null,
        created_at: "2026-09-13T00:00:00Z",
      })
    },
  })
  const previous = { ...process.env }
  process.env["DISPATCH_URL"] = `http://127.0.0.1:${server.port}`
  process.env["DISPATCH_TOKEN"] = "test-token"
  const followed: string[] = []
  const runtime: ChannelToolRuntime = {
    sessionId: "ses_claude",
    directory: process.cwd(),
    client: {} as EnvoyClient,
    session: sessionWith(followed),
  }

  try {
    const result = await executeEnvoyTool(runtime, "dispatch_ask", {
      issue: "DSP-3",
      question: "Approve the channel?",
    })

    expect(result).toEqual({
      text: "Opened ask ask-3: Approve the channel?",
      details: { issue: "DSP-3", topic: "notifications.dispatch.issue.DSP-3.>", ask: "ask-3" },
    })
    expect(followed).toEqual(["notifications.dispatch.issue.DSP-3.>"])
  } finally {
    server.stop(true)
    process.env = previous
  }
})
