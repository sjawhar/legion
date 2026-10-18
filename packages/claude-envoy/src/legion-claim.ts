import { readdir, stat } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import type { LegionRole } from "@legion/contracts"
import type { LegionRegistration } from "@legion/contracts/legion-api"
import { messageFor } from "@legion/envoy-client/errors"
import { readSecretFile } from "@legion/envoy-client/secret-file"
import type { EnvoyClient } from "@legion/envoy-client/transport"
import {
  callReadyWithRetry,
  LegionDaemonApiError,
  type LegionDaemonClient,
  type ReadyInput,
} from "@legion/pi-shared/daemon-client"
import { legion } from "../package.json" with { type: "json" }
import { configuredValue } from "./claude-session"
import type { ChannelSession } from "./envoy-channel-server"

/**
 * A Claude Code phase worker's claim on the Legion daemon, made by the MCP server Claude Code runs
 * for the pane (`dist/envoy-channel.js`, under the server name `legion`): what pi-legion's claim
 * session does in an Oh My Pi pane. The daemon launches the pane with its boot token
 * (`LEGION_BOOT_TOKEN_FILE`) and its address (`LEGION_DAEMON_URL`); the server registers the
 * session, claims the claim token's Envoy role, says it is ready, and says so again each time the
 * heartbeat takes the role back.
 */

/** What the daemon put on a Legion pane: its address, the boot token the registration spends, and
 * the claim's own Claude Code directory (`CLAUDE_CONFIG_DIR`), which holds the transcript. */
export interface LegionPane {
  readonly daemonUrl: string
  readonly bootToken: string
  readonly configDir: string
}

/** The claim a session registered: what the `legion` tool's operations need. */
export interface LegionClaimCapability {
  readonly sessionID: string
  readonly claimToken: string
  readonly tree: string
  readonly issue: string
  readonly role: LegionRole
  readonly secret: string
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

/**
 * Stops the pane's Claude Code, this server's parent, so the daemon launches the claim again: what
 * an Oh My Pi pane's own exit does. `legion claude-bridge` exits with Claude Code's status, and the
 * shim reports the death.
 */
export function stopPane(why: string): void {
  process.stderr.write(`[legion] ${why}; stopping the pane so the daemon launches it again\n`)
  process.kill(process.ppid, "SIGTERM")
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
  readonly capability: () => LegionClaimCapability | undefined
  /** Settles once the boot has run: ready sent, or the failure reported. Never rejects. */
  readonly booted: Promise<void>
}

/**
 * Boots the pane's claim in the background of the server: waits for the transcript, registers
 * with it as the session file (`agentId` is the session id, `pluginContract` this package's
 * `legion.daemonApiVersion`), claims the claim token's Envoy role soft and records it with the
 * channel session, whose heartbeat re-asserts it, and sends `claims/ready` with no capability
 * report. Each time the heartbeat regains the role it sends ready again.
 *
 * A registration the daemon refuses (a 4xx: a spent boot token, another contract) and any failure
 * after a registration stop the pane through `stopPane`, so the daemon launches it again; a 5xx or
 * a request that never reached the daemon is reported and leaves the pane to the daemon's
 * registration deadline.
 */
export function startLegionClaim(deps: {
  readonly pane: LegionPane
  readonly sessionID: string
  readonly daemon: LegionDaemonClient
  readonly client: Pick<EnvoyClient, "setRole">
  readonly session: Pick<ChannelSession, "rememberRole" | "onRoleRegained">
  readonly signal: AbortSignal
  readonly stopPane: (why: string) => void
  readonly pollMs?: number
}): LegionClaim {
  let capability: LegionClaimCapability | undefined
  const { daemon, sessionID } = deps

  const boot = async (): Promise<void> => {
    const transcript = await waitForTranscript(
      deps.pane.configDir,
      sessionID,
      deps.signal,
      deps.pollMs,
    )
    let claim: LegionRegistration
    try {
      claim = await daemon.register({
        bootToken: deps.pane.bootToken,
        sessionId: sessionID,
        ompSessionFile: transcript,
        agentId: sessionID,
        pluginContract: legion.daemonApiVersion,
      })
    } catch (error) {
      if (!(error instanceof LegionDaemonApiError) || error.status >= 500) throw error
      deps.stopPane(`claims/register registration failed (${error.status}): ${error.detail}`)
      return
    }
    try {
      capability = {
        sessionID,
        claimToken: claim.claimToken,
        tree: claim.tree,
        issue: claim.issue,
        role: claim.role,
        secret: claim.secret,
      }
      // The claim's role topic is where the daemon sends the worker's task and every notice.
      const role = await deps.client.setRole({ sessionID, role: claim.claimToken, soft: true })
      if (!role.claimed) {
        throw new Error(`Envoy role ${claim.claimToken} is held by live session ${role.holder}`)
      }
      await deps.session.rememberRole(claim.claimToken)
      const ready: ReadyInput = {
        claimToken: claim.claimToken,
        sessionId: sessionID,
        secret: claim.secret,
        generation: claim.generation,
      }
      await callReadyWithRetry("claims/ready", () => daemon.ready(ready))
      deps.session.onRoleRegained(async (regained, reason) => {
        if (regained !== claim.claimToken) return
        try {
          await callReadyWithRetry("claims/ready after role regain", () => daemon.ready(ready))
          process.stderr.write(
            `[legion] re-ran claims/ready after role ${regained} was ${reason}\n`,
          )
        } catch (error) {
          process.stderr.write(
            `[legion] claims/ready after role ${regained} was ${reason} failed; the daemon's queued task stays undelivered until the next regain or boot: ${messageFor(error)}\n`,
          )
        }
      })
    } catch (error) {
      deps.stopPane(
        `claim boot failed after claims/register registered ${claim.claimToken}: ${messageFor(error)}`,
      )
    }
  }

  const booted = boot().catch((error: unknown) => {
    if (deps.signal.aborted) return
    process.stderr.write(
      `[legion] claim boot failed before a registration; the daemon's registration deadline relaunches the pane: ${messageFor(error)}\n`,
    )
  })
  return { capability: () => capability, booted }
}
