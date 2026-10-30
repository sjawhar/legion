import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// The agents the plugin ships (package.json `files`), each dispatched by a Legion prompt.
const agentsDir = join(import.meta.dir, "..", "agents");
const agents = readdirSync(agentsDir).filter((name) => name.endsWith(".md"));

function definition(file: string): { fields: Record<string, unknown>; body: string } {
  const text = readFileSync(join(agentsDir, file), "utf8");
  const frontmatter = /^---\n([\s\S]*?)\n---\n/.exec(text);
  if (!frontmatter) throw new Error(`${file} has no frontmatter`);
  return {
    fields: Bun.YAML.parse(frontmatter[1]) as Record<string, unknown>,
    body: text.slice(frontmatter[0].length),
  };
}

// A prompt dispatches an agent by the name its frontmatter declares, and Oh My Pi finds it by that
// name, not by its file's.
test.each(agents)("%s declares the name of its file", (file) => {
  expect(definition(file).fields.name).toBe(file.replace(/\.md$/, ""));
});

// Legion names no model, provider, or route: an agent's model is a role alias, which the operator's
// modelRoles maps, and the daemon's boot gate refuses an alias no role configures. A concrete
// selector would run the agent on a model the operator never chose, or on the parent's when that
// model's provider has no key.
test.each(agents)("%s declares its model only as role aliases", (file) => {
  const { model } = definition(file).fields;
  // Oh My Pi splits a scalar, and each entry of a list, on commas (normalizeModelPatternList).
  expect(model === undefined || typeof model === "string" || Array.isArray(model)).toBe(true);
  const entries = model === undefined ? [] : Array.isArray(model) ? model : [model];
  for (const entry of entries) {
    expect(typeof entry).toBe("string");
    for (const selector of String(entry)
      .split(",")
      .map((part) => part.trim())
      .filter(Boolean)) {
      expect(selector).toMatch(/^@[a-z]/);
    }
  }
});

// Oh My Pi only logs an autoloaded skill it cannot find, and the agent is told not to read its
// rubric again, so a name that drifted would leave the review without it — in either direction:
// an autoload a deleted body no longer names leaves nothing "already in context" to skip re-reading,
// and a body that names a skill:// its frontmatter no longer autoloads is never actually in context.
// The daemon's boot gate resolves a skill only from a `skill://<name>` token
// (packages/daemon/internal/promptrefs, whose name pattern this repeats), so the set of names
// autoloaded must equal the set the body names, not merely contain it.
test.each(agents)("%s autoloads exactly the skill:// names its body already has", (file) => {
  const { fields, body } = definition(file);
  const autoload = fields.autoloadSkills;
  // Oh My Pi reads a list or a comma-separated scalar (parseArrayOrCSV).
  const entries =
    autoload === undefined ? [] : Array.isArray(autoload) ? autoload : String(autoload).split(",");
  const autoloaded = new Set(entries.map((name) => String(name).trim()).filter(Boolean));
  const named = new Set(
    [...body.matchAll(/skill:\/\/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)/g)].map(([, name]) => name)
  );
  expect([...autoloaded].sort(), `${file}'s autoloadSkills vs its skill:// names`).toEqual(
    [...named].sort()
  );
});

// deep-worker.md's frontmatter says why.
test("deep-worker.md is blocking", () => {
  expect(definition("deep-worker.md").fields.blocking).toBe(true);
});

// The planner's and the architect's two checks: each caller waits for each answer before it drafts,
// revises or requests approval (blocking), and each runs on the deployment's model role for its job.
// They change no file: no edit or write tool. They carry bash to check live state themselves, and
// Oh My Pi has no per-agent command limit, so each prompt holds the agent to commands that read.
test.each([
  ["plan-gap-analyst.md", "@oracle"],
  ["plan-reviewer.md", "@review"],
])("%s is a blocking check on %s that edits no file and runs only read-only commands", (file, model) => {
  const { fields, body } = definition(file);
  expect(fields.model).toEqual([model]);
  expect(fields.blocking).toBe(true);
  const allowed: Record<string, true> = {
    read: true,
    glob: true,
    grep: true,
    find: true,
    lsp: true,
    ast_grep: true,
    todo: true,
    bash: true,
  };
  const tools = String(fields.tools)
    .split(",")
    .map((tool) => tool.trim());
  expect(
    tools.filter((tool) => allowed[tool] !== true),
    `${file} lists a mutation tool`
  ).toEqual([]);
  expect(tools).toContain("bash");
  expect(body.replace(/\s+/g, " ")).toContain("Run only commands that read.");
});
