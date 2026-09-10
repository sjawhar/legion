import { expect, spyOn, test } from "bun:test"
import { dispatchToolSpecs, zodSchemaApi } from "@legion/contracts"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import type { Server } from "bun"
import { z } from "zod"
import type * as EnvoyMcpServer from "../src/envoy-mcp-server"
import { FakeNatsServer } from "./fake-nats-server"

// The tool list is decided from process.env when the module loads, so each
// gating case needs its own module instance: a distinct query string defeats
// the module cache, which a static import cannot do.
function loadServer(instance: string): Promise<typeof EnvoyMcpServer> {
  return import(`../src/envoy-mcp-server?${instance}`)
}

const ObjectJsonSchema = z.object({
  required: z.array(z.string()).optional(),
  properties: z.record(z.string(), z.unknown()),
})

const AskPost = z.object({
  question: z.string(),
  actor: z.object({
    kind: z.literal("session"),
    id: z.string(),
    origin: z.object({ host: z.literal("claude"), cwd: z.string() }),
  }),
})

const dispatchToolNames = [
  "dispatch_issue",
  "dispatch_ask",
  "dispatch_comment",
  "dispatch_suggest",
  "dispatch_message",
  "dispatch_doc_edit",
  "dispatch_doc_read",
  "dispatch_artifact",
  "dispatch_read",
] as const

const requiredDispatchFields: Readonly<Record<(typeof dispatchToolNames)[number], readonly string[]>> = {
  dispatch_issue: ["project", "title"],
  dispatch_ask: ["issue", "question"],
  dispatch_comment: ["issue", "body"],
  dispatch_suggest: ["issue", "artifact", "quote", "replace_with"],
  dispatch_message: ["issue", "body"],
  dispatch_doc_edit: ["issue", "artifact", "ops"],
  dispatch_doc_read: [],
  dispatch_artifact: ["issue", "name"],
  dispatch_read: [],
}

test("exposes every shared Dispatch tool with its required JSON Schema fields when Dispatch is enabled", async () => {
  // given
  const previous = { ...process.env }
  process.env["DISPATCH_URL"] = "http://127.0.0.1:1"
  process.env["DISPATCH_TOKEN"] = "test-token"
  delete process.env["DISPATCH_MCP_URL"]
  const stderr = captureStderr()
  try {
    const module = await loadServer("dispatch-enabled")
    const definitions = module.envoyMcpToolDefinitions
    const dispatchDefinitions = definitions.filter((definition) =>
      dispatchToolNames.includes(definition.name as (typeof dispatchToolNames)[number]),
    )

    // then
    expect(dispatchDefinitions.map((definition) => definition.name)).toEqual([...dispatchToolNames])
    expect(dispatchDefinitions.map((definition) => definition.name)).toEqual(
      dispatchToolSpecs.map((spec) => spec.name),
    )
    expect(definitions.map((definition) => definition.name)).not.toContain("envoy_inbox")
    for (const definition of dispatchDefinitions) {
      const schema = ObjectJsonSchema.parse(definition.inputSchema)
      expect(schema.required ?? []).toEqual([
        ...requiredDispatchFields[definition.name as (typeof dispatchToolNames)[number]],
      ])
    }
    expect(stderr.lines).toEqual([])
  } finally {
    process.env = previous
    stderr.restore()
  }
})

test("omits all Dispatch tools when Dispatch is not enabled", async () => {
  // given
  const previous = { ...process.env }
  delete process.env["DISPATCH_URL"]
  delete process.env["DISPATCH_TOKEN"]
  delete process.env["DISPATCH_MCP_URL"]
  process.env["HOME"] = "/nonexistent-home-for-dispatch-gating"
  const stderr = captureStderr()
  try {
    const module = await loadServer("dispatch-disabled")

    // then
    expect(module.envoyMcpToolDefinitions.map((definition) => definition.name)).toEqual(
      envoyToolSpecs.filter((spec) => spec.name !== "envoy_inbox").map(({ name }) => name),
    )
    expect(stderr.lines).toEqual([
      "envoy-mcp: Dispatch tools disabled — no Dispatch URL configured\n",
    ])
  } finally {
    stderr.restore()
    process.env = previous
  }
})

test("omits Dispatch tools and logs the missing bearer token once", async () => {
  // given
  const previous = { ...process.env }
  process.env["DISPATCH_URL"] = "http://127.0.0.1:8766"
  delete process.env["DISPATCH_TOKEN"]
  delete process.env["DISPATCH_MCP_URL"]
  process.env["HOME"] = "/nonexistent-home-for-dispatch-gating"
  const stderr = captureStderr()
  try {
    const module = await loadServer("dispatch-token-disabled")

    // then
    expect(module.envoyMcpToolDefinitions.map((definition) => definition.name)).toEqual(
      envoyToolSpecs.filter((spec) => spec.name !== "envoy_inbox").map(({ name }) => name),
    )
    expect(stderr.lines).toEqual([
      "envoy-mcp: Dispatch tools disabled — dispatch.token must be a non-empty bearer token\n",
    ])
  } finally {
    stderr.restore()
    process.env = previous
  }
})

const SubscribeBody = z.object({
  session_id: z.string(),
  dir: z.string(),
  topics: z.array(z.string()),
})

interface FakeEnvoy {
  readonly server: Server<undefined>
  readonly subscribes: unknown[]
  readonly unsubscribes: unknown[]
}

/** A stand-in Envoy listener that records /v1/interests/subscribe and /v1/interests/unsubscribe bodies. */
function fakeEnvoy(status = 200): FakeEnvoy {
  const subscribes: unknown[] = []
  const unsubscribes: unknown[] = []
  const server = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const path = new URL(request.url).pathname
      if (path === "/v1/interests/unsubscribe") {
        unsubscribes.push(await request.json())
        return new Response(null, { status: 202 })
      }
      if (path !== "/v1/interests/subscribe") {
        return new Response(null, { status: 404 })
      }
      const body = SubscribeBody.parse(await request.json())
      subscribes.push(body)
      if (status !== 200) return new Response("nope", { status })
      return Response.json({
        session_id: body.session_id,
        machine_id: "example-host",
        dir: body.dir,
        port: 0,
        title: "",
        topics: body.topics,
        self_subscribed: true,
      })
    },
  })
  return { server, subscribes, unsubscribes }
}

test("reports a legacy listener send as recipient-unconfirmed", async () => {
  const server = Bun.serve({
    fetch: async (request) => {
      expect(new URL(request.url).pathname).toBe("/v1/messages/send")
      expect(await request.json()).toEqual({
        source: "agent",
        source_session: "ses_claude",
        target_session: "ses_target",
        message: "hello",
        in_reply_to: "event-before",
        supersedes: "event-obsolete",
        urgency: "blocking",
        expects_reply: "required",
        expires_at: 1_788_956_000_000,
        idempotency_key: expect.any(String),
      })
      return Response.json({
        event_id: "event-legacy",
        source: "agent",
        source_event_id: "agent.ses_claude.event-legacy",
        source_session: "ses_claude",
        topic: "notifications.agent.ses_target",
        dedupe_key: "agent.ses_target.event-legacy",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-legacy",
      })
    },
  })
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["ENVOY_URL"] = `http://127.0.0.1:${server.port}`
  try {
    const module = await loadServer("legacy-send")

    const result = await module.executeEnvoyTool("envoy_send", {
      session_id: "ses_target",
      message: "hello",
      in_reply_to: "event-before",
      supersedes: "event-obsolete",
      urgency: "blocking",
      expects_reply: "required",
      expires_at: 1_788_956_000_000,
    })

    expect(result).toEqual({
      message: "sent event-legacy to ses_target (recipient unconfirmed by listener)",
      event_id: "event-legacy",
      recipient: "ses_target",
      confirmed: false,
    })
  } finally {
    server.stop(true)
    process.env = { ...previous }
  }
})

test("rejects an invalid urgency with the shared field validation error", async () => {
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  try {
    const module = await loadServer("shared-metadata-validation")
    const spec = envoyToolSpecs.find((candidate) => candidate.name === "envoy_send")
    if (spec === undefined) throw new Error("envoy_send specification is missing")
    const parsed = z.object(spec.arguments(zodSchemaApi(z)) as unknown as z.ZodRawShape).safeParse({
      session_id: "ses_target",
      message: "hello",
      urgency: "urgent",
    })
    if (parsed.success) throw new Error("invalid urgency unexpectedly parsed")

    await expect(
      module.executeEnvoyTool("envoy_send", {
        session_id: "ses_target",
        message: "hello",
        urgency: "urgent",
      }),
    ).rejects.toThrow(parsed.error.message)
  } finally {
    process.env = { ...previous }
  }
})

test("returns the role holder through envoy_role_get", async () => {
  let receivedPath = ""
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      receivedPath = new URL(request.url).pathname
      return Response.json({
        role: "reviewer",
        holder: "01a01234-1234-7123-8123-123456789abc",
        last_seen: 1_789_026_472,
      })
    },
  })
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["ENVOY_URL"] = `http://127.0.0.1:${server.port}`
  try {
    const module = await loadServer("role-get")

    const result = await module.executeEnvoyTool("envoy_role_get", { role: "reviewer" })

    expect(receivedPath).toBe("/v1/roles/reviewer")
    expect(result).toEqual({
      role: "reviewer",
      holder: "01a01234-1234-7123-8123-123456789abc",
      last_seen: 1_789_026_472,
    })
  } finally {
    server.stop(true)
    process.env = { ...previous }
  }
})

test("dispatch_ask sends the native ask request with the Claude session origin and follows its topic", async () => {
  // given
  const posts: Array<{ body: unknown; headers: Record<string, string> }> = []
  const envoy = fakeEnvoy()
  const service = Bun.serve({
    port: 0,
    fetch: async (request) => {
      if (new URL(request.url).pathname !== "/api/v1/issues/DSP-3/asks") {
        return new Response(null, { status: 404 })
      }
      posts.push({ body: await request.json(), headers: Object.fromEntries(request.headers) })
      return Response.json({
        id: "ask-3",
        issue_key: "DSP-3",
        author: { kind: "session", id: "ses_claude" },
        question: "Should the branch merge?",
        options: [],
        multiple: false,
        custom: true,
        urgency: "med",
        anchor: null,
        state: "open",
        answer: null,
        created_at: "2026-09-09T00:00:00Z",
      })
    },
  })
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  delete process.env["ENVOY_NATS_URL"]
  process.env["DISPATCH_URL"] = `http://127.0.0.1:${service.port}`
  process.env["DISPATCH_TOKEN"] = "test-token"
  delete process.env["DISPATCH_MCP_URL"]
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.server.port}`
  const stderr: string[] = []
  const stderrSpy = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    stderr.push(String(chunk))
    return true
  })
  try {
    const module = await loadServer("dispatch-ask")

    // when
    const result = await module.executeEnvoyTool("dispatch_ask", {
      issue: "DSP-3",
      question: "Should the branch merge?",
    })

    // then
    expect(result).toEqual({
      text: "Opened ask ask-3: Should the branch merge?",
      details: {
        issue: "DSP-3",
        topic: "notifications.dispatch.issue.DSP-3.>",
        ask: "ask-3",
      },
    })
    expect(posts).toHaveLength(1)
    expect(posts[0]?.headers["authorization"]).toBe("Bearer test-token")
    expect(AskPost.parse(posts[0]?.body)).toEqual({
      question: "Should the branch merge?",
      actor: {
        kind: "session",
        id: "ses_claude",
        origin: { host: "claude", cwd: process.cwd() },
      },
    })
    expect(envoy.subscribes).toEqual([
      {
        session_id: "ses_claude",
        dir: process.cwd(),
        topics: ["notifications.dispatch.issue.DSP-3", "notifications.dispatch.issue.DSP-3.>"],
      },
    ])
    expect(stderr).toEqual([
      "envoy-mcp: ENVOY_NATS_URL is not set; messages on subscribed topics will not reach this session\n",
    ])
  } finally {
    stderrSpy.mockRestore()
    service.stop(true)
    envoy.server.stop(true)
    process.env = { ...previous }
  }
})

test("a rejected native Dispatch ask follows no topic", async () => {
  // given
  const envoy = fakeEnvoy()
  const service = Bun.serve({
    port: 0,
    fetch: async () =>
      Response.json(
        { error: "DSP-3 is closed; open a new issue", code: "ISSUE_CLOSED" },
        { status: 409 },
      ),
  })
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["DISPATCH_URL"] = `http://127.0.0.1:${service.port}`
  process.env["DISPATCH_TOKEN"] = "test-token"
  delete process.env["DISPATCH_MCP_URL"]
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.server.port}`
  try {
    const module = await loadServer("dispatch-rejected")

    // when / then
    await expect(
      module.executeEnvoyTool("dispatch_ask", {
        issue: "DSP-3",
        question: "Should the branch merge?",
      }),
    ).rejects.toThrow("DSP-3 is closed; open a new issue")
    expect(envoy.subscribes).toEqual([])
  } finally {
    service.stop(true)
    envoy.server.stop(true)
    process.env = { ...previous }
  }
})

test("a failed auto-subscribe does not fail a native Dispatch ask", async () => {
  // given
  const envoy = fakeEnvoy(503)
  const service = Bun.serve({
    port: 0,
    fetch: async () =>
      Response.json({
        id: "ask-3",
        issue_key: "DSP-3",
        author: { kind: "session", id: "ses_claude" },
        question: "Should the branch merge?",
        options: [],
        multiple: false,
        custom: true,
        urgency: "med",
        anchor: null,
        state: "open",
        answer: null,
        created_at: "2026-09-09T00:00:00Z",
      }),
  })
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  delete process.env["ENVOY_NATS_URL"]
  process.env["DISPATCH_URL"] = `http://127.0.0.1:${service.port}`
  process.env["DISPATCH_TOKEN"] = "test-token"
  delete process.env["DISPATCH_MCP_URL"]
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.server.port}`
  try {
    const module = await loadServer("dispatch-subscribe-fails")

    // when
    const result = await module.executeEnvoyTool("dispatch_ask", {
      issue: "DSP-3",
      question: "Should the branch merge?",
    })

    // then
    expect(result).toEqual({
      text: "Opened ask ask-3: Should the branch merge?",
      details: {
        issue: "DSP-3",
        topic: "notifications.dispatch.issue.DSP-3.>",
        ask: "ask-3",
      },
    })
    expect(envoy.subscribes).toHaveLength(2)
  } finally {
    service.stop(true)
    envoy.server.stop(true)
    process.env = { ...previous }
  }
})

test("uses the shared transport for unsubscribe requests", async () => {
  // given
  let receivedPath = ""
  let receivedBody = ""
  const server = Bun.serve({
    port: 0,
    fetch: async (request) => {
      receivedPath = new URL(request.url).pathname
      receivedBody = await request.text()
      return new Response(null, { status: 202 })
    },
  })
  const previousSessionId = process.env["CLAUDE_CODE_SESSION_ID"]
  const previousOverride = process.env["ENVOY_SESSION_ID"]
  const previousEnvoyUrl = process.env["ENVOY_URL"]
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["ENVOY_URL"] = `http://127.0.0.1:${server.port}`
  const module = await import("../src/envoy-mcp-server")
  const execute = Reflect.get(module, "executeEnvoyTool")

  try {
    // when
    expect(typeof execute).toBe("function")
    if (typeof execute !== "function") return
    const result = await execute("envoy_unsubscribe", {
      topics: ["notifications.agent.ses_claude"],
    })

    // then
    expect(result).toEqual({ removed: ["notifications.agent.ses_claude"] })
    expect(receivedPath).toBe("/v1/interests/unsubscribe")
    expect(JSON.parse(receivedBody)).toEqual({
      session_id: "ses_claude",
      topics: ["notifications.agent.ses_claude"],
    })
  } finally {
    server.stop(true)
    if (previousSessionId === undefined) delete process.env["CLAUDE_CODE_SESSION_ID"]
    else process.env["CLAUDE_CODE_SESSION_ID"] = previousSessionId
    if (previousOverride === undefined) delete process.env["ENVOY_SESSION_ID"]
    else process.env["ENVOY_SESSION_ID"] = previousOverride
    if (previousEnvoyUrl === undefined) delete process.env["ENVOY_URL"]
    else process.env["ENVOY_URL"] = previousEnvoyUrl
  }
})

function captureStderr(): { readonly lines: string[]; restore(): void } {
  const lines: string[] = []
  const spy = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    lines.push(String(chunk))
    return true
  })
  return { lines, restore: () => spy.mockRestore() }
}

const THREAD_BASE = "notifications.github.acme-org.example-repo.issue.3"
const THREAD = `${THREAD_BASE}.>`
const COMMENT = "notifications.github.acme-org.example-repo.issue.3.comment"
const INBOX = "notifications.agent.ses_claude"

/** The fields of an Envoy envelope the forwarder reads; the rest is opaque to it. */
const envelope = (dedupeKey: string): string =>
  JSON.stringify({ dedupe_key: dedupeKey, payload_summary: "Sami answered: option A" })

test("a manual subscription fails without a reachable NATS forwarder while a native Dispatch ask stays best-effort", async () => {
  // given
  const envoy = fakeEnvoy()
  const service = Bun.serve({
    port: 0,
    fetch: async () =>
      Response.json({
        id: "ask-3",
        issue_key: "DSP-3",
        author: { kind: "session", id: "ses_claude" },
        question: "Should the branch merge?",
        options: [],
        multiple: false,
        custom: true,
        urgency: "med",
        anchor: null,
        state: "open",
        answer: null,
        created_at: "2026-09-09T00:00:00Z",
      }),
  })
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["ENVOY_NATS_URL"] = "nats://127.0.0.1:1"
  process.env["DISPATCH_URL"] = `http://127.0.0.1:${service.port}`
  process.env["DISPATCH_TOKEN"] = "test-token"
  delete process.env["DISPATCH_MCP_URL"]
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.server.port}`
  const stderr = captureStderr()
  try {
    const module = await loadServer("broker-unreachable")

    // when
    const result = await module.executeEnvoyTool("dispatch_ask", {
      issue: "DSP-3",
      question: "Should the branch merge?",
    })
    await expect(module.executeEnvoyTool("envoy_subscribe", { topics: [THREAD] })).rejects.toThrow(
      "ENVOY_NATS_URL",
    )

    // then: Dispatch recorded its best-effort interest; manual subscription did not.
    expect(result).toEqual({
      text: "Opened ask ask-3: Should the branch merge?",
      details: {
        issue: "DSP-3",
        topic: "notifications.dispatch.issue.DSP-3.>",
        ask: "ask-3",
      },
    })
    expect(envoy.subscribes).toHaveLength(1)
    expect(stderr.lines).toHaveLength(2)
    for (const line of stderr.lines) {
      expect(line).toStartWith(
        "envoy-mcp: cannot reach nats://127.0.0.1:1; messages on subscribed topics will not reach this session — ",
      )
    }
  } finally {
    stderr.restore()
    service.stop(true)
    envoy.server.stop(true)
    process.env = { ...previous }
  }
})

test("envoy_subscribe follows its topics on the broker and envoy_unsubscribe stops them", async () => {
  // given
  const nats = new FakeNatsServer()
  const envoy = fakeEnvoy()
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["ENVOY_NATS_URL"] = nats.url
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.server.port}`
  process.env["DISPATCH_URL"] = "http://127.0.0.1:1"
  process.env["DISPATCH_TOKEN"] = "test-token"
  const stderr = captureStderr()
  try {
    const module = await loadServer("subscribe-follows")

    // when
    const interest = await module.executeEnvoyTool("envoy_subscribe", { topics: [THREAD] })
    await nats.until(() => nats.subscribed.includes(THREAD))
    // the broker delivers a reply twice, then a second reply
    nats.deliver(COMMENT, envelope("github.comment.1"))
    nats.deliver(COMMENT, envelope("github.comment.1"))
    nats.deliver(COMMENT, envelope("github.comment.2"))
    await nats.until(() => nats.published.length >= 2)

    // then: the registry heard the subscribe and each reply reached the agent subject exactly once
    expect(interest).toMatchObject({ session_id: "ses_claude", topics: [THREAD_BASE, THREAD] })
    expect(envoy.subscribes).toEqual([
      { session_id: "ses_claude", dir: process.cwd(), topics: [THREAD_BASE, THREAD] },
    ])
    expect(nats.published).toEqual([
      { subject: INBOX, payload: envelope("github.comment.1") },
      { subject: INBOX, payload: envelope("github.comment.2") },
    ])

    // when
    const removed = await module.executeEnvoyTool("envoy_unsubscribe", { topics: [THREAD] })
    await nats.until(() => nats.unsubscribed.includes(THREAD))
    await nats.until(() => nats.unsubscribed.includes(THREAD_BASE))

    // then
    expect(removed).toEqual({ removed: [THREAD_BASE, THREAD] })
    expect(envoy.unsubscribes).toEqual([
      { session_id: "ses_claude", topics: [THREAD_BASE, THREAD] },
    ])
    expect(stderr.lines).toEqual([])
  } finally {
    stderr.restore()
    await nats.stop()
    envoy.server.stop(true)
    process.env = { ...previous }
  }
})

test("a broker connection nats.js gave up on is replaced by the next envoy_subscribe, which still succeeds", async () => {
  // given: a session following a thread whose connection the broker then refuses for good
  const nats = new FakeNatsServer()
  const envoy = fakeEnvoy()
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["ENVOY_NATS_URL"] = nats.url
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.server.port}`
  process.env["DISPATCH_URL"] = "http://127.0.0.1:1"
  process.env["DISPATCH_TOKEN"] = "test-token"
  const other = "notifications.github.acme-org.example-repo.issue.4.>"
  const otherBase = "notifications.github.acme-org.example-repo.issue.4"
  const stderr = captureStderr()
  try {
    const module = await loadServer("connection-closed")
    await module.executeEnvoyTool("envoy_subscribe", { topics: [THREAD] })
    await nats.until(() => nats.subscribed.includes(THREAD))
    await nats.closeClients()

    // when
    const interest = await module.executeEnvoyTool("envoy_subscribe", { topics: [other] })
    await nats.until(() => nats.connections === 2 && nats.subscribed.includes(other))
    nats.deliver(
      "notifications.github.acme-org.example-repo.issue.4.comment",
      envelope("github.comment.9"),
    )
    nats.deliver(COMMENT, envelope("github.comment.1"))
    await nats.until(() => nats.published.length >= 2)

    // then: the tool succeeded, a fresh connection follows the new topic and the one it inherited
    expect(interest).toMatchObject({ session_id: "ses_claude", topics: [otherBase, other] })
    expect(nats.subscribed).toEqual([
      THREAD_BASE,
      THREAD,
      THREAD_BASE,
      THREAD,
      otherBase,
      other,
    ])
    expect(nats.published).toEqual([
      { subject: INBOX, payload: envelope("github.comment.9") },
      { subject: INBOX, payload: envelope("github.comment.1") },
    ])
    expect(stderr.lines).toEqual([
      "envoy-mcp: the broker connection closed; reopening it for 4 topic(s)\n",
    ])
  } finally {
    stderr.restore()
    await nats.stop()
    envoy.server.stop(true)
    process.env = { ...previous }
  }
})

test("closes a replacement connection when shutdown races a manual subscription", async () => {
  // given: a session whose broker connection was given up on, and a broker that
  // accepts the replacement but leaves its handshake hanging
  const nats = new FakeNatsServer()
  const envoy = fakeEnvoy()
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["ENVOY_NATS_URL"] = nats.url
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.server.port}`
  process.env["DISPATCH_URL"] = "http://127.0.0.1:1"
  process.env["DISPATCH_TOKEN"] = "test-token"
  const other = "notifications.github.acme-org.example-repo.issue.4.>"
  const stderr = captureStderr()
  try {
    const module = await loadServer("shutdown-during-reopen")
    await module.executeEnvoyTool("envoy_subscribe", { topics: [THREAD] })
    await nats.until(() => nats.subscribed.includes(THREAD))
    await nats.closeClients()
    nats.holdNext()
    const reopening = module.executeEnvoyTool("envoy_subscribe", { topics: [other] })
    await nats.until(() => nats.connections === 2)

    // when: stdin ends while that connect is pending, then the handshake completes
    const shutdown = module.shutdownForwarder()
    nats.release()
    await expect(reopening).rejects.toThrow("ENVOY_NATS_URL")
    await shutdown

    // then: no new registry interest is accepted after shutdown, and the broker saw the late connection close.
    await nats.until(() => nats.liveConnections === 0)
    expect(nats.connections).toBe(2)
    expect(stderr.lines).toEqual([
      "envoy-mcp: the broker connection closed; reopening it for 4 topic(s)\n",
    ])
  } finally {
    stderr.restore()
    await nats.stop()
    envoy.server.stop(true)
    process.env = { ...previous }
  }
})
test("rejects malformed wildcard bases before connecting to NATS or recording interest", async () => {
  const envoy = fakeEnvoy()
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  delete process.env["ENVOY_NATS_URL"]
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.server.port}`
  try {
    const module = await loadServer("malformed-wildcard-base")

    for (const topic of ["a..b.>", "a.>"]) {
      await expect(module.executeEnvoyTool("envoy_subscribe", { topics: [topic] })).rejects.toThrow(
        "concrete base",
      )
    }
    expect(envoy.subscribes).toEqual([])
  } finally {
    envoy.server.stop(true)
    process.env = { ...previous }
  }
})


test("rejects a manual envoy_subscribe without ENVOY_NATS_URL before recording the interest", async () => {
  const envoy = fakeEnvoy()
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  delete process.env["ENVOY_SESSION_ID"]
  delete process.env["ENVOY_NATS_URL"]
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.server.port}`
  try {
    const module = await loadServer("manual-subscribe-no-nats")

    await expect(
      module.executeEnvoyTool("envoy_subscribe", { topics: [THREAD] }),
    ).rejects.toThrow("ENVOY_NATS_URL")
    expect(envoy.subscribes).toEqual([])
  } finally {
    envoy.server.stop(true)
    process.env = { ...previous }
  }
})
