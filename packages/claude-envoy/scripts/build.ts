// Bundle the plugin's executables into self-contained files the Claude Code
// plugin cache can run: the cache holds main's git tree with no node_modules,
// so every dependency (workspace packages included) is inlined here, with
// `THIRD_PARTY_NOTICES` beside the bundles holding the license of every
// third-party package they inline. The same build generates `legion/`, the
// Legion plugin a Claude Code pane loads beside this one (`buildLegionPlugin`).
//
// `dist/` and `legion/` stay tracked on main, written only by main's Rebuild
// Claude Envoy plugin workflow after each merge, from a checked `--out` build
// (scripts/verify-build.sh); a pull request never commits them, and no mode
// writes them. Every mode builds only on the Bun the repo-root `.bun-version`
// pins, because bundler output depends on the exact Bun build (LEGION-568):
//   --check      build into a scratch directory and fail when the tracked files
//                differ from it;
//   --out <dir>  make <dir>, a new or empty directory outside the repository, a
//                runnable copy of the plugin built from this tree.
//
// scripts/build-inputs.txt must name every file this build reads, since main's push job decides
// from it alone whether a moved main changed the build. tests/build-inputs.test.ts checks it against
// what buildBundles and buildLegionPlugin report reading; a file this script reads by path or
// imports from outside the listed packages (.bun-version, scripts/third-party-notices.ts, the
// scripts it runs) is listed by hand.
import { cp, mkdir, mkdtemp, readdir, readFile, realpath, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from "node:path"
import { thirdPartyNotices } from "../../../scripts/third-party-notices"
import { PHASE_HOOK_TOOL_NAME } from "../src/legion-phase-hook"

const packageRoot = resolve(import.meta.dir, "..")
const repoRoot = resolve(packageRoot, "..", "..")
/** Read once, here, rather than wherever it is needed: every caller below compares against the
 *  same string for the same process, and `assertPinnedBun`'s test already covers the comparison
 *  itself without needing a real file on disk. */
const pinnedBunVersion = (await readFile(join(repoRoot, ".bun-version"), "utf8")).trim()

/** Output name -> source entrypoint, relative to the package root. `dispatch` is the `dispatch`
 *  CLI that `bin/dispatch` runs, built from the shared library's entry. `session-title-hook` is a
 *  bundle of its own rather than a mode of `session-hook`, because Claude Code blocks the user's
 *  prompt on it: an ESM entrypoint evaluates its whole module graph before it reads `argv`, so
 *  sharing one would make every prompt pay for zod and the Dispatch client. */
export const BUNDLE_ENTRYPOINTS = {
  "envoy-channel": "bin/envoy-channel.ts",
  "session-hook": "hooks/session-hook.ts",
  "session-title-hook": "hooks/session-title-hook.ts",
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

/** The phase reminder's hook entries (INFRA-220): the events Claude Code fires at a phase
 *  worker, each an `mcp_tool` hook on the pane's own Legion MCP server, which `claude` registers
 *  as the bare name `legion` (packages/daemon/internal/runtime/tmux/claude.go), and on the tool
 *  `legion_phase_hook` that server offers (packages/claude-envoy/src/legion-phase-hook.ts).
 *  Claude Code substitutes each `${…}` as a string, which the endpoint parses.
 *
 *  They belong to the Legion plugin alone, never to `hooks/hooks.json`: `@sjawhar/claude-envoy`
 *  is also installed as a plain Claude Code plugin, where `--mcp-config` registers no `legion`
 *  server and every one of these would be a hook error on every turn. A Legion pane loads both
 *  plugin directories, so its worker still gets them. */
// biome-ignore-start lint/suspicious/noTemplateCurlyInString: Claude Code substitutes each `${…}`
// into the hook's input before it calls the tool; they are its syntax, not this file's.
const LEGION_HOOKS: readonly {
  readonly event: string
  readonly input: Readonly<Record<string, string>>
  readonly timeout: number
}[] = [
  {
    event: "Stop",
    input: {
      event: "Stop",
      lastAssistantMessage: "${last_assistant_message}",
      stopHookActive: "${stop_hook_active}",
      // The session's own background tasks, each `{ id, type, status, description, command }`,
      // rendered as JSON text: a turn that ends with one still running is a worker waiting to be
      // woken by it rather than a stalled one, and earns no reminder (INFRA-338 R8).
      backgroundTasks: "${background_tasks}",
    },
    // The `Stop` answer waits on the reminder's record check, which bounds its own daemon read at
    // 5 s (RECORD_READ_BOUND_MS, packages/pi-shared/src/phase-reminder.ts); the others answer from
    // memory and the state file alone.
    timeout: 30,
  },
  {
    event: "StopFailure",
    input: { event: "StopFailure", lastAssistantMessage: "${last_assistant_message}" },
    timeout: 10,
  },
  {
    event: "PostToolUse",
    input: { event: "PostToolUse", toolName: "${tool_name}", agentId: "${agent_id}" },
    timeout: 10,
  },
  {
    event: "PostToolUseFailure",
    input: { event: "PostToolUseFailure", toolName: "${tool_name}", agentId: "${agent_id}" },
    timeout: 10,
  },
  {
    event: "UserPromptSubmit",
    input: { event: "UserPromptSubmit", prompt: "${prompt}" },
    timeout: 10,
  },
]
// biome-ignore-end lint/suspicious/noTemplateCurlyInString: the hook inputs end here.

/** `legion/`, the Legion plugin a Claude Code pane loads with `--plugin-dir` beside this one
 *  (packages/daemon/internal/runtime/tmux/claude.go): its manifest, named `legion`, so Claude Code
 *  names its skills and agents `legion:<name>`; the skills pi-legion ships, staged by the one script
 *  that partitions them (scripts/pi-plugin-prepack.sh --stage-skills); pi-legion's task agents
 *  in Claude Code's format (`claudeAgent`); and the phase reminder's hooks (`LEGION_HOOKS`).
 *  Generated, never edited by hand.
 *
 *  Returns, as `buildBundles` does, the absolute path of every repository file it read: each agent
 *  file, and the source each staged skill file was copied from. tests/build-inputs.test.ts checks
 *  the input list against it, so a source added here is one the list must name rather than one the
 *  test can miss. Callers that only want the output ignore the result. */
export async function buildLegionPlugin(outdir: string): Promise<string[]> {
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
  const stagedSkills = join(outdir, "skills")
  const staged = Bun.spawnSync([
    "bash",
    join(repoRoot, "scripts", "pi-plugin-prepack.sh"),
    "--stage-skills",
    "@sjawhar/pi-legion",
    stagedSkills,
  ])
  if (staged.exitCode !== 0) {
    throw new Error(`staging pi-legion's skills failed: ${staged.stderr.toString().trim()}`)
  }
  // Each staged file is a copy of skills/<the same path>, since stage_skills copies whole skill
  // directories (`cp -R "$root/skills/$skill" "$dest/$skill"`), so the file read is that source and
  // not the copy written here. A source that is not there means this mapping, not a skill, is wrong.
  const reads: string[] = []
  for (const entry of await readdir(stagedSkills, { recursive: true, withFileTypes: true })) {
    if (entry.isDirectory()) continue
    const relativePath = relative(stagedSkills, join(entry.parentPath, entry.name))
    const source = join(repoRoot, "skills", relativePath)
    if (!(await Bun.file(source).exists())) {
      throw new Error(`staged skill file ${relativePath} does not come from ${source}`)
    }
    reads.push(source)
  }
  const agents = join(repoRoot, "packages", "pi-legion", "agents")
  await mkdir(join(outdir, "agents"))
  for (const name of (await readdir(agents)).filter((file) => file.endsWith(".md")).sort()) {
    const source = await readFile(join(agents, name), "utf8")
    await writeFile(join(outdir, "agents", name), claudeAgent(name, source))
    reads.push(join(agents, name))
  }
  await mkdir(join(outdir, "hooks"))
  await writeFile(
    join(outdir, "hooks", "hooks.json"),
    `${JSON.stringify(
      {
        description:
          "Legion: tell a phase worker whose turn ends with its phase still open, through the legion_phase_hook tool on the pane's own Legion MCP server.",
        hooks: Object.fromEntries(
          LEGION_HOOKS.map(({ event, input, timeout }) => [
            event,
            [
              {
                hooks: [
                  {
                    type: "mcp_tool",
                    server: "legion",
                    tool: PHASE_HOOK_TOOL_NAME,
                    input,
                    timeout,
                  },
                ],
              },
            ],
          ]),
        ),
      },
      null,
      2,
    )}\n`,
  )
  return reads
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
async function legionPluginDifferences(committed: string): Promise<string[]> {
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

/** The bundles and their notices, built into `outdir`; returns the absolute path of every file the
 *  bundles read (the metafile's inputs), which tests/build-inputs.test.ts checks the input list
 *  against. Bun writes each inlined module's path into the bundle relative to the working
 *  directory (`// ../envoy-client/bin/dispatch.ts` from the package, `// packages/envoy-client/...`
 *  from the repository root), so the build runs from the package root whatever the caller's
 *  directory: main's committed bytes and every check of them then agree. */
export async function buildBundles(outdir: string): Promise<string[]> {
  assertPinnedBun(Bun.version, pinnedBunVersion)
  const target = resolve(outdir)
  const callerDirectory = process.cwd()
  process.chdir(packageRoot)
  try {
    return await bundleFromPackageRoot(target)
  } finally {
    process.chdir(callerDirectory)
  }
}

async function bundleFromPackageRoot(outdir: string): Promise<string[]> {
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
  // The metafile's keys are relative to the working directory, the package root.
  const inputs = Object.keys(result.metafile.inputs)
  await writeFile(join(outdir, "THIRD_PARTY_NOTICES"), await thirdPartyNotices(inputs, packageRoot))
  return inputs.map((input) => resolve(packageRoot, input))
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

/** What `--out` copies from the package beside the built `dist/` and `legion/`: the plugin's
 *  manifests, its MCP and hook configuration, and the shim and skills they reach. */
const PLUGIN_SOURCES = [
  ".claude-plugin",
  ".mcp.json",
  ".omp-plugin",
  "bin",
  "hooks",
  "package.json",
  "skills",
] as const

/** `path`, absolute, with the symlinks of its deepest existing ancestor resolved and the missing
 *  rest joined on, so a directory `--out` has yet to create compares like one that exists. */
async function resolvedPath(path: string): Promise<string> {
  let existing = resolve(path)
  const missing: string[] = []
  for (;;) {
    try {
      return join(await realpath(existing), ...missing)
    } catch (error) {
      const parent = dirname(existing)
      if ((error as NodeJS.ErrnoException).code !== "ENOENT" || parent === existing) throw error
      missing.unshift(basename(existing))
      existing = parent
    }
  }
}

/** Why `--out` may not write `out`, or null when it may: inside the repository its files would be
 *  ones a pull request could commit, and an existing file there would be mixed with the build. */
async function outRefusal(out: string): Promise<string | null> {
  const [target, root] = await Promise.all([resolvedPath(out), realpath(repoRoot)])
  const fromRoot = relative(root, target)
  if (fromRoot !== ".." && !fromRoot.startsWith(`..${sep}`) && !isAbsolute(fromRoot)) {
    return `${out} is inside the repository (${root}); a pull request never commits a build`
  }
  try {
    if ((await readdir(target)).length > 0) return `${out} exists and is not empty`
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code
    if (code === "ENOTDIR") return `${out} exists and is not a directory`
    if (code !== "ENOENT") throw error
  }
  return null
}

/** `out` as a runnable plugin directory: the package's plugin files, with each `skills/<name>`
 *  link to the repository's skill copied as the files it points at (file modes kept, so
 *  `bin/dispatch` stays executable), then a fresh `dist/` and `legion/`. */
async function buildOut(out: string): Promise<void> {
  await mkdir(out, { recursive: true })
  for (const source of PLUGIN_SOURCES) {
    await cp(join(packageRoot, source), join(out, source), { recursive: true, dereference: true })
  }
  await buildBundles(join(out, "dist"))
  await buildLegionPlugin(join(out, "legion"))
}

const USAGE =
  "usage: bun scripts/build.ts --check | --out <dir>. The tracked dist/ and legion/ are written " +
  "only by main's Rebuild Claude Envoy plugin workflow, and a pull request never commits them."

type Mode = { readonly kind: "check" } | { readonly kind: "out"; readonly dir: string }

/** The one mode `args` names, or null for none, more than one, an unknown argument, or `--out`
 *  with no directory after it. */
function parseMode(args: readonly string[]): Mode | null {
  const [first, second, ...rest] = args
  if (rest.length > 0) return null
  if (second === undefined) {
    if (first === "--check") return { kind: "check" }
    return null
  }
  if (first === "--out" && !second.startsWith("--")) return { kind: "out", dir: second }
  return null
}

if (import.meta.main) {
  // Checked before anything else touches the filesystem: --check's scratch build and --out's copy
  // cost real work for a build this process was always going to refuse.
  assertPinnedBun(Bun.version, pinnedBunVersion)
  const mode = parseMode(process.argv.slice(2))
  const distDirectory = join(packageRoot, "dist")
  const legionDirectory = join(packageRoot, "legion")
  if (mode === null) {
    process.stderr.write(`${USAGE}\n`)
    process.exit(2)
  } else if (mode.kind === "check") {
    const stale = [
      ...(await checkBundles(distDirectory)).map((file) => `dist/${file}`),
      ...(await legionPluginDifferences(legionDirectory)).map((file) => `legion/${file}`),
    ]
    if (stale.length > 0) {
      process.stderr.write(
        `generated files are stale (${stale.join(", ")}); main's Rebuild Claude Envoy plugin workflow commits a fresh build after each merge, and a pull request never commits these files\n`,
      )
      process.exit(1)
    }
    process.stdout.write("dist/ and legion/ match a fresh build\n")
  } else {
    const refusal = await outRefusal(mode.dir)
    if (refusal !== null) {
      process.stderr.write(`refusing --out: ${refusal}\n`)
      process.exit(1)
    }
    await buildOut(mode.dir)
    process.stdout.write(`built the Claude Envoy plugin into ${resolve(mode.dir)}\n`)
  }
}
