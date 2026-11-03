import { expect, test } from "bun:test"
import { existsSync } from "node:fs"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path"

const packageRoot = resolve(import.meta.dir, "..")
const repoRoot = resolve(packageRoot, "..", "..")

/** `command` run in `cwd`; its stdout, or an error naming the command and its stderr when it fails. */
async function run(command: string[], cwd: string, env?: Record<string, string>): Promise<string> {
  const child = Bun.spawn(command, {
    cwd,
    ...(env === undefined ? {} : { env }),
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ])
  if (exitCode !== 0) {
    throw new Error(`${command.join(" ")} exited ${exitCode}: ${stderr.trim()}`)
  }
  return stdout
}

/** Every file the build reads, repository-relative, by running it: both halves of the build report
 *  what they read, so a source either of them starts reading is covered here without this test
 *  listing any of them itself. The build runs in a process of its own because a second Bun.build in
 *  a test process that has already run other test files fails reading its sources with EISDIR
 *  (tests/dist-fresh.test.ts builds the same way). */
async function filesTheBuildReads(scratch: string): Promise<string[]> {
  const build = JSON.stringify(join(packageRoot, "scripts", "build.ts"))
  const script = [
    `import { buildBundles, buildLegionPlugin } from ${build}`,
    "console.log(",
    "  JSON.stringify({",
    `    bundles: await buildBundles(${JSON.stringify(join(scratch, "dist"))}),`,
    `    legion: await buildLegionPlugin(${JSON.stringify(join(scratch, "legion"))}),`,
    "  }),",
    ")",
  ].join("\n")
  const read: { bundles: string[]; legion: string[] } = JSON.parse(
    await run([process.execPath, "--eval", script], packageRoot),
  )
  const bundleInputs = read.bundles.map((input) => relative(repoRoot, input))
  const legionInputs = read.legion.map((input) => relative(repoRoot, input))
  const outside = [...bundleInputs, ...legionInputs].filter(
    (input) => input.startsWith("..") || isAbsolute(input),
  )
  expect(outside).toEqual([])

  // The node_modules and package-root probes below are about how Bun resolved and transpiled a
  // bundled source, so only the bundles' inputs go through them. `buildLegionPlugin` reads the
  // skills and agents as plain files, with no resolution or notices behind them, and probing their
  // package roots would demand packages/pi-legion/package.json and the like from the list for
  // files the build never reads.
  const probes = new Set<string>(legionInputs)
  const local = bundleInputs.filter((input) => !input.split(sep).includes("node_modules"))
  for (const input of local) probes.add(input)
  // A module under node_modules is whatever `bun install` put there, which the lockfile, the root
  // manifest and the patches it applies decide.
  if (local.length < bundleInputs.length) {
    const manifest = JSON.parse(await readFile(join(repoRoot, "package.json"), "utf8")) as {
      patchedDependencies?: Record<string, string>
    }
    probes.add("bun.lock")
    probes.add("package.json")
    for (const patch of Object.values(manifest.patchedDependencies ?? {})) probes.add(patch)
  }
  // The workspace package a source sits in: its manifest and tsconfig steer how Bun resolves and
  // transpiles the source, and a license or notice file at its top puts the package in the bundle's
  // notices. "LICENSE" and "NOTICE" are names scripts/third-party-notices.ts's unexported
  // LICENSE_FILE and NOTICE_FILE patterns match; a file that does not exist yet is probed too.
  for (const input of local) {
    let directory = dirname(input)
    while (directory !== "." && !existsSync(join(repoRoot, directory, "package.json"))) {
      directory = dirname(directory)
    }
    if (directory === ".") continue
    for (const name of ["package.json", "tsconfig.json", "LICENSE", "NOTICE"]) {
      probes.add(join(directory, name))
    }
  }
  return [...probes].sort()
}

/** The pathspecs of scripts/build-inputs.txt, under the list's one rule, as release-push.sh
 *  push-built applies it: drop empty and # lines. */
async function buildInputPathspecs(): Promise<string[]> {
  const pathspecs = (await readFile(join(packageRoot, "scripts", "build-inputs.txt"), "utf8"))
    .split("\n")
    .filter((line) => line !== "" && !line.startsWith("#"))
  // With no pathspec at all, git would list every file and a check over them would pass on nothing.
  expect(pathspecs.length).toBeGreaterThan(0)
  return pathspecs
}

// Main's push job lands a build onto a moved main and decides from scripts/build-inputs.txt alone
// whether the move changed the build, so the list must match every file the build reads. Git does
// the matching, as it does in the push job, in a scratch repository holding an empty file at each
// path, so the workspace's own git or jj state plays no part.
test("scripts/build-inputs.txt matches every file the build reads", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-build-inputs-"))
  try {
    const probes = await filesTheBuildReads(join(scratch, "build"))
    const pathspecs = await buildInputPathspecs()
    const repository = join(scratch, "repository")
    const env = { PATH: process.env["PATH"] ?? "", HOME: scratch, GIT_CONFIG_NOSYSTEM: "1" }
    await run(["git", "init", "--quiet", repository], scratch, env)
    for (const probe of probes) {
      await mkdir(join(repository, dirname(probe)), { recursive: true })
      await writeFile(join(repository, probe), "")
    }
    const listed = new Set(
      (await run(["git", "ls-files", "--others", "-z", "--", ...pathspecs], repository, env))
        .split("\0")
        .filter((path) => path !== ""),
    )
    expect(probes.filter((probe) => !listed.has(probe))).toEqual([])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
}, 120_000)

/** `pattern` as GitHub matches an `on.push.paths` entry against a repository path: `*` matches any
 *  run of characters but `/`, `**` any run at all, and every other character is itself. GitHub's
 *  filter patterns also take `+`, `?` and character ranges, which this does not model, so a
 *  pattern holding one throws rather than being read wrongly as a literal. */
function pathFilter(pattern: string): RegExp {
  let expression = "^"
  for (let i = 0; i < pattern.length; i++) {
    const character = pattern[i] as string
    if (character === "*") {
      if (pattern[i + 1] === "*") {
        expression += ".*"
        i++
      } else {
        expression += "[^/]*"
      }
    } else if ("+?[]".includes(character)) {
      throw new Error(
        `the path filter ${pattern} holds ${character}, which this test does not read`,
      )
    } else {
      expression += character.replace(/[.^${}()|\\]/g, "\\$&")
    }
  }
  return new RegExp(`${expression}$`)
}

// The rebuild's `on.push.paths` decides which pushes to main are built at all, and
// scripts/build-inputs.txt decides, in the push job, whether a move of main changed the build.
// The two must name the same files: an input the paths leave out changes the build and starts no
// run, and a file the paths take and the list calls a non-input starts a run that lands nothing.
// So this holds the two decisions equal over every tracked file, git making the one and GitHub's
// rules the other.
test(".github/workflows/claude-envoy-rebuild.yaml's push paths are the build inputs", async () => {
  const workflow = ".github/workflows/claude-envoy-rebuild.yaml"
  const parsed = Bun.YAML.parse(await readFile(join(repoRoot, workflow), "utf8")) as {
    on?: { push?: { paths?: string[] } }
  }
  const patterns = parsed.on?.push?.paths ?? []
  expect(patterns.length).toBeGreaterThan(0)
  const rules = patterns.map((pattern) => ({
    negated: pattern.startsWith("!"),
    matches: pathFilter(pattern.replace(/^!/, "")),
  }))
  // GitHub reads the patterns in order and the last one that matches decides, so a `!` pattern
  // after the pattern it narrows excludes the path.
  const starts = (path: string) =>
    rules.reduce((on, rule) => (rule.matches.test(path) ? !rule.negated : on), false)

  const lsFiles = async (pathspecs: string[]) =>
    (await run(["git", "ls-files", "-z", "--", ...pathspecs], repoRoot))
      .split("\0")
      .filter((path) => path !== "")
  const inputs = new Set(await lsFiles(await buildInputPathspecs()))
  // An empty listing (outside a git checkout, say) would hold the patterns to nothing.
  expect(inputs.size).toBeGreaterThan(0)
  const tracked = await lsFiles(["."])

  expect([...inputs].filter((input) => !starts(input)).sort()).toEqual([])
  expect(tracked.filter((path) => !inputs.has(path) && starts(path)).sort()).toEqual([])
})
