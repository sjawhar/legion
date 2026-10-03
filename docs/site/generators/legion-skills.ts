#!/usr/bin/env bun
// Lists the repository's skills (skills/*/SKILL.md) by the name and description in each one's
// frontmatter, as legion/reference/skills.md. Contract: scripts/generate.ts.
import { mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { YAML } from "bun";

const contentDir = process.argv[2];
if (!contentDir) throw new Error("usage: legion-skills.ts <content dir>");

const skillsDir = "skills";
const FRONTMATTER = /^---\n([\s\S]*?)\n---\n/;

const skills = readdirSync(skillsDir, { withFileTypes: true })
  .filter((entry) => entry.isDirectory())
  .map((entry) => {
    const path = join(skillsDir, entry.name, "SKILL.md");
    const frontmatter = FRONTMATTER.exec(readFileSync(path, "utf8"))?.[1];
    if (frontmatter === undefined) throw new Error(`${path} has no YAML frontmatter`);
    const { name, description } = YAML.parse(frontmatter) as {
      name?: unknown;
      description?: unknown;
    };
    if (typeof name !== "string" || typeof description !== "string") {
      throw new Error(`${path} frontmatter needs string name and description fields`);
    }
    return { name, description, path };
  })
  .sort((a, b) => a.name.localeCompare(b.name));
if (skills.length === 0) throw new Error(`no skills found under ${skillsDir}/`);

const page = [
  "---",
  "title: Skills",
  "description: Every skill in the repository's skills/ directory, by the name and description its SKILL.md declares.",
  "editUrl: false",
  "---",
  "",
  "Generated at build time from each `skills/<name>/SKILL.md` frontmatter. An agent loads a skill by",
  "name when its description matches the task in front of it.",
  "",
  ...skills.flatMap(({ name, description, path }) => [
    `## ${name}`,
    "",
    description.replaceAll("<", "&lt;"),
    "",
    `Source: [\`${path}\`](https://github.com/sjawhar/legion/blob/main/${path})`,
    "",
  ]),
].join("\n");

const referenceDir = join(contentDir, "legion", "reference");
mkdirSync(referenceDir, { recursive: true });
writeFileSync(join(referenceDir, "skills.md"), page);
