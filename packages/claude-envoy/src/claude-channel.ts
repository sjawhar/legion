import { spawnSync } from "node:child_process"
import { readdirSync, readFileSync, readlinkSync } from "node:fs"
import { homedir } from "node:os"
import { basename, join, resolve, sep } from "node:path"
import { z } from "zod"

/**
 * Whether Claude Code passes this server's channel notifications to the model. It says nothing
 * about that to the server: the MCP handshake is the same with the channel on or off, and a
 * notification sent while it is off is dropped without a word. So the server reads the `claude`
 * process's own command line and applies the gate Claude Code applies to it (as of 2.1.301): the
 * entry must be named by `--channels` or, in an interactive session,
 * `--dangerously-load-development-channels`; a plugin entry must name the marketplace the plugin
 * is installed from; and a `--channels` entry must be allowlisted, which for this plugin means
 * `allowedChannelPlugins` in the managed settings Claude Code applies, since it is not on
 * Anthropic's own list. Claude Code's account and provider checks are not repeated here, and
 * neither is a channel an SDK host turns on after startup (`enableChannel`): the check runs once,
 * so such a session advertises no mode and Dispatch refuses messages Claude Code would pass on.
 */
export type ChannelGate =
  | { readonly loaded: true }
  | { readonly loaded: false; readonly why: string }

/** One `plugin:<name>@<marketplace>` or `server:<name>` entry of either channel flag. */
export interface ChannelEntry {
  readonly kind: "plugin" | "server"
  readonly name: string
  readonly marketplace?: string
  readonly dev: boolean
}

export interface AllowedChannelPlugin {
  readonly marketplace: string
  readonly plugin: string
}

/** The name `.mcp.json` gives this plugin's server. */
const PLUGIN_SERVER = "envoy"

/** The flags that name channels, and whether each is the development one. */
const CHANNEL_FLAGS: Readonly<Record<string, boolean>> = {
  "--channels": false,
  "--dangerously-load-development-channels": true,
}

function parseEntry(value: string, dev: boolean): ChannelEntry | undefined {
  if (value.startsWith("plugin:")) {
    const spec = value.slice("plugin:".length)
    const at = spec.indexOf("@")
    if (at <= 0 || at === spec.length - 1) return undefined
    return { kind: "plugin", name: spec.slice(0, at), marketplace: spec.slice(at + 1), dev }
  }
  if (value.startsWith("server:") && value.length > "server:".length) {
    return { kind: "server", name: value.slice("server:".length), dev }
  }
  return undefined
}

// Claude Code 2.1.301's own argument scanner: the options that take one value, the ones whose value
// is optional (taken when the next argument is not an option), and the ones that take every value
// up to the next option. Its print-mode test and its channel flags skip those values, and `--`
// ends the options.
const VALUE_OPTIONS: ReadonlySet<string> = new Set([
  "--prefill",
  "--prefill-b64",
  "--deep-link-repo",
  "--deep-link-last-fetch",
  "--deep-link-cwd-b64",
  "--handle-uri",
  "--settings",
  "--managed-settings",
  "--setting-sources",
  "--client-data-url",
  "--watch-artifact",
  "--watch-artifact-no-autoreact",
  "--team-name",
  "--agent-id",
  "--agent-name",
  "--agent-color",
  "--parent-session-id",
  "--agent-type",
  "--model",
  "--agent",
  "--routine",
  "--effort",
  "--fast-mode",
  "--permission-mode",
  "--inherit-permission-mode",
  "--proactivity",
  "--debug-file",
  "--system-prompt",
  "--system-prompt-file",
  "--append-system-prompt",
  "--append-system-prompt-file",
  "--system-prompt-snapshot",
  "--append-subagent-system-prompt",
  "--append-subagent-system-prompt-file",
  "--plan-mode-instructions",
  "--permission-prompt-tool",
  "--permission-prompts",
  "--json-schema",
  "--fallback-model",
  "--advisor",
  "--agents",
  "--name",
  "-n",
  "--allowedTools",
  "--allowed-tools",
  "--disallowedTools",
  "--disallowed-tools",
  "--tools",
  "--add-dir",
  "--mcp-config",
  "--betas",
  "--file",
  "--channels",
  "--dangerously-load-development-channels",
  "--plugin-dir",
  "--plugin-dir-no-mcp",
  "--plugin-url",
  "--remote-control-session-name-prefix",
  "--exec",
  "-m",
  "--sdk-url",
  "--thinking",
  "--thinking-display",
  "--max-thinking-tokens",
  "--max-turns",
  "--max-budget-usd",
  "--task-budget",
  "--autocompact",
  "--rewind-files",
  "--resume-session-at",
  "--resume-drops-turn",
  "--workload",
  "--output-format",
  "--input-format",
  "--teammate-mode",
  "--messaging-socket-path",
  "--session-id",
  "--environment",
  "--pool",
  "--ref",
  "--on-branch",
  "--correlation-id",
  "--forward-home-settings",
  "--project-config-root",
  "--attach-serve",
])
const OPTIONAL_VALUE_OPTIONS: ReadonlySet<string> = new Set([
  "-d",
  "--debug",
  "-r",
  "--resume",
  "--from-pr",
  "-w",
  "--worktree",
  "--teleport",
  "--cloud",
  "--remote",
  "--project",
  "--remote-control",
  "--rc",
  "--prompt-suggestions",
])
const MULTI_VALUE_OPTIONS: ReadonlySet<string> = new Set([
  "--allowedTools",
  "--allowed-tools",
  "--disallowedTools",
  "--disallowed-tools",
  "--tools",
  "--add-dir",
  "--mcp-config",
  "--betas",
  "--file",
  "--channels",
  "--dangerously-load-development-channels",
])

function isOption(argument: string): boolean {
  return argument.length > 1 && argument.startsWith("-")
}

/** The index of the last argument `argv[index]` consumes as its values, as Claude Code skips them. */
function lastValueIndex(argv: readonly string[], index: number): number {
  const argument = argv[index] ?? ""
  if (OPTIONAL_VALUE_OPTIONS.has(argument)) {
    const next = argv[index + 1]
    return next !== undefined && !isOption(next) ? index + 1 : index
  }
  if (!VALUE_OPTIONS.has(argument)) return index
  let last = index + 1
  if (MULTI_VALUE_OPTIONS.has(argument)) {
    while (last + 1 < argv.length && !isOption(argv[last + 1] ?? "")) last++
  }
  return last
}

/**
 * Whether a command line asks for a non-interactive session, as Claude Code reads it: `-p`,
 * `--print`, `--init-only` or `--sdk-url` as an option, not as another option's value, before `--`.
 */
export function asksForPrintMode(argv: readonly string[]): boolean {
  for (let index = 1; index < argv.length; index++) {
    const argument = argv[index] ?? ""
    if (argument === "--") return false
    if (
      argument === "-p" ||
      argument === "--print" ||
      argument === "--init-only" ||
      argument.startsWith("--sdk-url")
    ) {
      return true
    }
    index = lastValueIndex(argv, index)
  }
  return false
}

/**
 * The channel entries a `claude` command line names, as Claude Code reads them: each flag, before
 * `--`, takes the values after it up to the next option, or one inline `=` value, and untagged values
 * are dropped; every `--channels` entry comes before every development one, since Claude Code
 * matches the first entry naming the server. Development channels load only in an interactive
 * session, so they are dropped when `interactive` is false. A command line split on whitespace
 * (`ps`, not `exact`) cannot tell an option from a word of a prompt, so none of that reading
 * applies to it: its print mode is never read, `--` ends nothing and no value is skipped, since
 * each could drop a loaded channel and refuse messages the session takes.
 */
export function claudeChannelEntries(
  argv: readonly string[],
  exact = true,
  stdoutIsTerminal: boolean | undefined = undefined,
): ChannelEntry[] {
  const interactive = !exact || (!asksForPrintMode(argv) && stdoutIsTerminal !== false)
  const approved: ChannelEntry[] = []
  const development: ChannelEntry[] = []
  for (let index = 1; index < argv.length; index++) {
    const argument = argv[index] ?? ""
    if (exact && argument === "--") break
    const equals = argument.indexOf("=")
    const flag = equals === -1 ? argument : argument.slice(0, equals)
    const dev = CHANNEL_FLAGS[flag]
    if (dev === undefined) {
      if (exact) index = lastValueIndex(argv, index)
      continue
    }
    const values: string[] = []
    if (equals !== -1) values.push(argument.slice(equals + 1))
    else {
      while (index + 1 < argv.length && !isOption(argv[index + 1] ?? "-")) {
        values.push(argv[index + 1] ?? "")
        index++
      }
    }
    if (dev && !interactive) continue
    for (const value of values) {
      const entry = parseEntry(value, dev)
      if (entry !== undefined) (dev ? development : approved).push(entry)
    }
  }
  return [...approved, ...development]
}

/** A process's command line: its arguments, and whether they are exact (`/proc`) rather than
 *  `ps` output split on whitespace. */
export interface CommandLine {
  readonly argv: readonly string[]
  readonly exact: boolean
  /** Whether the process's stdout is a terminal, when that can be read (`/proc`): Claude Code is
   *  non-interactive when it is not, and then ignores development channels. */
  readonly stdoutIsTerminal?: boolean
  /** The process's working directory, when it can be read (`/proc`), which a relative
   *  `--plugin-dir` value is relative to. */
  readonly cwd?: string
}

/**
 * Whether the `claude` process with `commandLine` loads this server's channel. An undefined
 * command line could not be read, and is taken as loaded: that is what the server advertised
 * before it could tell, and refusing every message on a guess would be worse.
 */
export function claudeChannelGate(input: {
  readonly commandLine: CommandLine | undefined
  /** This plugin's name and, when the install path says it, its marketplace; undefined when the
   *  server runs from a bare MCP config rather than as a plugin, where only `server:` entries
   *  apply (a plugin's server is `plugin:<name>:<server>`, which no `server:` entry names). */
  readonly plugin: { readonly name: string; readonly marketplace: string | undefined } | undefined
  /** The managed settings' `allowedChannelPlugins`, or undefined when they do not set it. */
  readonly allowlist: readonly AllowedChannelPlugin[] | undefined
}): ChannelGate {
  const { commandLine, plugin, allowlist } = input
  if (commandLine === undefined) return { loaded: true }
  const ours =
    plugin === undefined
      ? "server:<name>"
      : `plugin:${plugin.name}@${plugin.marketplace ?? "<marketplace>"}`
  // A bare config's own server name is not known here, so any `server:` entry counts for it.
  const entry = claudeChannelEntries(
    commandLine.argv,
    commandLine.exact,
    commandLine.stdoutIsTerminal,
  ).find((candidate) =>
    plugin === undefined
      ? candidate.kind === "server"
      : // Claude Code names a plugin's server `plugin:<plugin>:<server>`, which a `server:` entry
        // can name too.
        (candidate.kind === "plugin" && candidate.name === plugin.name) ||
        (candidate.kind === "server" &&
          candidate.name === `plugin:${plugin.name}:${PLUGIN_SERVER}`),
  )
  if (entry === undefined) {
    return {
      loaded: false,
      why: `claude was started without --channels ${ours} (or --dangerously-load-development-channels ${ours})`,
    }
  }
  if (entry.kind === "server") {
    return entry.dev
      ? { loaded: true }
      : {
          loaded: false,
          why: `server:${entry.name} is not on Claude Code's approved channels allowlist (use --dangerously-load-development-channels server:${entry.name})`,
        }
  }
  const asked = `plugin:${entry.name}@${entry.marketplace}`
  // The install path holds the marketplace as Claude Code names its cache directory, with every
  // character outside [A-Za-z0-9_-] made "-", while the flag holds it as written. So both are
  // compared that way; a flag naming `legion-plugins` for a plugin from `legion.plugins` therefore
  // counts as a match here, which Claude Code refuses, since the path no longer says which it was.
  if (
    plugin?.marketplace !== undefined &&
    cacheDirectoryName(entry.marketplace ?? "") !== cacheDirectoryName(plugin.marketplace)
  ) {
    return {
      loaded: false,
      why: `claude asked for ${asked} but this plugin is installed from ${plugin.marketplace}`,
    }
  }
  if (entry.dev) return { loaded: true }
  if (allowlist === undefined) {
    return {
      loaded: false,
      why: `${asked} is not on Claude Code's approved channels allowlist (add it to allowedChannelPlugins in ${MANAGED_SETTINGS_HINT}; for one session use --dangerously-load-development-channels ${asked})`,
    }
  }
  return allowlist.some(
    (allowed) => allowed.marketplace === entry.marketplace && allowed.plugin === entry.name,
  )
    ? { loaded: true }
    : {
        loaded: false,
        why: `${asked} is not on Claude Code's approved channels allowlist (allowedChannelPlugins in ${MANAGED_SETTINGS_HINT})`,
      }
}

/** Where an allowlist goes: the machine's managed-settings.json, unless the account receives
 *  server-managed settings, which then own `allowedChannelPlugins` (see `channelAllowlist`). */
const MANAGED_SETTINGS_HINT = `${join(managedSettingsDirectory(), "managed-settings.json")}, or in the server-managed settings when the account receives them`

/** A marketplace name as Claude Code writes it into its plugin cache path. */
function cacheDirectoryName(marketplace: string): string {
  return marketplace.replace(/[^a-zA-Z0-9\-_]/g, "-")
}

/** The executable process `pid` runs, or undefined when it cannot be read. */
export function processExecutable(pid: number): string | undefined {
  if (process.platform === "linux") {
    try {
      return readlinkSync(`/proc/${pid}/exe`)
    } catch {
      return undefined
    }
  }
  if (process.platform === "win32") return undefined
  const result = spawnSync("lsof", ["-a", "-p", String(pid), "-d", "txt", "-Fn"], {
    encoding: "utf8",
    timeout: 2_000,
  })
  const first = (result.stdout ?? "").split("\n").find((line) => line.startsWith("n"))
  return first === undefined ? undefined : first.slice(1)
}

/** Whether process `pid`'s stdout is a terminal, from `/proc` on Linux; undefined elsewhere or
 *  when it cannot be read. */
function stdoutIsTerminal(pid: number): boolean | undefined {
  if (process.platform !== "linux") return undefined
  try {
    const target = readlinkSync(`/proc/${pid}/fd/1`)
    return target.startsWith("/dev/pts/") || /^\/dev\/tty[^S]/.test(target) || target === "/dev/tty"
  } catch {
    return undefined
  }
}

/** Process `pid`'s working directory, from `/proc`; undefined when it cannot be read. */
function workingDirectory(pid: number): string | undefined {
  try {
    return readlinkSync(`/proc/${pid}/cwd`)
  } catch {
    return undefined
  }
}

/**
 * The command line of process `pid`, or undefined when it cannot be read: `/proc` on Linux
 * (exact, with whether its stdout is a terminal), `ps` elsewhere (split on whitespace, which no
 * channel entry contains), and nothing on Windows. Claude Code sets `process.title` to `claude`,
 * which under Node overwrites the arguments `ps` and `/proc` show, so such a command line cannot
 * be read either: on Linux it is the title followed by NUL padding over the old arguments, and
 * elsewhere one word from a program whose name starts with `node` (`node-22`, `nodejs`). A native
 * (Bun) build keeps its arguments, and its one-word command line means `claude` alone.
 */
export function processCommandLine(
  pid: number,
  executable: (pid: number) => string | undefined = processExecutable,
): CommandLine | undefined {
  let commandLine: CommandLine | undefined
  if (process.platform === "linux") {
    try {
      // Each argument ends in one NUL. A process that set its title (Node, for `process.title`)
      // leaves the title alone, which holds no NUL, then NUL padding over its old arguments, so
      // one piece before several empty ones is a title and its arguments are gone. Otherwise the
      // empty pieces past the final NUL are blank arguments (`--name ""`), which keep their place,
      // since dropping one would shift which word is a value.
      const pieces = readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0")
      let trailing = 0
      while (pieces.length > 0 && pieces.at(-1) === "") {
        pieces.pop()
        trailing++
      }
      if (trailing > 1 && pieces.length === 1) return undefined
      for (let blank = 1; blank < trailing; blank++) pieces.push("")
      const argv = pieces
      const terminal = stdoutIsTerminal(pid)
      const cwd = workingDirectory(pid)
      commandLine =
        argv.length === 0
          ? undefined
          : {
              argv,
              exact: true,
              ...(terminal === undefined ? {} : { stdoutIsTerminal: terminal }),
              ...(cwd === undefined ? {} : { cwd }),
            }
    } catch {
      commandLine = undefined
    }
  } else if (process.platform !== "win32") {
    const result = spawnSync("ps", ["-ww", "-o", "args=", "-p", String(pid)], {
      encoding: "utf8",
      timeout: 1_000,
    })
    const line = result.status === 0 ? result.stdout.trim() : ""
    commandLine = line === "" ? undefined : { argv: line.split(/\s+/), exact: false }
  }
  if (commandLine === undefined || commandLine.argv.length > 1) return commandLine
  return basename(executable(pid) ?? "").startsWith("node") ? undefined : commandLine
}

/** The parent of process `pid`, or undefined when it cannot be read. */
export function parentProcess(pid: number): number | undefined {
  try {
    if (process.platform === "linux") {
      // The command name, in parentheses, may hold spaces; the parent is the second field after it.
      const stat = readFileSync(`/proc/${pid}/stat`, "utf8")
      const parent = Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1])
      return Number.isInteger(parent) && parent > 0 ? parent : undefined
    }
    if (process.platform === "win32") return undefined
    const result = spawnSync("ps", ["-o", "ppid=", "-p", String(pid)], {
      encoding: "utf8",
      timeout: 1_000,
    })
    const parent = Number((result.stdout ?? "").trim())
    return result.status === 0 && Number.isInteger(parent) && parent > 0 ? parent : undefined
  } catch {
    return undefined
  }
}

/** How many processes up from the server `claudeCommandLine` looks for Claude Code. */
const MAX_CLAUDE_DEPTH = 8

/**
 * The command line of the Claude Code process that runs this server: the nearest of `pid` and its
 * ancestors whose program is `claude`, since Claude Code may run the server through a shell prefix
 * (`CLAUDE_CODE_SHELL_PREFIX`) or a version manager's `bun` shim. Undefined, read as "cannot tell",
 * when no such process is found or a command line cannot be read.
 */
export function claudeCommandLine(
  pid: number,
  read: (pid: number) => CommandLine | undefined = processCommandLine,
  parent: (pid: number) => number | undefined = parentProcess,
): CommandLine | undefined {
  let current: number | undefined = pid
  for (let depth = 0; current !== undefined && current > 1 && depth < MAX_CLAUDE_DEPTH; depth++) {
    const commandLine = read(current)
    if (commandLine === undefined) return undefined
    if (basename(commandLine.argv[0] ?? "") === "claude") return commandLine
    current = parent(current)
  }
  return undefined
}

/** The segments after `…/plugins/cache/` in a plugin install path, or undefined for any other path. */
function pluginCacheSegments(bundlePath: string): string[] | undefined {
  const parts = bundlePath.split(sep)
  for (let index = 0; index + 1 < parts.length; index++) {
    if (parts[index] === "plugins" && parts[index + 1] === "cache") return parts.slice(index + 2)
  }
  return undefined
}

/**
 * The marketplace a plugin install path names (`…/plugins/cache/<marketplace>/<plugin>/…`), or
 * undefined for any other path: a source checkout, `--plugin-dir`, or a bare server config.
 */
export function installedMarketplace(bundlePath: string): string | undefined {
  return pluginCacheSegments(bundlePath)?.[0]
}

/**
 * The version a plugin install path names (`…/plugins/cache/<marketplace>/<plugin>/<version>/…`,
 * the marketplace commit's first 12 characters for this plugin), or undefined for any other path.
 */
export function installedPluginVersion(bundlePath: string): string | undefined {
  return pluginCacheSegments(bundlePath)?.[2]
}

/** The marketplace Claude Code names a plugin it loads with `--plugin-dir` under. */
export const INLINE_MARKETPLACE = "inline"

/**
 * The marketplace this plugin, rooted at `pluginRoot`, is loaded from: `inline` when a
 * `--plugin-dir` value on the `claude` command line names that directory, the plugin cache's
 * marketplace when `bundlePath` lies in it, and undefined otherwise (a directory-source
 * marketplace, say), which the gate does not check.
 */
export function loadedMarketplace(
  pluginRoot: string,
  bundlePath: string,
  commandLine: { readonly argv: readonly string[]; readonly cwd: string } | undefined,
): string | undefined {
  if (commandLine !== undefined) {
    const { argv, cwd } = commandLine
    const root = resolve(pluginRoot)
    for (let index = 1; index < argv.length; index++) {
      const argument = argv[index] ?? ""
      if (argument === "--") break
      const equals = argument.indexOf("=")
      const flag = equals === -1 ? argument : argument.slice(0, equals)
      if (flag === "--plugin-dir" || flag === "--plugin-dir-no-mcp") {
        const value = equals === -1 ? argv[index + 1] : argument.slice(equals + 1)
        if (value !== undefined && resolve(cwd, value) === root) return INLINE_MARKETPLACE
      }
      if (equals === -1) index = lastValueIndex(argv, index)
    }
  }
  return installedMarketplace(bundlePath)
}

/** One `allowedChannelPlugins` entry as Claude Code accepts it: the documented object, or a
 *  `plugin@marketplace` string, which it also accepts with a warning. */
const AllowedChannelEntry = z.union([
  z.object({ marketplace: z.string(), plugin: z.string() }),
  z
    .string()
    .regex(/^[^@]+@[^@]+$/)
    .transform((value) => {
      const at = value.indexOf("@")
      return { plugin: value.slice(0, at), marketplace: value.slice(at + 1) }
    }),
])

/** An `allowedChannelPlugins` value's entries, each invalid one dropped on its own as Claude Code
 *  drops it; undefined when the value is not a list. */
function allowedEntries(value: unknown): readonly AllowedChannelPlugin[] | undefined {
  if (!Array.isArray(value)) return undefined
  return value.flatMap((entry: unknown) => {
    const parsed = AllowedChannelEntry.safeParse(entry)
    return parsed.success ? [parsed.data] : []
  })
}

/** Claude Code's managed settings directory on this platform. */
export function managedSettingsDirectory(): string {
  switch (process.platform) {
    case "darwin":
      return "/Library/Application Support/ClaudeCode"
    case "win32":
      return "C:\\Program Files\\ClaudeCode"
    default:
      return "/etc/claude-code"
  }
}

interface ChannelSettings {
  /** The file's `allowedChannelPlugins`, or undefined when it does not set one. */
  readonly list: readonly AllowedChannelPlugin[] | undefined
  /** The file's `managedSourcesBehavior`, when it sets one. */
  readonly behavior: string | undefined
}

/** Keys that say how sources compose, which make no settings file a source on their own. */
const COMPOSITION_KEYS: ReadonlySet<string> = new Set([
  "managedSourcesBehavior",
  "wslInheritsWindowsSettings",
])

/** A settings file's channel settings, or undefined when the file is missing or is not a JSON
 *  object, and (with `asSource`) when it holds nothing but `COMPOSITION_KEYS`, as Claude Code does
 *  not count such a file as a source. */
function readChannelSettings(file: string, asSource = false): ChannelSettings | undefined {
  try {
    const value: unknown = JSON.parse(readFileSync(file, "utf8"))
    if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined
    if (asSource && Object.keys(value).every((key) => COMPOSITION_KEYS.has(key))) return undefined
    const record = value as Record<string, unknown>
    const behavior = record["managedSourcesBehavior"]
    return {
      list: allowedEntries(record["allowedChannelPlugins"]),
      behavior: typeof behavior === "string" ? behavior : undefined,
    }
  } catch {
    return undefined
  }
}

/**
 * The `allowedChannelPlugins` of the managed settings Claude Code applies, read from files.
 * Server-managed settings (cached as `remote-settings.json` in the config directory) outrank an
 * MDM profile or the Windows registry, which outrank `managed-settings.json` with its
 * `managed-settings.d` drop-ins (`.json` files whose names do not start with a dot). Under
 * `managedSourcesBehavior` "first-wins", the default, the highest source present is the tier
 * alone; under "merge", the highest source that sets the key owns it. Undefined when that source
 * does not set the key, or none is present, which leaves Claude Code's own list. MDM and registry
 * settings are not read; `ENVOY_CHANNEL_LOADED=1` covers an allowlist only they deliver.
 */
export function channelAllowlist(directories: {
  readonly managedDirectory: string
  readonly configDirectory: string
}): readonly AllowedChannelPlugin[] | undefined {
  const remote = readChannelSettings(
    join(directories.configDirectory, "remote-settings.json"),
    true,
  )
  const dropIns = join(directories.managedDirectory, "managed-settings.d")
  let dropInFiles: string[] = []
  try {
    dropInFiles = readdirSync(dropIns)
      .filter((name) => name.endsWith(".json") && !name.startsWith("."))
      .sort()
      .map((name) => join(dropIns, name))
  } catch {
    dropInFiles = []
  }
  const files = [join(directories.managedDirectory, "managed-settings.json"), ...dropInFiles]
    .map((file) => readChannelSettings(file))
    .filter((settings) => settings !== undefined)
  const lists = files.map((settings) => settings.list).filter((list) => list !== undefined)
  const fileList = lists.length === 0 ? undefined : lists.flat()
  const behavior = remote?.behavior ?? files.find((settings) => settings.behavior)?.behavior
  if (remote === undefined) return fileList
  return behavior === "merge" ? (remote.list ?? fileList) : remote.list
}

/**
 * Whether this server's channel is loaded, for the `claude` process `claudePid` running the bundle
 * at `bundlePath`: as a plugin when Claude Code set `CLAUDE_PLUGIN_ROOT`, else from a bare MCP
 * config. `ENVOY_CHANNEL_LOADED=1` or `0` overrides the check.
 */
export function detectClaudeChannel(input: {
  readonly claudePid: number
  readonly bundlePath: string
  readonly environment: NodeJS.ProcessEnv
}): ChannelGate {
  const override = input.environment["ENVOY_CHANNEL_LOADED"]?.trim()
  if (override === "1") return { loaded: true }
  if (override === "0") return { loaded: false, why: "ENVOY_CHANNEL_LOADED=0" }
  const configDirectory =
    input.environment["CLAUDE_CONFIG_DIR"]?.trim() || join(homedir(), ".claude")
  const commandLine = claudeCommandLine(input.claudePid)
  const pluginRoot = input.environment["CLAUDE_PLUGIN_ROOT"]?.trim()
  const plugin = pluginRoot
    ? {
        name: "claude-envoy",
        // A `--plugin-dir` value is relative to the `claude` process's directory, which only
        // `/proc` gives, with the exact command line.
        marketplace: loadedMarketplace(
          pluginRoot,
          input.bundlePath,
          commandLine?.exact && commandLine.cwd !== undefined
            ? { argv: commandLine.argv, cwd: commandLine.cwd }
            : undefined,
        ),
      }
    : undefined
  return claudeChannelGate({
    commandLine,
    plugin,
    allowlist: channelAllowlist({ managedDirectory: managedSettingsDirectory(), configDirectory }),
  })
}
