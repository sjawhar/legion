import { expect, test } from "bun:test"
import { execFileSync, spawn } from "node:child_process"
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import {
  asksForPrintMode,
  type CommandLine,
  channelAllowlist,
  claudeChannelEntries,
  claudeChannelGate,
  claudeCommandLine,
  installedMarketplace,
  installedPluginVersion,
  loadedMarketplace,
  managedSettingsDirectory,
  processCommandLine,
} from "../src/claude-channel"

const plugin = { name: "claude-envoy", marketplace: "legion-plugins" }
const ORG_LIST = [{ marketplace: "legion-plugins", plugin: "claude-envoy" }]
const exact = (...argv: string[]): CommandLine => ({ argv: ["claude", ...argv], exact: true })

test("reads both channel flags, space-separated and inline, tagged entries only", () => {
  expect(
    claudeChannelEntries([
      "claude",
      "--channels",
      "plugin:claude-envoy@legion-plugins",
      "server:envoy",
      "untagged",
      "--model=opus",
      "--dangerously-load-development-channels=plugin:other@dev",
    ]),
  ).toEqual([
    { kind: "plugin", name: "claude-envoy", marketplace: "legion-plugins", dev: false },
    { kind: "server", name: "envoy", dev: false },
    { kind: "plugin", name: "other", marketplace: "dev", dev: true },
  ])
})

test("ignores a development channel in a print-mode session, but only on an exact command line", () => {
  const argv = [
    "claude",
    "-p",
    "--dangerously-load-development-channels",
    "plugin:claude-envoy@legion-plugins",
  ]
  expect(claudeChannelEntries(argv, true)).toEqual([])
  // `ps` output split on whitespace: a `-p` may be a word of a prompt, so the entry stays.
  expect(claudeChannelEntries(argv, false)).toHaveLength(1)
})

test("print mode is read as Claude Code reads it: an option, not another option's value, before --", () => {
  expect(asksForPrintMode(["claude", "-p", "hi"])).toBe(true)
  expect(asksForPrintMode(["claude", "--print"])).toBe(true)
  expect(asksForPrintMode(["claude", "--sdk-url=ws://x"])).toBe(true)
  expect(asksForPrintMode(["claude", "--init-only"])).toBe(true)
  // `-p` as the value of `--name`, and `-p` after `--`, are not print mode.
  expect(asksForPrintMode(["claude", "--name", "-p"])).toBe(false)
  expect(asksForPrintMode(["claude", "--", "-p"])).toBe(false)
  const dev = ["--dangerously-load-development-channels", "plugin:claude-envoy@inline"]
  expect(claudeChannelEntries(["claude", "--name", "-p", ...dev])).toHaveLength(1)
  expect(claudeChannelEntries(["claude", ...dev, "--", "-p"])).toHaveLength(1)
})

test("a development channel is dropped when the session's stdout is not a terminal", () => {
  const argv = ["claude", "--dangerously-load-development-channels", "plugin:claude-envoy@inline"]
  expect(claudeChannelEntries(argv, true, false)).toEqual([])
  expect(claudeChannelEntries(argv, true, true)).toHaveLength(1)
  expect(claudeChannelEntries(argv, true, undefined)).toHaveLength(1)
})

test("a server: entry naming this plugin's server loads it, as Claude Code matches it", () => {
  expect(
    claudeChannelGate({
      commandLine: exact(
        "--dangerously-load-development-channels",
        "server:plugin:claude-envoy:envoy",
      ),
      plugin,
      allowlist: undefined,
    }),
  ).toEqual({ loaded: true })
})

const nodeOnPath = Bun.which("node")

test.skipIf(nodeOnPath === null)(
  "a Node process that set its title cannot be read, whatever its executable is named",
  async () => {
    // The Node binary itself, as Node names it: the first `node` on PATH may be a launcher that
    // execs Node later (mise's shim does), and until it does the command line is the launcher's.
    // Asked here, not as the file loads, so a broken `node` fails this test alone.
    const node = execFileSync(nodeOnPath ?? "node", ["-p", "process.execPath"], {
      encoding: "utf8",
    }).trim()
    // Node writes process.title over its argument area and pads the rest with NULs; a versioned
    // Node (`node-22`) is not named `node`, so the padding, not the name, is what says so.
    const child = spawn(
      node,
      [
        "-e",
        'process.title = "claude"; console.log("titled"); setTimeout(() => {}, 5000)',
        "--",
        "--dangerously-load-development-channels",
        "x",
      ],
      { stdio: ["ignore", "pipe", "ignore"] },
    )
    try {
      // Read the command line once the child says its title is set, within the test's timeout.
      const titled = Promise.withResolvers<void>()
      child.stdout.once("data", () => titled.resolve())
      child.once("exit", () => titled.reject(new Error("node exited before it set its title")))
      await titled.promise
      const pid = child.pid ?? 0
      expect(processCommandLine(pid, () => "/usr/bin/node-22")).toBe(undefined)
      expect(processCommandLine(pid, () => "/bin/cat")).toBe(undefined)
      expect(processCommandLine(pid)).toBe(undefined)
    } finally {
      child.kill()
    }
  },
)

test("a command line whose last arguments are empty is still read, not taken for title padding", async () => {
  const child = spawn("sh", ["-c", "sleep 5", "claude", "--name", "", ""], { stdio: "ignore" })
  try {
    await Bun.sleep(50)
    expect(processCommandLine(child.pid ?? 0)?.argv).toEqual([
      "sh",
      "-c",
      "sleep 5",
      "claude",
      "--name",
      "",
      "",
    ])
  } finally {
    child.kill()
  }
})

test("an empty argument on the real command line keeps its place", async () => {
  // `--append-system-prompt ""` takes the empty value; dropping it would read the channel flag as
  // that option's value.
  const child = spawn(
    "sh",
    [
      "-c",
      "sleep 5",
      "claude",
      "--append-system-prompt",
      "",
      "--dangerously-load-development-channels",
      "x",
    ],
    { stdio: "ignore" },
  )
  try {
    await Bun.sleep(50)
    expect(processCommandLine(child.pid ?? 0)?.argv).toEqual([
      "sh",
      "-c",
      "sleep 5",
      "claude",
      "--append-system-prompt",
      "",
      "--dangerously-load-development-channels",
      "x",
    ])
  } finally {
    child.kill()
  }
})

test("a command line split on whitespace skips no value and is not ended by --", () => {
  // From `ps`: a `--` or an option-looking word may belong to a prompt, so a later flag still counts.
  expect(
    claudeChannelEntries(
      [
        "claude",
        "run",
        "the",
        "tests",
        "--",
        "verbose",
        "--dangerously-load-development-channels",
        "plugin:claude-envoy@legion-plugins",
      ],
      false,
    ),
  ).toHaveLength(1)
  expect(
    claudeChannelEntries(["claude", "--model", "--channels=plugin:claude-envoy@x"], false),
  ).toHaveLength(1)
})

test("--channels entries come first, and a flag after -- is a positional argument", () => {
  expect(
    claudeChannelEntries([
      "claude",
      "--dangerously-load-development-channels",
      "plugin:claude-envoy@legion-plugins",
      "--channels",
      "plugin:claude-envoy@legion-plugins",
    ]).map((entry) => entry.dev),
  ).toEqual([false, true])
  expect(
    claudeChannelEntries(["claude", "--", "--channels", "plugin:claude-envoy@legion-plugins"]),
  ).toEqual([])
  // A value-taking option's value is not read as a flag.
  expect(claudeChannelEntries(["claude", "--model", "--channels=plugin:x@y"])).toEqual([])
})

test("Claude Code is found among the server's ancestors, through a shell prefix or a shim", () => {
  const lines: Record<number, CommandLine> = {
    30: { argv: ["/bin/sh", "/home/u/.shims/bun", "server.js"], exact: true },
    20: { argv: ["/bin/sh", "-c", "bun server.js"], exact: true },
    10: { argv: ["claude", "--channels", "plugin:claude-envoy@legion-plugins"], exact: true },
  }
  const parents: Record<number, number> = { 30: 20, 20: 10, 10: 1 }
  expect(
    claudeCommandLine(
      30,
      (pid) => lines[pid],
      (pid) => parents[pid],
    )?.argv[0],
  ).toBe("claude")
  // No `claude` among them, or a command line that cannot be read: cannot tell.
  expect(
    claudeCommandLine(
      30,
      (pid) => (pid === 10 ? undefined : lines[pid]),
      (pid) => parents[pid],
    ),
  ).toBeUndefined()
  expect(claudeChannelGate({ commandLine: undefined, plugin, allowlist: undefined })).toEqual({
    loaded: true,
  })
})

test("a plugin loaded with --plugin-dir is named @inline, so another marketplace does not load it", () => {
  const argv = ["claude", "--plugin-dir", "plugins/claude-envoy"]
  expect(
    loadedMarketplace("/w/plugins/claude-envoy", "/w/plugins/claude-envoy/dist/x.js", {
      argv,
      cwd: "/w",
    }),
  ).toBe("inline")
  expect(
    loadedMarketplace("/w/plugins/claude-envoy", "/w/plugins/claude-envoy/dist/x.js", {
      argv: ["claude", "--plugin-dir=/w/plugins/claude-envoy"],
      cwd: "/",
    }),
  ).toBe("inline")
  expect(
    loadedMarketplace(
      "/c/plugins/cache/legion-plugins/claude-envoy/abc",
      "/c/plugins/cache/legion-plugins/claude-envoy/abc/dist/x.js",
      { argv, cwd: "/w" },
    ),
  ).toBe("legion-plugins")
  expect(
    claudeChannelGate({
      commandLine: exact(
        "--dangerously-load-development-channels",
        "plugin:claude-envoy@legion-plugins",
      ),
      plugin: { name: "claude-envoy", marketplace: "inline" },
      allowlist: undefined,
    }),
  ).toEqual({
    loaded: false,
    why: "claude asked for plugin:claude-envoy@legion-plugins but this plugin is installed from inline",
  })
})

test("a session started without either flag has no channel", () => {
  const gate = claudeChannelGate({ commandLine: exact("--resume"), plugin, allowlist: ORG_LIST })
  expect(gate.loaded).toBe(false)
  expect(gate.loaded ? "" : gate.why).toBe(
    "claude was started without --channels plugin:claude-envoy@legion-plugins (or --dangerously-load-development-channels plugin:claude-envoy@legion-plugins)",
  )
})

test("a development channel naming this plugin is loaded", () => {
  expect(
    claudeChannelGate({
      commandLine: exact(
        "--dangerously-load-development-channels",
        "plugin:claude-envoy@legion-plugins",
      ),
      plugin,
      allowlist: undefined,
    }),
  ).toEqual({ loaded: true })
})

test("--channels loads this plugin only when the managed settings allowlist names it", () => {
  const commandLine = exact("--channels", "plugin:claude-envoy@legion-plugins")
  const where = `${join(managedSettingsDirectory(), "managed-settings.json")}, or in the server-managed settings when the account receives them`
  expect(claudeChannelGate({ commandLine, plugin, allowlist: ORG_LIST })).toEqual({ loaded: true })
  expect(claudeChannelGate({ commandLine, plugin, allowlist: undefined })).toEqual({
    loaded: false,
    why: `plugin:claude-envoy@legion-plugins is not on Claude Code's approved channels allowlist (add it to allowedChannelPlugins in ${where}; for one session use --dangerously-load-development-channels plugin:claude-envoy@legion-plugins)`,
  })
  expect(
    claudeChannelGate({
      commandLine,
      plugin,
      allowlist: [{ marketplace: "other", plugin: "x" }],
    }),
  ).toEqual({
    loaded: false,
    why: `plugin:claude-envoy@legion-plugins is not on Claude Code's approved channels allowlist (allowedChannelPlugins in ${where})`,
  })
})

test("an entry from another marketplace than the installed plugin is skipped", () => {
  expect(
    claudeChannelGate({
      commandLine: exact("--dangerously-load-development-channels", "plugin:claude-envoy@dev"),
      plugin,
      allowlist: undefined,
    }),
  ).toEqual({
    loaded: false,
    why: "claude asked for plugin:claude-envoy@dev but this plugin is installed from legion-plugins",
  })
})

test("a marketplace whose name holds a dot matches its cache directory's sanitized name", () => {
  // Claude Code writes the cache path with every character outside [A-Za-z0-9_-] made "-", and
  // compares the flag's marketplace as written.
  expect(
    claudeChannelGate({
      commandLine: exact(
        "--dangerously-load-development-channels",
        "plugin:claude-envoy@acme.tools",
      ),
      plugin: { name: "claude-envoy", marketplace: "acme-tools" },
      allowlist: undefined,
    }),
  ).toEqual({ loaded: true })
})

// A plugin's server is `plugin:claude-envoy:envoy` to Claude Code, which no `server:` entry names;
// a bare config's server is named by its config, which this process does not know.
test("a plugin takes only plugin entries, and a bare config any development server entry", () => {
  const serverEntry = exact("--dangerously-load-development-channels", "server:my-envoy")
  expect(claudeChannelGate({ commandLine: serverEntry, plugin, allowlist: undefined }).loaded).toBe(
    false,
  )
  expect(
    claudeChannelGate({ commandLine: serverEntry, plugin: undefined, allowlist: undefined }),
  ).toEqual({ loaded: true })
  expect(
    claudeChannelGate({
      commandLine: exact("--channels", "server:my-envoy"),
      plugin: undefined,
      allowlist: ORG_LIST,
    }).loaded,
  ).toBe(false)
})

test("a process it cannot read is taken as loaded, which is what it advertised before", () => {
  expect(claudeChannelGate({ commandLine: undefined, plugin, allowlist: undefined })).toEqual({
    loaded: true,
  })
})

test("names the marketplace of a cached plugin install, and none for any other path", () => {
  expect(
    installedMarketplace(
      "/home/u/.claude/plugins/cache/legion-plugins/claude-envoy/afe10b2f3e1b/dist/envoy-channel.js",
    ),
  ).toBe("legion-plugins")
  expect(installedMarketplace("/work/legion/packages/claude-envoy/dist/envoy-channel.js")).toBe(
    undefined,
  )
})

test("names the version of a cached plugin install, and none for any other path", () => {
  expect(
    installedPluginVersion(
      "/home/u/.claude/plugins/cache/legion-plugins/claude-envoy/afe10b2f3e1b/dist/envoy-channel.js",
    ),
  ).toBe("afe10b2f3e1b")
  expect(
    installedPluginVersion("/work/legion/packages/claude-envoy/dist/envoy-channel.js"),
  ).toBeUndefined()
})

test("the highest managed settings source present is the allowlist alone", async () => {
  const root = await mkdtemp(join(tmpdir(), "claude-channel-"))
  try {
    const managed = join(root, "managed")
    const config = join(root, "config")
    const directories = { managedDirectory: managed, configDirectory: config }
    await mkdir(join(managed, "managed-settings.d"), { recursive: true })
    await mkdir(config, { recursive: true })
    expect(channelAllowlist(directories)).toBe(undefined)

    await writeFile(join(managed, "managed-settings.json"), JSON.stringify({ model: "x" }))
    await writeFile(
      join(managed, "managed-settings.d", "10-channels.json"),
      JSON.stringify({ allowedChannelPlugins: ORG_LIST }),
    )
    // An empty or unreadable cache of server-managed settings is no source.
    await writeFile(join(config, "remote-settings.json"), "{}")
    expect(channelAllowlist(directories)).toEqual(ORG_LIST)
    await writeFile(join(config, "remote-settings.json"), "not json")
    expect(channelAllowlist(directories)).toEqual(ORG_LIST)

    // Server-managed settings outrank the file, even when they set no allowlist.
    await writeFile(join(config, "remote-settings.json"), JSON.stringify({ model: "y" }))
    expect(channelAllowlist(directories)).toBe(undefined)
    // Under "merge", the highest source that sets the key owns it.
    await writeFile(
      join(config, "remote-settings.json"),
      JSON.stringify({ model: "y", managedSourcesBehavior: "merge" }),
    )
    expect(channelAllowlist(directories)).toEqual(ORG_LIST)
    // A remote file holding only composition keys is no source of its own.
    await writeFile(
      join(config, "remote-settings.json"),
      JSON.stringify({ managedSourcesBehavior: "first-wins" }),
    )
    expect(channelAllowlist(directories)).toEqual(ORG_LIST)
    await writeFile(
      join(config, "remote-settings.json"),
      JSON.stringify({ wslInheritsWindowsSettings: false }),
    )
    expect(channelAllowlist(directories)).toEqual(ORG_LIST)
  } finally {
    await rm(root, { recursive: true, force: true })
  }
})

test("an allowlist takes Claude Code's string entries, drops each invalid one alone, and skips dot files", async () => {
  const root = await mkdtemp(join(tmpdir(), "claude-channel-"))
  try {
    const managed = join(root, "managed")
    const directories = { managedDirectory: managed, configDirectory: join(root, "config") }
    await mkdir(join(managed, "managed-settings.d"), { recursive: true })
    await writeFile(
      join(managed, "managed-settings.json"),
      JSON.stringify({
        allowedChannelPlugins: ["claude-envoy@legion-plugins", { marketplace: "x" }, 7],
      }),
    )
    await writeFile(
      join(managed, "managed-settings.d", ".off.json"),
      JSON.stringify({ allowedChannelPlugins: [{ marketplace: "dot", plugin: "file" }] }),
    )
    expect(channelAllowlist(directories)).toEqual(ORG_LIST)
  } finally {
    await rm(root, { recursive: true, force: true })
  }
})

test("reads a live process's command line", () => {
  const commandLine = processCommandLine(process.pid)
  expect(commandLine?.argv.some((argument) => argument.includes("bun"))).toBe(true)
})

// Claude Code sets process.title to "claude"; under Node that overwrites the arguments `ps` and
// /proc show, so a one-word command line from a Node process says nothing about the flags.
test("a one-word command line counts as unreadable only when Node runs it", async () => {
  const child = spawn("cat", [], { stdio: ["pipe", "ignore", "ignore"] })
  try {
    const pid = child.pid ?? 0
    await Bun.sleep(50)
    expect(processCommandLine(pid, () => "/usr/local/bin/node")).toBe(undefined)
    expect(processCommandLine(pid, () => "/usr/bin/node-22")).toBe(undefined)
    expect(processCommandLine(pid, () => "/bin/cat")?.argv).toHaveLength(1)
  } finally {
    child.kill()
  }
})
