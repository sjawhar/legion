import { expect, test } from "bun:test"
import { cp, mkdtemp, readdir, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { BUNDLE_ENTRYPOINTS, buildBundles } from "../scripts/build"

const packageRoot = resolve(import.meta.dir, "..")
const distDirectory = join(packageRoot, "dist")

test("the committed bundle starts without node_modules and demands ENVOY_NATS_URL", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-dist-"))
  try {
    await cp(distDirectory, join(scratch, "dist"), { recursive: true })
    const run = Bun.spawn(["bun", join(scratch, "dist", "envoy-channel.js")], {
      cwd: scratch,
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: scratch,
        CLAUDE_CODE_SESSION_ID: "test",
        CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      },
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
    })
    const [exitCode, stderr] = await Promise.all([run.exited, new Response(run.stderr).text()])

    expect(exitCode).toBe(1)
    expect(stderr).toContain("ENVOY_NATS_URL is required")
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("the committed bundle inlines every dependency", async () => {
  for (const name of Object.keys(BUNDLE_ENTRYPOINTS)) {
    const bundle = await readFile(join(distDirectory, `${name}.js`), "utf8")
    expect(bundle).not.toContain("workspace:")
    expect(bundle).not.toContain("node_modules/")
  }
})

/** Where two bundles part ways, with 80 characters of each side around it. */
function describeMismatch(file: string, fresh: Buffer, current: Buffer): string {
  const shorter = Math.min(fresh.length, current.length)
  let offset = 0
  while (offset < shorter && fresh[offset] === current[offset]) offset += 1
  const context = (bytes: Buffer): string =>
    JSON.stringify(bytes.subarray(Math.max(0, offset - 80), offset + 80).toString("utf8"))
  return [
    `${file}: fresh build differs from committed dist/`,
    `fresh ${fresh.length} bytes, committed ${current.length} bytes, first difference at byte ${offset}`,
    `fresh:     ${context(fresh)}`,
    `committed: ${context(current)}`,
  ].join("\n")
}

test("rebuilding reproduces the committed bundle byte for byte", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-rebuild-"))
  try {
    await buildBundles(scratch)
    const committed = (await readdir(distDirectory)).sort()
    expect((await readdir(scratch)).sort()).toEqual(committed)
    for (const file of committed) {
      const fresh = await readFile(join(scratch, file))
      const current = await readFile(join(distDirectory, file))
      if (!fresh.equals(current)) throw new Error(describeMismatch(file, fresh, current))
    }
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
}, 30_000)
