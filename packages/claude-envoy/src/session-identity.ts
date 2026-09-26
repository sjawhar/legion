import { mkdir, readdir, readFile, rename, rm, writeFile } from "node:fs/promises"
import { dirname, join } from "node:path"

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

/** Role state is keyed by session id so `claude --resume <id>` finds the role it held. */
export function roleStateFile(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, "roles", `${encodeURIComponent(sessionId)}.json`)
}

/** True when `error` is a Node errno error carrying `code` (ENOENT, EPERM, …). */
export function hasErrnoCode(error: unknown, code: string): boolean {
  return error instanceof Error && "code" in error && error.code === code
}

/**
 * Write a private state file atomically: a 0700 directory, a 0600 temporary file
 * unique to this process, then a rename onto `file`.
 */
export async function writeAtomicStateFile(file: string, contents: string): Promise<void> {
  await mkdir(dirname(file), { recursive: true, mode: 0o700 })
  const temporary = `${file}.${process.pid}.${crypto.randomUUID()}.tmp`
  await writeFile(temporary, contents, { mode: 0o600 })
  await rename(temporary, file)
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

/** Drop `sessions/<pid>` directories whose Claude process has exited. */
export async function pruneStaleSessionHandoffs(stateDirectory: string): Promise<void> {
  const sessions = join(stateDirectory, "sessions")
  const entries = await readdir(sessions).catch((error: unknown) => {
    if (hasErrnoCode(error, "ENOENT")) return []
    throw error
  })
  for (const entry of entries) {
    if (!/^\d+$/.test(entry) || processExists(Number(entry))) continue
    await rm(join(sessions, entry), { recursive: true, force: true })
  }
}
