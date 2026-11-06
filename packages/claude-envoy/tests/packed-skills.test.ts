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
/** The packed package, as <scratch>/packed/…/claude-envoy: what the release's pack runs over. */
let packageCopy = ""
let tarball = ""
let skillNames: string[] = []

/** A copy of this package at <scratch>/<name>/repository/packages/claude-envoy: the committed
 *  package.json and prepack, the eight links as the checkout holds them, and a skills/ of the
 *  repository's own, so a pack over it is the release's.
 *
 *  A pack runs in a copy, never in the checkout: the prepack writes into skills/, and jj records
 *  whatever a test writes into the working copy. The copy keeps the layout the links need —
 *  <repository>/packages/claude-envoy — with the repository's own skills/ behind them, so what
 *  they resolve to is what a release packs. */
async function copyPackage(name: string): Promise<string> {
  const copy = join(scratch, name, "repository", "packages", "claude-envoy")
  await mkdir(join(copy, "scripts"), { recursive: true })
  await symlink(join(repoRoot, "skills"), join(scratch, name, "repository", "skills"))
  // The committed manifest, its `files` and its prepack as they are, less the devDependencies:
  // a pack outside the workspace cannot resolve their `workspace:*`, and nothing about them
  // decides what is packed.
  const manifest = JSON.parse(await Bun.file(join(packageRoot, "package.json")).text()) as Record<
    string,
    unknown
  >
  delete manifest["devDependencies"]
  await Bun.write(join(copy, "package.json"), `${JSON.stringify(manifest, null, 2)}\n`)
  await cp(join(packageRoot, "scripts", "pack-skills.sh"), join(copy, "scripts", "pack-skills.sh"))
  await mkdir(join(copy, "skills"))
  for (const skill of skillNames) {
    await symlink(join("..", "..", "..", "skills", skill), join(copy, "skills", skill))
  }
  return copy
}

/** Every entry of <copy>/skills by name, each with what it links to, and null where it is no
 *  link but the directory a prepack staged. */
async function links(copy: string): Promise<Record<string, string | null>> {
  const found: Record<string, string | null> = {}
  for (const name of (await readdir(join(copy, "skills"))).sort()) {
    found[name] = await readlink(join(copy, "skills", name)).catch(() => null)
  }
  return found
}

/** What every entry of a checkout's skills/ links to: the tracked links, as they stand there. */
function tracked(): Record<string, string> {
  return Object.fromEntries(skillNames.map((name) => [name, `../../../skills/${name}`]))
}

beforeAll(async () => {
  scratch = await mkdtemp(join(tmpdir(), "claude-envoy-packed-skills-"))
  skillNames = (await readdir(join(packageRoot, "skills"))).sort()
  packageCopy = await copyPackage("packed")
  await ran([process.execPath, "pm", "pack"], packageCopy)
  const packed = (await readdir(packageCopy)).filter((entry) => entry.endsWith(".tgz"))
  expect(packed).toHaveLength(1)
  tarball = join(packageCopy, packed[0] as string)
}, 60_000)

afterAll(async () => {
  await rm(scratch, { recursive: true, force: true })
})

/** The tarball's members, one name a line. */
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

// What the check below cannot make of a tarball on its own: that every one of the links the
// package tracks is in it. The check reads the tarball's own members, and a pack that resolved
// none of them would carry no skills/<name> for it to find fault with.
test("the pack resolves every skills link into the skill's own files", async () => {
  expect(skillNames).toContain("dispatch-first")
  const packed = await members(tarball)
  expect(skillNames.filter((name) => !packed.includes(`package/skills/${name}/SKILL.md`))).toEqual(
    [],
  )
})

test("the pack puts the links back, leaving the package as it found it", async () => {
  expect(await links(packageCopy)).toEqual(tracked())
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

// The two ways a pack can stop with the links already swapped. The links are tracked files, so
// what the prepack leaves behind is what the working copy holds: jj's next command would record
// eight deleted links and eight copied skill trees.
test("a prepack refused by one entry leaves every link as it found it", async () => {
  const copy = await copyPackage("wrong-target")
  // The last name, so the entries before it are the ones a loop that swapped as it checked would
  // have left staged.
  const last = skillNames[skillNames.length - 1] as string
  await rm(join(copy, "skills", last))
  await symlink(join("..", "..", "..", "skills", "nowhere"), join(copy, "skills", last))
  const packed = await run([process.execPath, "pm", "pack"], copy)
  expect(packed.exitCode).not.toBe(0)
  expect(packed.stderr).toContain(`skills/${last} points at ../../../skills/nowhere`)
  expect(await links(copy)).toEqual({ ...tracked(), [last]: "../../../skills/nowhere" })
})

test("a pack that fails after its prepack is repaired by --restore, which the refusal names", async () => {
  const copy = await copyPackage("pack-fails")
  // A destination that is a file, not a directory: bun runs the prepack, then fails writing the
  // tarball, and runs no postpack — which is what leaves the staged directories behind.
  const destination = join(scratch, "pack-fails-destination")
  await Bun.write(destination, "")
  const packed = await run([process.execPath, "pm", "pack", "--destination", destination], copy)
  expect(packed.exitCode).not.toBe(0)
  expect(await links(copy)).toEqual(Object.fromEntries(skillNames.map((name) => [name, null])))
  const again = await run(["bash", join(copy, "scripts", "pack-skills.sh")], copy)
  expect(again.exitCode).toBe(1)
  for (const name of skillNames) {
    expect(again.stderr).toContain(`skills/${name} is a staged copy of`)
  }
  expect(again.stderr).toContain("bash scripts/pack-skills.sh --restore")
  await ran(["bash", join(copy, "scripts", "pack-skills.sh"), "--restore"], copy)
  expect(await links(copy)).toEqual(tracked())
})

// What the advice must judge before it gives itself: --restore rm -rf's every entry, so it puts
// back a skills/ whose entries are all staged copies of the repository's skills and holds
// nothing of its own, and would delete anything else among them. The reviewer's reproduction is
// the case: a prepack, an edit to one staged entry, and a second prepack. The edited entry is
// the last by name — agent-secrets sorts ahead of it — so a judgement that stopped at the first
// entry that is no link would call the whole of skills/ a staged copy and send the person to
// --restore, which would take the edit with it.
test("an edited staged entry is refused by the prepack and by --restore, neither touching it", async () => {
  const copy = await copyPackage("edited-staged-copy")
  await ran(["bash", join(copy, "scripts", "pack-skills.sh")], copy)
  const staged = Object.fromEntries(skillNames.map((name) => [name, null]))
  expect(await links(copy)).toEqual(staged)
  const name = skillNames[skillNames.length - 1] as string
  expect(skillNames[0]).not.toBe(name)
  const edited = join(copy, "skills", name, "SKILL.md")
  const mine = `${await Bun.file(edited).text()}\nthe line a developer wrote into the staged copy\n`
  await Bun.write(edited, mine)

  const again = await run(["bash", join(copy, "scripts", "pack-skills.sh")], copy)
  expect(again.exitCode).toBe(1)
  expect(again.stderr).toContain(`skills/${name} is neither a link nor a staged copy of`)
  expect(again.stderr).toContain(`it must be the link ../../../skills/${name}`)
  expect(again.stderr).not.toContain("--restore")

  const restore = await run(["bash", join(copy, "scripts", "pack-skills.sh"), "--restore"], copy)
  expect(restore.exitCode).toBe(1)
  expect(restore.stderr).toContain(`skills/${name} is neither a link nor a staged copy of`)
  expect(await Bun.file(edited).text()).toBe(mine)
  expect(await links(copy)).toEqual(staged)
  for (const other of skillNames.filter((entry) => entry !== name)) {
    const same = await run(
      ["diff", "-r", join(repoRoot, "skills", other), join(copy, "skills", other)],
      scratch,
    )
    expect({ name: other, ...same }).toEqual({ name: other, exitCode: 0, stdout: "", stderr: "" })
  }
})
