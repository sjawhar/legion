// One session's delivery record, in one file per session id under `CLAUDE_PLUGIN_DATA`: the
// replay engine's (`@legion/envoy-client/replay`: its cursors, the subjects it follows and the
// frames it was shown in the last 72 hours), and consumer delivery's
// (`@legion/envoy-client/consumer-delivery`: the resume point and when each topic was first
// followed). Claude Code can resume a session at any later time, so no sweep prunes these files;
// each holds only cursors, followed subjects, a resume point and at most 72 hours of ledger,
// since the engine drops older ledger entries before every write.
import { readFile } from "node:fs/promises"
import { join } from "node:path"
import {
  type ConsumerDeliverySnapshot,
  ConsumerDeliverySnapshotSchema,
} from "@legion/envoy-client/consumer-delivery"
import { hasErrnoCode } from "@legion/envoy-client/errors"
import { type ReplaySnapshot, ReplaySnapshotSchema } from "@legion/envoy-client/replay"
import type { z } from "zod"
import { writeAtomicStateFile } from "./session-state"

/**
 * A session's file: the replay snapshot, and the consumer delivery snapshot once the session had
 * one. `consumer` is optional, so a file a release before consumer delivery wrote still reads, and
 * such a release reading a newer file passes over it.
 */
export const ReplayStateFileSchema = ReplaySnapshotSchema.extend({
  consumer: ConsumerDeliverySnapshotSchema.optional(),
})

export type ReplayStateFile = z.infer<typeof ReplayStateFileSchema>

/** The `.json` suffix is what keeps a session id of `.` or `..` a file of the directory itself,
 *  since `encodeURIComponent` leaves a dot alone. */
export function replayStateFile(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, `replay-${encodeURIComponent(sessionId)}.json`)
}

/**
 * The record a session's file holds; an empty one when there is no file, which is a session that
 * never ran this plugin, or a new id (a `/clear`, a fresh start). A file that cannot be read or
 * holds no replay snapshot throws.
 */
export async function readReplayState(file: string): Promise<ReplayStateFile> {
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
  const snapshot = ReplayStateFileSchema.safeParse(parsed)
  if (!snapshot.success) throw new Error(`${file} holds no replay state`)
  return snapshot.data
}

/** Replaces the session's file by rename, mode 0600, as every other state file is written. */
export async function writeReplayState(file: string, state: ReplayStateFile): Promise<void> {
  await writeAtomicStateFile(file, `${JSON.stringify(state)}\n`)
}

/** Writes one session's file from its two parts, each written whenever its owner changes it. */
export interface ReplayStateWriter {
  /** The replay engine's snapshot changed; resolves once a file holding it is in place. */
  replay(snapshot: ReplaySnapshot): Promise<void>
  /** The consumer delivery snapshot changed; resolves once a file holding it is in place. */
  consumer(snapshot: ConsumerDeliverySnapshot): Promise<void>
  /** Resolves once every write asked for so far has finished, failed ones included. */
  idle(): Promise<void>
}

/**
 * The writer of `file`, starting from what it held (`restored`). Every write replaces the whole
 * file with both parts as they stand when it runs, one write at a time, so a write that finishes
 * last never puts back an older part the other owner has replaced since.
 */
export function replayStateWriter(file: string, restored: ReplayStateFile): ReplayStateWriter {
  let current: ReplayStateFile = restored
  let writing: Promise<void> = Promise.resolve()
  const write = (next: ReplayStateFile): Promise<void> => {
    current = next
    const run = writing.then(() => writeReplayState(file, current))
    writing = run.catch(() => undefined)
    return run
  }
  return {
    replay: (snapshot) =>
      write(
        current.consumer === undefined ? snapshot : { ...snapshot, consumer: current.consumer },
      ),
    consumer: (snapshot) => write({ ...current, consumer: snapshot }),
    idle: () => writing,
  }
}
