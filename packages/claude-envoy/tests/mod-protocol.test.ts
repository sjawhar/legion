import { expect, test } from "bun:test"
import {
  MOD_MIN_CLAUDE_VERSION,
  MOD_SOCKET_PATH_MAX_BYTES,
  modSocketPath,
  modVersionAtLeast,
  resolveModSocketPath,
} from "../hooks/mod-protocol"

/** A base directory whose own path is `length` bytes, so the socket under it is a known size. */
function baseOfLength(length: number): string {
  return `/${"d".repeat(length - 1)}`
}

const SESSION = "0c3b6f6e-0000-4000-8000-000000000001"

test("the socket sits under the first base that is set to an absolute path", () => {
  expect(modSocketPath("s1", { XDG_RUNTIME_DIR: "/run/user/1000", TMPDIR: "/scratch" })).toBe(
    "/run/user/1000/claude-envoy-s1.sock",
  )
  expect(modSocketPath("s1", { TMPDIR: "/scratch" })).toBe("/scratch/claude-envoy-s1.sock")
  expect(modSocketPath("s1", {})).toBe("/tmp/claude-envoy-s1.sock")
})

test("a base that is unset, empty or relative is passed over, and a trailing slash is not doubled", () => {
  expect(modSocketPath("s1", { XDG_RUNTIME_DIR: "", TMPDIR: "/scratch" })).toBe(
    "/scratch/claude-envoy-s1.sock",
  )
  expect(modSocketPath("s1", { XDG_RUNTIME_DIR: "run/user/1000", TMPDIR: "/scratch" })).toBe(
    "/scratch/claude-envoy-s1.sock",
  )
  expect(modSocketPath("s1", { TMPDIR: "/scratch/" })).toBe("/scratch/claude-envoy-s1.sock")
})

test("a base whose socket path would pass 100 bytes is passed over for the next one", () => {
  // `/claude-envoy-<uuid>.sock` is 55 bytes, so a base of 46 fits exactly and one of 47 does not.
  const file = `/claude-envoy-${SESSION}.sock`
  expect(file.length).toBe(55)
  const fits = baseOfLength(MOD_SOCKET_PATH_MAX_BYTES - file.length)
  const tooDeep = `${fits}d`
  expect(modSocketPath(SESSION, { XDG_RUNTIME_DIR: fits })).toBe(`${fits}${file}`)
  expect(modSocketPath(SESSION, { XDG_RUNTIME_DIR: tooDeep, TMPDIR: fits })).toBe(`${fits}${file}`)
  expect(modSocketPath(SESSION, { XDG_RUNTIME_DIR: tooDeep })).toBe(`/tmp${file}`)
})

test("the 100 bytes are bytes, not characters: a multi-byte base is measured as it is encoded", () => {
  const file = `/claude-envoy-${SESSION}.sock`
  // Each `é` is two bytes, so 23 of them are 46 bytes: the same bound as 46 plain characters.
  const fits = `/${"é".repeat((MOD_SOCKET_PATH_MAX_BYTES - file.length - 1) / 2)}`
  expect(fits.length).toBe(23)
  expect(modSocketPath(SESSION, { XDG_RUNTIME_DIR: fits })).toBe(`${fits}${file}`)
  expect(modSocketPath(SESSION, { XDG_RUNTIME_DIR: `${fits}é` })).toBe(`/tmp${file}`)
})

test("a session no base can hold has no socket at all, and the refusal names the bases tried", () => {
  const deep = baseOfLength(90)
  // Long enough that even `/tmp` leaves the path over the bound: 4 + 14 + 90 + 5 bytes.
  const long = "x".repeat(90)
  const resolution = resolveModSocketPath(long, { XDG_RUNTIME_DIR: deep, TMPDIR: deep })
  expect(resolution.path).toBeUndefined()
  expect(resolution.refusal).toContain("100 bytes")
  expect(resolution.refusal).toContain(deep)
  expect(resolution.refusal).toContain("/tmp")
  expect(modSocketPath(long, {})).toBeUndefined()
})

test("a session id that is not a plain path segment is refused rather than encoded", () => {
  for (const id of ["../other", "a/b", "a\0b", "", ".", "..", "a\\b"]) {
    const resolution = resolveModSocketPath(id, { TMPDIR: "/scratch" })
    expect(resolution.path).toBeUndefined()
    expect(resolution.refusal).toContain("plain path segment")
  }
})

test("a Claude Code older than the mod's minimum is refused and the minimum itself is not", () => {
  expect(modVersionAtLeast(MOD_MIN_CLAUDE_VERSION, MOD_MIN_CLAUDE_VERSION)).toBe(true)
  expect(modVersionAtLeast("2.1.311", MOD_MIN_CLAUDE_VERSION)).toBe(true)
  expect(modVersionAtLeast("2.2.0", MOD_MIN_CLAUDE_VERSION)).toBe(true)
  expect(modVersionAtLeast("3.0.0", MOD_MIN_CLAUDE_VERSION)).toBe(true)
  expect(modVersionAtLeast("2.1.309", MOD_MIN_CLAUDE_VERSION)).toBe(false)
  expect(modVersionAtLeast("2.0.76", MOD_MIN_CLAUDE_VERSION)).toBe(false)
  expect(modVersionAtLeast("1.9.999", MOD_MIN_CLAUDE_VERSION)).toBe(false)
})

test("a development build counts as the release it is built from, and a version with no numbers does not count at all", () => {
  expect(modVersionAtLeast("2.1.310-dev.20260920.t101500.sha1a2b3c4", MOD_MIN_CLAUDE_VERSION)).toBe(
    true,
  )
  expect(modVersionAtLeast("2.1.309-dev.20260920.t101500.sha1a2b3c4", MOD_MIN_CLAUDE_VERSION)).toBe(
    false,
  )
  expect(modVersionAtLeast("", MOD_MIN_CLAUDE_VERSION)).toBe(false)
  expect(modVersionAtLeast("unknown", MOD_MIN_CLAUDE_VERSION)).toBe(false)
})
