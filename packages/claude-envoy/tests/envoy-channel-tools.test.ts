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
    message: "sent evt-sent to ses_receiver, which is connected",
    event_id: "evt-sent",
    recipient: "ses_receiver",
    confirmed: true,
    state: "connected",
  })
})

test("a send that is not confirmed names its key, and a resend under that key sends it again", async () => {
  const keys: string[] = []
  let answer: "time out" | "repeat" = "time out"
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async (_request, init) => {
      keys.push((JSON.parse(String(init?.body)) as { idempotency_key: string }).idempotency_key)
      if (answer === "time out") throw new DOMException("The operation timed out.", "TimeoutError")
      return Response.json({
        event_id: "evt-resent",
        source: "agent",
        source_event_id: "agent.ses_claude.evt-resent",
        source_session: "ses_claude",
        topic: "notifications.agent.ses_receiver",
        dedupe_key: `agent.ses_receiver.${keys[0]}`,
        issued_at: 1,
        payload_summary: "ready",
        trace_id: "trace-resent",
        recipient: "ses_receiver",
        duplicate: true,
      })
    },
  })
  const runtime: ChannelToolRuntime = { identity, client, session: sessionWith([]) }

  const failed = await executeEnvoyTool(runtime, "envoy_send", {
    session_id: "ses_receiver",
    message: "ready",
  }).then(
    () => undefined,
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  const key = keys[0] as string
  expect(failed).toContain("The send to ses_receiver is not confirmed delivered")
  expect(failed).toContain(
    `Sending it again with idempotency_key ${key} within 72 hours is the safe resend`,
  )

  answer = "repeat"
  const resent = await executeEnvoyTool(runtime, "envoy_send", {
    session_id: "ses_receiver",
    message: "ready",
    idempotency_key: key,
  })

  expect(keys).toEqual([key, key, key])
  expect(resent).toMatchObject({
    message:
      "sent to ses_receiver once: an earlier attempt under the same idempotency_key was already stored, so this one stored no second copy",
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

test("envoy_sessions filters by the machine a session runs on and by the listener it registered through", async () => {
  const sessions = [
    {
      session_id: "ses_on_hub",
      machine_id: "envoy-hub",
      host: "gke-node-7",
      dir: "/work",
      port: 0,
      title: "",
      topics: [],
      self_subscribed: true,
    },
    {
      session_id: "ses_on_devbox",
      machine_id: "devbox",
      host: "devbox",
      dir: "/work",
      port: 0,
      title: "",
      topics: [],
      self_subscribed: true,
    },
  ]
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async () => Response.json(sessions),
  })
  const runtime: ChannelToolRuntime = { identity, client, session: sessionWith([]) }
  const listed = async (machine: string) =>
    (
      (await executeEnvoyTool(runtime, "envoy_sessions", { machine })) as { session_id: string }[]
    ).map((session) => session.session_id)

  expect(await listed("envoy-hub")).toEqual(["ses_on_hub"])
  expect(await listed("gke-node-7")).toEqual(["ses_on_hub"])
  expect(await listed("devbox")).toEqual(["ses_on_devbox"])
  expect(await listed("other")).toEqual([])
})

/** One envelope, as the listener echoes it back to a send or a publish. */
function envelope(topic: string): Record<string, unknown> {
  return {
    event_id: "evt-text",
    source: "agent",
    source_event_id: "agent.ses_claude.evt-text",
    source_session: "ses_claude",
    topic,
    dedupe_key: "dedupe-text",
    issued_at: 1,
    payload_summary: "message",
    trace_id: "trace-text",
  }
}

/** A runtime whose listener answers every send and publish with `answer`. */
function runtimeAnswering(answer: Record<string, unknown>): ChannelToolRuntime {
  return {
    identity,
    client: createEnvoyClient({
      baseUrl: "http://envoy.test",
      fetch: async () => Response.json(answer),
    }),
    session: sessionWith([]),
  }
}

test("tells the model its message was queued, never sent, when the target is stopped", async () => {
  const runtime = runtimeAnswering({
    ...envelope("notifications.agent.ses_receiver"),
    recipient: "ses_receiver",
    state: "stopped",
    last_seen: Date.UTC(2026, 10, 2, 9, 15, 0),
  })

  const result = await executeEnvoyTool(runtime, "envoy_send", {
    session_id: "ses_receiver",
    message: "ready",
  })

  const { message } = result as { readonly message: string }
  expect(message).toMatch(/^queued \S+ for /)
  expect(message).not.toContain("sent")
  expect(result).toEqual({
    message:
      "queued evt-text for ses_receiver: the session is stopped (last seen 2026-11-02T09:15:00Z); if it runs again within 72 hours, it gets the message",
    event_id: "evt-text",
    recipient: "ses_receiver",
    confirmed: true,
    state: "stopped",
    last_seen: Date.UTC(2026, 10, 2, 9, 15, 0),
  })
})

for (const published of [
  {
    counts: { recipients: 0, recipients_connected: 0 },
    label: "no other session subscribes",
    pattern: /^published \S+; no other session subscribes to /,
    text: "published evt-text; no other session subscribes to team.test, so no other session gets it",
  },
  {
    counts: { recipients: 3, recipients_connected: 2 },
    label: "three other sessions get it",
    pattern: /^published \S+ to 3 other sessions/,
    text: "published evt-text to 3 other sessions (2 running now)",
  },
  {
    counts: {},
    label: "a listener that counts nobody",
    pattern: /^published \S+$/,
    text: "published evt-text",
  },
]) {
  test(`renders a publish the listener answered with ${published.label}`, async () => {
    const runtime = runtimeAnswering({ ...envelope("team.test"), ...published.counts })

    const result = (await executeEnvoyTool(runtime, "envoy_publish", {
      topic: "team.test",
      message: "broadcast",
    })) as { readonly message: string }

    expect(result.message).toMatch(published.pattern)
    expect(result.message).toBe(published.text)
  })
}
