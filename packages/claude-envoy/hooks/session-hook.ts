// Claude Code hook commands, one bundle for both; hooks.json names the mode as the argument. Each
// mode is a hook command of its own because Claude Code caps each hook's output at 10,000
// characters, so a long open-asks summary sharing a command with the skill could push it out.
//
// open-asks (SessionStart: startup, resume, clear, compact, fork). Two jobs:
// 1. Record the CURRENT session id for this Claude process so the channel
//    server, which keeps the id it was spawned with, can follow a `/clear`.
// 2. Put the session's open Dispatch asks into the model's context.
// Plain stdout becomes context; it always exits 0 so a Dispatch outage never
// blocks a session from starting.
//
// dispatch-first (SessionStart: startup, clear, compact; and SubagentStart): puts the
// dispatch-first skill into the model's context when Dispatch is configured for the project, as
// `hookSpecificOutput.additionalContext` under the event that ran it. A subagent holds the main
// conversation's Dispatch tools and starts with no SessionStart of its own. Resume is left out:
// Claude Code keeps SessionStart context in the transcript a resumed session reloads. A plugin
// installed without the skill file exits non-zero naming it, which Claude Code shows the user.

import { join } from "node:path"
import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config"
import { formatOpenAsksSummary } from "@legion/envoy-client/dispatch-execute"
import {
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { messageFor } from "@legion/envoy-client/errors"
import { z } from "zod"
import { claudeProjectDirectory, configuredValue } from "../src/claude-session"
import { sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"

const OPEN_ASKS_TIMEOUT_MS = 3_000
const OpenAsksInput = z.object({ session_id: z.string().min(1), cwd: z.string().optional() })
const DispatchFirstInput = z.object({
  hook_event_name: z.enum(["SessionStart", "SubagentStart"]),
  cwd: z.string().optional(),
})

async function openAsks(raw: unknown): Promise<void> {
  const input = OpenAsksInput.parse(raw)
  const pluginData = process.env["CLAUDE_PLUGIN_DATA"]
  if (pluginData !== undefined && pluginData.trim().length > 0) {
    try {
      await writeSessionHandoff(sessionHandoffFile(pluginData, process.ppid), input.session_id)
    } catch (error) {
      process.stderr.write(`envoy: could not record the session id — ${messageFor(error)}\n`)
    }
  }

  const directory = claudeProjectDirectory(
    { CLAUDE_PROJECT_DIR: process.env["CLAUDE_PROJECT_DIR"] },
    input.cwd ?? process.cwd(),
  )
  const config = resolveDispatchConfig(process.env, { cwd: directory })
  if (config.enabled && config.url !== null && config.token !== null) {
    try {
      const snapshot = await new DispatchClient(
        config.url,
        config.token,
        fetch,
        AbortSignal.timeout(OPEN_ASKS_TIMEOUT_MS),
      ).openAsks(input.session_id)
      process.stdout.write(
        `Dispatch authored-ask summary:\n${formatOpenAsksSummary(snapshot, config.url)}\n`,
      )
    } catch (error) {
      process.stdout.write(`Dispatch authored-ask summary unavailable: ${messageFor(error)}\n`)
    }
  } else if (config.error !== null) {
    process.stdout.write(`Dispatch authored-ask summary unavailable: ${config.error}\n`)
  }
}

function dispatchFirst(raw: unknown): void {
  const input = DispatchFirstInput.parse(raw)
  const directory = claudeProjectDirectory(
    { CLAUDE_PROJECT_DIR: process.env["CLAUDE_PROJECT_DIR"] },
    input.cwd ?? process.cwd(),
  )
  if (!resolveDispatchConfig(process.env, { cwd: directory }).enabled) return
  const pluginRoot = configuredValue(process.env["CLAUDE_PLUGIN_ROOT"])
  if (pluginRoot === undefined) {
    throw new Error("CLAUDE_PLUGIN_ROOT is unset; Claude Code sets it for every plugin hook")
  }
  process.stdout.write(
    JSON.stringify({
      hookSpecificOutput: {
        hookEventName: input.hook_event_name,
        additionalContext: readDispatchFirstContext(
          dispatchFirstSkillFile(join(pluginRoot, "skills")),
        ),
      },
    }),
  )
}

const mode = process.argv[2]
if (mode !== "open-asks" && mode !== "dispatch-first") {
  throw new Error(
    `session-hook: unknown mode ${JSON.stringify(mode)}; hooks.json names open-asks or dispatch-first`,
  )
}
const input: unknown = JSON.parse(await Bun.stdin.text())
if (mode === "open-asks") await openAsks(input)
else dispatchFirst(input)
