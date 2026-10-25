import { randomUUID } from "node:crypto"
import { readFileSync, renameSync, rmSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import type { LegionState } from "@legion/contracts/legion-api"
import { messageFor } from "@legion/envoy-client/errors"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import {
  createPhaseReminder,
  type EnvoyPublish,
  type PhaseReminderLog,
} from "@legion/pi-shared/phase-reminder"
import { type PhaseStallEntry, phaseStallEntryFrom } from "@legion/pi-shared/phase-stall"
import { stallLimits } from "@legion/pi-shared/stall-limits"
import { workspaceRecreatedNotice } from "@legion/pi-shared/workspace-recreated"
import { z } from "zod"
import { configuredValue } from "./claude-session"

/**
 * The endless phase reminder's Claude Code host (INFRA-220). A Claude Code phase worker runs no Oh
 * My Pi process, so the machine `packages/pi-shared/src/phase-reminder.ts` holds — the one
 * `packages/pi-legion/extensions/legion.ts` drives from Oh My Pi's session hooks — is driven here
 * instead, from Claude Code's own hooks: each fires an `mcp_tool` hook against `legion_phase_hook`
 * on the pane's `legion` MCP server, the server this file's endpoint is registered on
 * (`./envoy-channel-server`). Nothing of the machine is re-decided here.
 *
 * The record lives in a file rather than a transcript entry, `<CLAUDE_CONFIG_DIR>/
 * legion-phase-stall-<session id>.json`. Nothing else writes that file: the Go bridge, which
 * wrote it until this change made it pure transport, is now only what carries a line into the
 * session. The path and the atomic rename are kept as the bridge left them, so a session carried
 * across the cutover from the build now in the pods still reads its record.
 *
 * The first reminder of a run is the `Stop` hook's `block`, which Claude Code turns into one more
 * turn of the same session. Every one after it is written into the session over the bridge's
 * inbox, from a timer this server arms at a turn's end: a line written while a turn is running is
 * appended to that turn as its next step (`deliver`/`step` in
 * `packages/daemon/internal/claudebridge/bridge.go`), which would destroy the turn boundary the
 * whole split exists for.
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

/**
 * How many inbox lines this server wrote are kept waiting for the prompt that carries them back.
 * Each is dropped as its prompt arrives, so the set stays at the one or two lines in flight; the
 * cap is what bounds it when a line never comes back at all, because the bridge handed two of
 * them to one turn. A turn whose line was dropped by the cap counts as an assignment rather than
 * an inbound event: both put the count back to 0, but an assignment reopens a phase an inbound
 * event would leave closed, so the mistake is the unsafe direction and the record check is all
 * that bounds it. The cap is high enough that only a bridge that lost a line reaches it.
 */
const WRITTEN_LINES_KEPT = 32

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
  /** One line this server has just written into the bridge's inbox for an Envoy delivery. The
   * prompt that carries it back is that delivery's turn, not a fresh assignment, and this is the
   * only way the endpoint can tell the two apart: the server writes every delivery itself. */
  readonly deliveryWritten: (content: string) => void
  /** The `legion` tool's `handoff_complete`, or its `request_backward_move`, returned without
   * throwing: this session's phase is no longer open. The CallTool handler of the same server
   * calls it (`./envoy-channel-server`), as `onPhaseCompleted` does on the Oh My Pi host. */
  readonly phaseClosed: () => void
  /** Drops the armed timer, so a 15-minute WAITING wait never holds a shutting-down server
   * open (the server's own `stop`). */
  readonly stop: () => void
}

export interface PhaseHookDeps {
  /** The record's file (`phaseStallFile`). */
  readonly stateFile: string
  /** The pane's registered claim, or undefined while its boot is still registering. */
  readonly claim: () => ClaimCapability | undefined
  /** Settles once the pane's claim boot has run (`startLegionClaim`'s `booted`). The resume
   * reminder and the recreated-workspace notice both need the claim, and neither is sent before
   * one exists. Never rejects. */
  readonly booted: Promise<void>
  /** The daemon's whole state, for the reminder's record check. */
  readonly readState: () => Promise<LegionState>
  /** Writes one line into the session over the bridge's inbox, which turns it into a user turn,
   * and resolves once the bridge has queued it. Undefined for a session outside a Legion pane,
   * where nothing can reach the worker between its turns. */
  readonly inbox: ((text: string) => Promise<void>) | undefined
  /** The server's own Envoy client's `publish`, or undefined when Envoy is off for the session:
   * the architect is then not told, and the count still climbs. */
  readonly publish: EnvoyPublish | undefined
  /** The worker's environment: `LEGION_PROJECT`, the four `LEGION_STALL_*` numbers and
   * `LEGION_WORKSPACE_RECREATED`. */
  readonly env: Readonly<Record<string, string | undefined>>
  readonly log: PhaseReminderLog
}

/** `<CLAUDE_CONFIG_DIR>/legion-phase-stall-<session id>.json`, the path the Go bridge built while
 * it kept this record itself: kept as it was, so a session carried across the cutover from the
 * build now in the pods finds the record where it left it. */
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
 * What this host's state file holds: the record the machine shares with the Oh My Pi host, and the
 * one key only this host keeps. `PhaseStallEntry` is the machine's record and both hosts read it,
 * so the extra key is composed around it here rather than added to it, and a record the Go bridge
 * left behind, which has no such key, still reads.
 */
export interface PhaseStallRecord {
  /** The phase and count the file holds, or undefined when it holds none this build knows. */
  readonly entry: PhaseStallEntry | undefined
  /** Whether this launch's recreated-workspace notice has already been written into the session
   * (`recreatedNoticeSent` in the file); false for a record that names no such key. */
  readonly recreatedNoticeSent: boolean
}

/** A file this session holds no record in: a fresh session, an unreadable one, or one written by
 * a build whose state this one does not know. */
const NO_RECORD: PhaseStallRecord = { entry: undefined, recreatedNoticeSent: false }

/**
 * The record `file` holds: a missing file is no record at all, so a fresh session restores
 * nothing. The entry itself is assembled where the Oh My Pi host's is
 * (`phaseStallEntryFrom`), so a record written by the build now in the pods —
 * `{"state":"quiet"}` or `{"state":"text-call"}` — reads as an open phase with no reminders yet
 * here too, and a missing `count`, `reported` or `reportKey` reads as 0, false and absent. This
 * host adds the warning for a record it understands none of, and its own `recreatedNoticeSent`.
 */
export function readPhaseStallRecord(file: string, log: PhaseReminderLog): PhaseStallRecord {
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
    return NO_RECORD
  }
  let data: unknown
  try {
    data = JSON.parse(raw)
  } catch (error) {
    log.warn("legion: the phase-stall state is not JSON; this session starts afresh", {
      file,
      error: messageFor(error),
    })
    return NO_RECORD
  }
  if (typeof data !== "object" || data === null) {
    log.warn("legion: the phase-stall state is not an object; this session starts afresh", { file })
    return NO_RECORD
  }
  const entry = phaseStallEntryFrom(data)
  if (entry === undefined) {
    log.warn("legion: the phase-stall state holds no state this build knows; starting afresh", {
      file,
    })
    return NO_RECORD
  }
  return {
    entry,
    recreatedNoticeSent: "recreatedNoticeSent" in data && data.recreatedNoticeSent === true,
  }
}

/** Replaces `file` with `entry` and this host's own key, through a temporary file in the same
 * directory renamed into place, so a process killed mid-write never leaves half a record to read:
 * the shape the Go bridge wrote the same file in while it kept this record, kept so a session
 * carried across the cutover reads whatever the build now in the pods left. The key is written
 * only once it is true, so a record this host writes is otherwise the shared entry and nothing
 * more. A write that fails is logged rather than thrown: the worker still gets its reminder. */
export function writePhaseStallRecord(
  file: string,
  entry: PhaseStallEntry,
  recreatedNoticeSent: boolean,
  log: PhaseReminderLog,
): void {
  const temporary = join(dirname(file), `.legion-phase-stall-${randomUUID()}`)
  try {
    writeFileSync(
      temporary,
      JSON.stringify({ ...entry, ...(recreatedNoticeSent ? { recreatedNoticeSent } : {}) }),
      { mode: 0o600 },
    )
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
  const limits = stallLimits(deps.env, deps.log)
  const restored = readPhaseStallRecord(deps.stateFile, deps.log)
  // The machine's entry and this host's own key are one record, so the last entry written is kept
  // for the write the notice makes.
  let entry: PhaseStallEntry = restored.entry ?? { state: "closed", count: 0, reported: false }
  let recreatedNoticeSent = restored.recreatedNoticeSent
  const saveRecord = (next: PhaseStallEntry): void => {
    entry = next
    writePhaseStallRecord(deps.stateFile, next, recreatedNoticeSent, deps.log)
  }

  /** The one timer this server has armed, or undefined when none is. One at a time: arming the
   * next drops it, and so does the server's `stop`. */
  let timer: ReturnType<typeof setTimeout> | undefined
  const dropTimer = (): void => {
    if (timer === undefined) return
    clearTimeout(timer)
    timer = undefined
  }
  const armTimer = (callback: () => void | Promise<void>, delayMs: number): void => {
    dropTimer()
    timer = setTimeout(() => {
      timer = undefined
      void callback()
    }, delayMs)
  }

  /** The reminder last written to the inbox: the prompt that carries it back is this endpoint's
   * own reminder, and a reminder must never reset the count it just moved. */
  let lastReminder: string | undefined
  /** The lines this server wrote into the inbox for something that is not a reminder — an Envoy
   * delivery, the recreated-workspace notice — whose prompt has not arrived yet. Each is dropped
   * as its prompt arrives (`WRITTEN_LINES_KEPT` bounds the rest). */
  const written = new Set<string>()
  const noteWritten = (content: string): void => {
    written.add(content)
    for (const oldest of written) {
      if (written.size <= WRITTEN_LINES_KEPT) break
      written.delete(oldest)
    }
  }

  const writeToSession = (text: string, what: string): void => {
    const { inbox } = deps
    if (inbox === undefined) {
      deps.log.warn(`legion: ${what} has no way to reach this worker: this session has no inbox`, {
        text,
      })
      return
    }
    void inbox(text).catch((error: unknown) => {
      deps.log.warn(`legion: ${what} did not reach this worker`, { error: messageFor(error) })
    })
  }

  const sendReminder = (text: string): void => {
    lastReminder = text
    writeToSession(text, "a phase reminder")
  }

  const reminder = createPhaseReminder({
    // The bridge turns one inbox line into a user turn, which is what every reminder after the
    // `Stop` continuation is.
    sendReminder,
    armTimer,
    // The record lands before the hook answers: every change of it is this synchronous write, and
    // the machine makes it inside `settle` before that promise resolves. Claude Code may restart
    // this server inside the same `claude` process, and a reminder the worker was given but whose
    // count was never recorded is what the count exists to catch.
    saveEntry: saveRecord,
    claim: deps.claim,
    readState: deps.readState,
    // The server's own listener client, so the notice reaches the one this session already talks
    // to; Envoy off for the session leaves it undefined, the runner says so once at debug, and
    // the reminder keeps coming without a notice.
    publisher: () => deps.publish,
    project: configuredValue(deps.env["LEGION_PROJECT"]),
    limits,
    log: deps.log,
  })
  reminder.restore(restored.entry)

  /**
   * D29: a worker whose launcher recreated its workspace is told so, as an Oh My Pi one is, over
   * the same inbox the reminders use. Once per launch: the record it is written in is the one the
   * phase and count live in, so a server Claude Code restarts inside the same `claude` process
   * does not repeat it. The record lands before the line does — a notice a broken socket loses is
   * one notice lost, while one written twice is the thing it must not be — and the line is noted
   * as written, so the turn it starts counts as an inbound event and leaves a closed phase closed.
   */
  const sendRecreatedNotice = (issue: string): void => {
    if (recreatedNoticeSent) return
    const notice = workspaceRecreatedNotice(deps.env, issue)
    if (notice === undefined) return
    recreatedNoticeSent = true
    saveRecord(entry)
    noteWritten(notice.content)
    writeToSession(notice.content, "the recreated-workspace notice")
  }

  void deps.booted.then(() => {
    const claim = deps.claim()
    if (claim === undefined) return
    sendRecreatedNotice(claim.issue)
    // D10: a worker the daemon relaunched into an open phase is reminded from a timer, since
    // nothing else would wake it. The guard drops that timer when a turn started meanwhile, which
    // is exactly the case to drop — the daemon re-sending an interrupted task arrives as a
    // `UserPromptSubmit`.
    if (claim.role !== "architect") reminder.armResumeReminder()
  })

  return {
    deliveryWritten: noteWritten,
    phaseClosed: () => reminder.apply({ kind: "phase-closed" }),
    stop: dropTimer,
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
        case "StopFailure": {
          // The turn ended on an API error. Claude Code ignores whatever this hook returns, so
          // this event cannot carry a continuation: the machine is told so and arms the reminder
          // its settle earned itself, under every guard an armed reminder has. This is the one
          // event of the five the pod measurements did not cover; it records only, so a
          // `StopFailure` that never fires costs the reminder after an API-error turn and nothing
          // else.
          await reminder.settle(event.lastAssistantMessage ?? "", { canContinue: false })
          return {}
        }
        case "PostToolUse":
        case "PostToolUseFailure": {
          // The worker is working, whatever it is working on, and an attempted call that failed
          // is work too. `${agent_id}` substitutes to "" in the main conversation and to a
          // non-empty id for a subagent (measured on the real binary), so a helper agent's call
          // is not this worker answering its reminder.
          if ((event.agentId ?? "") !== "") return {}
          reminder.apply({ kind: "tool-call" })
          return {}
        }
        case "UserPromptSubmit": {
          // D7: the only sight this server has of an inbound turn. The daemon's task reaches
          // Claude Code's stdin from the bridge and never this process, so what a turn means is
          // read from the prompt itself. The turn is declared first, which makes every armed
          // timer stale, and the pending one is dropped outright: the runner's generation guard
          // answers the same for the timers it armed, and the one this endpoint arms itself (the
          // `StopFailure` reminder) obeys the same rule that way rather than through a second
          // guard of its own.
          reminder.turnStarted()
          dropTimer()
          const prompt = event.prompt ?? ""
          if (prompt === lastReminder) return {}
          // A line this server wrote is dropped as its prompt arrives, so a later turn carrying
          // the same text is an assignment, which is what a repeat of it would be.
          const delivered = written.delete(prompt)
          reminder.apply({ kind: delivered ? "inbound-event" : "assignment" })
          return {}
        }
      }
    },
  }
}
