// What this session did with the direct messages Dispatch sent it, in one file per session id
// under `CLAUDE_PLUGIN_DATA` (INFRA-1026), beside the `replay-*.json` files and the `roles/`
// records.
//
// The Oh My Pi plugin keeps both of these in its host's transcript
// (`@legion/pi-shared/dispatch-user-turn`, `HANDLED_ATTEMPT_ENTRY`); Claude Code gives this server
// no transcript to write, so they live here:
//
//   - the attempts this session has already handled, as a card or as a turn. The accept route may
//     be asked for an attempt once, so the record goes in before Dispatch is asked, and a frame
//     naming a recorded attempt is a card with no accept call - a replay, or one forged inside the
//     accept's minute. It has to outlive the process, because a restart must not accept an attempt
//     the process before it already spent.
//   - the Dispatch message each delivered row of the conversation is, which is what tags the row
//     on the live view's frames so a person's own message shows once rather than twice
//     (`packages/dispatch/web/src/features/agent-view/conversation.ts`). A row is recorded only
//     once it exists, which is the point of the write order the router keeps.
//
// Both are bounded, by the rule the package writes once (`rememberBounded`,
// `rememberBoundedEntry` in `./bounded-set`): a session that runs for weeks must not grow a file
// without end, and neither record is worth keeping once it is thousands of messages old.
import { join } from "node:path"
import { z } from "zod"
import { rememberBounded, rememberBoundedEntry } from "./bounded-set"
import { pruneStateDirectory, writeAtomicStateFile, writtenLongAgo } from "./session-state"
import { readStateFile } from "./state-file"

/** The directory the records live in, named once for the writer, the reader and the sweep. */
const TURNS_DIRECTORY = "turns"

/** The most handled attempts one session's file keeps, oldest dropped first. A person sends far
 *  fewer than this to one session, and an attempt this old can no longer be accepted: Dispatch
 *  accepts only an attempt a person asked for within the last minute. */
const HANDLED_ATTEMPTS_MAX = 1_000

/** The most delivered rows one session's file keeps. It only has to outlive the live view's own
 *  ring, which holds far fewer messages than this. */
const DELIVERED_ROWS_MAX = 1_000

/** One row of the conversation this session delivered a person's message as. */
const DeliveredRowSchema = z.object({ row: z.string(), message_id: z.string() })

/**
 * A session's file. Both lists are oldest first, and a reader of a file written by another
 * release takes what it knows: an unknown key is left alone, and a missing one is empty.
 */
export const UserTurnStateSchema = z.object({
  handled: z.array(z.string()).default([]),
  rows: z.array(DeliveredRowSchema).default([]),
})

export type UserTurnStateFile = z.infer<typeof UserTurnStateSchema>

/** The `.json` suffix is what keeps a session id of `.` or `..` a file of the directory itself,
 *  since `encodeURIComponent` leaves a dot alone. */
export function userTurnStateFile(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, TURNS_DIRECTORY, `${encodeURIComponent(sessionId)}.json`)
}

/**
 * What a session's file holds; nothing when there is no file, which is a session that has taken
 * no direct message yet. A file that cannot be read, or holds something else, rejects, and the
 * caller then builds no router for the session id (`restoreTurns`): every message arrives as a
 * card, since a record it could not read may hold an attempt already spent.
 */
export async function readUserTurnState(file: string): Promise<UserTurnStateFile> {
  return await readStateFile(
    file,
    UserTurnStateSchema,
    { handled: [], rows: [] },
    "holds no record of this session's turns",
  )
}

/** One session's record, in memory and on disk. */
export interface UserTurnState {
  /** Whether this session already handled the attempt `key`
   *  (`handledAttemptKey`, `@legion/pi-shared/dispatch-user-turn`), in this process or before it. */
  handled(key: string): boolean
  /**
   * Records the attempt `key` as handled, and resolves once the file holding it is in place. The
   * router awaits this before it asks Dispatch to accept: an attempt whose record did not reach
   * the disk must not be spent.
   */
  handle(key: string): Promise<void>
  /** The Dispatch message a delivered row is, or undefined for a row of the session's own. */
  messageOf(row: string): string | undefined
  /** Records that row `row` of the conversation is the Dispatch message `messageId`. */
  delivered(row: string, messageId: string): Promise<void>
  /** Resolves once every write asked for so far has finished, failed ones included. */
  idle(): Promise<void>
}

/** The record in `file`, starting from what it held (`restored`). Every write replaces the whole
 *  file with the record as it stands when it runs, one write at a time. */
export function userTurnState(file: string, restored: UserTurnStateFile): UserTurnState {
  const handled = new Set(restored.handled)
  const rows = new Map(restored.rows.map(({ row, message_id }) => [row, message_id]))
  let writing: Promise<void> = Promise.resolve()

  const write = (): Promise<void> => {
    const state: UserTurnStateFile = {
      handled: [...handled].slice(-HANDLED_ATTEMPTS_MAX),
      rows: [...rows].slice(-DELIVERED_ROWS_MAX).map(([row, message_id]) => ({ row, message_id })),
    }
    const run = writing.then(() => writeAtomicStateFile(file, `${JSON.stringify(state)}\n`))
    writing = run.catch(() => undefined)
    return run
  }

  return {
    handled: (key) => handled.has(key),
    handle(key) {
      rememberBounded(handled, key, HANDLED_ATTEMPTS_MAX)
      return write()
    },
    messageOf: (row) => rows.get(row),
    delivered(row, messageId) {
      // The router records the same pair twice by design - the append event the session reported
      // and the call that delivered it, whichever wins - and the second call has nothing to add.
      // The write already running is the one putting this pair in place, so it is what resolves.
      if (rows.get(row) === messageId) return writing
      rememberBoundedEntry(rows, row, messageId, DELIVERED_ROWS_MAX)
      return write()
    },
    idle: () => writing,
  }
}

/** Drop `turns/` records nothing has touched in `STATE_FILE_MAX_AGE_MS`, as the titles are
 *  dropped: a session's id dies with it, so nothing else ever removes one. */
export async function pruneStaleSessionTurns(stateDirectory: string): Promise<void> {
  await pruneStateDirectory(join(stateDirectory, TURNS_DIRECTORY), writtenLongAgo)
}
