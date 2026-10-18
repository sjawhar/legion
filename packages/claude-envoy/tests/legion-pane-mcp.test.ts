import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { createServer, type Socket } from "node:net"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { legion } from "../package.json" with { type: "json" }
import { waitFor } from "./channel-session-harness"
import { FakeNatsServer } from "./fake-nats-server"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")
const SESSION = "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f"

async function daemonFixture(name: string): Promise<Record<string, unknown>> {
  const file = resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name)
  return JSON.parse(await readFile(file, "utf8"))
}

// A Legion pane's MCP server as Claude Code runs it under `legion claude-bridge`: a boot token,
// the daemon's address, its own CLAUDE_CONFIG_DIR and the bridge's inbox socket on its
// environment. It registers once the transcript exists, claims the claim token's role, says
// ready, offers the `legion` tool beside the Envoy tools, advertises the Send (steer) the bridge
// serves, and hands an Envoy delivery to the inbox.
test("a Legion pane's server registers on its transcript, claims its role, serves the legion tool and delivers through the inbox", async () => {
  const root = await mkdtemp(join(tmpdir(), "claude-envoy-legion-pane-"))
  const configDir = join(root, "claude")
  const registered = await daemonFixture("register.json")
  const claimToken = String(registered["claimToken"])
  const daemonCalls: { path: string; body: unknown }[] = []
  const daemon = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const path = new URL(request.url).pathname
      daemonCalls.push({
        path,
        body: request.method === "GET" ? undefined : await request.json(),
      })
      if (path === "/legion/v1/claims/register") return Response.json(registered)
      if (path === "/legion/v1/claims/ready") return new Response(null, { status: 204 })
      if (path === "/legion/v1/state") return Response.json(await daemonFixture("state.json"))
      return Response.json({ error: "unexpected" }, { status: 500 })
    },
  })
  const roles: unknown[] = []
  const advertised: unknown[] = []
  const holders = new Map<string, string>()
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      const interest = { session_id: SESSION, machine_id: "test", dir: root, topics: [] }
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const registration = (await request.json()) as { readonly capabilities?: unknown }
        advertised.push(registration.capabilities)
        return Response.json(interest)
      }
      if (request.method === "POST" && url.pathname === "/v1/roles/set") {
        const claim = (await request.json()) as { session_id: string; role: string }
        roles.push(claim)
        holders.set(claim.role, claim.session_id)
        return Response.json(interest)
      }
      const role = /^\/v1\/roles\/(.+)$/.exec(url.pathname)?.[1]
      const holder = role === undefined ? undefined : holders.get(decodeURIComponent(role))
      if (request.method === "GET" && role !== undefined && holder !== undefined) {
        return Response.json({ role: decodeURIComponent(role), holder, last_seen: 1 })
      }
      if (request.method === "DELETE") return new Response(null, { status: 200 })
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  const broker = new FakeNatsServer()
  const inboxLines: { id: string; content: string }[] = []
  const inboxPath = join(root, "inbox.sock")
  const inbox = createServer((socket: Socket) => {
    let buffered = ""
    socket.setEncoding("utf8")
    socket.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const line = JSON.parse(buffered.slice(0, end)) as { id: string; content: string }
        buffered = buffered.slice(end + 1)
        inboxLines.push(line)
        socket.write(`${JSON.stringify({ id: line.id, ok: true })}\n`)
      }
    })
  })
  await new Promise<void>((listening) => inbox.listen(inboxPath, listening))
  await mkdir(configDir, { recursive: true })
  const tokenFile = join(root, "boot-token")
  await writeFile(tokenFile, "boot-token-from-the-pane\n")

  const client = new Client({ name: "test", version: "0" })
  const transport = new StdioClientTransport({
    command: "bun",
    args: [entry],
    cwd: root,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: root,
      ENVOY_NATS_URL: broker.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      CLAUDE_CODE_SESSION_ID: SESSION,
      CLAUDE_CONFIG_DIR: configDir,
      CLAUDE_PLUGIN_DATA: join(root, "plugin-data"),
      LEGION_BOOT_TOKEN_FILE: tokenFile,
      LEGION_DAEMON_URL: `http://127.0.0.1:${daemon.port}`,
      LEGION_CLAUDE_INBOX: inboxPath,
    },
    stderr: "pipe",
  })

  try {
    await client.connect(transport)
    const { tools } = await client.listTools()
    expect(tools.map(({ name }) => name)).toEqual([
      ...envoyToolSpecs.map(({ name }) => name),
      "legion",
    ])
    await expect(
      client.callTool({ name: "legion", arguments: { op: "read_record", issue: "LEGION-208" } }),
    ).rejects.toThrow("this session has no Legion claim yet")

    // The bridge's /rename seed writes the transcript; only then does the pane register.
    const transcript = join(configDir, "projects", "-work", `${SESSION}.jsonl`)
    await mkdir(join(configDir, "projects", "-work"), { recursive: true })
    await writeFile(transcript, "{}\n")
    await waitFor(
      async () => daemonCalls.some(({ path }) => path === "/legion/v1/claims/ready"),
      "the pane to say it is ready",
    )
    expect(daemonCalls).toEqual([
      {
        path: "/legion/v1/claims/register",
        body: {
          bootToken: "boot-token-from-the-pane",
          sessionId: SESSION,
          ompSessionFile: transcript,
          agentId: SESSION,
          pluginContract: legion.daemonApiVersion,
        },
      },
      {
        path: "/legion/v1/claims/ready",
        body: {
          claimToken,
          sessionId: SESSION,
          secret: registered["secret"],
          generation: registered["generation"],
        },
      },
    ])
    expect(roles).toEqual([{ session_id: SESSION, role: claimToken, soft: true }])
    // Dispatch refuses a mode the session does not advertise, and sends a Send by default: every
    // registration of the session carries both modes the bridge serves.
    expect(advertised.length).toBeGreaterThan(0)
    for (const modes of advertised) expect(modes).toEqual(["aside", "steer"])

    const record = await client.callTool({
      name: "legion",
      arguments: { op: "read_record", issue: "LEGION-208" },
    })
    const state = (await daemonFixture("state.json")) as { issues: Record<string, unknown> }
    expect(record.content).toEqual([
      { type: "text", text: JSON.stringify({ record: state.issues["LEGION-208"] }) },
    ])

    broker.deliver(
      `notifications.agent.${SESSION}`,
      JSON.stringify({
        event_id: "evt-legion-1",
        dedupe_key: "legion-1",
        source: "agent",
        source_session: "ses_architect",
        topic: `notifications.agent.${SESSION}`,
        issued_at: 1_760_000_000_000,
        payload_summary: "Start implementing",
      }),
    )
    await waitFor(async () => inboxLines.length > 0, "the delivery to reach the inbox")
    expect(inboxLines[0]?.id).toBe("evt-legion-1")
    expect(inboxLines[0]?.content).toContain("Start implementing")
  } finally {
    await client.close().catch(() => undefined)
    inbox.close()
    daemon.stop(true)
    listener.stop(true)
    await broker.stop()
    await rm(root, { recursive: true, force: true })
  }
}, 20_000)
