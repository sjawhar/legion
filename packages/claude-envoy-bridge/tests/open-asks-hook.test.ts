import { expect, test } from "bun:test"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

const hookEntry = resolve(import.meta.dir, "..", "hooks", "open-asks-hook.ts")

async function runHook(
  input: unknown,
  env: Record<string, string>,
): Promise<{ readonly exitCode: number; readonly stdout: string; readonly stderr: string }> {
  const run = Bun.spawn(["bun", hookEntry], {
    env: { PATH: process.env["PATH"] ?? "", ...env },
    stdin: new TextEncoder().encode(JSON.stringify(input)),
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exitCode, stdout, stderr] = await Promise.all([
    run.exited,
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
  ])
  return { exitCode, stdout, stderr }
}

const hookInput = {
  session_id: "s-1",
  source: "startup",
  cwd: "/tmp",
  hook_event_name: "SessionStart",
}

test("summarises the session's open asks into context and records the session id for its Claude process", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-"))
  const requests: string[] = []
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      requests.push(
        `${request.headers.get("authorization")} ${new URL(request.url).pathname}?${new URL(request.url).searchParams}`,
      )
      return Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 1,
        waiting_on_human: 1,
        waiting_on_agent: 0,
        asks: [
          {
            id: "ask-9",
            ref: "/issues/LEGION-9#ask-9",
            question: "Ship the bundle?",
            kind: "decision",
            urgency: "med",
            created_at: "2026-09-14T00:00:00Z",
            age_seconds: 90,
            priority: 1,
            owner: { issue: { key: "LEGION-9", title: "Bundle" } },
            human_replied: false,
            last_reply: null,
            waiting_on: "human",
          },
        ],
      })
    },
  })
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: "hook-token",
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith(
      "Dispatch authored-ask summary:\n1 unanswered ask you authored",
    )
    expect(result.stdout).toContain("Ship the bundle?")
    expect(requests).toEqual(["Bearer hook-token /api/v1/asks/open?author_session=s-1"])
    expect(
      await readFile(
        join(scratch, "plugin-data", "sessions", String(process.pid), "session-id"),
        "utf8",
      ),
    ).toBe("s-1\n")
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

test("reports an unreachable Dispatch as unavailable, still exits 0, and still records the session id", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-down-"))
  const closed = Bun.serve({ port: 0, fetch: () => new Response() })
  const port = closed.port
  closed.stop(true)
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      DISPATCH_URL: `http://127.0.0.1:${port}`,
      DISPATCH_TOKEN: "hook-token",
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith("Dispatch authored-ask summary unavailable: ")
    expect(
      await readFile(
        join(scratch, "plugin-data", "sessions", String(process.pid), "session-id"),
        "utf8",
      ),
    ).toBe("s-1\n")
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("adds nothing to context when Dispatch is not configured", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-off-"))
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toBe("")
    expect(
      await readFile(
        join(scratch, "plugin-data", "sessions", String(process.pid), "session-id"),
        "utf8",
      ),
    ).toBe("s-1\n")
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})
