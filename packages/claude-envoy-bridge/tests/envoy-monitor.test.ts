import { expect, test } from "bun:test"
import { createServer } from "node:net"
import { agentSubject } from "@legion/contracts"
import { decode } from "@toon-format/toon"
import { envoyInboundMessage, runEnvoyMonitor } from "../src/envoy-monitor"
import { FakeNatsServer } from "./fake-nats-server"
import { tmpdir } from "node:os"
import { join } from "node:path"

test("encodes authenticated native user-message frames for Claude Code", async () => {
  // given
  const module = await import("../src/envoy-monitor")
  const candidate = Reflect.get(module, "nativeMessageFrames")

  // when
  const output =
    typeof candidate === "function"
      ? candidate({ token: "socket-token", message: "Envoy native delivery" })
      : undefined

  // then
  expect(output).toEqual([
    '{"type":"auth","token":"socket-token"}',
    '{"type":"user","message":{"role":"user","content":"Envoy native delivery"}}',
  ])
})

test("rejects native delivery when Claude Code does not provide messaging credentials", async () => {
  // given
  const module = await import("../src/envoy-monitor")
  const candidate = Reflect.get(module, "nativeMessagingCredentials")

  // when
  const invoke = (): unknown => (typeof candidate === "function" ? candidate({}) : undefined)

  // then
  expect(invoke).toThrow("Claude Code native messaging is unavailable")
})

test("writes native delivery frames to Claude Code's Unix socket", async () => {
  // given
  const socketPath = join(tmpdir(), `envoy-claude-${crypto.randomUUID()}.sock`)
  const received = new Promise<string>((resolve, reject) => {
    const server = createServer((socket) => {
      let frames = ""
      socket.setEncoding("utf8")
      socket.on("data", (chunk: string) => {
        frames += chunk
      })
      socket.on("end", () => {
        server.close((error) => {
          if (error) {
            reject(error)
            return
          }
          resolve(frames)
        })
      })
    })
    server.listen(socketPath)
  })
  const { sendNativeMessage } = await import("../src/envoy-monitor")

  // when
  await sendNativeMessage({ socketPath, token: "socket-token" }, "Envoy native delivery")

  // then
  expect(await received).toBe(
    '{"type":"auth","token":"socket-token"}\n{"type":"user","message":{"role":"user","content":"Envoy native delivery"}}\n',
  )
})

test("renders an agent envelope with its sender and reply instruction", async () => {
  const rendered = envoyInboundMessage(
    JSON.stringify({
      source: "agent",
      source_session: "ses_sender",
      topic: "notifications.agent.ses_reader",
      payload_summary: "Please report the deployment result.",
    }),
  )
  expect(decode(rendered ?? "")).toEqual({
    envoy: {
      from: "ses_sender",
      at: "unknown",
      id: "unknown",
      reply_with: 'envoy_send(session_id="ses_sender", message="...")',
      summary: "Please report the deployment result.",
    },
  })
})

test("renders a human envelope without a reply instruction", async () => {
  const rendered = envoyInboundMessage(
    JSON.stringify({
      source: "human",
      topic: "notifications.agent.ses_reader",
      payload_summary: "Please report the deployment result.",
    }),
  )
  expect(decode(rendered ?? "")).toEqual({
    envoy: {
      from: "human",
      at: "unknown",
      id: "unknown",
      summary: "Please report the deployment result.",
    },
  })
})

test("renders a GitHub envelope without a reply instruction", async () => {
  const rendered = envoyInboundMessage(
    JSON.stringify({
      source: "github",
      topic: "notifications.github.example-org.example-repo.issue.42.comment",
      payload_summary: "The human answered.",
    }),
  )
  expect(decode(rendered ?? "")).toEqual({
    envoy: {
      from: "github",
      at: "unknown",
      id: "unknown",
      summary: "The human answered.",
    },
  })
})

test("skips a GitHub dispatch echo for the originating session", async () => {
  const envelope = JSON.stringify({
    source: "github",
    topic: "notifications.agent.ses_origin",
    payload_summary: "Please report the deployment result.",
    payload: JSON.stringify({ dispatch_session: "ses_origin" }),
  })

  expect(envoyInboundMessage(envelope, "ses_origin")).toBeUndefined()
})

test("registers, heartbeats, and deregisters the monitor session", async () => {
  const nats = new FakeNatsServer()
  const registrations: Record<string, unknown>[] = []
  const deletions: string[] = []
  const firstRegistration = Promise.withResolvers<void>()
  const secondRegistration = Promise.withResolvers<void>()
  const envoy = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      if (url.pathname === "/v1/interests/subscribe") {
        // The direct subject must be subscribed on the wire before the
        // session is advertised; a sender may deliver the moment this returns.
        expect(nats.subscribed).toContain(agentSubject("ses_monitor"))
        const registration = (await request.json()) as {
          readonly session_id: string
          readonly dir: string
          readonly topics: string[]
        }
        registrations.push(registration)
        if (registrations.length === 1) firstRegistration.resolve()
        if (registrations.length === 2) secondRegistration.resolve()
        return Response.json({
          session_id: registration.session_id,
          machine_id: "example-host",
          dir: registration.dir,
          topics: registration.topics,
        })
      }
      if (url.pathname === "/v1/sessions/ses_monitor") {
        deletions.push(url.pathname)
        return new Response(null, { status: 200 })
      }
      return new Response(null, { status: 404 })
    },
  })
  const previous = { ...process.env }
  let monitor: Promise<void> | undefined
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_monitor"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["ENVOY_NATS_URL"] = nats.url
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.port}`
  process.env["ENVOY_HEARTBEAT_MS"] = "25"
  process.env["CLAUDE_CODE_MESSAGING_SOCKET"] = "/tmp/envoy-monitor-test.sock"
  process.env["CLAUDE_CODE_MESSAGING_TOKEN"] = "test-token"
  try {
    monitor = runEnvoyMonitor()
    // Subscription first, then registration: the handler above asserts the
    // order on every registration; here we wait for the registration itself.
    await firstRegistration.promise

    expect(registrations).toHaveLength(1)
    expect(registrations[0]).toMatchObject({
      session_id: "ses_monitor",
      port: 0,
      self_subscribed: true,
      topics: [agentSubject("ses_monitor")],
    })

    await secondRegistration.promise
    expect(registrations).toHaveLength(2)

    process.emit("SIGTERM")
    await monitor
    expect(deletions).toEqual(["/v1/sessions/ses_monitor"])
  } finally {
    process.emit("SIGTERM")
    await monitor?.catch(() => undefined)
    envoy.stop(true)
    await nats.stop()
    for (const key of Object.keys(process.env)) {
      if (!(key in previous)) delete process.env[key]
    }
    Object.assign(process.env, previous)
  }
})

test("reports a registry outage once and clears it on the next successful heartbeat", async () => {
  const nats = new FakeNatsServer()
  let attempts = 0
  const fourthAttempt = Promise.withResolvers<void>()
  const envoy = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      if (url.pathname === "/v1/interests/subscribe") {
        attempts += 1
        if (attempts === 4) fourthAttempt.resolve()
        // Attempts 1-3 fail (one outage); attempt 4 succeeds.
        if (attempts < 4) return new Response("registry down", { status: 503 })
        const registration = (await request.json()) as { readonly session_id: string }
        return Response.json({
          session_id: registration.session_id,
          machine_id: "example-host",
          dir: "/tmp",
          topics: [],
        })
      }
      return new Response(null, { status: 200 })
    },
  })
  const stderr: string[] = []
  const write = process.stderr.write.bind(process.stderr)
  process.stderr.write = ((chunk: string | Uint8Array) => {
    stderr.push(String(chunk))
    return true
  }) as typeof process.stderr.write
  const previous = { ...process.env }
  let monitor: Promise<void> | undefined
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_outage"
  delete process.env["ENVOY_SESSION_ID"]
  process.env["ENVOY_NATS_URL"] = nats.url
  process.env["ENVOY_URL"] = `http://127.0.0.1:${envoy.port}`
  process.env["ENVOY_HEARTBEAT_MS"] = "25"
  process.env["CLAUDE_CODE_MESSAGING_SOCKET"] = "/tmp/envoy-monitor-test.sock"
  process.env["CLAUDE_CODE_MESSAGING_TOKEN"] = "test-token"
  try {
    monitor = runEnvoyMonitor()
    await fourthAttempt.promise
    const failures = stderr.filter((line) => line.includes("registry heartbeat failed"))
    expect(failures).toHaveLength(1)
    process.emit("SIGTERM")
    await monitor
  } finally {
    process.stderr.write = write
    process.emit("SIGTERM")
    await monitor?.catch(() => undefined)
    envoy.stop(true)
    await nats.stop()
    for (const key of Object.keys(process.env)) {
      if (!(key in previous)) delete process.env[key]
    }
    Object.assign(process.env, previous)
  }
})

test("uses the shared renderer output for a direct agent delivery", async () => {
  const reader = "01a01111-2222-7333-4444-555555555555"
  const sender = "01a00000-0000-7000-0000-000000000001"
  const rendered = envoyInboundMessage(
    JSON.stringify({
      event_id: "agent-message-1",
      source: "agent",
      source_session: sender,
      topic: `notifications.agent.${reader}`,
      issued_at: Date.parse("2026-09-07T04:41:12Z"),
      payload_summary: "Please review this.",
      sender: { session_id: sender, title: "Reviewer", roles: ["legion-reviewer"] },
    }),
    reader,
    `notifications.agent.${reader}`,
  )

  expect(rendered).toBe(
    [
      "envoy:",
      "  to: you (01a0…)",
      `  from: ${sender} (Reviewer)`,
      '  at: "2026-09-07T04:41:12Z"',
      "  id: agent-message-1",
      `  reply_with: "envoy_send(session_id=\\"${sender}\\", message=\\"...\\")"`,
      '  reply_role: "envoy_publish(topic=\\"notifications.role.legion-reviewer\\", message=\\"...\\")"',
      "  summary: Please review this.",
    ].join("\n"),
  )
})
