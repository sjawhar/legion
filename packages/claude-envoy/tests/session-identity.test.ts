import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readdir, rm, utimes, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import {
  pruneStaleSessionHandoffs,
  pruneStaleSessionTitles,
  readSessionHandoff,
  roleStateFile,
  SESSION_TITLE_MAX_AGE_MS,
  SessionIdentity,
  sessionHandoffDirectory,
  sessionHandoffFile,
  sessionTitleFile,
  writeChosenTitle,
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

test("pruning drops title files older than the age, keeps fresh ones, and tolerates no directory", async () => {
  const stateDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-titles-"))
  try {
    await pruneStaleSessionTitles(stateDirectory)

    const fresh = sessionTitleFile(stateDirectory, "ses_fresh")
    const old = sessionTitleFile(stateDirectory, "ses_old")
    await writeChosenTitle(fresh, "review the listener")
    await writeChosenTitle(old, "last month")
    const past = new Date(Date.now() - SESSION_TITLE_MAX_AGE_MS - 60_000)
    await utimes(old, past, past)

    await pruneStaleSessionTitles(stateDirectory)

    expect(await readdir(join(stateDirectory, "titles"))).toEqual(["ses_fresh"])
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
