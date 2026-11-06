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

/** What `sessionId`'s socket is called under its base: the private directory and the name the
 *  id hashes to, read off a resolution rather than hashed a second time here. */
function under(sessionId: string): string {
  const path = modSocketPath(sessionId, { XDG_RUNTIME_DIR: "/scratch" })
  if (path === undefined) throw new Error(`${sessionId} names no socket`)
  return path.slice("/scratch".length)
}

test("the socket sits under the run directory, else Claude Code's configuration directory", () => {
  const path = modSocketPath("s1", {
    XDG_RUNTIME_DIR: "/run/user/1000",
    CLAUDE_CONFIG_DIR: "/home/kim/.claude",
    HOME: "/home/kim",
  })
  expect(path).toMatch(/^\/run\/user\/1000\/claude-envoy\/ce-[0-9a-f]{16}\.sock$/)
  // With no run directory the base is the configuration directory, which `HOME` names only
  // where `CLAUDE_CONFIG_DIR` names none.
  expect(modSocketPath("s1", { CLAUDE_CONFIG_DIR: "/legion/claude", HOME: "/home/kim" })).toBe(
    `/legion/claude${under("s1")}`,
  )
  expect(modSocketPath("s1", { HOME: "/home/kim" })).toBe(`/home/kim/.claude${under("s1")}`)
  // A base that is unset, empty or relative is passed over, and a trailing slash is not doubled.
  expect(modSocketPath("s1", { XDG_RUNTIME_DIR: "", CLAUDE_CONFIG_DIR: "/home/kim/.claude" })).toBe(
    `/home/kim/.claude${under("s1")}`,
  )
  expect(
    modSocketPath("s1", {
      XDG_RUNTIME_DIR: "run/user/1000",
      CLAUDE_CONFIG_DIR: "/home/kim/.claude/",
    }),
  ).toBe(`/home/kim/.claude${under("s1")}`)
  expect(modSocketPath("s1", { CLAUDE_CONFIG_DIR: "", HOME: "/home/kim/" })).toBe(
    `/home/kim/.claude${under("s1")}`,
  )
  // The name is the session's own, and the same at every call.
  expect(under("s2")).not.toBe(under("s1"))
  expect(under("s1")).toBe(under("s1"))
})

// No other local user can create anything inside a home or a run directory, which is why those
// are the only bases. In one other users write - `TMPDIR`, `/tmp` - another local user binds the
// path first, a session id being no secret, and then reads the conversation and submits prompts
// as the person (INFRA-1026 review).
test("a session with no run, configuration or home directory gets no socket, and TMPDIR names no base", () => {
  const resolution = resolveModSocketPath("s1", {})
  expect(resolution.path).toBeUndefined()
  expect(resolution.refusal).toContain("XDG_RUNTIME_DIR")
  expect(resolution.refusal).toContain("CLAUDE_CONFIG_DIR")
  expect(resolution.refusal).toContain("HOME")
  expect(
    modSocketPath("s1", { XDG_RUNTIME_DIR: "", CLAUDE_CONFIG_DIR: "", HOME: "" }),
  ).toBeUndefined()
  expect(modSocketPath("s1", { HOME: "relative/dir" })).toBeUndefined()
  const shared = resolveModSocketPath("s1", { TMPDIR: "/scratch" })
  expect(shared.path).toBeUndefined()
  expect(shared.refusal).toContain("other users share")
  expect(modSocketPath("s1", { TMPDIR: "/tmp" })).toBeUndefined()
})

// The reviewer's measurement (INFRA-1026 review): a usual macOS `TMPDIR` with
// `/claude-envoy-<uuid>.sock` after it is 103 bytes, which is why the name is the id's hash. A
// macOS session resolves under its own home's `.claude` now, well inside the bound.
test("a macOS session resolves under its configuration directory, inside the bound", () => {
  const macos = "/var/folders/8h/5c0c8vc14t97_l9_4qvl1b6c0000gn/T"
  expect(`${macos}/claude-envoy-${SESSION}.sock`.length).toBe(103)
  const home = "/Users/alexandra"
  expect(`${home}/.claude`.length).toBe(24)
  const path = modSocketPath(SESSION, { HOME: home, TMPDIR: macos })
  expect(path).toBe(`${home}/.claude${under(SESSION)}`)
  expect(path?.length).toBe(62)
  expect(path?.length).toBeLessThanOrEqual(MOD_SOCKET_PATH_MAX_BYTES)
})

// A Legion pane's `CLAUDE_CONFIG_DIR` is the claim's own directory under `/legion`
// (`claudeConfigDir`, `<role state home>/claude`, in
// `packages/daemon/internal/runtime/sandbox/manifest.go`): the longest base this plugin runs on.
test("a Legion pane's configuration directory is inside the bound as well", () => {
  const pane = "/legion/scratch/state/implementer/claude"
  expect(pane.length).toBe(40)
  const path = modSocketPath(SESSION, { CLAUDE_CONFIG_DIR: pane, HOME: "/legion/scratch/home" })
  expect(path).toBe(`${pane}${under(SESSION)}`)
  expect(path?.length).toBe(78)
  expect(path?.length).toBeLessThanOrEqual(MOD_SOCKET_PATH_MAX_BYTES)
})

test("a base whose socket path would pass 100 bytes is passed over, and two such leave none", () => {
  // `/claude-envoy/ce-<16 hex>.sock` is 38 bytes, so a base of 62 fits exactly and 63 does not.
  const file = under(SESSION)
  expect(file.length).toBe(38)
  const fits = baseOfLength(MOD_SOCKET_PATH_MAX_BYTES - file.length)
  const tooDeep = `${fits}d`
  expect(modSocketPath(SESSION, { XDG_RUNTIME_DIR: fits })).toBe(`${fits}${file}`)
  expect(modSocketPath(SESSION, { XDG_RUNTIME_DIR: tooDeep, CLAUDE_CONFIG_DIR: fits })).toBe(
    `${fits}${file}`,
  )
  const resolution = resolveModSocketPath(SESSION, {
    XDG_RUNTIME_DIR: tooDeep,
    CLAUDE_CONFIG_DIR: tooDeep,
  })
  expect(resolution.path).toBeUndefined()
  expect(resolution.refusal).toContain("100 bytes")
  expect(resolution.refusal).toContain(tooDeep)
})

test("the 100 bytes are bytes, not characters: a multi-byte base is measured as it is encoded", () => {
  // Each `é` is two bytes, so 30 of them and the leading slash are 61 of the 62 bytes a base has.
  const fits = `/${"é".repeat(30)}`
  expect(fits.length).toBe(31)
  expect(modSocketPath(SESSION, { XDG_RUNTIME_DIR: fits })).toBe(`${fits}${under(SESSION)}`)
  expect(modSocketPath(SESSION, { XDG_RUNTIME_DIR: `${fits}é` })).toBeUndefined()
})

test("a session id that is not a plain path segment is refused rather than hashed into a name", () => {
  for (const id of ["../other", "a/b", "a\0b", "", ".", "..", "a\\b"]) {
    const resolution = resolveModSocketPath(id, { XDG_RUNTIME_DIR: "/scratch" })
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
