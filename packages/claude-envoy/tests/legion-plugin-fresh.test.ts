import { expect, test } from "bun:test"
import { readFile } from "node:fs/promises"
import { join, resolve } from "node:path"
import { claudeAgent, legionPluginDifferences } from "../scripts/build"

const packageRoot = resolve(import.meta.dir, "..")

// The daemon's boot requires legion/.claude-plugin/plugin.json under LEGION_CLAUDE_PLUGIN_DIR
// (packages/daemon/internal/daemon/tools.go), and a Claude Code pane loads the directory with
// --plugin-dir: generated from the repository's skills/ and pi-legion's agents, so it is held to a
// fresh generation here, as dist/ is in dist-fresh.test.ts.
test("the committed legion/ is what a fresh generation writes", async () => {
  const stale = await legionPluginDifferences(join(packageRoot, "legion"))
  if (stale.length > 0) {
    throw new Error(
      `legion/ is stale (${stale.join(", ")}): run \`bun scripts/build.ts\` on the pinned Bun and commit the result`,
    )
  }
})

test("the Legion plugin is named legion, so its skills and agents load as legion:<name>", async () => {
  const manifest = JSON.parse(
    await readFile(join(packageRoot, "legion", ".claude-plugin", "plugin.json"), "utf8"),
  ) as { readonly name: string }
  expect(manifest.name).toBe("legion")
})

test("an agent's tools take Claude Code's names, and its autoloaded skills its skills field", () => {
  const source = [
    "---",
    "name: worker",
    "description: |",
    "  Two lines",
    "  of description.",
    "# a comment about the model role",
    'model: ["@deep"]',
    "tools: read, glob, grep, bash, edit, write, todo",
    "autoloadSkills: [legion-worker, ce-simplify-code]",
    "blocking: true",
    "color: cyan",
    "---",
    "",
    "The body, kept as it is: read, glob.",
    "",
  ].join("\n")
  expect(claudeAgent("worker.md", source)).toBe(
    [
      "---",
      "name: worker",
      "description: |",
      "  Two lines",
      "  of description.",
      "tools: Read, Bash, Edit, Write",
      "skills: [legion:legion-worker, legion:ce-simplify-code]",
      "color: cyan",
      "---",
      "",
      "The body, kept as it is: read, glob.",
      "",
    ].join("\n"),
  )
})

test("an agent naming a tool with no Claude Code mapping, or keeping none, is refused", () => {
  expect(() => claudeAgent("a.md", "---\nname: a\ntools: read, lsp\n---\n")).toThrow(
    "a.md lists the tool lsp, which CLAUDE_TOOL_NAMES does not map",
  )
  expect(() => claudeAgent("b.md", "---\nname: b\ntools: glob, grep\n---\n")).toThrow(
    "b.md keeps no tool Claude Code has",
  )
  expect(() => claudeAgent("c.md", "no frontmatter\n")).toThrow("c.md has no frontmatter")
})
