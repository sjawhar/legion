import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readdir, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import {
  pruneStaleSessionHandoffs,
  readSessionHandoff,
  roleStateFile,
  SessionIdentity,
  sessionHandoffDirectory,
  sessionHandoffFile,
  writeSessionHandoff,
} from "../src/session-identity"

test("the handoff file round-trips the current session id per Claude process", async () => {
  const stateDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-identity-"))
  try {
    const file = sessionHandoffFile(stateDirectory, 4242)
    expect(file).toBe(join(stateDirectory, "sessions", "4242", "session-id"))
    expect(await readSessionHandoff(file)).toBeUndefined()

    await writeSessionHandoff(file, "ses_new\n")

    expect(await readSessionHandoff(file)).toBe("ses_new")
  } finally {
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("role state is keyed by session id, never by process", () => {
  expect(roleStateFile("/data", "ses_a")).toBe("/data/roles/ses_a.json")
  expect(roleStateFile("/data", "qa/override")).toBe("/data/roles/qa%2Foverride.json")
})

test("pruning removes handoff directories whose Claude process is gone and keeps live ones", async () => {
  const stateDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-prune-"))
  try {
    const live = sessionHandoffDirectory(stateDirectory, process.pid)
    const dead = sessionHandoffDirectory(stateDirectory, 2 ** 22 - 1)
    const junk = join(stateDirectory, "sessions", "not-a-pid")
    for (const directory of [live, dead, junk]) await mkdir(directory, { recursive: true })
    await writeFile(join(dead, "session-id"), "ses_dead")

    await pruneStaleSessionHandoffs(stateDirectory)

    expect((await readdir(join(stateDirectory, "sessions"))).sort()).toEqual(
      [String(process.pid), "not-a-pid"].sort(),
    )
  } finally {
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

test("SessionIdentity holds one mutable id shared by reference", () => {
  const identity = new SessionIdentity("ses_a", "/work")
  expect(identity.id).toBe("ses_a")
  expect(identity.directory).toBe("/work")

  identity.set("ses_b")

  expect(identity.id).toBe("ses_b")
})
