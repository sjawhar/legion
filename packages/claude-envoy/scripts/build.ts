// Bundle the plugin's executables into self-contained files the Claude Code
// plugin cache can run: the cache holds the git tree with no node_modules, so
// every dependency (workspace packages included) is inlined here and `dist/`
// is committed, with `THIRD_PARTY_NOTICES` beside the bundles holding the
// license of every third-party package they inline. `--check` rebuilds into a
// scratch directory and fails when the result differs from the committed
// files; CI runs it on the Bun version pinned in the repo-root `.bun-version`,
// because bundler output depends on the exact Bun build (LEGION-568).
import { mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { thirdPartyNotices } from "../../../scripts/third-party-notices"

const packageRoot = resolve(import.meta.dir, "..")
const repoRoot = resolve(packageRoot, "..", "..")

/** Output name -> source entrypoint, relative to the package root. */
export const BUNDLE_ENTRYPOINTS = {
  "envoy-channel": "bin/envoy-channel.ts",
  "session-hook": "hooks/session-hook.ts",
} as const

/**
 * `bun run build` and `bun run check-dist` spawn a new "bun" process to run this file:
 * package.json's script text is a shell command, and Bun's own `run` resolves the "bun" in it from
 * PATH rather than reusing whichever binary the caller invoked `bun run` with. A devbox whose
 * default Bun (a version manager's active version, distinct from the one the caller meant to
 * invoke) differs from the repository's pin then silently bundles with that other build instead —
 * and the bundler's literal output depends on the exact Bun build (LEGION-568): confirmed directly
 * between a devbox and the CI runner, and between two Bun releases on one machine, same source,
 * same lockfile. This refuses rather than commit whatever that other build produced. Exported so
 * the test can check the message without installing a second real Bun.
 */
export function assertPinnedBun(running: string, pinned: string): void {
  if (running === pinned) return
  throw new Error(
    `refusing to build under Bun ${running}: .bun-version pins ${pinned}, and the bundler's ` +
      `output depends on the exact Bun build (LEGION-568). Invoke the pinned binary's own path ` +
      `directly — not "bun run", which resolves "bun" from PATH — when a version manager's ` +
      "default differs from the pin.",
  )
}

export async function buildBundles(outdir: string): Promise<void> {
  assertPinnedBun(Bun.version, (await readFile(join(repoRoot, ".bun-version"), "utf8")).trim())
  const result = await Bun.build({
    entrypoints: Object.values(BUNDLE_ENTRYPOINTS).map((entry) => join(packageRoot, entry)),
    outdir,
    target: "bun",
    // Minification is off entirely, not case by case: disabling whitespace/identifier/syntax
    // minification individually (`{whitespace: false, identifiers: false, syntax: false}`) still
    // routes through Bun 1.3.14's minifying code-generation path, which picks a non-deterministic
    // CJS/ESM interop check on repeated builds of this exact module graph (observed directly: ten
    // rebuilds of one committed checkout, eight of them disagreeing byte for byte with each
    // other) — on top of the syntax minifier's own separate bug, truncating constant-folded
    // multi-operand string concatenation in CI builds, that made `syntax: false` necessary before
    // this. A plain `false` bypasses that whole path and reproducible across repeated builds was
    // the same ten rebuilds, now agreeing every time. Whitespace stays readable as a side effect,
    // so unrelated source changes still retain distinct bundle lines and merge cleanly.
    minify: false,
    sourcemap: "none",
    naming: "[name].[ext]",
    metafile: true,
  })
  if (!result.success) {
    throw new AggregateError(result.logs, "bundle build failed")
  }
  if (!result.metafile) throw new Error("Bun.build returned no metafile")
  await writeFile(
    join(outdir, "THIRD_PARTY_NOTICES"),
    await thirdPartyNotices(Object.keys(result.metafile.inputs), process.cwd()),
  )
}

async function checkBundles(distDirectory: string): Promise<string[]> {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-check-dist-"))
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
  // Checked before anything else touches the filesystem: --check's scratch build and a plain
  // build's `rm` of the committed dist/ both cost real work or a real deletion for a build this
  // process was always going to refuse.
  assertPinnedBun(Bun.version, (await readFile(join(repoRoot, ".bun-version"), "utf8")).trim())
  const distDirectory = join(packageRoot, "dist")
  if (process.argv.includes("--check")) {
    const differences = await checkBundles(distDirectory)
    if (differences.length > 0) {
      process.stderr.write(
        `dist/ is stale (${differences.join(", ")}); run \`bun run build\` on Bun ${await readFile(join(repoRoot, ".bun-version"), "utf8").then((version) => version.trim())} and commit the result\n`,
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
