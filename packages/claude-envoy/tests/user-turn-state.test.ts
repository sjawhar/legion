/**
 * The record of what a session did with the messages Dispatch sent it (`../src/user-turn-state`):
 * the attempts it handled, which must outlive the process, and the row each delivered message
 * turned out to be.
 */
import { expect, test } from "bun:test"
import { mkdtemp, readdir, rm, utimes } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { STATE_FILE_MAX_AGE_MS } from "../src/session-state"
import {
  pruneStaleSessionTurns,
  readUserTurnState,
  userTurnState,
  userTurnStateFile,
} from "../src/user-turn-state"

test("the record is kept per session id, and a session id that is not a path segment is encoded", () => {
  expect(userTurnStateFile("/data", "ses_a")).toBe("/data/turns/ses_a.json")
  expect(userTurnStateFile("/data", "..")).toBe("/data/turns/...json")
})

test("a handled attempt is on disk before the write resolves, and a new reader finds it", async () => {
  const directory = await mkdtemp(join(tmpdir(), "claude-envoy-turn-state-"))
  try {
    const file = userTurnStateFile(directory, "ses_a")
    const state = userTurnState(file, await readUserTurnState(file))
    expect(state.handled("m1#1")).toBe(false)

    await state.handle("m1#1")
    await state.delivered("row-7", "m1")

    const reread = userTurnState(file, await readUserTurnState(file))
    expect(reread.handled("m1#1")).toBe(true)
    expect(reread.handled("m1#2")).toBe(false)
    expect(reread.messageOf("row-7")).toBe("m1")
  } finally {
    await rm(directory, { recursive: true, force: true })
  }
})

test("a record nothing has touched for a month is swept, and a current one is kept", async () => {
  const directory = await mkdtemp(join(tmpdir(), "claude-envoy-turn-state-"))
  try {
    const old = userTurnStateFile(directory, "ses_old")
    const live = userTurnStateFile(directory, "ses_live")
    await userTurnState(old, { handled: [], rows: [] }).handle("m1#1")
    await userTurnState(live, { handled: [], rows: [] }).handle("m2#1")
    const longAgo = new Date(Date.now() - STATE_FILE_MAX_AGE_MS - 60_000)
    await utimes(old, longAgo, longAgo)

    await pruneStaleSessionTurns(directory)

    expect(await readdir(join(directory, "turns"))).toEqual(["ses_live.json"])
  } finally {
    await rm(directory, { recursive: true, force: true })
  }
})
