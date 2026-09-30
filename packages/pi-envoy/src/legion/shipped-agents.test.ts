import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// The agents the plugin ships (package.json `files`), each dispatched by a Legion prompt.
const agentsDir = join(import.meta.dir, "..", "..", "agents");
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
// rubric again, so a name that drifted would leave the review without it. The Go daemon's boot gate
// resolves a skill only from a `skill://<name>` token (packages/daemon-go/internal/promptrefs,
// whose name pattern this repeats), so each autoloaded name must also appear in the body as one.
test.each(agents)("%s names every skill it autoloads as skill://<name>", (file) => {
  const { fields, body } = definition(file);
  const autoload = fields.autoloadSkills;
  // Oh My Pi reads a list or a comma-separated scalar (parseArrayOrCSV).
  const entries =
    autoload === undefined ? [] : Array.isArray(autoload) ? autoload : String(autoload).split(",");
  const named = [...body.matchAll(/skill:\/\/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)/g)].map(
    ([, name]) => name
  );
  for (const entry of entries.map((name) => String(name).trim()).filter(Boolean)) {
    expect(named, `${file} autoloads ${entry} but names no skill://${entry}`).toContain(entry);
  }
});
