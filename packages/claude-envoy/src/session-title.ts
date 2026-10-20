import { execFileSync } from "node:child_process"
import { readFileSync } from "node:fs"
import { basename } from "node:path"
import { configuredValue } from "./claude-session"

export interface SessionTitleEnvironment {
  readonly ENVOY_SESSION_TITLE?: string | undefined
  readonly TMUX?: string | undefined
  readonly TMUX_PANE?: string | undefined
}

/**
 * The title registered for the session's current id. Every registration calls it, so the short
 * id in it follows a `/clear` handoff.
 */
export type SessionTitle = (sessionId: string) => string

/** What `tmux -S <socket> display-message -p` prints for `pane` and `format`; undefined when tmux
 *  cannot say. */
export type TmuxQuery = (socket: string, pane: string, format: string) => string | undefined

// `automatic-rename` is the window option, which tmux expands as a format variable (1 or 0). A
// window left on it is named after the running command, `claude` in every pane, which tells
// sessions apart no better than nothing, so only a window with it off lends its name. A window
// index is never used: with `renumber-windows` on, tmux moves it after the title is worked out.
// A pane of grouped sessions answers with whichever session of the group tmux picks, so the
// group's name stands in for it, which tmux keeps even after the sessions are renamed or end.
// `pid` is the server's: a pane id names a pane only on the server that gave it out, and a server
// started again on the same socket gives out the same ids. `pane_pid` is the pane's first process:
// tmux hands a popup, `run-shell` job or hook a `TMUX_PANE` left over from wherever the server was
// started, so a pane is this session's only when its process is one of this session's ancestors.
const TMUX_PANE_FORMAT = [
  "#{session_name}",
  "#{window_name}",
  "#{automatic-rename}",
  "#{session_group}",
  "#{pid}",
  "#{pane_pid}",
].join("\t")

/** Bounds the one tmux call the server makes as it starts. */
const TMUX_TIMEOUT_MS = 1_000
const SHORT_ID_LENGTH = 8

/** How many parents `isAncestor` follows before it stops. */
const MAX_ANCESTORS = 64

/** A terminal control sequence (a colour code: ESC [ parameters final byte). Built from the ESC
 *  code, since a control character in a regular expression literal is a lint error. */
const CSI_SEQUENCE = new RegExp(`${String.fromCharCode(0x1b)}\\[[0-?]*[ -/]*[@-~]`, "gu")

// Bidirectional overrides and isolates would reorder the short id after the name on the Agents
// page. Other format characters stay: a zero-width joiner holds an emoji sequence together.
function oneLine(value: string): string {
  return (
    value
      // A colour code goes whole; any other escape loses only its ESC, below.
      .replace(CSI_SEQUENCE, "")
      .replace(/[\u202a-\u202e\u2066-\u2069]/gu, "")
      .replace(/[\s\p{Cc}]+/gu, " ")
      .trim()
  )
}

/** Whether `value` holds a character that shows: not whitespace, a control, format, surrogate or
 *  unassigned code point, a combining mark with nothing to sit on, an ignorable code point, or
 *  the blank braille pattern. A private-use glyph (a patched font's icon) counts. */
function visible(value: string): boolean {
  return (
    value.replace(
      /[\s\p{Cc}\p{Cf}\p{Cs}\p{Cn}\p{M}\p{Default_Ignorable_Code_Point}\u2800]/gu,
      "",
    ) !== ""
  )
}

/** The parent of process `pid`, or undefined when it cannot be read. */
function parentPid(pid: number): number | undefined {
  try {
    if (process.platform === "linux") {
      // The command name, in parentheses, may hold spaces; the parent is the second field after it.
      const stat = readFileSync(`/proc/${pid}/stat`, "utf8")
      const parent = Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1])
      return Number.isInteger(parent) ? parent : undefined
    }
    const parent = Number(
      execFileSync("ps", ["-o", "ppid=", "-p", String(pid)], {
        encoding: "utf8",
        timeout: TMUX_TIMEOUT_MS,
        killSignal: "SIGKILL",
      }).trim(),
    )
    return Number.isInteger(parent) ? parent : undefined
  } catch {
    return undefined
  }
}

/** Whether process `candidate` is `pid` or one of its ancestors, walking up from `pid` and
 *  stopping at the first match, at init, or after `MAX_ANCESTORS` steps. */
export function isAncestor(candidate: string, pid: number = process.pid): boolean {
  let current: number | undefined = pid
  for (let step = 0; current !== undefined && current > 1 && step < MAX_ANCESTORS; step++) {
    if (String(current) === candidate) return true
    current = parentPid(current)
  }
  return false
}

/**
 * `tmux -S <socket> display-message -p -t <pane> <format>`; undefined when tmux is missing, slow
 * or refuses, or prints nothing: a running tmux server answers a pane it does not have with an
 * empty line and exit status 0 (tmux 3.7b), not an error.
 */
export function queryTmux(socket: string, pane: string, format: string): string | undefined {
  try {
    const answer = execFileSync(
      "tmux",
      ["-S", socket, "display-message", "-p", "-t", pane, format],
      {
        encoding: "utf8",
        stdio: ["ignore", "pipe", "ignore"],
        timeout: TMUX_TIMEOUT_MS,
        // A `tmux` that ignores SIGTERM (a wrapper script) would hold the server's start past the
        // bound; SIGKILL cannot be ignored.
        killSignal: "SIGKILL",
      },
    )
    return answer.trim() === "" ? undefined : answer
  } catch {
    return undefined
  }
}

/**
 * The tmux name of `pane` on the server at `socket` whose process is `serverPid`: `session:window`
 * for a window whose `automatic-rename` is off, else the tmux session's name. Undefined when tmux
 * cannot say, when another server now answers on that socket, when `ownPane` refuses the pane's
 * first process (the pane is not this session's), and for a session named only by digits (plain
 * `tmux` names its first one `0`) in a window nobody named, since that says nothing on any machine.
 */
export function tmuxPaneTitle(
  socket: string,
  serverPid: string,
  pane: string,
  ownPane: (panePid: string) => boolean,
  query: TmuxQuery = queryTmux,
): string | undefined {
  const fields = query(socket, pane, TMUX_PANE_FORMAT)
    ?.replace(/\r?\n$/, "")
    .split("\t")
  if (fields === undefined || fields.length < 6) return undefined
  const [name = "", window = "", automaticRename, group = "", pid, panePid = ""] =
    fields.map(oneLine)
  if (pid !== serverPid || !ownPane(panePid)) return undefined
  const session = group === "" ? name : group
  // A tmux too old to expand a format prints it as it was sent; a name with nothing that shows
  // would be a blank label.
  if (!visible(session) || session.startsWith("#{")) return undefined
  if (visible(window) && (automaticRename === "0" || automaticRename === "off")) {
    return `${session}:${window}`
  }
  return /^[0-9]+$/.test(session) ? undefined : session
}

/**
 * The title this session registers with the Envoy listener, which Dispatch's Agents page shows in
 * place of the session id: `ENVOY_SESSION_TITLE`; else a name and the current id's first eight
 * characters, the name being the pane's tmux name inside tmux (`TMUX` and `TMUX_PANE`, asked of
 * the server `TMUX` names, for a pane whose first process `ownPane` accepts, by default one of this
 * process's ancestors) and the project directory's name otherwise. The short id keeps two sessions that
 * share a tmux window or session apart. The name is worked out once, as the server starts: a tmux
 * rename after that shows on the next start.
 */
export function sessionTitle(
  environment: SessionTitleEnvironment,
  directory: string,
  query: TmuxQuery = queryTmux,
  ownPane: (panePid: string) => boolean = (panePid) => isAncestor(panePid),
): SessionTitle {
  const explicit = oneLine(configuredValue(environment.ENVOY_SESSION_TITLE) ?? "")
  // A title with nothing that shows once cleaned (a zero-width space, a colour code alone) would
  // be a blank row.
  if (visible(explicit)) return () => explicit
  // `TMUX` is `<socket>,<server pid>,<session>`, and a socket path may hold a comma.
  const parts = configuredValue(environment.TMUX)?.split(",") ?? []
  const socket = parts.slice(0, -2).join(",")
  const serverPid = parts.at(-2) ?? ""
  const pane = configuredValue(environment.TMUX_PANE)?.trim()
  // A directory name holds whatever the filesystem allows, newlines and overrides among them.
  const directoryName = oneLine(basename(directory) || directory)
  const name =
    (socket === "" || serverPid === "" || pane === undefined
      ? undefined
      : tmuxPaneTitle(socket, serverPid, pane, ownPane, query)) ??
    (visible(directoryName) ? directoryName : "session")
  return (sessionId) => `${name} ${sessionId.slice(0, SHORT_ID_LENGTH)}`
}
