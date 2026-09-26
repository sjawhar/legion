import { expect, test } from "bun:test"
import { cp, mkdtemp, readdir, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { BUNDLE_ENTRYPOINTS, buildBundles } from "../scripts/build"

const packageRoot = resolve(import.meta.dir, "..")
const distDirectory = join(packageRoot, "dist")

const externalDependencySpecifierPatterns = [
  /\b(?:require|import\.meta\.require|import)\s*\(\s*(["'])[^"'\r\n]*node_modules\/[^"'\r\n]*\1\s*\)/g,
  /\b(?:import|export)\s+(?:[^\r\n;]*?\s+from\s+)?(["'])[^"'\r\n]*node_modules\/[^"'\r\n]*\1/g,
]

function findExternalDependencySpecifiers(source: string): string[] {
  const code = source.replace(/\/\*[\s\S]*?\*\/|\/\/[^\r\n]*/g, "")
  return externalDependencySpecifierPatterns.flatMap((pattern) =>
    Array.from(code.matchAll(pattern), ([match]) => match),
  )
}

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

test("the dependency guard ignores comments and detects runtime node_modules specifiers", async () => {
  const comments = [
    '// require("/tmp/node_modules/comment.js")',
    '/* import.meta.require("/tmp/node_modules/comment.js") */',
  ].join("\n")
  expect(findExternalDependencySpecifiers(comments)).toEqual([])

  const specifiers = [
    'require("/tmp/node_modules/require.js")',
    'import.meta.require("/tmp/node_modules/meta-require.js")',
    'import("/tmp/node_modules/import.js")',
    'import dependency from "/tmp/node_modules/static-import.js"',
    'export { dependency } from "/tmp/node_modules/static-export.js"',
  ]
  const matches = findExternalDependencySpecifiers(specifiers.join("\n"))
  for (const specifier of specifiers) expect(matches).toContain(specifier)
})

test("the committed bundles contain no external dependency call specifiers", async () => {
  for (const name of Object.keys(BUNDLE_ENTRYPOINTS)) {
    const bundle = await readFile(join(distDirectory, `${name}.js`), "utf8")
    expect(findExternalDependencySpecifiers(bundle)).toEqual([])
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
