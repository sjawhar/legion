import { expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { FakeNatsServer } from "./fake-nats-server"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

test("a hangup from the Claude process group deregisters the session before the server exits", async () => {
  // tmux kill-session (and a closing terminal) hang up the whole process group;
  // the channel server shares it with `claude`, so SIGHUP arrives directly and
  // must still lead to `DELETE /v1/sessions/<id>` rather than a silent death
  // that leaves the session live in the registry until its TTL.
  const broker = new FakeNatsServer()
  const calls: string[] = []
  const registered = Promise.withResolvers<void>()
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      calls.push(`${request.method} ${url.pathname}`)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as { session_id: string; topics: string[] }
        registered.resolve()
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
  const stateDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-lifecycle-"))
  const server = Bun.spawn(["bun", entry], {
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: broker.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      CLAUDE_CODE_SESSION_ID: "ses_hangup",
      CLAUDE_PLUGIN_DATA: stateDirectory,
    },
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  })

  try {
    await registered.promise
    server.kill("SIGHUP")
    const exitCode = await server.exited

    expect(exitCode).toBe(0)
    expect(calls).toContain("DELETE /v1/sessions/ses_hangup")
  } finally {
    server.kill("SIGKILL")
    listener.stop(true)
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
}, 20_000)
