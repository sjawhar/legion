import { expect, test } from "bun:test"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"

test("exposes the shared Envoy contract through MCP", async () => {
  // given
  const module = await import("../src/envoy-mcp-server")
  const definitions = Reflect.get(module, "envoyMcpToolDefinitions")

  // when
  const exposedContract =
    Array.isArray(definitions) &&
    definitions.every((definition) => typeof definition === "object" && definition !== null)
      ? definitions.map((definition) => ({
          name: Reflect.get(definition, "name"),
          description: Reflect.get(definition, "description"),
        }))
      : undefined

  // then
  expect(exposedContract).toEqual(
    envoyToolSpecs.map(({ name, description }) => ({ name, description })),
  )
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
  const previousEnvoyUrl = process.env["ENVOY_URL"]
  process.env["CLAUDE_CODE_SESSION_ID"] = "ses_claude"
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
    if (previousEnvoyUrl === undefined) delete process.env["ENVOY_URL"]
    else process.env["ENVOY_URL"] = previousEnvoyUrl
  }
})
