// Claude Code hook commands, one bundle for all three; hooks.json names the mode as the argument.
// Each mode is a hook command of its own because Claude Code caps each hook's output at 10,000
// characters, so a long open-asks summary or answers list sharing a command with the skill could
// push it out.
//
// open-asks (every SessionStart: startup, resume, clear, compact, fork). Three jobs:
// 1. Name Claude Code as the `dispatch` command's host and hand on the session's envoy.json:
//    append `export DISPATCH_HOST=claude` and `export ENVOY_CONFIG_FILE='<path>'` (the file
//    Claude Code's rule picks, `$CLAUDE_CONFIG_DIR/envoy.json` when that is set) to
//    `CLAUDE_ENV_FILE`, which Claude Code sources into every later Bash command of the session,
//    each unless an earlier SessionStart already wrote that line there. The `dispatch` command
//    reads the file ENVOY_CONFIG_FILE names whenever DISPATCH_HOST is set. A subagent's Bash can
//    lack what that file holds (anthropics/claude-code#46696); the `dispatch` command it runs then
//    takes host `claude` from `CLAUDE_CODE_CHILD_SESSION=1` and reads `CLAUDE_CONFIG_DIR` itself.
//    The path is single-quoted, the one form Claude Code's PowerShell
//    bridge also reads. The command reads the session id from `CLAUDE_CODE_SESSION_ID`, which
//    Claude Code sets fresh in each Bash command, so a `/clear` needs nothing more here.
// 2. Record the CURRENT session id for this Claude process so the channel
//    server, which keeps the id it was spawned with, can follow a `/clear`.
// 3. Put the session's open Dispatch asks into the model's context, after one `envoy: <warning>`
//    line for each file the read found and did not read (a checkout's `.opencode/envoy.json`,
//    naming its rename), as the other plugins' start notices and the `dispatch` command name it.
// Plain stdout becomes context; it always exits 0 so a Dispatch outage never
// blocks a session from starting.
//
// dispatch-first (every SessionStart, and SubagentStart): puts the dispatch-first skill into the
// model's context when Dispatch is configured for the project, as
// `hookSpecificOutput.additionalContext` under the event that ran it. A subagent runs the main
// conversation's `dispatch` command and starts with no SessionStart of its own. On resume and fork
// Claude Code adds the context only when the transcript does not already hold the same text, so a
// session that started with the skill keeps one copy, and one that started without it (opened
// before this plugin version, or before Dispatch was configured) gets it. A plugin installed
// without the skill file exits non-zero naming it, which Claude Code shows the user.
//
// answers (every SessionStart, and SubagentStart): puts what people wrote to the session in the
// last 72 hours into the model's context, as
// `hookSpecificOutput.additionalContext` under the event that ran it: `readSessionAnswers`' list,
// or its failure line naming why when a read failed and no list read before has an item to show,
// and nothing when the Dispatch has no usable answers route (`source: "unavailable"`).
// The session is the hook input's `session_id`, which at SubagentStart is the parent's, the
// session a subagent's `dispatch` command writes as.
// The hook prints by `ANSWERS_BY_MS` after its process started (bun's start-up included), or
// `ANSWERS_LEAST_WAIT_MS` after its code began when the start-up took longer, carrying the last
// list read, from `answers.json` in the session's Dispatch state directory, when Dispatch has not
// answered by then; it then gives a read still in flight `ANSWERS_SETTLE_MS` more to write that
// file for the next hook, and exits 0 whatever happened. Dispatch is resolved without running a
// `!` token command first (`commands: "kept"`). A fresh process has kept none, so when that
// resolution is off with an error the hook starts this bundle again, detached, in mode
// `answers-refresh` (never named by hooks.json): that process resolves Dispatch with commands run,
// reads the list with a long deadline, which writes `answers.json`, and leaves what it read in
// `answers-refresh.json` beside it, with the Dispatch URL it targeted (`sessionDispatchUrl`, which
// runs no command), its failure line included when the command failed. The hook carries that when
// it lands by the print deadline and names the Dispatch URL envoy.json names to the hook now, and
// otherwise the last list read from that URL (resolved the same way; a list another Dispatch
// answered never stands in) or the failure line: the refresher is in its own
// session with no stdio of the hook's, so the command always runs to its end or its 10 s timeout
// inside `envoy-config`'s own run, which kills its process group and removes its temporary output,
// and the next hook carries the list it read. One refresher runs per session at a time
// (`answers-refresh.lock`, holding its pid), so a burst of SubagentStart hooks runs the command
// once; a refresher started while envoy.json named another Dispatch answers no hook.
//
// The name a person gave the session is recorded by a bundle of its own,
// `hooks/session-title-hook.ts`: it runs before every user prompt, with the prompt blocked on it,
// so it must not pay for this bundle's zod and Dispatch client. `open-asks` records it too, from
// the same `session_title` field, so a session starts under its name before its first prompt.

import { appendFileSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs"
import { join } from "node:path"
import { formatOpenAsksSummary } from "@legion/envoy-client/dispatch-execute"
import {
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { sessionDirectory, writeAtomically } from "@legion/envoy-client/dispatch-session-directory"
import { offWithError } from "@legion/envoy-client/envoy-config"
import { hasErrnoCode, messageFor } from "@legion/envoy-client/errors"
import {
  readSessionAnswers,
  SESSION_ANSWERS_DEADLINE_MS,
  sessionAnswersSettled,
} from "@legion/envoy-client/session-answers"
import {
  sessionConfigFile,
  sessionDispatchSettings,
  sessionDispatchUrl,
} from "@legion/envoy-client/session-settings"
import { z } from "zod"
import { claudeProjectDirectory, configuredValue } from "../src/claude-session"
import { sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import { recordChosenTitle } from "../src/session-state"

const OPEN_ASKS_TIMEOUT_MS = 3_000
const OpenAsksInput = z.object({
  session_id: z.string().min(1),
  cwd: z.string().optional(),
  session_title: z.string().optional(),
})
const DispatchFirstInput = z.object({
  hook_event_name: z.enum(["SessionStart", "SubagentStart"]),
  cwd: z.string().optional(),
})
// session_id first, as the schema always had it, so a ZodError lists its issues in the same order.
const AnswersInput = z.object({ session_id: z.string().min(1), ...DispatchFirstInput.shape })

/** When the answers hook prints, in ms after its process started: the per-request deadline, with
 *  bun's start-up and Dispatch's resolution inside it, so the session waits no longer on it than a
 *  model request on Oh My Pi waits on the same read. */
const ANSWERS_BY_MS = SESSION_ANSWERS_DEADLINE_MS
/** The least the hook waits on Dispatch once its own code runs, whatever the start-up spent:
 *  Claude Code starts every hook beside itself, so on a loaded host bun's start-up alone can pass
 *  `ANSWERS_BY_MS`, and a budget already spent would carry the failure line or a stale list while
 *  Dispatch answers at once. A read that answers in this time is carried; one that does not costs
 *  the session at most this much past the start-up. */
const ANSWERS_LEAST_WAIT_MS = 150
/** How long after the hook's print deadline a read that answered past it may still write
 *  `answers.json` for the next hook before the process exits. */
const ANSWERS_SETTLE_MS = 50
/** The refresher's lock, holding its pid, and what it read for the hooks, in the session's
 *  Dispatch state directory. */
const REFRESH_LOCK = "answers-refresh.lock"
const REFRESH_OUTCOME = "answers-refresh.json"
/** How often a hook waiting on the refresher looks for what it read. */
const REFRESH_POLL_MS = 20
/** How long the refresher waits on its read: past the read's own 10 s timeout, which ends it. */
const REFRESH_READ_MS = 12_000
/** The age past which a lock is no refresher's, whatever pid it holds: past a refresher's longest
 *  run, a 10 s token command and a 10 s read after bun's start-up. */
const REFRESH_LOCK_STALE_MS = 30_000
const RefreshOutcome = z.object({
  /** When the refresher finished (epoch ms): a hook carries only an outcome from after it started. */
  finished_at: z.number(),
  /** The Dispatch URL the refresher targeted, as `sessionDispatchUrl` resolves it, the URL the
   *  hook compares with, whether or not the token command then failed; null when envoy.json names
   *  none. A hook carries only an outcome read for the URL envoy.json names to it. */
  dispatch_url: z.string().nullable(),
  /** `readSessionAnswers`' message; null when there was nothing to show. */
  text: z.string().nullable(),
})
/** Tests only: the file the answers hook writes its own timing to, `{ began, printed, ended }` in
 *  ms after its process started (when its code began, when it printed or found nothing to print,
 *  and when it exits), so a test can hold the hook to its deadline rather than to bun's start-up.
 *  Unset, as Claude Code runs it, the hook writes nothing. */
const ANSWERS_TIMING_FILE = "ENVOY_ANSWERS_TIMING_FILE"

const DISPATCH_HOST_LINE = "export DISPATCH_HOST=claude"

/** Appends `line` to the env file, once: every SessionStart of the session runs this. */
function appendOnce(envFile: string, line: string): void {
  let written = ""
  try {
    written = readFileSync(envFile, "utf8")
  } catch (error) {
    if (!hasErrnoCode(error, "ENOENT")) throw error
  }
  if (written.split("\n").includes(line)) return
  const separator = written === "" || written.endsWith("\n") ? "" : "\n"
  appendFileSync(envFile, `${separator}${line}\n`)
}

async function openAsks(raw: unknown): Promise<void> {
  const input = OpenAsksInput.parse(raw)
  const envFile = process.env["CLAUDE_ENV_FILE"]
  // A file location that cannot be worked out hands nothing on: the open-asks summary below says
  // why Dispatch is unavailable.
  const chosen = sessionConfigFile("claude", process.env)
  if (envFile) {
    appendOnce(envFile, DISPATCH_HOST_LINE)
    if (chosen.path !== null) {
      appendOnce(envFile, `export ENVOY_CONFIG_FILE='${chosen.path.replaceAll("'", "'\\''")}'`)
    }
  }
  const pluginData = process.env["CLAUDE_PLUGIN_DATA"]
  if (pluginData !== undefined && pluginData.trim().length > 0) {
    try {
      await writeSessionHandoff(sessionHandoffFile(pluginData, process.ppid), input.session_id)
    } catch (error) {
      process.stderr.write(`envoy: could not record the session id — ${messageFor(error)}\n`)
    }
  }
  try {
    await recordChosenTitle(input.session_id, input.session_title)
  } catch (error) {
    process.stderr.write(`envoy: could not record the session title — ${messageFor(error)}\n`)
  }

  const directory = claudeProjectDirectory(
    { CLAUDE_PROJECT_DIR: process.env["CLAUDE_PROJECT_DIR"] },
    input.cwd ?? process.cwd(),
  )
  const { dispatch, warnings, missing } = sessionDispatchSettings(chosen, process.env, {
    cwd: directory,
  })
  for (const warning of warnings) process.stdout.write(`envoy: ${warning}\n`)
  if (dispatch.enabled) {
    try {
      const snapshot = await new DispatchClient(
        dispatch.url,
        dispatch.token,
        fetch,
        AbortSignal.timeout(OPEN_ASKS_TIMEOUT_MS),
      ).openAsks(input.session_id)
      process.stdout.write(
        `Dispatch authored-ask summary:\n${formatOpenAsksSummary(snapshot, dispatch.publicUrl ?? dispatch.url)}\n`,
      )
    } catch (error) {
      process.stdout.write(`Dispatch authored-ask summary unavailable: ${messageFor(error)}\n`)
    }
  } else if (dispatch.error !== null) {
    process.stdout.write(`Dispatch authored-ask summary unavailable: ${dispatch.error}\n`)
  } else if (
    missing?.oldUserFileNotice !== undefined &&
    dispatch.notice.endsWith(missing.oldUserFileNotice)
  ) {
    // The missing file is why Dispatch is off, and that notice names the OpenCode-era file this
    // machine still holds: say so and what to do. Off for any other reason says nothing.
    process.stdout.write(`Dispatch authored-ask summary unavailable: ${dispatch.notice}\n`)
  }
}

function dispatchFirst(raw: unknown): void {
  const input = DispatchFirstInput.parse(raw)
  const directory = claudeProjectDirectory(
    { CLAUDE_PROJECT_DIR: process.env["CLAUDE_PROJECT_DIR"] },
    input.cwd ?? process.cwd(),
  )
  const chosen = sessionConfigFile("claude", process.env)
  if (!sessionDispatchSettings(chosen, process.env, { cwd: directory }).dispatch.enabled) return
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

/** The ms left until `at`, a `performance.now()` reading, in whole ms because the failure line
 *  names the wait. */
function remainingUntil(at: number): number {
  return Math.max(0, Math.round(at - performance.now()))
}

/** Whether a refresher holds `lock`: it exists, is younger than `REFRESH_LOCK_STALE_MS`, and the
 *  pid in it is alive or not yet written. A lock its refresher died holding is stale at once. */
function refreshLockHeld(lock: string): boolean {
  let age: number
  let text: string
  try {
    age = Date.now() - statSync(lock).mtimeMs
    text = readFileSync(lock, "utf8")
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return false
    throw error
  }
  if (age > REFRESH_LOCK_STALE_MS) return false
  const pid = Number(text)
  if (!Number.isSafeInteger(pid) || pid <= 0) return true
  try {
    process.kill(pid, 0)
    return true
  } catch (error) {
    return !hasErrnoCode(error, "ESRCH")
  }
}

/** Takes `lock` for this process, replacing a stale one; false when another refresher holds it. */
function takeRefreshLock(lock: string): boolean {
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      writeFileSync(lock, String(process.pid), { flag: "wx", mode: 0o600 })
      return true
    } catch (error) {
      if (!hasErrnoCode(error, "EEXIST")) throw error
    }
    if (refreshLockHeld(lock)) return false
    rmSync(lock, { force: true })
  }
  return false
}

/** What the refresher read, when it finished after `since` (epoch ms), whichever Dispatch it read. */
function refreshOutcome(file: string, since: number): z.infer<typeof RefreshOutcome> | undefined {
  let value: unknown
  try {
    value = JSON.parse(readFileSync(file, "utf8"))
  } catch {
    // None written yet: nothing to carry from it now.
    return undefined
  }
  const outcome = RefreshOutcome.safeParse(value)
  return outcome.success && outcome.data.finished_at >= since ? outcome.data : undefined
}

/** What the refresher gave the hook: its message (undefined: nothing to show), or why none. */
type Refreshed = { readonly text: string | undefined } | { readonly reason: string }

/** Starts the session's refresher unless one is running, and waits until `by` for what it reads.
 *  `startedAt` is when this process started (epoch ms): an outcome from before then is not this
 *  hook's. The hook carries an outcome only when the Dispatch URL its refresher targeted is
 *  `dispatchUrl`, the one envoy.json names to this hook, a failed token command's error included;
 *  any other is a refresher started while envoy.json named another, and the hook carries none of
 *  it: a hook after the refresher this one started carries the list of the Dispatch named now. */
async function refreshedAnswers(
  session: string,
  cwd: string,
  dispatchUrl: string,
  startedAt: number,
  by: number,
): Promise<Refreshed> {
  let directory: string
  try {
    directory = sessionDirectory(process.env, session)
  } catch (error) {
    // An id that is not one plain path segment has no directory for the lock and the outcome.
    return { reason: messageFor(error) }
  }
  const lock = join(directory, REFRESH_LOCK)
  if (!refreshLockHeld(lock)) {
    Bun.spawn([process.execPath, Bun.main, "answers-refresh", session, cwd], {
      stdin: "ignore",
      stdout: "ignore",
      stderr: "ignore",
      detached: true,
    }).unref()
  }
  const file = join(directory, REFRESH_OUTCOME)
  for (;;) {
    const outcome = refreshOutcome(file, startedAt)
    if (outcome !== undefined) {
      if (outcome.dispatch_url === dispatchUrl) return { text: outcome.text ?? undefined }
      return {
        reason: `the refresher read ${outcome.dispatch_url ?? "no Dispatch"}, not the Dispatch envoy.json names now`,
      }
    }
    const left = remainingUntil(by)
    if (left === 0) {
      return {
        reason: `a "!" command in envoy.json had not finished ${Math.round(by)} ms after the hook started`,
      }
    }
    await Bun.sleep(Math.min(REFRESH_POLL_MS, left))
  }
}

/** The answers hook's message for `session`. Dispatch is read first without running any `!`
 *  command; when that is off with an error, the refresher's message stands, or, when it has not
 *  landed by `by`, the last list read from the Dispatch URL envoy.json names now (resolved without
 *  the token), never another's, or the failure line naming why. */
async function answersMessage(
  session: string,
  cwd: string,
  startedAt: number,
  by: number,
): Promise<string | undefined> {
  const file = sessionConfigFile("claude", process.env)
  let { dispatch } = sessionDispatchSettings(file, process.env, { cwd, commands: "kept" })
  let dispatchUrl: string | undefined
  if (!dispatch.enabled && dispatch.error !== null) {
    // The configured URL alone, no command run. With none (envoy.json unreadable, Dispatch off or
    // no URL), no token command can name one either: the hook starts no refresher and carries the
    // failure line naming why, never a list read for some URL.
    dispatchUrl = sessionDispatchUrl(file, process.env, { cwd })
    if (dispatchUrl !== undefined) {
      const refreshed = await refreshedAnswers(session, cwd, dispatchUrl, startedAt, by)
      if ("text" in refreshed) return refreshed.text
      dispatch = offWithError(refreshed.reason)
    }
  }
  const read = await readSessionAnswers({
    session,
    dispatch,
    dispatchUrl,
    env: process.env,
    // The read itself always gets the least wait, so a start-up and a resolution that spent the
    // budget on a loaded host never cost a list Dispatch answers at once.
    deadlineMs: Math.max(ANSWERS_LEAST_WAIT_MS, remainingUntil(by)),
  })
  return read.text
}

/** The answers mode: never throws, and ends the process with status 0. */
async function answers(text: string): Promise<never> {
  // The print deadline: `ANSWERS_BY_MS` after the process started, or `ANSWERS_LEAST_WAIT_MS`
  // from now when the start-up already spent that.
  const began = performance.now()
  const by = Math.max(ANSWERS_BY_MS, began + ANSWERS_LEAST_WAIT_MS)
  const startedAt = Date.now() - performance.now()
  let printed: number | null = null
  try {
    const input = AnswersInput.parse(JSON.parse(text))
    const directory = claudeProjectDirectory(
      { CLAUDE_PROJECT_DIR: process.env["CLAUDE_PROJECT_DIR"] },
      input.cwd ?? process.cwd(),
    )
    const message = await answersMessage(input.session_id, directory, startedAt, by)
    if (message !== undefined) {
      await Bun.write(
        Bun.stdout,
        JSON.stringify({
          hookSpecificOutput: {
            hookEventName: input.hook_event_name,
            additionalContext: message,
          },
        }),
      )
    }
    printed = performance.now()
    await Promise.race([
      sessionAnswersSettled(input.session_id),
      Bun.sleep(remainingUntil(by + ANSWERS_SETTLE_MS)),
    ])
  } catch (error) {
    process.stderr.write(`envoy: people's answers were not added — ${messageFor(error)}\n`)
  }
  const timingFile = process.env[ANSWERS_TIMING_FILE]
  if (timingFile !== undefined) {
    try {
      writeFileSync(timingFile, JSON.stringify({ began, printed, ended: performance.now() }))
    } catch {
      // The tests' seam only: a path it cannot write never changes what the hook printed or its exit.
    }
  }
  // Exits rather than returns: a read still running would hold the process open.
  process.exit(0)
}

/** The answers-refresh mode, which the answers hook starts detached with the session id and the
 *  project directory: resolves Dispatch with `!` commands run, reads the session's list (which
 *  writes `answers.json`) and writes what it read, and for which Dispatch URL, to
 *  `answers-refresh.json` for the hooks waiting on it. A second refresher for the session exits at
 *  once. Nothing reads its output. */
async function answersRefresh(session: string, cwd: string): Promise<never> {
  const directory = sessionDirectory(process.env, session)
  mkdirSync(directory, { recursive: true, mode: 0o700 })
  const lock = join(directory, REFRESH_LOCK)
  if (takeRefreshLock(lock)) {
    try {
      const file = sessionConfigFile("claude", process.env)
      // The URL this refresher targets, resolved as the hook resolves the one it compares with and
      // before any command runs: when envoy.json moved on since the hook started this refresher, no
      // hook carries what it read, and when the token command fails, the hook for the same URL
      // carries this refresher's failure line naming why.
      const dispatchUrl = sessionDispatchUrl(file, process.env, { cwd }) ?? null
      const { dispatch } = sessionDispatchSettings(file, process.env, { cwd })
      const read = await readSessionAnswers({
        session,
        dispatch,
        env: process.env,
        deadlineMs: REFRESH_READ_MS,
      })
      const outcome: z.infer<typeof RefreshOutcome> = {
        finished_at: Date.now(),
        dispatch_url: dispatchUrl,
        text: read.text ?? null,
      }
      writeAtomically(join(directory, REFRESH_OUTCOME), JSON.stringify(outcome))
    } finally {
      rmSync(lock, { force: true })
    }
  }
  process.exit(0)
}

const [mode, ...modeArguments] = process.argv.slice(2)
if (mode === "answers-refresh") {
  const [session, cwd] = modeArguments
  if (session === undefined || cwd === undefined) {
    throw new Error("session-hook: answers-refresh takes a session id and a directory")
  }
  await answersRefresh(session, cwd)
}
if (mode !== "open-asks" && mode !== "dispatch-first" && mode !== "answers") {
  throw new Error(
    `session-hook: unknown mode ${JSON.stringify(mode)}; hooks.json names open-asks, dispatch-first or answers`,
  )
}
const text = await Bun.stdin.text()
if (mode === "answers") await answers(text)
const input: unknown = JSON.parse(text)
if (mode === "open-asks") await openAsks(input)
else dispatchFirst(input)
