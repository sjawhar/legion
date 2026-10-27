// The replay engine's record of one session (`@legion/envoy-client/replay`): its cursors, the
// subjects it follows and the frames it was shown in the last 72 hours, in one file per session
// id under `CLAUDE_PLUGIN_DATA`. Claude Code can resume a session at any later time, so no sweep
// prunes these files; each holds only cursors, followed subjects and at most 72 hours of ledger,
// since the engine drops older ledger entries before every write.
import { readFile } from "node:fs/promises"
import { join } from "node:path"
import { hasErrnoCode } from "@legion/envoy-client/errors"
import { type ReplaySnapshot, ReplaySnapshotSchema } from "@legion/envoy-client/replay"
import { writeAtomicStateFile } from "./session-state"

/** The `.json` suffix is what keeps a session id of `.` or `..` a file of the directory itself,
 *  since `encodeURIComponent` leaves a dot alone. */
export function replayStateFile(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, `replay-${encodeURIComponent(sessionId)}.json`)
}

/**
 * The snapshot a session's file holds; an empty one when there is no file, which is a session
 * that never ran this plugin, or a new id (a `/clear`, a fresh start). A file that cannot be read
 * or holds no snapshot throws.
 */
export async function readReplayState(file: string): Promise<ReplaySnapshot> {
  let text: string
  try {
    text = await readFile(file, "utf8")
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return { ledger: [] }
    throw error
  }
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    throw new Error(`${file} is not JSON`)
  }
  const snapshot = ReplaySnapshotSchema.safeParse(parsed)
  if (!snapshot.success) throw new Error(`${file} holds no replay state`)
  return snapshot.data
}

/** Replaces the session's file by rename, mode 0600, as every other state file is written. */
export async function writeReplayState(file: string, snapshot: ReplaySnapshot): Promise<void> {
  await writeAtomicStateFile(file, `${JSON.stringify(snapshot)}\n`)
}
