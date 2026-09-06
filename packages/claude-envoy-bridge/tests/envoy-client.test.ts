import { expect, test } from "bun:test"

test("sends a direct message through Envoy's HTTP API", async () => {
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
  const module = await import("../src/envoy-client")
  const candidate = Reflect.get(module, "sendEnvoyMessage")

  try {
    // when
    if (typeof candidate === "function") {
      await candidate({
        baseUrl: `http://127.0.0.1:${server.port}`,
        targetSession: "ses_receiver",
        message: "adapter send proof",
      })
    }

    // then
    expect(receivedPath).toBe("/v1/messages/send")
    expect(JSON.parse(receivedBody)).toEqual({
      target_session: "ses_receiver",
      message: "adapter send proof",
    })
  } finally {
    server.stop(true)
  }
})
