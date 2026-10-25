import { expect, test } from "bun:test"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first"

const packageRoot = resolve(import.meta.dir, "..")
const hookEntry = join(packageRoot, "hooks", "session-hook.ts")
const sessionStart = {
  session_id: "s-1",
  source: "startup",
  cwd: "/tmp",
  hook_event_name: "SessionStart",
}

async function runHook(
  env: Record<string, string>,
  input: Record<string, unknown> = sessionStart,
  mode = "dispatch-first",
): Promise<{ readonly exitCode: number; readonly stdout: string; readonly stderr: string }> {
  const run = Bun.spawn(["bun", hookEntry, mode], {
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

const withDispatch = { DISPATCH_URL: "http://127.0.0.1:9", DISPATCH_TOKEN: "hook-token" }

test("with Dispatch configured, puts the plugin's dispatch-first skill into the session as SessionStart context", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-dispatch-first-"))
  try {
    const result = await runHook({ HOME: home, CLAUDE_PLUGIN_ROOT: packageRoot, ...withDispatch })

    expect(result.exitCode).toBe(0)
    const output = JSON.parse(result.stdout)
    expect(output.hookSpecificOutput.hookEventName).toBe("SessionStart")
    const context: string = output.hookSpecificOutput.additionalContext
    expect(context.startsWith(DISPATCH_FIRST_MARKER)).toBe(true)
    // The skill arrives whole from the plugin's own skills/ link, frontmatter dropped.
    const skill = await readFile(join(packageRoot, "skills", "dispatch-first", "SKILL.md"), "utf8")
    expect(context).toContain(skill.slice(skill.indexOf("# Dispatch first")).trim())
    expect(context).not.toContain("name: dispatch-first")
    // Claude Code keeps an additionalContext string whole only up to 10,000 characters.
    expect(context.length).toBeLessThan(10_000)
  } finally {
    await rm(home, { recursive: true, force: true })
  }
})

test("a subagent gets the skill under its own SubagentStart event", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-dispatch-first-subagent-"))
  try {
    const result = await runHook(
      { HOME: home, CLAUDE_PLUGIN_ROOT: packageRoot, ...withDispatch },
      {
        session_id: "s-1",
        cwd: "/tmp",
        hook_event_name: "SubagentStart",
        agent_id: "agent-1",
        agent_type: "general-purpose",
      },
    )

    expect(result.exitCode).toBe(0)
    const output = JSON.parse(result.stdout)
    // Claude Code applies additionalContext only under the event that ran the hook.
    expect(output.hookSpecificOutput.hookEventName).toBe("SubagentStart")
    expect(output.hookSpecificOutput.additionalContext.startsWith(DISPATCH_FIRST_MARKER)).toBe(true)
  } finally {
    await rm(home, { recursive: true, force: true })
  }
})

test("adds nothing when Dispatch is not configured", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-dispatch-first-off-"))
  try {
    const result = await runHook({ HOME: home, CLAUDE_PLUGIN_ROOT: packageRoot })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toBe("")
  } finally {
    await rm(home, { recursive: true, force: true })
  }
})

test("a plugin installed without the skill fails the hook naming the missing file", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-dispatch-first-missing-"))
  try {
    const result = await runHook({ HOME: home, CLAUDE_PLUGIN_ROOT: home, ...withDispatch })

    expect(result.exitCode).not.toBe(0)
    expect(result.stdout).toBe("")
    expect(result.stderr).toContain(join(home, "skills", "dispatch-first", "SKILL.md"))
  } finally {
    await rm(home, { recursive: true, force: true })
  }
})

test("a mode hooks.json does not name fails the hook naming the modes", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-session-hook-mode-"))
  try {
    const result = await runHook(
      { HOME: home, CLAUDE_PLUGIN_ROOT: packageRoot, ...withDispatch },
      sessionStart,
      "dispatch_first",
    )

    expect(result.exitCode).not.toBe(0)
    expect(result.stdout).toBe("")
    expect(result.stderr).toContain("open-asks or dispatch-first")
  } finally {
    await rm(home, { recursive: true, force: true })
  }
})
