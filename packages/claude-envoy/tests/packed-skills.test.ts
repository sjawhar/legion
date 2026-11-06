import { afterAll, beforeAll, expect, test } from "bun:test"
import { cp, mkdir, mkdtemp, readdir, readlink, rm, symlink } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

// What the first tarball of this package held: no package/skills member at all. skills/<name> is
// a link into the repository's skills/, `bun pm pack` packs a link as nothing, and so every
// install lost all eight skills and the dispatch-first hook hooks/hooks.json runs on every
// session threw on the SKILL.md it reads under the plugin root. scripts/pack-skills.sh is the
// prepack that resolves the links and the postpack that puts them back, and
// scripts/check-packed-skills.sh is what release.yaml's claude_envoy_build reads out of the
// tarball it hands the publish job. Both are exercised here over a real `bun pm pack`.
const packageRoot = resolve(import.meta.dir, "..")
const repoRoot = resolve(packageRoot, "..", "..")
const checkScript = join(packageRoot, "scripts", "check-packed-skills.sh")

type Run = { readonly exitCode: number; readonly stdout: string; readonly stderr: string }

async function run(command: string[], cwd: string): Promise<Run> {
  const child = Bun.spawn(command, { cwd, stdout: "pipe", stderr: "pipe" })
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ])
  return { exitCode, stdout, stderr }
}

async function ran(command: string[], cwd: string): Promise<Run> {
  const result = await run(command, cwd)
  if (result.exitCode !== 0) {
    throw new Error(`${command.join(" ")} exited ${result.exitCode}: ${result.stderr.trim()}`)
  }
  return result
}

let scratch = ""
/** The packed package: the committed package.json and prepack, the eight links as the checkout
 *  holds them, and a skills/ of the repository's own, so the pack under test is the release's. */
let packageCopy = ""
let tarball = ""
let skillNames: string[] = []

beforeAll(async () => {
  scratch = await mkdtemp(join(tmpdir(), "claude-envoy-packed-skills-"))
  // The pack runs in a copy, never in the checkout: the prepack writes into skills/, and jj
  // records whatever a test writes into the working copy. The copy keeps the layout the links
  // need — <repository>/packages/claude-envoy — with the repository's own skills/ behind the
  // links, so what they resolve to is what a release packs.
  packageCopy = join(scratch, "repository", "packages", "claude-envoy")
  await mkdir(join(packageCopy, "scripts"), { recursive: true })
  await symlink(join(repoRoot, "skills"), join(scratch, "repository", "skills"))
  // The committed manifest, its `files` and its prepack as they are, less the devDependencies:
  // a pack outside the workspace cannot resolve their `workspace:*`, and nothing about them
  // decides what is packed.
  const manifest = JSON.parse(await Bun.file(join(packageRoot, "package.json")).text()) as Record<
    string,
    unknown
  >
  delete manifest["devDependencies"]
  await Bun.write(join(packageCopy, "package.json"), `${JSON.stringify(manifest, null, 2)}\n`)
  await cp(
    join(packageRoot, "scripts", "pack-skills.sh"),
    join(packageCopy, "scripts", "pack-skills.sh"),
  )
  await mkdir(join(packageCopy, "skills"))
  skillNames = (await readdir(join(packageRoot, "skills"))).sort()
  for (const name of skillNames) {
    await symlink(join("..", "..", "..", "skills", name), join(packageCopy, "skills", name))
  }
  await ran([process.execPath, "pm", "pack"], packageCopy)
  const packed = (await readdir(packageCopy)).filter((entry) => entry.endsWith(".tgz"))
  expect(packed).toHaveLength(1)
  tarball = join(packageCopy, packed[0] as string)
}, 60_000)

afterAll(async () => {
  await rm(scratch, { recursive: true, force: true })
})

/** The tarball's members, and its listing with each member's type in the first column. */
async function members(tar: string): Promise<string[]> {
  const listed = await ran(["tar", "-tzf", tar], scratch)
  return listed.stdout.split("\n").filter((line) => line.length > 0)
}

/** <dir>/package packed as a tarball of its own, for the tarballs the check must refuse. */
async function packDirectory(name: string): Promise<string> {
  const tar = join(scratch, `${name}.tgz`)
  await ran(["tar", "-czf", tar, "-C", join(scratch, name), "package"], scratch)
  return tar
}

test("the pack resolves every skills link into the skill's own files", async () => {
  expect(skillNames).toContain("dispatch-first")
  const packed = await members(tarball)
  expect(skillNames.filter((name) => !packed.includes(`package/skills/${name}/SKILL.md`))).toEqual(
    [],
  )
  const listing = await ran(["tar", "-tvzf", tarball], scratch)
  const links = listing.stdout.split("\n").filter((line) => /^[lh]/.test(line))
  expect(links).toEqual([])
})

test("the pack puts the links back, leaving the package as it found it", async () => {
  const entries = (await readdir(join(packageCopy, "skills"))).sort()
  expect(entries).toEqual(skillNames)
  const targets = await Promise.all(
    entries.map((name) => readlink(join(packageCopy, "skills", name))),
  )
  expect(targets).toEqual(skillNames.map((name) => `../../../skills/${name}`))
})

test("the check passes a tarball the pack made, naming what it carries", async () => {
  const result = await ran(["bash", checkScript, tarball], scratch)
  expect(result.stdout.trim()).toBe(
    `check-packed-skills.sh: ${tarball} carries ${skillNames.length} skills as real files`,
  )
})

test("the check refuses a tarball with no dispatch-first skill", async () => {
  const other = join(scratch, "no-dispatch-first", "package", "skills", "envoy")
  await mkdir(other, { recursive: true })
  await Bun.write(join(other, "SKILL.md"), "---\nname: envoy\n---\n")
  const result = await run(["bash", checkScript, await packDirectory("no-dispatch-first")], scratch)
  expect(result.exitCode).toBe(1)
  expect(result.stderr).toContain("holds no package/skills/dispatch-first/SKILL.md")
})

test("the check refuses a tarball whose skills hold a link", async () => {
  const skills = join(scratch, "a-link", "package", "skills")
  await mkdir(join(skills, "dispatch-first"), { recursive: true })
  await Bun.write(join(skills, "dispatch-first", "SKILL.md"), "---\nname: dispatch-first\n---\n")
  await symlink(join("..", "..", "..", "skills", "envoy"), join(skills, "envoy"))
  const result = await run(["bash", checkScript, await packDirectory("a-link")], scratch)
  expect(result.exitCode).toBe(1)
  expect(result.stderr).toContain("packs a link under package/skills")
})

test("the check refuses a skill packed without its SKILL.md", async () => {
  const skills = join(scratch, "half-staged", "package", "skills")
  await mkdir(join(skills, "dispatch-first"), { recursive: true })
  await Bun.write(join(skills, "dispatch-first", "SKILL.md"), "---\nname: dispatch-first\n---\n")
  await mkdir(join(skills, "envoy", "references"), { recursive: true })
  await Bun.write(join(skills, "envoy", "references", "topics.md"), "topics\n")
  const result = await run(["bash", checkScript, await packDirectory("half-staged")], scratch)
  expect(result.exitCode).toBe(1)
  expect(result.stderr).toContain("packs package/skills/envoy with no SKILL.md")
})
