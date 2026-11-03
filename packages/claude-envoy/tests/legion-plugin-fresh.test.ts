import { expect, test } from "bun:test"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { buildLegionPlugin, claudeAgent } from "../scripts/build"

// The daemon's boot requires legion/.claude-plugin/plugin.json under LEGION_CLAUDE_PLUGIN_DIR
// (packages/daemon/internal/daemon/tools.go), and a Claude Code pane loads the directory with
// --plugin-dir. It is generated from the repository's skills/ and pi-legion's agents, and only main's
// Rebuild Claude Envoy plugin workflow commits it, so the test below reads a fresh generation.
test("the Legion plugin is named legion, so its skills and agents load as legion:<name>", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-legion-"))
  try {
    await buildLegionPlugin(scratch)
    const manifest = JSON.parse(
      await readFile(join(scratch, ".claude-plugin", "plugin.json"), "utf8"),
    ) as { readonly name: string }
    expect(manifest.name).toBe("legion")
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
}, 30_000)

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
