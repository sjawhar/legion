import { expect, test } from "bun:test"
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

const hookEntry = resolve(import.meta.dir, "..", "hooks", "session-hook.ts")

/** Runs the session-title hook as Claude Code does, with `env` as its whole environment and `raw`
 * on its stdin, so a payload that is not JSON can be sent as it stands. */
async function runHook(
  raw: string,
  env: Record<string, string>,
): Promise<{ readonly exitCode: number; readonly stdout: string; readonly stderr: string }> {
  const run = Bun.spawn(["bun", hookEntry, "session-title"], {
    env: { PATH: process.env["PATH"] ?? "", ...env },
    stdin: new TextEncoder().encode(raw),
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

const payload = (title?: string) =>
  JSON.stringify({
    session_id: "s-1",
    hook_event_name: "UserPromptSubmit",
    ...(title === undefined ? {} : { session_title: title }),
  })

test("records the name a UserPromptSubmit payload carries, and removes it when it carries none", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-title-hook-"))
  try {
    const named = await runHook(payload("review the listener"), { CLAUDE_PLUGIN_DATA: scratch })

    expect(named.exitCode).toBe(0)
    // Claude Code would add this hook's stdout to the model's context.
    expect(named.stdout).toBe("")
    expect(await readFile(join(scratch, "titles", "s-1"), "utf8")).toBe("review the listener\n")

    const unnamed = await runHook(payload(), { CLAUDE_PLUGIN_DATA: scratch })

    expect(unnamed.exitCode).toBe(0)
    expect(unnamed.stdout).toBe("")
    expect(await readdir(join(scratch, "titles"))).toEqual([])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("exits 0 on a malformed payload and with no CLAUDE_PLUGIN_DATA, since exit 2 blocks the prompt", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-title-hook-bad-"))
  try {
    const malformed = await runHook("not json at all", { CLAUDE_PLUGIN_DATA: scratch })

    expect(malformed.exitCode).toBe(0)
    expect(malformed.stdout).toBe("")
    expect(malformed.stderr).toContain("envoy: could not record the session title")

    const unconfigured = await runHook(payload("review the listener"), {})

    expect(unconfigured.exitCode).toBe(0)
    expect(unconfigured.stdout).toBe("")
    expect(await readdir(scratch)).toEqual([])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})
