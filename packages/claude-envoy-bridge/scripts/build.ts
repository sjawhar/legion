// Bundle the plugin's two executables into self-contained files the Claude Code
// plugin cache can run: the cache holds the git tree with no node_modules, so
// every dependency (workspace packages included) is inlined here and `dist/`
// is committed. `--check` rebuilds into a scratch directory and fails when the
// result differs from the committed files; CI runs it on the Bun version pinned
// in the repo-root `.bun-version`, because bundler output differs across Bun
// releases.
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

const packageRoot = resolve(import.meta.dir, "..")

/** Output name -> source entrypoint, relative to the package root. */
export const BUNDLE_ENTRYPOINTS = {
  "envoy-channel": "bin/envoy-channel.ts",
  "open-asks-hook": "hooks/open-asks-hook.ts",
} as const

export async function buildBundles(outdir: string): Promise<void> {
  const result = await Bun.build({
    entrypoints: Object.values(BUNDLE_ENTRYPOINTS).map((entry) => join(packageRoot, entry)),
    outdir,
    target: "bun",
    // Syntax minification stays off: Bun 1.3.14's constant folding of a multi-operand
    // string concatenation (`"a " + "b " + "c"`) sometimes emits only the first operand
    // in CI builds (~1 in 10; run 35220693690 truncated dispatch_open_asks' description at
    // "Omit project to "), which corrupted the tool text and failed check-dist against a
    // correct committed bundle. Whitespace and identifier minification are unaffected.
    minify: { whitespace: true, identifiers: true, syntax: false },
    sourcemap: "none",
    naming: "[name].[ext]",
  })
  if (!result.success) {
    throw new AggregateError(result.logs, "bundle build failed")
  }
}

async function checkBundles(distDirectory: string): Promise<string[]> {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-bridge-check-dist-"))
  try {
    await buildBundles(scratch)
    const fresh = (await readdir(scratch)).sort()
    const committed = (await readdir(distDirectory).catch(() => [])).sort()
    const differences: string[] = []
    for (const file of new Set([...fresh, ...committed])) {
      if (!fresh.includes(file) || !committed.includes(file)) {
        differences.push(file)
        continue
      }
      const [freshBytes, committedBytes] = await Promise.all([
        readFile(join(scratch, file)),
        readFile(join(distDirectory, file)),
      ])
      if (!freshBytes.equals(committedBytes)) differences.push(file)
    }
    return differences
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
}

if (import.meta.main) {
  const distDirectory = join(packageRoot, "dist")
  if (process.argv.includes("--check")) {
    const differences = await checkBundles(distDirectory)
    if (differences.length > 0) {
      process.stderr.write(
        `dist/ is stale (${differences.join(", ")}); run \`bun run build\` on Bun ${await readFile(join(packageRoot, "..", "..", ".bun-version"), "utf8").then((version) => version.trim())} and commit the result\n`,
      )
      process.exit(1)
    }
    process.stdout.write("dist/ matches a fresh build\n")
  } else {
    await rm(distDirectory, { recursive: true, force: true })
    await buildBundles(distDirectory)
    process.stdout.write(`built ${Object.keys(BUNDLE_ENTRYPOINTS).join(", ")} into dist/\n`)
  }
}
