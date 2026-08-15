import { expect, test } from "bun:test"
import { createServer } from "node:net"
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
  const invoke = (): unknown =>
    typeof candidate === "function" ? candidate({}) : undefined

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

test("delivers an Envoy envelope's summary rather than Monitor-style telemetry", async () => {
  // given
  const module = await import("../src/envoy-monitor")
  const candidate = Reflect.get(module, "envoyInboundMessage")
  const envelope = JSON.stringify({
    source_session: "ses_sender",
    payload_summary: "Please report the deployment result.",
  })

  // when
  const output = typeof candidate === "function" ? candidate(envelope) : undefined

  // then
  expect(output).toBe("Please report the deployment result.")
})
