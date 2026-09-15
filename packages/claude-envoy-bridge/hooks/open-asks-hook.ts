// Claude Code SessionStart hook (startup, resume, clear, compact, fork). Two jobs:
// 1. Record the CURRENT session id for this Claude process so the channel
//    server, which keeps the id it was spawned with, can follow a `/clear`.
// 2. Put the session's open Dispatch asks into the model's context, the same
//    summary pi-envoy injects before an OMP agent's first turn.
// Plain stdout becomes context; the hook always exits 0 so a Dispatch outage
// never blocks a session from starting.

import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config"
import { formatOpenAsksSummary } from "@legion/envoy-client/dispatch-execute"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { messageFor } from "@legion/envoy-client/errors"
import { z } from "zod"
import { claudeProjectDirectory } from "../src/claude-session"
import { sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"

const OPEN_ASKS_TIMEOUT_MS = 3_000
const HookInput = z.object({ session_id: z.string().min(1), cwd: z.string().optional() })

const input = HookInput.parse(JSON.parse(await Bun.stdin.text()))

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
