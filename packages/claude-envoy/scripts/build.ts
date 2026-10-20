// Bundle the plugin's executables into self-contained files the Claude Code
// plugin cache can run: the cache holds the git tree with no node_modules, so
// every dependency (workspace packages included) is inlined here and `dist/`
// is committed, with `THIRD_PARTY_NOTICES` beside the bundles holding the
// license of every third-party package they inline. `--check` rebuilds into a
// scratch directory and fails when the result differs from the committed
// files; CI runs it on the Bun version pinned in the repo-root `.bun-version`,
// because bundler output depends on the exact Bun build (LEGION-568).
//
// The same run generates `legion/`, the Legion plugin a Claude Code pane loads
// beside this one (`buildLegionPlugin`), and `--check` holds it to a fresh
// generation too.
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { thirdPartyNotices } from "../../../scripts/third-party-notices"

const packageRoot = resolve(import.meta.dir, "..")
const repoRoot = resolve(packageRoot, "..", "..")
/** Read once, here, rather than wherever it is needed: every caller below compares against the
 *  same string for the same process, and `assertPinnedBun`'s test already covers the comparison
 *  itself without needing a real file on disk. */
const pinnedBunVersion = (await readFile(join(repoRoot, ".bun-version"), "utf8")).trim()

/** Output name -> source entrypoint, relative to the package root. `dispatch` is the `dispatch`
 *  CLI that `bin/dispatch` runs, built from the shared library's entry. */
export const BUNDLE_ENTRYPOINTS = {
  "envoy-channel": "bin/envoy-channel.ts",
  "session-hook": "hooks/session-hook.ts",
  dispatch: "../envoy-client/bin/dispatch.ts",
} as const

/** Oh My Pi's tool names, as pi-legion's agents list them, to Claude Code's. A tool this Claude
 *  Code build lacks maps to nothing: it has no Glob or Grep tool (an agent searches with `rg` or
 *  `fd` through Bash where it has Bash), and its own task list stands in for `todo`. A name in
 *  neither list is refused, so a new tool is mapped on purpose. */
export const CLAUDE_TOOL_NAMES: Readonly<Record<string, string | null>> = {
  read: "Read",
  bash: "Bash",
  edit: "Edit",
  write: "Write",
  glob: null,
  grep: null,
  todo: null,
}

/** One pi-legion agent file in Claude Code's agent format. The frontmatter keeps `name`,
 *  `description` and `color`; `tools` is mapped through `CLAUDE_TOOL_NAMES`; `autoloadSkills`
 *  becomes `skills`, each the plugin's namespaced `legion:<skill>`; `model` (an Oh My Pi model
 *  role, which Claude Code does not read, so the agent runs on the pane's model), `blocking` (Claude
 *  Code waits on an agent unless told otherwise) and every comment are dropped. The body is kept
 *  as it is. */
export function claudeAgent(file: string, source: string): string {
  const match = /^---\n([\s\S]*?)\n---\n/.exec(source)
  if (match === null) throw new Error(`${file} has no frontmatter`)
  const lines: string[] = []
  let dropping = false
  for (const line of (match[1] as string).split("\n")) {
    if (line.startsWith("#")) continue
    const key = /^([A-Za-z]+):/.exec(line)?.[1]
    if (key === undefined) {
      if (!dropping) lines.push(line)
      continue
    }
    const value = line.slice(key.length + 1).trim()
    dropping = key === "model" || key === "blocking"
    if (dropping) continue
    if (key === "tools") {
      const tools = value.split(",").map((name) => {
        const tool = name.trim()
        const mapped = CLAUDE_TOOL_NAMES[tool]
        if (mapped === undefined) {
          throw new Error(`${file} lists the tool ${tool}, which CLAUDE_TOOL_NAMES does not map`)
        }
        return mapped
      })
      const claude = tools.filter((tool): tool is string => tool !== null)
      if (claude.length === 0) throw new Error(`${file} keeps no tool Claude Code has`)
      lines.push(`tools: ${claude.join(", ")}`)
    } else if (key === "autoloadSkills") {
      const skills = /^\[(.*)\]$/.exec(value)?.[1]
      if (skills === undefined) throw new Error(`${file}'s autoloadSkills is not a flow list`)
      const names = skills.split(",").map((skill) => `legion:${skill.trim()}`)
      lines.push(`skills: [${names.join(", ")}]`)
    } else {
      lines.push(line)
    }
  }
  return `---\n${lines.join("\n")}\n---\n${source.slice(match[0].length)}`
}

/** `legion/`, the Legion plugin a Claude Code pane loads with `--plugin-dir` beside this one
 *  (packages/daemon/internal/runtime/tmux/claude.go): its manifest, named `legion`, so Claude Code
 *  names its skills and agents `legion:<name>`; the skills pi-legion ships, staged by the one script
 *  that partitions them (scripts/pi-plugin-prepack.sh --stage-skills); and pi-legion's task agents
 *  in Claude Code's format (`claudeAgent`). Generated, never edited by hand. */
export async function buildLegionPlugin(outdir: string): Promise<void> {
  await mkdir(join(outdir, ".claude-plugin"), { recursive: true })
  await writeFile(
    join(outdir, ".claude-plugin", "plugin.json"),
    `${JSON.stringify(
      {
        name: "legion",
        description:
          "Legion's skills and task agents for a phase worker running on Claude Code, generated from the repository's skills/ and packages/pi-legion/agents by packages/claude-envoy/scripts/build.ts.",
      },
      null,
      2,
    )}\n`,
  )
  const staged = Bun.spawnSync([
    "bash",
    join(repoRoot, "scripts", "pi-plugin-prepack.sh"),
    "--stage-skills",
    "@sjawhar/pi-legion",
    join(outdir, "skills"),
  ])
  if (staged.exitCode !== 0) {
    throw new Error(`staging pi-legion's skills failed: ${staged.stderr.toString().trim()}`)
  }
  const agents = join(repoRoot, "packages", "pi-legion", "agents")
  await mkdir(join(outdir, "agents"))
  for (const name of (await readdir(agents)).filter((file) => file.endsWith(".md")).sort()) {
    const source = await readFile(join(agents, name), "utf8")
    await writeFile(join(outdir, "agents", name), claudeAgent(name, source))
  }
}

/** Each file under `fresh` or `committed` that is missing from the other or differs in bytes,
 *  relative to its root. */
async function treeDifferences(fresh: string, committed: string): Promise<string[]> {
  const files = async (root: string): Promise<Set<string>> => {
    const entries = await readdir(root, { recursive: true, withFileTypes: true }).catch(() => [])
    return new Set(
      entries
        .filter((entry) => entry.isFile())
        .map((entry) => join(entry.parentPath, entry.name).slice(root.length + 1)),
    )
  }
  const [freshFiles, committedFiles] = await Promise.all([files(fresh), files(committed)])
  const differences: string[] = []
  for (const file of [...freshFiles.union(committedFiles)].sort()) {
    if (!freshFiles.has(file) || !committedFiles.has(file)) {
      differences.push(file)
      continue
    }
    const [freshBytes, committedBytes] = await Promise.all([
      readFile(join(fresh, file)),
      readFile(join(committed, file)),
    ])
    if (!freshBytes.equals(committedBytes)) differences.push(file)
  }
  return differences
}

/** The files of the committed `legion/` a fresh generation would change. */
export async function legionPluginDifferences(committed: string): Promise<string[]> {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-check-legion-"))
  try {
    await buildLegionPlugin(scratch)
    return await treeDifferences(scratch, committed)
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
}

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
  assertPinnedBun(Bun.version, pinnedBunVersion)
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
    //
    // This is a separate, compounding cause from `assertPinnedBun` above: that guard stops a
    // wrong Bun *version* from running at all, on the identical pinned version this fixes a
    // non-determinism *within* one Bun build's own bundler. Either one alone could make
    // `check-dist` fail unpredictably; both were real.
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
    return await treeDifferences(scratch, distDirectory)
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
}

if (import.meta.main) {
  // Checked before anything else touches the filesystem: --check's scratch build and a plain
  // build's `rm` of the committed dist/ both cost real work or a real deletion for a build this
  // process was always going to refuse.
  assertPinnedBun(Bun.version, pinnedBunVersion)
  const distDirectory = join(packageRoot, "dist")
  const legionDirectory = join(packageRoot, "legion")
  if (process.argv.includes("--check")) {
    const stale = [
      ...(await checkBundles(distDirectory)).map((file) => `dist/${file}`),
      ...(await legionPluginDifferences(legionDirectory)).map((file) => `legion/${file}`),
    ]
    if (stale.length > 0) {
      process.stderr.write(
        `generated files are stale (${stale.join(", ")}); run \`bun run build\` on Bun ${pinnedBunVersion} and commit the result\n`,
      )
      process.exit(1)
    }
    process.stdout.write("dist/ and legion/ match a fresh build\n")
  } else {
    await rm(distDirectory, { recursive: true, force: true })
    await buildBundles(distDirectory)
    await rm(legionDirectory, { recursive: true, force: true })
    await buildLegionPlugin(legionDirectory)
    process.stdout.write(
      `built ${Object.keys(BUNDLE_ENTRYPOINTS).join(", ")} into dist/ and the Legion plugin into legion/\n`,
    )
  }
}
