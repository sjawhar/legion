import { randomUUID } from "node:crypto"
import { readFileSync, renameSync, rmSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import type { LegionState } from "@legion/contracts/legion-api"
import { messageFor } from "@legion/envoy-client/errors"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import {
  createPhaseReminder,
  type PhaseReminderLog,
  type PhaseStallEntry,
} from "@legion/pi-shared/phase-reminder"
import { phaseStallFrom, phaseStallReportFrom } from "@legion/pi-shared/phase-stall"
import type { StallLimits } from "@legion/pi-shared/stall-limits"
import { z } from "zod"

/**
 * The endless phase reminder's Claude Code host (INFRA-220). A Claude Code phase worker runs no Oh
 * My Pi process, so the machine `packages/pi-shared/src/phase-reminder.ts` holds — the one
 * `packages/pi-legion/extensions/legion.ts` drives from Oh My Pi's session hooks — is driven here
 * instead, from Claude Code's own hooks: each fires an `mcp_tool` hook against `legion_phase_hook`
 * on the pane's `legion` MCP server, the server this file's endpoint is registered on
 * (`./envoy-channel-server`). Nothing of the machine is re-decided here.
 *
 * The record lives in a file rather than a transcript entry, `<CLAUDE_CONFIG_DIR>/
 * legion-phase-stall-<session id>.json`, which is the path and the write the Go bridge already
 * uses (`packages/daemon/internal/claudebridge/config.go`, `stall.go`), so a session carried
 * across the cutover keeps one record in one place.
 */

/** The tool's name: Claude Code shows it as `mcp__legion__legion_phase_hook`, and a hook entry
 * names it as the bare `legion_phase_hook` on the server `legion`. */
export const PHASE_HOOK_TOOL_NAME = "legion_phase_hook"

/** What the tool says it is to the model that sees it in the tool list, and what a call this
 * endpoint does not recognise is answered with. A model that calls the tool anyway must not be
 * able to move the state, so that answer records nothing. */
export const PHASE_HOOK_REFUSAL =
  "legion_phase_hook is Claude Code's hook endpoint for Legion's phase reminder: Claude Code's " +
  "own hooks call it with the event they fire for, and it does nothing for a call the model " +
  "makes. Do not call it."

/** The events the five generated hook entries fire (`legion/hooks/hooks.json`). */
const HOOK_EVENTS = [
  "Stop",
  "StopFailure",
  "PostToolUse",
  "PostToolUseFailure",
  "UserPromptSubmit",
] as const

/** One hook call's arguments: the event, and that event's own fields. Claude Code substitutes
 * `${…}` as strings, so every field is one, `stopHookActive` ("true"/"false") included. */
const hookArguments = z.object({
  event: z.enum(HOOK_EVENTS),
  lastAssistantMessage: z.string().optional(),
  stopHookActive: z.string().optional(),
  toolName: z.string().optional(),
  agentId: z.string().optional(),
  prompt: z.string().optional(),
})

/** The tool as `tools/list` declares it. */
export function phaseHookToolDefinition() {
  return {
    name: PHASE_HOOK_TOOL_NAME,
    description: PHASE_HOOK_REFUSAL,
    inputSchema: z.toJSONSchema(hookArguments),
  }
}

/** One hook call's answer, as the tool's one JSON text block: `{}` when there is nothing to say,
 * `{"decision":"block","reason":…}` on the `Stop` that carries a reminder — which Claude Code
 * turns into one more turn of the same session, holding the reason — or the refusal. */
export interface PhaseHookAnswer {
  readonly decision?: "block"
  readonly reason?: string
  readonly error?: string
}

export interface PhaseHookEndpoint {
  /** Answers one hook call, with the record written before the answer. */
  readonly handle: (input: unknown) => Promise<PhaseHookAnswer>
}

export interface PhaseHookDeps {
  /** The record's file (`phaseStallFile`). */
  readonly stateFile: string
  /** The pane's registered claim, or undefined while its boot is still registering. */
  readonly claim: () => ClaimCapability | undefined
  /** The daemon's whole state, for the reminder's record check. */
  readonly readState: () => Promise<LegionState>
  /** `LEGION_PROJECT`: the project token every role topic carries. */
  readonly project: string | undefined
  readonly limits: StallLimits
  readonly log: PhaseReminderLog
}

/** `<CLAUDE_CONFIG_DIR>/legion-phase-stall-<session id>.json`, the path the Go bridge's
 * `Configure` builds (`packages/daemon/internal/claudebridge/config.go`). */
export function phaseStallFile(configDir: string, sessionID: string): string {
  return join(configDir, `legion-phase-stall-${sessionID}.json`)
}

/** `PhaseReminderLog` over the pane's own log (`logLegion`, `./legion-claim`): one line each,
 * with its context as JSON. */
export function paneReminderLog(write: (line: string) => void): PhaseReminderLog {
  const line = (level: string, message: string, context?: Record<string, unknown>): void => {
    write(
      context === undefined
        ? `${level}: ${message}`
        : `${level}: ${message} ${JSON.stringify(context)}`,
    )
  }
  return {
    debug: (message, context) => line("debug", message, context),
    warn: (message, context) => line("warn", message, context),
  }
}

/**
 * The record `file` holds, or undefined when it holds none: a missing file is no record at all, so
 * a fresh session restores nothing. A file written by the build now in the pods holds only
 * `{"state":"quiet"}` or `{"state":"text-call"}`, and `phaseStallFrom` is the one place that maps
 * those to an open phase with no reminders yet; a missing `count`, `reported` or `reportKey` reads
 * as 0, false and absent.
 */
export function readPhaseStallRecord(
  file: string,
  log: PhaseReminderLog,
): PhaseStallEntry | undefined {
  let raw: string
  try {
    raw = readFileSync(file, "utf8")
  } catch (error) {
    const missing =
      typeof error === "object" && error !== null && "code" in error && error.code === "ENOENT"
    if (!missing) {
      log.warn("legion: the phase-stall state could not be read; this session starts afresh", {
        file,
        error: messageFor(error),
      })
    }
    return undefined
  }
  let data: unknown
  try {
    data = JSON.parse(raw)
  } catch (error) {
    log.warn("legion: the phase-stall state is not JSON; this session starts afresh", {
      file,
      error: messageFor(error),
    })
    return undefined
  }
  if (typeof data !== "object" || data === null) {
    log.warn("legion: the phase-stall state is not an object; this session starts afresh", { file })
    return undefined
  }
  const stall = phaseStallFrom(data)
  if (stall === undefined) {
    log.warn("legion: the phase-stall state holds no state this build knows; starting afresh", {
      file,
    })
    return undefined
  }
  const report = phaseStallReportFrom(data)
  return {
    state: stall.state,
    count: stall.count,
    reported: report.reported,
    ...(report.reportKey === undefined ? {} : { reportKey: report.reportKey }),
  }
}

/** Replaces `file` with `entry`, through a temporary file in the same directory renamed into
 * place, so a process killed mid-write never leaves half a record to read: the Go bridge's
 * `writeStall` (`packages/daemon/internal/claudebridge/stall.go`), matched. A write that fails is
 * logged rather than thrown: the worker still gets its reminder. */
export function writePhaseStallRecord(
  file: string,
  entry: PhaseStallEntry,
  log: PhaseReminderLog,
): void {
  const temporary = join(dirname(file), `.legion-phase-stall-${randomUUID()}`)
  try {
    writeFileSync(temporary, JSON.stringify(entry), { mode: 0o600 })
    renameSync(temporary, file)
  } catch (error) {
    rmSync(temporary, { force: true })
    log.warn("legion: the phase-stall state is not recorded for a relaunch", {
      file,
      error: messageFor(error),
    })
  }
}

/**
 * The endpoint the pane's `legion_phase_hook` tool calls, over this session's own record, which is
 * restored here before any hook can be answered.
 */
export function createPhaseHookEndpoint(deps: PhaseHookDeps): PhaseHookEndpoint {
  const reminder = createPhaseReminder({
    // INFRA-220's later task delivers every reminder after the `Stop` continuation as a user turn
    // through the bridge's inbox. Nothing arms a timer here yet, so nothing reaches this.
    sendReminder: (text) => {
      deps.log.warn("legion: a phase reminder has no way to reach this worker yet", {
        reminder: text,
      })
    },
    // INFRA-220's later task arms the timer that carries the second and later reminders of a run;
    // until it does, the `Stop` continuation is the only reminder, and the count holds the rest.
    armTimer: () => {},
    // The record lands before the hook answers: every change of it is this synchronous write, and
    // the machine makes it inside `settle` before that promise resolves. Claude Code may restart
    // this server inside the same `claude` process, and a reminder the worker was given but whose
    // count was never recorded is what the count exists to catch.
    saveEntry: (entry) => writePhaseStallRecord(deps.stateFile, entry, deps.log),
    claim: deps.claim,
    readState: deps.readState,
    // INFRA-220's later task builds the listener the architect notice goes to. Until then no
    // delivered reminder reaches the threshold, since only the timer delivers one.
    publisher: () => {
      deps.log.debug("legion: this build tells no architect about a stalled worker")
      return undefined
    },
    project: deps.project,
    limits: deps.limits,
    log: deps.log,
  })
  reminder.restore(readPhaseStallRecord(deps.stateFile, deps.log))

  return {
    handle: async (input) => {
      const parsed = hookArguments.safeParse(input)
      // An event this endpoint does not know, or none at all, is the model calling the tool
      // itself: it is refused, and no state moves, so it cannot zero its own count.
      if (!parsed.success) return { error: PHASE_HOOK_REFUSAL }
      const event = parsed.data
      // The same gate as `phaseWorkerSession` in `packages/pi-legion/extensions/legion.ts`: the
      // machine runs for a phase worker whose claim has booted, and for nobody else. The bridge's
      // seeding `/rename` turn is what writes the transcript the claim boot waits for
      // (`waitForTranscript`, `./legion-claim`), so it runs before any claim exists, answers `{}`
      // here, and is never reminded.
      const claim = deps.claim()
      if (claim === undefined || claim.role === "architect") return {}
      switch (event.event) {
        case "Stop": {
          // `stop_hook_active` arrives as "true" once this hook has already continued the turn. It
          // decides nothing: the count is what bounds a run of reminders.
          deps.log.debug("legion: a Claude Code phase worker's turn is settling", {
            issue: claim.issue,
            role: claim.role,
            stopHookActive: event.stopHookActive ?? "",
          })
          const reminded = await reminder.settle(event.lastAssistantMessage ?? "")
          return reminded === undefined ? {} : { decision: "block", reason: reminded }
        }
        // INFRA-220's later task gives these four their meaning: `UserPromptSubmit` classifies the
        // inbound kind, `PostToolUse` and `PostToolUseFailure` reset the count, and `StopFailure`
        // settles a turn that ended in an error. Until then each answers `{}` and records nothing.
        case "StopFailure":
        case "PostToolUse":
        case "PostToolUseFailure":
        case "UserPromptSubmit":
          return {}
      }
    },
  }
}
