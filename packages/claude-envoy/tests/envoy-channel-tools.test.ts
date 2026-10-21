import { expect, test } from "bun:test"
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { delimiter, join } from "node:path"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { createEnvoyClient, type EnvoyClient } from "@legion/envoy-client/transport"
import {
  type ChannelSession,
  type ChannelToolRuntime,
  channelToolDefinitions,
  executeEnvoyTool,
} from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

function sessionWith(followed: string[]): ChannelSession {
  return {
    delivery: {
      enqueue: async () => true,
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
    onRoleRegained: () => undefined,
    shutdown: async () => undefined,
  }
}

const identity = new SessionIdentity("ses_claude", process.cwd())

test("declares exactly the shared Envoy tools, the bounded inbox included", () => {
  const definitions = channelToolDefinitions()

  expect(definitions.map(({ name }) => name)).toEqual(envoyToolSpecs.map(({ name }) => name))
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

test("replies in a Slack thread with slack_post, with the token the broker grants", async () => {
  // The broker's client on PATH, standing in for the exec form once LEGION_SLACK_BOT_TOKEN is
  // granted, and a fake Slack Web API at SLACK_POST_API_URL.
  const dir = mkdtempSync(join(tmpdir(), "claude-envoy-slack-"))
  writeFileSync(
    join(dir, "agent-secrets"),
    '#!/bin/sh\nwhile [ "$1" != "--" ]; do shift; done\nshift\nLEGION_SLACK_BOT_TOKEN=xoxb-granted exec "$@"\n',
  )
  chmodSync(join(dir, "agent-secrets"), 0o755)
  const posts: unknown[] = []
  const slack = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      posts.push({
        authorization: request.headers.get("authorization"),
        body: await request.json(),
      })
      return Response.json({ ok: true, channel: "C00000000", ts: "1700000000.000200" })
    },
  })
  const previous = {
    PATH: process.env["PATH"],
    SLACK_POST_API_URL: process.env["SLACK_POST_API_URL"],
  }
  process.env["PATH"] = `${dir}${delimiter}${previous.PATH ?? ""}`
  process.env["SLACK_POST_API_URL"] = `http://127.0.0.1:${slack.port}/api`
  const runtime: ChannelToolRuntime = {
    identity,
    client: createEnvoyClient({
      baseUrl: "http://envoy.test",
      fetch: async () => Response.json({}),
    }),
    session: sessionWith([]),
  }

  try {
    const result = await executeEnvoyTool(runtime, "slack_post", {
      channel: "C00000000",
      thread_ts: "1700000000.000100",
      text: "Filed as ACME-12.",
    })

    expect(posts).toEqual([
      {
        authorization: "Bearer xoxb-granted",
        body: { channel: "C00000000", thread_ts: "1700000000.000100", text: "Filed as ACME-12." },
      },
    ])
    expect(result).toEqual({
      channel: "C00000000",
      thread_ts: "1700000000.000100",
      ts: "1700000000.000200",
    })
  } finally {
    slack.stop(true)
    for (const [name, value] of Object.entries(previous)) {
      if (value === undefined) delete process.env[name]
      else process.env[name] = value
    }
    rmSync(dir, { recursive: true, force: true })
  }
})

test("a dispatch_* call is an unknown tool even with Dispatch configured, and reaches no Dispatch", async () => {
  const requests: string[] = []
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      requests.push(`${request.method} ${new URL(request.url).pathname}`)
      return Response.json({ error: "unexpected" }, { status: 500 })
    },
  })
  const previous = { ...process.env }
  process.env["DISPATCH_URL"] = `http://127.0.0.1:${server.port}`
  process.env["DISPATCH_TOKEN"] = "test-token"
  const runtime: ChannelToolRuntime = {
    identity,
    client: {} as EnvoyClient,
    session: sessionWith([]),
  }

  try {
    await expect(
      executeEnvoyTool(runtime, "dispatch_ask", { issue: "DSP-3", question: "Approve?" }),
    ).rejects.toThrow("Unsupported Envoy tool: dispatch_ask")
    expect(requests).toEqual([])
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
