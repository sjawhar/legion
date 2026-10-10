import { expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { waitFor } from "./channel-session-harness"
import { FakeNatsServer } from "./fake-nats-server"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

// What Claude Code sees over MCP, with Dispatch configured: the Envoy tools and nothing else, since
// agents reach Dispatch through the `dispatch` command the plugin's bin/ puts on the Bash tool's PATH.
test("over MCP the server offers only the Envoy tools, refuses dispatch_ask, and points at the dispatch command", async () => {
  const broker = new FakeNatsServer()
  const dispatchRequests: string[] = []
  const dispatch = Bun.serve({
    port: 0,
    fetch: (request) => {
      dispatchRequests.push(`${request.method} ${new URL(request.url).pathname}`)
      return Response.json({ error: "unexpected" }, { status: 500 })
    },
  })
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as { session_id: string; topics: string[] }
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
