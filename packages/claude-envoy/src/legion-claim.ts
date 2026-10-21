import { readdir, stat } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { messageFor } from "@legion/envoy-client/errors"
import { readSecretFile } from "@legion/envoy-client/secret-file"
import type { EnvoyClient } from "@legion/envoy-client/transport"
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client"
import { bootLegionClaim } from "@legion/pi-shared/legion-claim-core"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import { legion } from "../package.json" with { type: "json" }
import { configuredValue } from "./claude-session"
import type { ChannelSession } from "./envoy-channel-server"

/**
 * A Claude Code phase worker's claim on the Legion daemon, made by the MCP server Claude Code runs
 * for the pane (`dist/envoy-channel.js`, under the server name `legion`) through the claim boot
 * every harness shares (`@legion/pi-shared/legion-claim-core`). The daemon launches the pane with
 * its boot token (`LEGION_BOOT_TOKEN_FILE`) and its address (`LEGION_DAEMON_URL`).
 */

/** What the daemon put on a Legion pane: its address, the boot token the registration spends, and
 * the claim's own Claude Code directory (`CLAUDE_CONFIG_DIR`), which holds the transcript. */
export interface LegionPane {
  readonly daemonUrl: string
  readonly bootToken: string
  readonly configDir: string
}

/**
 * The Legion pane this server runs in, or undefined when no boot token is on its environment (a
 * Claude Code session outside Legion). A set `LEGION_BOOT_TOKEN_FILE` is authoritative, as in
 * every pane: a missing or blank file throws naming it. A pane without `LEGION_DAEMON_URL` or
 * `CLAUDE_CONFIG_DIR` throws naming the variable.
 */
export function legionPane(env: NodeJS.ProcessEnv): LegionPane | undefined {
  const pointer = env["LEGION_BOOT_TOKEN_FILE"]
  if (pointer === undefined && configuredValue(env["LEGION_BOOT_TOKEN"]) === undefined) {
    return undefined
  }
  const required = (key: string): string => {
    const value = configuredValue(env[key])
    if (value === undefined) {
      throw new Error(`${key} is required for Legion: the pane carries a boot token`)
    }
    return value
  }
  return {
    daemonUrl: required("LEGION_DAEMON_URL"),
    configDir: required("CLAUDE_CONFIG_DIR"),
    bootToken:
      pointer === undefined
        ? required("LEGION_BOOT_TOKEN")
        : readSecretFile("LEGION_BOOT_TOKEN_FILE", pointer),
  }
}

/** Writes one line to the pane's log, the server's stderr. */
export function logLegion(line: string): void {
  process.stderr.write(`[legion] ${line}\n`)
}

/**
 * Ends the pane's Claude Code, this server's parent, so the daemon launches the claim again: what
 * an Oh My Pi pane's own exit does. `legion claude-bridge` exits with Claude Code's status, and the
 * shim reports the death.
 */
export function endPane(): void {
  process.kill(process.ppid, "SIGTERM")
}

/** Logs why the pane cannot run, then ends it (`endPane`). */
export function stopPane(why: string): void {
  logLegion(`${why}; stopping the pane so the daemon launches it again`)
  endPane()
}

/**
 * The transcript Claude Code writes for `sessionID`, `<configDir>/projects/<cwd>/<id>.jsonl`, once
 * it exists: the value a relaunch's `--resume` continues. A fresh session writes nothing until its
 * first input, which is the bridge's `/rename` seed (`packages/daemon/internal/claudebridge`); a
 * resumed one already has it. Polls every `pollMs` until `signal` aborts.
 */
export async function waitForTranscript(
  configDir: string,
  sessionID: string,
  signal: AbortSignal,
  pollMs = 100,
): Promise<string> {
  const projects = join(configDir, "projects")
  const name = `${sessionID}.jsonl`
  for (;;) {
    signal.throwIfAborted()
    for (const directory of await readdir(projects).catch(() => [] as string[])) {
      const file = join(projects, directory, name)
      if (
        await stat(file).then(
          (info) => info.isFile(),
          () => false,
        )
      )
        return file
    }
    await delay(pollMs, undefined, { signal })
  }
}

export interface LegionClaim {
  /** The registered claim, or undefined until `claims/register` has answered. */
  readonly capability: () => ClaimCapability | undefined
  /** Settles once the boot has run: ready sent, or the failure reported. Never rejects. */
  readonly booted: Promise<void>
}

/**
 * Boots the pane's claim in the background of the server (`bootLegionClaim`): waits for the
 * transcript, registers with it as the session file (`agentId` is the session id,
 * `pluginContract` this package's `legion.daemonApiVersion`), claims the claim token's Envoy role
 * soft and records it with the channel session, whose heartbeat re-asserts it, and sends
 * `claims/ready` with no capability report, again each time the heartbeat regains the role. A
 * failure the core stops on ends the pane through `stop`; a failure before a registration is
 * logged and leaves the pane to the daemon's registration deadline.
 */
export function startLegionClaim(deps: {
  readonly pane: LegionPane
  readonly sessionID: string
  readonly daemon: LegionDaemonClient
  readonly client: Pick<EnvoyClient, "setRole">
  readonly session: Pick<ChannelSession, "rememberRole" | "onRoleRegained">
  readonly signal: AbortSignal
  /** Writes one line to the pane's log, the server's stderr. */
  readonly log: (line: string) => void
  /** Ends the pane's Claude Code, this server's parent, so the daemon launches the claim again. */
  readonly stop: () => void
  readonly pollMs?: number
}): LegionClaim {
  let capability: ClaimCapability | undefined
  const { sessionID } = deps

  const boot = async (): Promise<void> => {
    const transcript = await waitForTranscript(
      deps.pane.configDir,
      sessionID,
      deps.signal,
      deps.pollMs,
    )
    await bootLegionClaim({
      daemon: deps.daemon,
      registration: {
        bootToken: deps.pane.bootToken,
        sessionId: sessionID,
        ompSessionFile: transcript,
        agentId: sessionID,
        pluginContract: legion.daemonApiVersion,
      },
      setCapability: (registered) => {
        capability = registered
      },
      claimRole: async (role) => {
        const claimed = await deps.client.setRole({ sessionID, role, soft: true })
        if (!claimed.claimed) {
          throw new Error(`Envoy role ${role} is held by live session ${claimed.holder}`)
        }
        await deps.session.rememberRole(role)
      },
      onRoleRegained: (listener) => deps.session.onRoleRegained(listener),
      log: deps.log,
      stopping: "stopping the pane so the daemon launches it again",
      stop: deps.stop,
    })
  }

  const booted = boot().catch((error: unknown) => {
    if (deps.signal.aborted) return
    deps.log(
      `claim boot failed before a registration; the daemon's registration deadline relaunches the pane: ${messageFor(error)}`,
    )
  })
  return { capability: () => capability, booted }
}
