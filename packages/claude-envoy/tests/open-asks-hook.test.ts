import { expect, test } from "bun:test"
import { chmod, mkdir, mkdtemp, readdir, readFile, realpath, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

const hookEntry = resolve(import.meta.dir, "..", "hooks", "session-hook.ts")

/** Runs the hook as Claude Code does, with `env` as its whole environment. The session's cwd is
 * the test's scratch HOME unless the input names one: the hook reads `<cwd>/.envoy/envoy.json`, so a
 * shared directory's file would become every test's checkout. */
async function runHook(
  input: Record<string, unknown>,
  env: Record<string, string> & { readonly HOME: string },
): Promise<{ readonly exitCode: number; readonly stdout: string; readonly stderr: string }> {
  const run = Bun.spawn(["bun", hookEntry, "open-asks"], {
    env: { PATH: process.env["PATH"] ?? "", ...env },
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

test("a SessionStart carrying a session title records it, and one carrying none removes it", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-title-"))
  const pluginData = join(scratch, "plugin-data")
  const titleFile = join(pluginData, "titles", "s-1.txt")
  try {
    const named = await runHook(
      { ...hookInput, session_title: "review the listener" },
      { HOME: scratch, CLAUDE_PLUGIN_DATA: pluginData },
    )

    expect(named.exitCode).toBe(0)
    expect(await readFile(titleFile, "utf8")).toBe("review the listener\n")

    // The user undid the rename: Claude Code sends no session_title, and the record goes.
    const unnamed = await runHook(hookInput, { HOME: scratch, CLAUDE_PLUGIN_DATA: pluginData })

    expect(unnamed.exitCode).toBe(0)
    expect(await readdir(join(pluginData, "titles"))).toEqual([])
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

// A token file left without DISPATCH_URL names the lost variable, and the unread old file's advice
// still ends the notice, which is what the hook looks for before it says anything.
test("a DISPATCH_TOKEN_FILE with no DISPATCH_URL names the lost URL and still where the old file belongs", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-lost-url-"))
  const configDirectory = join(scratch, "claude-config")
  const old = join(scratch, ".config", "opencode", "envoy.json")
  const tokenFile = join(scratch, "DISPATCH_TOKEN")
  await mkdir(configDirectory)
  await mkdir(join(scratch, ".config", "opencode"), { recursive: true })
  await writeFile(old, JSON.stringify({ dispatch: { serverUrl: "https://old.dispatch.example" } }))
  await writeFile(tokenFile, "pointer-token\n")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      DISPATCH_TOKEN_FILE: tokenFile,
    })
    const chosen = join(configDirectory, "envoy.json")

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toBe(
      `Dispatch authored-ask summary unavailable: DISPATCH_TOKEN_FILE is set and DISPATCH_URL is not, so Dispatch is off: this shell lost the DISPATCH_URL it started with. Export DISPATCH_URL again, set to the Dispatch server that token is for; ${old} is not read for this profile; copy or link it to ${chosen}, or write that file\n`,
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
    // Only the session record the hook keeps for its Claude process is written (beside the cache
    // Bun itself keeps under HOME).
    const written = (await readdir(scratch)).filter((entry) => entry !== ".bun").sort()
    expect(written).toEqual(["plugin-data", "session-env.sh"])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

/** A directory holding a `jj` that runs `body`, to put first on the hook's PATH: the hook writes
 * the pane's attribution overlay over the config chain `jj config path --user` prints. */
async function jjDirectory(scratch: string, body: string): Promise<string> {
  const bin = join(scratch, "bin")
  await mkdir(bin, { recursive: true })
  const jj = join(bin, "jj")
  await writeFile(jj, `#!/bin/sh\n${body}\n`)
  await chmod(jj, 0o755)
  return bin
}

/** A Dispatch answering one empty open-asks snapshot, so a test can see the summary the hook
 * still prints beside whatever it wrote to the env file. */
function openAsksServer() {
  return Bun.serve({
    port: 0,
    fetch: () =>
      Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      }),
  })
}

// A Legion pane's jj commits carry the pane's session and its issue, through an overlay the hook
// writes and the JJ_CONFIG every later Bash command of the session sources. The hook writes it
// because the pane's claim boot cannot: that boot is the MCP server Claude Code spawns as a
// child, and a child cannot change its parent's environment. The issue line is scoped to the
// pane's own workspace, so a commit the same shell makes in another repository carries the
// session line alone.
test("writes the pane's attribution overlay and exports the chain layering it over jj's own config", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const workspace = join(scratch, "workspace")
  await mkdir(workspace)
  const userConfig = join(scratch, "jj-user.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
      LEGION_ISSUE_TRAILER: "ACME-7",
      LEGION_WORKSPACE: workspace,
    })

    expect(result.exitCode).toBe(0)
    const overlay = join(stateDir, "omp-attribution-s-1.toml")
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        `export JJ_CONFIG='${userConfig}:${overlay}'`,
        "",
      ].join("\n"),
    )
    expect(await readFile(overlay, "utf8")).toBe(
      `[templates]\ncommit_trailers = '"Omp-Session: s-1"'\n` +
        `\n[[--scope]]\n--when.workspaces = ["${await realpath(workspace)}"]\n` +
        `[--scope.templates]\ncommit_trailers = '"Omp-Session: s-1\\nDispatch: ACME-7"'\n`,
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// Without a workspace there is nothing to scope the key to, and an unscoped key would reach every
// jj repository the pane's shell commits in.
test("writes the session line alone when the pane names no workspace", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-no-ws-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
      LEGION_ISSUE_TRAILER: "ACME-7",
    })

    expect(result.exitCode).toBe(0)
    expect(await readFile(join(stateDir, "omp-attribution-s-1.toml"), "utf8")).toBe(
      `[templates]\ncommit_trailers = '"Omp-Session: s-1"'\n`,
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("writes the session line alone when the daemon named no issue key", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-no-key-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
    })

    expect(result.exitCode).toBe(0)
    const overlay = join(stateDir, "omp-attribution-s-1.toml")
    expect(await readFile(envFile, "utf8")).toContain(
      `export JJ_CONFIG='${userConfig}:${overlay}'\n`,
    )
    expect(await readFile(overlay, "utf8")).toBe(
      `[templates]\ncommit_trailers = '"Omp-Session: s-1"'\n`,
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// A Claude Code session that is no Legion pane commits as whoever runs it: no overlay, and no
// JJ_CONFIG to replace whatever jj config that person's shell already has.
test("writes no overlay and no JJ_CONFIG for a session that is no Legion pane", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-off-"))
  const bin = await jjDirectory(
    scratch,
    `printf '%s\\n' ${JSON.stringify(join(scratch, "jj-user.toml"))}`,
  )
  const envFile = join(scratch, "session-env.sh")
  const server = openAsksServer()
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: "hook-token",
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith("Dispatch authored-ask summary:\n")
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        "",
      ].join("\n"),
    )
    // No overlay was written anywhere: the only directory one could reach is the state directory
    // the session has none of (beside the cache Bun itself keeps under HOME).
    const written = (await readdir(scratch)).filter((entry) => entry !== ".bun").sort()
    expect(written).toEqual(["bin", "plugin-data", "session-env.sh"])
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// Claude Code gives each session id its own env file, so the runs that find a line already there
// are a resume and a compaction of the same id, which this hook runs again over the file that id
// wrote; the chain it resolves can differ between them. Two export lines in one sourced file
// would leave which chain wins to the order they were written in.
test("keeps one JJ_CONFIG export, naming the chain this run resolved, when the same id runs again", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-twice-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const laterConfig = join(scratch, "jj-user-later.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const laterBin = await jjDirectory(
    join(scratch, "later"),
    `printf '%s\\n' ${JSON.stringify(laterConfig)}`,
  )
  const envFile = join(scratch, "session-env.sh")
  await writeFile(envFile, "export FROM_ANOTHER_HOOK=1\n")
  try {
    const env = {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
    }
    expect((await runHook(hookInput, env)).exitCode).toBe(0)
    expect(
      (await runHook(hookInput, { ...env, PATH: `${laterBin}:${process.env["PATH"] ?? ""}` }))
        .exitCode,
    ).toBe(0)

    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export FROM_ANOTHER_HOOK=1",
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        `export JJ_CONFIG='${laterConfig}:${join(stateDir, `omp-attribution-${hookInput.session_id}.toml`)}'`,
        "",
      ].join("\n"),
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// An unquoted path holding a space or a quote would break every Bash command of the session.
test("single-quotes the chain, so a shell sourcing the file reads a state directory path whole", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-quoted-"))
  const stateDir = join(scratch, "it's a state dir")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
    })

    expect(result.exitCode).toBe(0)
    const overlay = join(stateDir, "omp-attribution-s-1.toml")
    const sourced = Bun.spawnSync(["bash", "-c", `. "$1"; printf %s "$JJ_CONFIG"`, "-", envFile])
    expect(sourced.stdout.toString()).toBe(`${userConfig}:${overlay}`)
    expect(await readFile(overlay, "utf8")).toBe(
      `[templates]\ncommit_trailers = '"Omp-Session: s-1"'\n`,
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// Attribution is worth nothing the session's own start: a jj that never answers costs the pane the
// export, and neither the two lines the `dispatch` command needs nor the summary.
test("gives up on a jj that never answers, keeping the other exports and the summary", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-hangs-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const bin = await jjDirectory(scratch, "exec sleep 30")
  const envFile = join(scratch, "session-env.sh")
  const server = openAsksServer()
  const started = Date.now()
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
      LEGION_ISSUE_TRAILER: "ACME-7",
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: "hook-token",
    })

    expect(result.exitCode).toBe(0)
    expect(Date.now() - started).toBeLessThan(15_000)
    expect(result.stdout).toStartWith("Dispatch authored-ask summary:\n")
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        "",
      ].join("\n"),
    )
    expect(await readdir(stateDir)).toEqual([])
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// `CLAUDE_ENV_FILE` is one file per session id
// (`<config>/session-env/<session id>/sessionstart-hook-<n>.sh` on the pinned Claude Code
// 2.1.311), so the only runs that find an `export JJ_CONFIG=` line already written are a resume
// or a compaction of that same id — which the Legion daemon does on every relaunch. The line
// there names this same session's own overlay, still on disk: a jj that never answers must leave
// it, since removing it would leave the session with neither trailer. The writer takes its base
// from an inherited JJ_CONFIG and spawns no jj at all when it finds one, so both runs are given
// an environment carrying none, as Claude Code's hook process is.
test("keeps the session's own export when a later run of the same id has no jj to answer", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-resume-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const envFile = join(scratch, "session-env.sh")
  try {
    const env = (bin: string) => ({
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
      LEGION_ISSUE_TRAILER: "ACME-7",
    })
    const overlay = join(stateDir, "omp-attribution-s-1.toml")
    const working = await jjDirectory(
      join(scratch, "working"),
      `printf '%s\\n' ${JSON.stringify(userConfig)}`,
    )
    expect((await runHook(hookInput, env(working))).exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).toContain(
      `export JJ_CONFIG='${userConfig}:${overlay}'\n`,
    )

    // The resume: same session id, same file, and a jj that never answers.
    const hanging = await jjDirectory(join(scratch, "hanging"), "exec sleep 30")
    expect((await runHook(hookInput, env(hanging))).exitCode).toBe(0)

    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        `export JJ_CONFIG='${userConfig}:${overlay}'`,
        "",
      ].join("\n"),
    )
    expect(await readdir(stateDir)).toEqual(["omp-attribution-s-1.toml"])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
}, 20_000)
