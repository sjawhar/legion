import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// The agents the plugin ships (package.json `files`), each dispatched by a Legion prompt.
const agentsDir = join(import.meta.dir, "..", "..", "agents");
const agents = readdirSync(agentsDir).filter((name) => name.endsWith(".md"));

// Legion names no model, provider, or route: an agent's model is a role alias, which the operator's
// modelRoles maps, and the daemon's boot gate refuses an alias no role configures. A concrete
// selector would run the agent on a model the operator never chose, or on the parent's when that
// model's provider has no key.
test.each(agents)("%s declares its model only as role aliases", (file) => {
  const text = readFileSync(join(agentsDir, file), "utf8");
  const frontmatter = /^---\n([\s\S]*?)\n---\n/.exec(text);
  if (!frontmatter) throw new Error(`${file} has no frontmatter`);
  const { model } = Bun.YAML.parse(frontmatter[1]) as { model?: unknown };
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

// Oh My Pi runs a task agent in the background, returning before it finishes, unless the agent is
// `blocking` (task/index.ts at the pin). An agent whose tools can edit the working copy must be
// waited for, or its caller checks and commits a tree the agent is still changing.
test.each(agents)("%s is blocking when its tools can edit files", (file) => {
  const frontmatter = /^---\n([\s\S]*?)\n---\n/.exec(readFileSync(join(agentsDir, file), "utf8"));
  if (!frontmatter) throw new Error(`${file} has no frontmatter`);
  const { tools, blocking } = Bun.YAML.parse(frontmatter[1]) as {
    tools?: string;
    blocking?: unknown;
  };
  const named = (tools ?? "").split(",").map((tool) => tool.trim());
  if (named.includes("edit") || named.includes("write"))
    expect(blocking, `${file} can edit files`).toBe(true);
});
