import { readFile } from "node:fs/promises"
import { basename, join } from "node:path"
import { hasErrnoCode } from "@legion/envoy-client/errors"
import { z } from "zod"
import { pruneStateDirectory, writeAtomicStateFile, writtenLongAgo } from "./session-state"

const PersistedRole = z.object({ session_id: z.string().min(1), role: z.string().min(1) })

/**
 * The session id the channel server acts as, held once and passed by reference.
 *
 * Claude Code mints a new session id on `/clear` but a stdio MCP server keeps the
 * `CLAUDE_CODE_SESSION_ID` it was spawned with, so the server learns the current id
 * from the SessionStart hook's handoff file and rebinds. Every registration, tool
 * call, role claim, and deregistration reads `id` at the moment it runs.
 */
export class SessionIdentity {
  #id: string

  constructor(
    id: string,
    /** The Claude project directory Dispatch operations are scoped to. */
    readonly directory: string,
  ) {
    this.#id = id
  }

  get id(): string {
    return this.#id
  }

  set(next: string): void {
    this.#id = next
  }
}

/**
 * Per-Claude-process handoff: `${CLAUDE_PLUGIN_DATA}/sessions/<pid>/session-id`, where
 * `<pid>` is the `claude` process both the SessionStart hook and the stdio MCP server are
 * spawned by. Two concurrent Claude sessions therefore never share a file.
 */
export function sessionHandoffDirectory(stateDirectory: string, claudePid: number): string {
  return join(stateDirectory, "sessions", String(claudePid))
}

export function sessionHandoffFile(stateDirectory: string, claudePid: number): string {
  return join(sessionHandoffDirectory(stateDirectory, claudePid), "session-id")
}

/** The directory the role records live in, named once for the writer and the sweep. */
const ROLES_DIRECTORY = "roles"

/** Role state is keyed by session id so `claude --resume <id>` finds the role it held. The
 *  `.json` suffix keeps the name a path segment: `encodeURIComponent` leaves a dot alone, so a
 *  session id of `.` or `..` would otherwise name the directory itself or its parent. */
export function roleStateFile(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, ROLES_DIRECTORY, `${encodeURIComponent(sessionId)}.json`)
}

/** The role a session held, from its role file; undefined when it has none. */
export async function readPersistedRole(
  roleFile: string,
): Promise<z.infer<typeof PersistedRole> | undefined> {
  try {
    return PersistedRole.parse(JSON.parse(await readFile(roleFile, "utf8")))
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return undefined
    throw error
  }
}

export async function writePersistedRole(
  roleFile: string,
  state: z.infer<typeof PersistedRole>,
): Promise<void> {
  await writeAtomicStateFile(roleFile, `${JSON.stringify(state)}\n`)
}

export async function writeSessionHandoff(file: string, sessionId: string): Promise<void> {
  await writeAtomicStateFile(file, `${sessionId.trim()}\n`)
}

export async function readSessionHandoff(file: string): Promise<string | undefined> {
  try {
    const id = (await readFile(file, "utf8")).trim()
    return id.length === 0 ? undefined : id
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return undefined
    throw error
  }
}

function processExists(pid: number): boolean {
  try {
    process.kill(pid, 0)
    return true
  } catch (error) {
    return hasErrnoCode(error, "EPERM")
  }
}

/** Drop `sessions/<pid>` directories whose Claude process has exited. Anything else under
 *  `sessions/` is left alone: it was never this sweep's to name. */
export async function pruneStaleSessionHandoffs(stateDirectory: string): Promise<void> {
  await pruneStateDirectory(join(stateDirectory, "sessions"), (file) => {
    const pid = basename(file)
    return /^\d+$/.test(pid) && !processExists(Number(pid))
  })
}

/**
 * Drop `roles/` records nothing has touched in `STATE_FILE_MAX_AGE_MS`, by the age rule the
 * title records follow.
 *
 * Only one path removes a role file by hand — a soft `setRole` that comes back unclaimed — so a
 * session that holds its role to the end leaves its file behind, and the handoff sweep cannot
 * reach it: that one walks `sessions/<pid>/` and asks the operating system about a pid, while a
 * role file is keyed by session id and has no process left to ask about.
 */
export async function pruneStaleSessionRoles(stateDirectory: string): Promise<void> {
  await pruneStateDirectory(join(stateDirectory, ROLES_DIRECTORY), writtenLongAgo)
}
