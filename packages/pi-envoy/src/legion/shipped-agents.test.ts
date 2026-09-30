import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// The agents the plugin ships (package.json `files`), each dispatched by a Legion prompt.
const agentsDir = join(import.meta.dir, "..", "..", "agents");
const agents = readdirSync(agentsDir).filter((name) => name.endsWith(".md"));

function frontmatter(file: string): Record<string, unknown> {
  const match = /^---\n([\s\S]*?)\n---\n/.exec(readFileSync(join(agentsDir, file), "utf8"));
  if (!match) throw new Error(`${file} has no frontmatter`);
  return Bun.YAML.parse(match[1]) as Record<string, unknown>;
}

// Legion names no model, provider, or route: an agent's model is a role alias, which the operator's
// modelRoles maps, and the daemon's boot gate refuses an alias no role configures. A concrete
// selector would run the agent on a model the operator never chose, or on the parent's when that
// model's provider has no key.
test.each(agents)("%s declares its model only as role aliases", (file) => {
  const { model } = frontmatter(file);
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

// deep-worker.md's frontmatter says why.
test("deep-worker.md is blocking", () => {
  expect(frontmatter("deep-worker.md").blocking).toBe(true);
});
