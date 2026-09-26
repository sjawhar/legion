import { expect, test } from "bun:test"
import { dispatchToolSpecs } from "@legion/contracts"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { createEnvoyClient, type EnvoyClient } from "@legion/envoy-client/transport"
import {
  type ChannelSession,
  type ChannelToolRuntime,
  channelToolDefinitions,
  executeEnvoyTool,
} from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"

function sessionWith(followed: string[]): ChannelSession {
  return {
    delivery: {
      enqueue: async () => undefined,
      announceFollow: async () => undefined,
      inbox: () => [],
    },
    topics: () => ["notifications.agent.ses_claude", ...followed],
    follow: async (topics) => {
      const fresh = topics.filter((topic) => !followed.includes(topic))
      followed.push(...fresh)
      return fresh
    },
    unfollow: async () => [],
    rememberRole: async () => undefined,
    shutdown: async () => undefined,
  }
}

const identity = new SessionIdentity("ses_claude", process.cwd())

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
  const runtime: ChannelToolRuntime = { identity, client, session: sessionWith(followed) }

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

test("keeps Dispatch asks on Dispatch, follows no topic, and announces the followed ask once", async () => {
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
  const announced: unknown[] = []
  const session = sessionWith(followed)
  const runtime: ChannelToolRuntime = {
    identity,
    client: {} as EnvoyClient,
    session: {
      ...session,
      delivery: {
        ...session.delivery,
        announceFollow: async (details) => {
          announced.push(details)
        },
      },
    },
  }

  try {
    const result = await executeEnvoyTool(runtime, "dispatch_ask", {
      issue: "DSP-3",
      question: "Approve the channel?",
    })

    expect(result).toEqual({
      text:
        "Asked ask-3 on DSP-3 (urgency med): Approve the channel?\n" +
        "You follow this ask: its answer and replies reach you directly. " +
        "For every event on DSP-3: envoy_subscribe notifications.dispatch.issue.DSP-3.>",
      details: { issue: "DSP-3", ask: "ask-3", follows: { ask: "ask-3" } },
    })
    expect(followed).toEqual([])
    expect(announced).toEqual([{ issue: "DSP-3", ask: "ask-3", follows: { ask: "ask-3" } }])
  } finally {
    server.stop(true)
    process.env = previous
  }
})

test("envoy_list reports the union of live NATS subscriptions and registry interests", async () => {
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async () =>
      Response.json({
        session_id: "ses_claude",
        machine_id: "devbox",
        dir: "/tmp",
        topics: ["notifications.agent.ses_claude", "notifications.dispatch.issue.DSP-9.>"],
      }),
  })
  const runtime: ChannelToolRuntime = {
    identity,
    client,
    session: sessionWith(["notifications.dispatch.issue.DSP-3.>"]),
  }

  const result = await executeEnvoyTool(runtime, "envoy_list", {})

  expect(result).toEqual({
    session_id: "ses_claude",
    machine_id: "devbox",
    dir: "/tmp",
    topics: [
      "notifications.agent.ses_claude",
      "notifications.dispatch.issue.DSP-9.>",
      "notifications.dispatch.issue.DSP-3.>",
    ],
    interests: [
      { topic: "notifications.agent.ses_claude", source: "both" },
      { topic: "notifications.dispatch.issue.DSP-9.>", source: "registry" },
      { topic: "notifications.dispatch.issue.DSP-3.>", source: "live" },
    ],
  })
})
