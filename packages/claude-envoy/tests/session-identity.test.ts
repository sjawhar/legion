import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readdir, rm, utimes, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import {
  pruneStaleSessionHandoffs,
  pruneStaleSessionRoles,
  readSessionHandoff,
  roleStateFile,
  SessionIdentity,
  sessionHandoffDirectory,
  sessionHandoffFile,
  writePersistedRole,
  writeSessionHandoff,
} from "../src/session-identity"
import {
  pruneStaleSessionTitles,
  STATE_FILE_MAX_AGE_MS,
  sessionTitleFile,
  writeChosenTitle,
} from "../src/session-state"

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
  // A session id that is nothing but dots stays inside `roles/`: encodeURIComponent leaves a dot
  // alone, and only the suffix keeps the name a path segment of its own.
  expect(roleStateFile("/data", "..")).toBe("/data/roles/...json")
})

test("title state is keyed by session id, and a dotted id names a file inside titles/", () => {
  expect(sessionTitleFile("/data", "ses_a")).toBe("/data/titles/ses_a.txt")
  expect(sessionTitleFile("/data", "qa/override")).toBe("/data/titles/qa%2Foverride.txt")
  expect(sessionTitleFile("/data", "..")).toBe("/data/titles/...txt")
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
    const past = new Date(Date.now() - STATE_FILE_MAX_AGE_MS - 60_000)
    await utimes(old, past, past)

    await pruneStaleSessionTitles(stateDirectory)

    expect(await readdir(join(stateDirectory, "titles"))).toEqual(["ses_fresh.txt"])
  } finally {
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

// Only one path removes a role file by hand (a soft setRole that comes back unclaimed), and the
// handoff prune cannot reach one: it walks `sessions/<pid>/` and asks about a pid, while a role
// file is keyed by session id. The record of the session running the sweep is never dropped,
// whatever its age: at startup, before any heartbeat has touched it, age alone would expire the
// record of the very session that is running.
test("pruning drops role files older than the age, keeps fresh ones and the sweeper's own, and tolerates no directory", async () => {
  const stateDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-roles-"))
  try {
    await pruneStaleSessionRoles(stateDirectory, "ses_running")

    const fresh = roleStateFile(stateDirectory, "ses_fresh")
    const old = roleStateFile(stateDirectory, "ses_old")
    const own = roleStateFile(stateDirectory, "ses_running")
    await writePersistedRole(fresh, { session_id: "ses_fresh", role: "implementer" })
    await writePersistedRole(old, { session_id: "ses_old", role: "reviewer" })
    await writePersistedRole(own, { session_id: "ses_running", role: "tester" })
    const past = new Date(Date.now() - STATE_FILE_MAX_AGE_MS - 60_000)
    await utimes(old, past, past)
    await utimes(own, past, past)

    await pruneStaleSessionRoles(stateDirectory, "ses_running")

    expect((await readdir(join(stateDirectory, "roles"))).sort()).toEqual([
      "ses_fresh.json",
      "ses_running.json",
    ])
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
