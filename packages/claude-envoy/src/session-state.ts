// The plugin's state files under `CLAUDE_PLUGIN_DATA`: how one is written, how a directory of
// them is swept, and the chosen-title file itself.
//
// This module is the title hook's whole dependency graph beyond `node:` builtins, and it must
// stay that way: `hooks/session-title-hook.ts` runs before every user prompt, with Claude Code
// blocking the prompt on it, so anything this file pulls in (zod, the Dispatch client, the
// session-settings reader — all of which `src/session-identity.ts` reaches through its role
// schema) is parsed and evaluated on every prompt. The role helpers import from here, never the
// other way round.
import { readFileSync } from "node:fs"
import { mkdir, readdir, rename, rm, stat, utimes, writeFile } from "node:fs/promises"
import { dirname, join } from "node:path"
import { hasErrnoCode } from "@legion/envoy-client/errors"
import { configuredValue } from "./claude-session"

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

/**
 * Mark `file` as current without touching what it holds: the sweeps below read an mtime for
 * "last seen", and a holder that is still holding has nothing new to write. A touch also leaves
 * no temporary file beside it, which an atomic rewrite on every heartbeat would.
 */
export async function touchStateFile(file: string): Promise<void> {
  const now = new Date()
  await utimes(file, now, now)
}

/** Whether an entry of a state directory may go, given its path. */
export type StaleStateEntry = (file: string) => boolean | Promise<boolean>

/**
 * Remove every entry of `directory` that `isStale` gives up, directories and all. A directory
 * that does not exist holds nothing stale: nothing here has run yet, or the sweep before this one
 * emptied it.
 *
 * The one sweep behind all three state directories, each of which differs only in what it calls
 * stale: a handoff whose Claude process is gone, a title or a role nothing has touched in
 * `STATE_FILE_MAX_AGE_MS`.
 */
export async function pruneStateDirectory(
  directory: string,
  isStale: StaleStateEntry,
): Promise<void> {
  const entries = await readdir(directory).catch((error: unknown) => {
    if (hasErrnoCode(error, "ENOENT")) return []
    throw error
  })
  for (const entry of entries) {
    const file = join(directory, entry)
    if (!(await isStale(file))) continue
    await rm(file, { recursive: true, force: true })
  }
}

/** Thirty days: long enough that a session resumed weeks later still finds the name and the role
 *  it held. */
export const STATE_FILE_MAX_AGE_MS = 30 * 24 * 60 * 60 * 1_000

/**
 * Stale by age: last written more than `STATE_FILE_MAX_AGE_MS` ago. A file whose age cannot be
 * read is kept, since a sweep that cannot tell must not delete.
 *
 * Both the title and the role file are keyed by session id, and a session's id dies with it,
 * unmourned, so nothing else ever deletes one: without this each directory grows by a file per
 * session for ever.
 */
export async function writtenLongAgo(file: string): Promise<boolean> {
  const modified = await stat(file).then(
    (stats) => stats.mtimeMs,
    () => undefined,
  )
  return modified !== undefined && Date.now() - modified > STATE_FILE_MAX_AGE_MS
}

/** The directory the title records live in, named once for the writer, the reader and the
 *  sweep. */
const TITLES_DIRECTORY = "titles"

/** Title state is keyed by session id, like role state, so `claude --resume <id>` finds the name
 *  the user gave that session and a `/clear`'s new id starts without one. The `.txt` suffix is
 *  what keeps the name a path segment: `encodeURIComponent` leaves a dot alone, so a session id
 *  of `.` or `..` would otherwise name the directory itself or its parent. */
export function sessionTitleFile(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, TITLES_DIRECTORY, `${encodeURIComponent(sessionId)}.txt`)
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

/**
 * Records the session's custom title under its session id, or removes the record when the session
 * has none: an absent or blank `session_title` is how Claude Code says a rename was undone. The
 * title is written as Claude Code gave it; `src/session-title.ts` cleans and judges it when it
 * reads the record. A session with no `CLAUDE_PLUGIN_DATA` records nothing, since there is
 * nowhere the channel server would read it from.
 */
export async function recordChosenTitle(
  sessionId: string,
  title: string | undefined,
): Promise<void> {
  const pluginData = configuredValue(process.env["CLAUDE_PLUGIN_DATA"])
  if (pluginData === undefined) return
  const file = sessionTitleFile(pluginData, sessionId)
  const chosen = configuredValue(title)
  if (chosen === undefined) await removeChosenTitle(file)
  else await writeChosenTitle(file, chosen)
}

/** Drop `titles/` records nothing has touched in `STATE_FILE_MAX_AGE_MS`. */
export async function pruneStaleSessionTitles(stateDirectory: string): Promise<void> {
  await pruneStateDirectory(join(stateDirectory, TITLES_DIRECTORY), writtenLongAgo)
}
