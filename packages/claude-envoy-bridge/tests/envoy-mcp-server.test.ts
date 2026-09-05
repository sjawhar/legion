import { expect, test } from "bun:test"
import { mkdtemp, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { z } from "zod"
import type * as EnvoyMcpServer from "../src/envoy-mcp-server"

// The tool list is decided from process.env when the module loads, so each
// gating case needs its own module instance: a distinct query string defeats
// the module cache, which a static import cannot do.
function loadServer(instance: string): Promise<typeof EnvoyMcpServer> {
  return import(`../src/envoy-mcp-server?${instance}`)
}

const ObjectJsonSchema = z.object({
  required: z.array(z.string()),
  properties: z.record(z.string(), z.unknown()),
})

const ServicePost = z.object({
  params: z.object({
    arguments: z.object({
      thread: z.string(),
      origin: z.record(z.string(), z.unknown()),
    }),
  }),
})

test("exposes the shared Envoy contract plus dispatch when dispatch is enabled", async () => {
  // given
  const previous = process.env["DISPATCH_MCP_URL"]
  process.env["DISPATCH_MCP_URL"] = "http://127.0.0.1:1/mcp"
  try {
    const module = await loadServer("dispatch-enabled")
    const definitions = module.envoyMcpToolDefinitions

    // then
    expect(definitions.map((definition) => definition.name)).toEqual([
      ...envoyToolSpecs.map(({ name }) => name),
      "dispatch",
    ])
    const dispatch = definitions.find((definition) => definition.name === "dispatch")
    const schema = ObjectJsonSchema.parse(dispatch?.inputSchema)
    expect(schema.required).toEqual(["context", "question"])
    expect(Object.keys(schema.properties).sort()).toEqual([
      "ask",
      "context",
      "parent",
      "question",
      "repo",
      "subject",
      "thread",
      "urgency",
    ])
  } finally {
    if (previous === undefined) delete process.env["DISPATCH_MCP_URL"]
    else process.env["DISPATCH_MCP_URL"] = previous
  }
})

test("omits dispatch when it is not enabled", async () => {
  // given
  const previousUrl = process.env["DISPATCH_MCP_URL"]
  const previousHome = process.env["HOME"]
  delete process.env["DISPATCH_MCP_URL"]
  process.env["HOME"] = "/nonexistent-home-for-dispatch-gating"
  try {
    const module = await loadServer("dispatch-disabled")

    // then
    expect(module.envoyMcpToolDefinitions.map((definition) => definition.name)).toEqual(
      envoyToolSpecs.map(({ name }) => name),
    )
  } finally {
    if (previousUrl !== undefined) process.env["DISPATCH_MCP_URL"] = previousUrl
    if (previousHome === undefined) delete process.env["HOME"]
    else process.env["HOME"] = previousHome
  }
})

test("dispatch posts one stateless call stamped with the Claude session id and host", async () => {
  // given
  const posts: Array<{ body: unknown; headers: Record<string, string> }> = []
  const service = Bun.serve({
    port: 0,
    fetch: async (request) => {
      posts.push({ body: await request.json(), headers: Object.fromEntries(request.headers) })
      return Response.json({
        jsonrpc: "2.0",
        id: 1,
        result: {
          content: [
            {
              type: "text",
              text: '{"thread":3,"url":"https://github.com/acme-org/example-repo/issues/3"}',
            },
          ],
        },
      })
    },
  })
  const ghDir = await mkdtemp(`${tmpdir()}/fake-gh-`)
  await writeFile(`${ghDir}/gh`, "#!/bin/sh\necho test-token\n", { mode: 0o755 })
  const previous = { ...process.env }
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
  // ENVOY_SESSION_ID outranks CLAUDE_CODE_SESSION_ID; a runner exporting it must not leak in.
  delete process.env["ENVOY_SESSION_ID"]
  process.env["DISPATCH_MCP_URL"] = `http://127.0.0.1:${service.port}/mcp`
  process.env["PATH"] = `${ghDir}:${process.env["PATH"] ?? ""}`
  try {
    const module = await loadServer("dispatch-call")

    // when
    const result = await module.executeEnvoyTool("dispatch", {
      thread: "acme-org/example-repo#3",
      context: "c",
      question: "q",
    })

    // then
    expect(result).toEqual({ thread: 3, url: "https://github.com/acme-org/example-repo/issues/3" })
    expect(posts).toHaveLength(1)
    expect(posts[0]?.headers["authorization"]).toBe("Bearer test-token")
    expect(posts[0]?.headers["mcp-session-id"]).toBeUndefined()
    const { arguments: args } = ServicePost.parse(posts[0]?.body).params
    expect(args.thread).toBe("acme-org/example-repo#3")
    expect(args.origin).toMatchObject({
      host: "claude",
      sessionId: "ses_claude",
      cwd: process.cwd(),
    })
    expect("sessionTitle" in args.origin).toBe(false)
  } finally {
    service.stop(true)
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
    await execute("envoy_unsubscribe", { topics: ["notifications.agent.ses_claude"] })

    // then
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
