import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

const hookEntry = resolve(import.meta.dir, "..", "hooks", "session-hook.ts")

/** Runs the hook as Claude Code does, with `env` as its whole environment beside `PATH` and Bun's
 * transpiler cache turned off: that cache lives under HOME (`~/.bun` on Linux,
 * `~/Library/Caches/bun` on macOS), and HOME is the test's scratch directory. The session's cwd is
 * the test's scratch HOME unless the input names one: the hook reads `<cwd>/.envoy/envoy.json`, so a
 * shared directory's file would become every test's checkout. */
async function runHook(
  input: Record<string, unknown>,
  env: Record<string, string> & { readonly HOME: string },
): Promise<{ readonly exitCode: number; readonly stdout: string; readonly stderr: string }> {
  const run = Bun.spawn(["bun", hookEntry, "open-asks"], {
    env: { PATH: process.env["PATH"] ?? "", BUN_RUNTIME_TRANSPILER_CACHE_PATH: "0", ...env },
    stdin: new TextEncoder().encode(JSON.stringify({ cwd: env.HOME, ...input })),
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

test("summarises a missing Claude profile file with where its unread old file belongs", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-profile-off-"))
  const configDirectory = join(scratch, "claude-config")
  const old = join(scratch, ".config", "opencode", "envoy.json")
  await mkdir(configDirectory)
  await mkdir(join(scratch, ".config", "opencode"), { recursive: true })
  await writeFile(
    old,
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: "https://old.dispatch.example",
        token: "old-token",
      },
    }),
  )
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
    })
    const chosen = join(configDirectory, "envoy.json")

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toBe(
      `Dispatch authored-ask summary unavailable: no envoy.json at ${chosen}: Dispatch is off; ${old} is not read for this profile; copy or link it to ${chosen}, or write that file\n`,
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("says nothing of the unread old user file when a checkout's enabled: false turns Dispatch off", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-checkout-off-"))
  const checkout = join(scratch, "checkout")
  const old = join(scratch, ".config", "opencode", "envoy.json")
  await mkdir(join(checkout, ".envoy"), { recursive: true })
  await writeFile(
    join(checkout, ".envoy", "envoy.json"),
    JSON.stringify({ dispatch: { enabled: false } }),
  )
  await mkdir(join(scratch, ".config", "opencode"), { recursive: true })
  await writeFile(
    old,
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: "https://old.dispatch.example",
        token: "old-token",
      },
    }),
  )
  try {
    // No ~/.config/legion/envoy.json, so the read names the old file beside the missing one; the
    // checkout's own enabled: false, not the missing file, is what turns Dispatch off.
    const result = await runHook({ ...hookInput, cwd: checkout }, { HOME: scratch })
    expect(result.exitCode).toBe(0)
    expect(result.stdout).toBe("")

    // The same session without the old file prints the same.
    await rm(old)
    const withoutOld = await runHook({ ...hookInput, cwd: checkout }, { HOME: scratch })
    expect(withoutOld.exitCode).toBe(0)
    expect(withoutOld.stdout).toBe(result.stdout)
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("names a checkout's unread .opencode/envoy.json and still summarises the asks from the user file's server", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-old-checkout-"))
  const checkout = join(scratch, "checkout")
  const old = join(checkout, ".opencode", "envoy.json")
  const requests: string[] = []
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      requests.push(`${request.headers.get("authorization")} ${new URL(request.url).pathname}`)
      return Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      })
    },
  })
  await mkdir(join(scratch, ".config", "legion"), { recursive: true })
  await writeFile(
    join(scratch, ".config", "legion", "envoy.json"),
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: `http://127.0.0.1:${server.port}`,
        token: "user-token",
      },
    }),
  )
  await mkdir(join(checkout, ".opencode"), { recursive: true })
  await writeFile(
    old,
    JSON.stringify({
      dispatch: { enabled: true, serverUrl: "http://127.0.0.1:1", token: "checkout-token" },
    }),
  )
  const rename = `envoy: ${old} is not read: mkdir -p ${join(checkout, ".envoy")} && mv ${old} ${join(checkout, ".envoy", "envoy.json")}\n`
  try {
    const result = await runHook(
      { ...hookInput, cwd: checkout },
      { HOME: scratch, CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data") },
    )

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith(`${rename}Dispatch authored-ask summary:\n`)
    expect(result.stdout.split(rename)).toHaveLength(2)
    expect(requests).toEqual(["Bearer user-token /api/v1/asks/open"])

    // With no user file the checkout's old file is all that ever configured Dispatch here: the
    // hook still names it, beside the missing file's quiet off.
    await rm(join(scratch, ".config", "legion", "envoy.json"))
    const off = await runHook(
      { ...hookInput, cwd: checkout },
      { HOME: scratch, CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data") },
    )
    expect(off.exitCode).toBe(0)
    expect(off.stdout).toBe(rename)
    expect(requests).toHaveLength(1)
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// The `dispatch` command the plugin's bin/ puts on the Bash tool's PATH reads its host from
// DISPATCH_HOST, and its envoy.json by Claude Code's rule: CLAUDE_CONFIG_DIR's, else the file
// ENVOY_CONFIG_FILE names; Claude Code sources CLAUDE_ENV_FILE into every later Bash command of the
// session.
test("names Claude Code as the dispatch host and hands on the session's envoy.json in CLAUDE_ENV_FILE, after whatever other hooks wrote", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-env-"))
  const envFile = join(scratch, "session-env.sh")
  await writeFile(envFile, "export FROM_ANOTHER_HOOK=1\n")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
    })

    expect(result.exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export FROM_ANOTHER_HOOK=1",
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        "",
      ].join("\n"),
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("hands on the CLAUDE_CONFIG_DIR envoy.json, single-quoted so the shell reads the path as written", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-config-"))
  const configDirectory = join(scratch, "it's claude")
  await mkdir(configDirectory)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_ENV_FILE: envFile,
    })

    expect(result.exitCode).toBe(0)
    const file = join(configDirectory, "envoy.json")
    expect(await readFile(envFile, "utf8")).toBe(
      `export DISPATCH_HOST=claude\nexport ENVOY_CONFIG_FILE='${file.replaceAll("'", "'\\''")}'\n`,
    )
    const sourced = Bun.spawnSync([
      "bash",
      "-c",
      `. "$1"; printf %s "$ENVOY_CONFIG_FILE"`,
      "-",
      envFile,
    ])
    expect(sourced.stdout.toString()).toBe(file)
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// Swap the hook's harness for the `command` rule, or omp's, and it would hand on, and read open
// asks from, a file an outer session or launcher left in the environment.
test("reads and hands on Claude Code's own envoy.json, never the ENVOY_CONFIG_FILE or omp profile it inherited", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-harness-"))
  const requests: string[] = []
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      const url = new URL(request.url)
      requests.push(`${request.headers.get("authorization")} ${url.pathname}?${url.searchParams}`)
      return Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      })
    },
  })
  const configDirectory = join(scratch, "claude-config")
  await mkdir(configDirectory)
  await writeFile(
    join(configDirectory, "envoy.json"),
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: `http://127.0.0.1:${server.port}`,
        token: "claude-token",
      },
    }),
  )
  const decoy = join(scratch, "decoy")
  await mkdir(decoy)
  await writeFile(join(decoy, "envoy.json"), "{not json")
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      CLAUDE_CONFIG_DIR: configDirectory,
      ENVOY_CONFIG_FILE: join(decoy, "envoy.json"),
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: decoy,
    })

    expect(result.exitCode).toBe(0)
    expect(requests).toEqual(["Bearer claude-token /api/v1/asks/open?author_session=s-1"])
    expect(await readFile(envFile, "utf8")).toContain(
      `export ENVOY_CONFIG_FILE='${join(configDirectory, "envoy.json")}'`,
    )
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

test("with a relative XDG_CONFIG_HOME, hands on no envoy.json, says why Dispatch is unavailable, and still exits 0", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-xdg-"))
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      XDG_CONFIG_HOME: "relative/config",
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
    })

    expect(result.exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).toBe("export DISPATCH_HOST=claude\n")
    expect(result.stdout).toBe(
      "Dispatch authored-ask summary unavailable: XDG_CONFIG_HOME is relative/config, which is not an absolute path\n",
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// SessionStart fires on startup, resume, clear, compact and fork; the file keeps one of each line.
test("writes the host and envoy.json lines once however many SessionStarts write the same env file", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-env-twice-"))
  const envFile = join(scratch, "session-env.sh")
  await writeFile(envFile, "export FROM_ANOTHER_HOOK=1\n")
  try {
    const env = {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
    }
    expect((await runHook(hookInput, env)).exitCode).toBe(0)
    expect((await runHook(hookInput, env)).exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export FROM_ANOTHER_HOOK=1",
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        "",
      ].join("\n"),
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("writes no environment line anywhere when Claude Code names no env file", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-no-env-"))
  const envFile = join(scratch, "session-env.sh")
  await writeFile(envFile, "export FROM_ANOTHER_HOOK=1\n")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).not.toContain("DISPATCH_HOST")
    expect(result.stdout).not.toContain("ENVOY_CONFIG_FILE")
    expect(await readFile(envFile, "utf8")).toBe("export FROM_ANOTHER_HOOK=1\n")
    // Only the session record the hook keeps for its Claude process is written.
    expect((await readdir(scratch)).sort()).toEqual(["plugin-data", "session-env.sh"])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})
