import { expect, test } from "bun:test"
import { mkdtemp, rm, stat } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { modSocketPath } from "../hooks/mod-protocol"
import { waitFor } from "./channel-session-harness"
import { startFakeMod } from "./fake-mod"
import { FakeNatsServer } from "./fake-nats-server"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

// What Claude Code sees over MCP, with Dispatch configured: the Envoy tools and nothing else, since
// agents reach Dispatch through the `dispatch` command the plugin's bin/ puts on the Bash tool's
// PATH. A terminal session reaches the model only through its mod (INFRA-1026), so it advertises
// no delivery mode until the mod connects, and Aside, BTW and Send once one has.
test("over MCP the server offers only the Envoy tools, refuses dispatch_ask, points at the dispatch command, and advertises nothing until its mod connects", async () => {
  const broker = new FakeNatsServer()
  const dispatchRequests: string[] = []
  const dispatch = Bun.serve({
    port: 0,
    fetch: (request) => {
      dispatchRequests.push(`${request.method} ${new URL(request.url).pathname}`)
      return Response.json({ error: "unexpected" }, { status: 500 })
    },
  })
  const advertised: unknown[] = []
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as {
          session_id: string
          topics: string[]
          capabilities?: unknown
        }
        advertised.push(body.capabilities)
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
        })
      }
      if (request.method === "DELETE") return new Response(null, { status: 200 })
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  const stateDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-mcp-"))
  const client = new Client({ name: "test", version: "0" })
  const transport = new StdioClientTransport({
    command: "bun",
    args: [entry],
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: broker.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      CLAUDE_CODE_SESSION_ID: "ses_mcp",
      CLAUDE_PLUGIN_DATA: stateDirectory,
      // The socket the server binds for its mod, and the one the fake mod below connects to,
      // are both resolved from this base (`resolveModSocketPath`).
      XDG_RUNTIME_DIR: stateDirectory,
      DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
      DISPATCH_TOKEN: "test-token",
    },
    stderr: "pipe",
  })

  try {
    await client.connect(transport)
    // The server answers tool calls only once its channel session has started.
    await waitFor(
      () =>
        client.callTool({ name: "envoy_whoami", arguments: {} }).then(
          () => true,
          (error: unknown) => {
            if (String(error).includes("still starting")) return false
            throw error
          },
        ),
      "the channel session to start",
    )

    const { tools } = await client.listTools()
    expect(tools.map(({ name }) => name)).toEqual(envoyToolSpecs.map(({ name }) => name))
    // Nothing until the mod connects: without one no message reaches the model at all.
    expect(advertised.length).toBeGreaterThan(0)
    for (const modes of advertised) expect(modes).toEqual([])

    const socketPath = modSocketPath("ses_mcp", { XDG_RUNTIME_DIR: stateDirectory })
    if (socketPath === undefined) throw new Error("the session names no mod socket")
    const mod = await startFakeMod(socketPath, { sessionId: "ses_mcp" })
    try {
      // The mod's hello makes the session register again, with what it can take now.
      await waitFor(
        async () => (advertised.at(-1) as string[] | undefined)?.length === 3,
        "the session to advertise its delivery modes",
        () => JSON.stringify(advertised),
      )
      expect(advertised.at(-1)).toEqual(["aside", "btw", "steer"])
    } finally {
      await mod.stop()
    }
    const instructions = client.getInstructions() ?? ""
    expect(instructions).toContain("`dispatch` command")
    expect(instructions).not.toContain("Dispatch tools")

    await expect(
      client.callTool({ name: "dispatch_ask", arguments: { issue: "DSP-3", question: "Ship?" } }),
    ).rejects.toThrow("Unsupported Envoy tool: dispatch_ask")
    expect(dispatchRequests).toEqual([])
  } finally {
    await client.close().catch(() => undefined)
    dispatch.stop(true)
    listener.stop(true)
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
}, 20_000)

// A mod says hello once per Claude Code process, so a server Claude Code starts again inside
// that process hears none, and neither does a listener added after the hello landed: the session
// then advertised nothing until the next heartbeat, up to 90 s later, and Dispatch showed it as
// taking no messages (INFRA-1026 review). A poll says a mod is there just as well.
test("the session advertises its delivery modes for a mod that polls and never says hello", async () => {
  const broker = new FakeNatsServer()
  const advertised: unknown[] = []
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as {
          session_id: string
          topics: string[]
          capabilities?: unknown
        }
        advertised.push(body.capabilities)
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
        })
      }
      if (request.method === "DELETE") return new Response(null, { status: 200 })
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  const stateDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-quiet-"))
  const client = new Client({ name: "test", version: "0" })
  const transport = new StdioClientTransport({
    command: "bun",
    args: [entry],
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: broker.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      CLAUDE_CODE_SESSION_ID: "ses_quiet",
      CLAUDE_PLUGIN_DATA: stateDirectory,
      XDG_RUNTIME_DIR: stateDirectory,
    },
    stderr: "pipe",
  })

  try {
    await client.connect(transport)
    await waitFor(
      () =>
        client.callTool({ name: "envoy_whoami", arguments: {} }).then(
          () => true,
          (error: unknown) => {
            if (String(error).includes("still starting")) return false
            throw error
          },
        ),
      "the channel session to start",
    )
    expect(advertised.at(-1)).toEqual([])

    const socketPath = modSocketPath("ses_quiet", { XDG_RUNTIME_DIR: stateDirectory })
    if (socketPath === undefined) throw new Error("the session names no mod socket")
    await waitFor(
      () =>
        stat(socketPath).then(
          () => true,
          () => false,
        ),
      "the server to bind its mod socket",
    )
    const mod = await startFakeMod(socketPath, { sessionId: "ses_quiet", hello: false })
    try {
      await waitFor(
        async () => (advertised.at(-1) as string[] | undefined)?.length === 3,
        "the session to advertise its delivery modes",
        () => JSON.stringify(advertised),
      )
      expect(advertised.at(-1)).toEqual(["aside", "btw", "steer"])
    } finally {
      await mod.stop()
    }
  } finally {
    await client.close().catch(() => undefined)
    listener.stop(true)
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
}, 20_000)
