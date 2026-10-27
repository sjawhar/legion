import { afterAll, beforeAll, expect, test } from "bun:test"
import { randomUUID } from "node:crypto"
import { existsSync } from "node:fs"
import { lstat, mkdtemp, readdir, rm, stat, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

const packageRoot = resolve(import.meta.dir, "..")
const buildScript = join(packageRoot, "scripts", "build.ts")

/** Runs scripts/build.ts on the Bun running this test (the pinned one), from outside the repo. */
async function build(...args: string[]): Promise<{ exitCode: number; stderr: string }> {
  const run = Bun.spawn([process.execPath, buildScript, ...args], {
    cwd: tmpdir(),
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exitCode, stderr] = await Promise.all([run.exited, new Response(run.stderr).text()])
  return { exitCode, stderr }
}

let scratch = ""
let out = ""

beforeAll(async () => {
  scratch = await mkdtemp(join(tmpdir(), "claude-envoy-build-out-"))
  out = join(scratch, "plugin")
  const result = await build("--out", out)
  expect(result).toMatchObject({ exitCode: 0 })
}, 120_000)

afterAll(async () => {
  await rm(scratch, { recursive: true, force: true })
})

test("--out builds a plugin whose channel server runs without node_modules and, with Envoy off, exits 0 once stdin ends", async () => {
  const home = await mkdtemp(join(tmpdir(), "claude-envoy-build-out-home-"))
  try {
    const run = Bun.spawn([process.execPath, join(out, "dist", "envoy-channel.js")], {
      cwd: home,
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: home,
        CLAUDE_CODE_SESSION_ID: "test",
        CLAUDE_PLUGIN_DATA: join(home, "plugin-data"),
      },
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
    })
    const [exitCode, stderr] = await Promise.all([run.exited, new Response(run.stderr).text()])

    expect(stderr).toContain(
      `envoy-channel: no envoy.json at ${join(home, ".config", "legion", "envoy.json")}: Dispatch and Envoy are off`,
    )
    expect(exitCode).toBe(0)
  } finally {
    await rm(home, { recursive: true, force: true })
  }
}, 30_000)

test("--out copies the skills as files, not links, and generates the Legion plugin", async () => {
  expect((await lstat(join(out, "skills", "dispatch-first", "SKILL.md"))).isFile()).toBe(true)
  expect((await lstat(join(out, "skills", "dispatch-first"))).isDirectory()).toBe(true)
  expect(existsSync(join(out, "legion", ".claude-plugin", "plugin.json"))).toBe(true)
})

test("--out keeps bin/dispatch executable, and it runs the built CLI", async () => {
  const shim = join(out, "bin", "dispatch")
  expect((await stat(shim)).mode & 0o111).toBe(0o111)
  const run = Bun.spawn([shim, "--help"], {
    cwd: scratch,
    env: { PATH: process.env["PATH"] ?? "", HOME: scratch },
    stdout: "pipe",
    stderr: "pipe",
  })
  expect(await run.exited).toBe(0)
}, 30_000)

test("--out refuses a directory inside the repository and creates nothing", async () => {
  const inside = join(packageRoot, `build-out-${randomUUID()}`)
  try {
    const result = await build("--out", inside)
    expect(result.exitCode).not.toBe(0)
    expect(existsSync(inside)).toBe(false)
  } finally {
    await rm(inside, { recursive: true, force: true })
  }
}, 30_000)

test("--out refuses an existing directory that is not empty and leaves it as it was", async () => {
  const occupied = await mkdtemp(join(tmpdir(), "claude-envoy-build-out-occupied-"))
  try {
    await writeFile(join(occupied, "keep"), "")
    const result = await build("--out", occupied)
    expect(result.exitCode).not.toBe(0)
    expect(await readdir(occupied)).toEqual(["keep"])
  } finally {
    await rm(occupied, { recursive: true, force: true })
  }
}, 30_000)

test("with no mode, more than one, or --out without a directory the script exits 2", async () => {
  expect((await build()).exitCode).toBe(2)
  expect((await build("--check", "--in-place")).exitCode).toBe(2)
  expect((await build("--out")).exitCode).toBe(2)
  expect((await build("--unknown")).exitCode).toBe(2)
}, 30_000)
