import { readFileSync } from "node:fs"
import { mkdir, readdir, readFile, rename, rm, stat, writeFile } from "node:fs/promises"
import { dirname, join } from "node:path"
import { hasErrnoCode } from "@legion/envoy-client/errors"
import { z } from "zod"

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

/** Role state is keyed by session id so `claude --resume <id>` finds the role it held. */
export function roleStateFile(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, "roles", `${encodeURIComponent(sessionId)}.json`)
}

/** Title state is keyed by session id, like role state, so `claude --resume <id>` finds the name
 *  the user gave that session and a `/clear`'s new id starts without one. */
export function sessionTitleFile(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, "titles", encodeURIComponent(sessionId))
}

export async function writeChosenTitle(file: string, title: string): Promise<void> {
  await writeAtomicStateFile(file, `${title}\n`)
}

/** The session has no custom title any more: a rename can be undone, and the fallback returns. */
export async function removeChosenTitle(file: string): Promise<void> {
  await rm(file, { force: true })
}

/**
 * The name a user gave the session, as recorded; undefined when the file cannot be read.
 *
 * Synchronous and never throwing, because the registration path reads it inside a
 * `(sessionId: string) => string`: the title is cosmetic, so a failed read must leave the session
 * on its fallback name rather than fail the registration that carries it.
 */
export function readChosenTitleSync(file: string): string | undefined {
  try {
    return readFileSync(file, "utf8")
  } catch {
    return undefined
  }
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

/** Thirty days: long enough that a session resumed weeks later still finds its name. */
export const SESSION_TITLE_MAX_AGE_MS = 30 * 24 * 60 * 60 * 1_000

/**
 * Drop `titles/<session id>` files last written longer than `SESSION_TITLE_MAX_AGE_MS` ago.
 * Nothing else deletes a file keyed by session id — a session's id dies with it, unmourned — so
 * without this the directory grows by one file per named session for ever.
 */
export async function pruneStaleSessionTitles(stateDirectory: string): Promise<void> {
  const titles = join(stateDirectory, "titles")
  const entries = await readdir(titles).catch((error: unknown) => {
    if (hasErrnoCode(error, "ENOENT")) return []
    throw error
  })
  for (const entry of entries) {
    const file = join(titles, entry)
    const modified = await stat(file).then(
      (stats) => stats.mtimeMs,
      () => undefined,
    )
    if (modified === undefined || Date.now() - modified <= SESSION_TITLE_MAX_AGE_MS) continue
    await rm(file, { force: true })
  }
}
