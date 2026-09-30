// Claude Code SessionStart hook (startup, clear, compact): puts the dispatch-first skill into the
// model's context when Dispatch is configured for the project. It is a command of its own, not
// part of the open-asks hook, because Claude Code caps each hook's output at 10,000 characters
// and a long open-asks summary would push the skill out of a shared one. Resume is left out:
// Claude Code keeps SessionStart context in the transcript a resumed session reloads. A plugin
// installed without the skill file exits non-zero naming it, which Claude Code shows the user.

import { join } from "node:path"
import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config"
import {
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first"
import { z } from "zod"
import { claudeProjectDirectory, configuredValue } from "../src/claude-session"

const HookInput = z.object({ cwd: z.string().optional() })

const input = HookInput.parse(JSON.parse(await Bun.stdin.text()))
const directory = claudeProjectDirectory(
  { CLAUDE_PROJECT_DIR: process.env["CLAUDE_PROJECT_DIR"] },
  input.cwd ?? process.cwd(),
)
if (resolveDispatchConfig(process.env, { cwd: directory }).enabled) {
  const pluginRoot = configuredValue(process.env["CLAUDE_PLUGIN_ROOT"])
  if (pluginRoot === undefined) {
    throw new Error("CLAUDE_PLUGIN_ROOT is unset; Claude Code sets it for every plugin hook")
  }
  process.stdout.write(
    JSON.stringify({
      hookSpecificOutput: {
        hookEventName: "SessionStart",
        additionalContext: readDispatchFirstContext(
          dispatchFirstSkillFile(join(pluginRoot, "skills")),
        ),
      },
    }),
  )
}
