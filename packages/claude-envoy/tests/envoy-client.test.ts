import { afterAll, expect, test } from "bun:test"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { sendEnvoyMessage } from "../src/envoy-client"

const requests: { path: string; authorization: string | null; body: string }[] = []
const server = Bun.serve({
  port: 0,
  fetch: async (request) => {
    requests.push({
      path: new URL(request.url).pathname,
      authorization: request.headers.get("authorization"),
      body: await request.text(),
    })
    return new Response(null, { status: 202 })
  },
})
const scratch = mkdtempSync(join(tmpdir(), "claude-envoy-send-"))
afterAll(() => {
  server.stop(true)
  rmSync(scratch, { recursive: true, force: true })
})

test("sends a direct message through Envoy's HTTP API, with no bearer for a listener that asks for none", async () => {
  requests.length = 0
  await sendEnvoyMessage({
    baseUrl: `http://127.0.0.1:${server.port}`,
    token: undefined,
    targetSession: "ses_receiver",
    message: "adapter send proof",
  })
  expect(requests).toEqual([
    {
      path: "/v1/messages/send",
      authorization: null,
      body: JSON.stringify({ target_session: "ses_receiver", message: "adapter send proof" }),
    },
  ])
})

test("envoy-send sends to the listener and with the bearer its envoy.json names", async () => {
  requests.length = 0
  const file = join(scratch, "envoy.json")
  writeFileSync(
    file,
    JSON.stringify({
      envoy: { enabled: true, url: `http://127.0.0.1:${server.port}`, token: "listener-bearer" },
    }),
  )
  const send = Bun.spawn(
    ["bun", resolve(import.meta.dir, "../bin/envoy-send.ts"), "ses_receiver", "file", "send"],
    {
      cwd: scratch,
      env: { PATH: process.env["PATH"] ?? "", HOME: scratch, ENVOY_CONFIG_FILE: file },
      stdout: "pipe",
      stderr: "pipe",
    },
  )
  expect(await send.exited).toBe(0)
  expect(await new Response(send.stdout).text()).toBe("Sent Envoy message to ses_receiver.\n")
  expect(requests).toEqual([
    {
      path: "/v1/messages/send",
      authorization: "Bearer listener-bearer",
      body: JSON.stringify({ target_session: "ses_receiver", message: "file send" }),
    },
  ])
})

test("envoy-send with no Envoy configured refuses, saying why, and sends nothing", async () => {
  requests.length = 0
  const missing = join(scratch, "absent", "envoy.json")
  const send = Bun.spawn(
    ["bun", resolve(import.meta.dir, "../bin/envoy-send.ts"), "ses_receiver", "nothing"],
    {
      cwd: scratch,
      env: { PATH: process.env["PATH"] ?? "", HOME: scratch, ENVOY_CONFIG_FILE: missing },
      stdout: "pipe",
      stderr: "pipe",
    },
  )
  expect(await send.exited).toBe(1)
  expect(await new Response(send.stderr).text()).toBe(
    `Claude Envoy send failed: Envoy is off: no envoy.json at ${missing}: Envoy is off\n`,
  )
  expect(requests).toEqual([])
})
