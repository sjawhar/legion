#!/usr/bin/env bun
// Builds the dispatch-web skill, which teaches Claude on claude.ai (Claude in Chrome included) to
// guide a person through Dispatch's web app, as public/downloads/dispatch-web.zip: the hand-written
// claude-skills/dispatch-web/SKILL.md, this section's pages as its references/, and the
// repository's LICENSE. It writes <content dir>/dispatch/reference/web-skill.md, which shows what
// the zip holds. The references are these pages at this commit, so the skill cannot drift from
// the guide, and the build fails when SKILL.md cites a heading, or names a control or key, that
// the pages no longer have. Contract: scripts/generate.ts.
import { mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { deflateRawSync } from "node:zlib";
import { YAML } from "bun";
import { writePage } from "./lib/markdown.ts";

const NAME = "dispatch-web";
const SITE = "https://sjawhar.github.io/legion";
const SOURCE = `docs/site/claude-skills/${NAME}/SKILL.md`;
const PAGES_DIR = "docs/site/src/content/docs/dispatch";
const ZIP = `docs/site/public/downloads/${NAME}.zip`;
// The section's pages the skill carries, by file name, and the ones it leaves on the site: the
// operators' pages, and the page about this skill. A page in neither list fails the build, so a
// new page is a choice rather than an omission.
const PAGES = [
  "index",
  "inbox-and-asks",
  "issues-and-projects",
  "documents",
  "agents",
  "keyboard",
  "delivery",
  "for-agents",
];
const LEFT_OUT = ["running-dispatch", "envoy", "claude-skill"];
// claude.ai refuses a description over 1,024 characters, or one with angle brackets.
const DESCRIPTION_MAX = 1024;

const FRONTMATTER = /^---\n([\s\S]*?)\n---\n/;
const VIDEO = /<video\b[\s\S]*?<source src="([^"]+)"[\s\S]*?<\/video>/g;
const IMAGE = /!\[([^\]]*)\]\((\/legion\/[^)\s]+)\)/g;
const LINK = /\]\((\/legion\/[^)\s]*)\)/g;

const contentDir = process.argv[2];
if (contentDir === undefined) throw new Error("usage: dispatch-web-skill.ts <content dir>");

function site(path: string): string {
  return SITE + path.slice("/legion".length);
}

/** A link to another carried page points at its reference file; any other at the published site. */
function relink(path: string): string {
  const match = /^\/legion\/dispatch\/(?:([a-z-]+)\/)?(#[\w-]+)?$/.exec(path);
  const page = match && (match[1] ?? "index");
  if (page && PAGES.includes(page)) return `${page}.md${match[2] ?? ""}`;
  return site(path);
}

/** A page as a reference file: its frontmatter as a heading, its screenshots and videos as links
 *  to the published site, and its site links rewritten to resolve outside the site. */
function reference(page: string): string {
  const text = readFileSync(join(contentDir, "dispatch", `${page}.md`), "utf8");
  const frontmatter = FRONTMATTER.exec(text);
  if (!frontmatter?.[1]) throw new Error(`${PAGES_DIR}/${page}.md has no YAML frontmatter`);
  const { title, description } = YAML.parse(frontmatter[1]) as {
    title?: unknown;
    description?: unknown;
  };
  if (typeof title !== "string") throw new Error(`${PAGES_DIR}/${page}.md has no title`);
  const body = text
    .slice(frontmatter[0].length)
    .replace(VIDEO, (_, src: string) => `[Video walkthrough](${site(src)})`)
    .replace(IMAGE, (_, alt: string, src: string) => `[Screenshot: ${alt}](${site(src)})`)
    .replace(LINK, (_, path: string) => `](${relink(path)})`);
  // A link the patterns above do not read (one with a title, an HTML link, a reference-style
  // definition) would still point at the site's root.
  if (/[("'=:\s]\/legion\//.test(body)) {
    throw new Error(`${PAGES_DIR}/${page}.md has a /legion/ link this generator cannot rewrite`);
  }
  const lead = typeof description === "string" ? `> ${description}\n\n` : "";
  return `# ${title}\n\n${lead}${body.trimStart()}`;
}

function commit(): string {
  const result = Bun.spawnSync(["git", "rev-parse", "--short", "HEAD"]);
  if (result.exitCode !== 0) throw new Error(`git rev-parse failed: ${result.stderr.toString()}`);
  return result.stdout.toString().trim();
}

/** Fails on a frontmatter claude.ai would refuse, and on any citation the pages do not back: every
 *  `references/<file>` must be a file the skill carries, and the section headings quoted after it,
 *  as `, "A"`, `, "A" and "B"` or `, "A" or "B"`, headings of that page. Every **bold** label and
 *  `code` span, which SKILL.md keeps for the controls, keys and terms the pages name, must be bold
 *  or code in some page. */
function check(skill: string, references: Map<string, string>): void {
  const problems: string[] = [];
  const frontmatter = FRONTMATTER.exec(skill)?.[1];
  const { name, description } = (frontmatter ? YAML.parse(frontmatter) : {}) as {
    name?: unknown;
    description?: unknown;
  };
  if (name !== NAME) problems.push(`its name is ${JSON.stringify(name)}, not "${NAME}"`);
  if (typeof description !== "string" || description.length > DESCRIPTION_MAX) {
    problems.push(`its description is missing or over ${DESCRIPTION_MAX} characters`);
  } else if (/[<>]/.test(description)) {
    problems.push("its description has an angle bracket");
  }
  // Line breaks fall anywhere in a paragraph, so both sides are read with whitespace runs as one
  // space.
  const flat = (text: string) => text.replace(/\s+/g, " ");
  const body = flat(skill.slice(FRONTMATTER.exec(skill)?.[0].length ?? 0));
  const all = flat([...references.values()].join("\n"));
  for (const cite of body.matchAll(/`references\/([^`]+)`((?:,? (?:and |or )?"[^"]+")*)/g)) {
    const [, file = "", headings = ""] = cite;
    if (file === "SOURCE.md" && headings === "") continue;
    const page = references.get(file);
    if (page === undefined) {
      problems.push(`it cites references/${file}, which the skill does not carry`);
      continue;
    }
    const sections = new Set(
      page.split("\n").flatMap((line) => /^#{2,4} (.+)$/.exec(line)?.slice(1) ?? [])
    );
    for (const [, heading = ""] of headings.matchAll(/"([^"]+)"/g)) {
      if (!sections.has(heading)) {
        problems.push(`it cites "${heading}" in references/${file}, which has no such heading`);
      }
    }
  }
  for (const [bold = ""] of body.matchAll(/\*\*[^*]+\*\*/g)) {
    if (!all.includes(bold)) problems.push(`no page has its ${bold} in bold`);
  }
  for (const [span = ""] of body.matchAll(/`[^`]+`/g)) {
    if (!span.startsWith("`references/") && !all.includes(span)) {
      problems.push(`no page has its ${span} as code`);
    }
  }
  if (problems.length > 0) {
    throw new Error(`${SOURCE}:\n  ${problems.join("\n  ")}`);
  }
}

/** A zip of the files, deflated, in the order given, with a fixed timestamp so the same files make
 *  the same zip. */
function zip(files: ReadonlyMap<string, string>): Uint8Array {
  const DOS_DATE = (0 << 9) | (1 << 5) | 1; // 1980-01-01, the earliest a zip can record
  const UTF8_NAMES = 0x0800;
  const local: Buffer[] = [];
  const central: Buffer[] = [];
  let offset = 0;
  for (const [path, text] of files) {
    const name = Buffer.from(path);
    const data = Buffer.from(text);
    const packed = deflateRawSync(data, { level: 9 });
    const crc = Bun.hash.crc32(data);
    const head = Buffer.alloc(30);
    head.writeUInt32LE(0x04034b50, 0);
    head.writeUInt16LE(20, 4);
    head.writeUInt16LE(UTF8_NAMES, 6);
    head.writeUInt16LE(8, 8);
    head.writeUInt16LE(DOS_DATE, 12);
    head.writeUInt32LE(crc, 14);
    head.writeUInt32LE(packed.length, 18);
    head.writeUInt32LE(data.length, 22);
    head.writeUInt16LE(name.length, 26);
    const entry = Buffer.alloc(46);
    entry.writeUInt32LE(0x02014b50, 0);
    entry.writeUInt16LE(20, 4);
    entry.writeUInt16LE(20, 6);
    entry.writeUInt16LE(UTF8_NAMES, 8);
    entry.writeUInt16LE(8, 10);
    entry.writeUInt16LE(DOS_DATE, 14);
    entry.writeUInt32LE(crc, 16);
    entry.writeUInt32LE(packed.length, 20);
    entry.writeUInt32LE(data.length, 24);
    entry.writeUInt16LE(name.length, 28);
    entry.writeUInt32LE(offset, 42);
    local.push(head, name, packed);
    central.push(entry, name);
    offset += head.length + name.length + packed.length;
  }
  const directory = Buffer.concat(central);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(files.size, 8);
  end.writeUInt16LE(files.size, 10);
  end.writeUInt32LE(directory.length, 12);
  end.writeUInt32LE(offset, 16);
  return Buffer.concat([...local, directory, end]);
}

const unlisted = readdirSync(join(contentDir, "dispatch"))
  .filter((file) => /\.mdx?$/.test(file))
  .map((file) => file.replace(/\.mdx?$/, ""))
  .filter((page) => !PAGES.includes(page) && !LEFT_OUT.includes(page));
if (unlisted.length > 0) {
  throw new Error(
    `${PAGES_DIR} has ${unlisted.join(", ")}, in neither PAGES nor LEFT_OUT of generators/dispatch-web-skill.ts`
  );
}
const skill = readFileSync(SOURCE, "utf8");
const references = new Map(PAGES.map((page) => [`${page}.md`, reference(page)]));
check(skill, references);

const at = commit();
const files = new Map<string, string>([
  [`${NAME}/SKILL.md`, skill],
  [`${NAME}/LICENSE`, readFileSync("LICENSE", "utf8")],
  ...[...references].map(([file, text]): [string, string] => [`${NAME}/references/${file}`, text]),
  [
    `${NAME}/references/SOURCE.md`,
    [
      "# Source",
      "",
      `These files are Dispatch's user guide from https://github.com/sjawhar/legion (\`${PAGES_DIR}\`, commit \`${at}\`), licensed under the Apache License 2.0 (\`../LICENSE\`).`,
      `The published guide is at ${SITE}/dispatch/.`,
      "Each page's frontmatter is now its heading, its screenshots and videos are links to the published site, and its links point at these files or at the site.",
      "",
    ].join("\n"),
  ],
]);
mkdirSync(resolve(ZIP, ".."), { recursive: true });
writeFileSync(ZIP, zip(files));
console.log(`wrote ${resolve(ZIP)}`);

writePage({
  path: "dispatch/reference/web-skill.md",
  title: "The dispatch-web skill",
  description:
    "Every file in the dispatch-web skill for Claude, as this build of the site made it, and its SKILL.md in full.",
  sources: [SOURCE, PAGES_DIR],
  body: () =>
    [
      `What [${NAME}.zip](/legion/downloads/${NAME}.zip) holds, built from commit \`${at}\`.`,
      "[Dispatch in Claude](/legion/dispatch/claude-skill/) says how to install it.",
      "",
      "| File | Bytes |",
      "| --- | --- |",
      ...[...files].map(([path, text]) => `| \`${path}\` | ${Buffer.byteLength(text)} |`),
      "",
      "## SKILL.md",
      "",
      "````markdown",
      skill.trimEnd(),
      "````",
      "",
    ].join("\n"),
});
