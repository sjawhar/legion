// Shared by the reference generators beside this directory; scripts/generate.ts never runs a file
// in a subdirectory of generators/.
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";

const REPO_ROOT = resolve(import.meta.dir, "../../../..");

/** Escapes Markdown outside code spans, so description text renders as the text it is: `<login>`
 *  is not an HTML tag, `&lt;` not an entity, `~x~` not strikethrough, and a line opening with
 *  `- `, `+ `, `= ` or `1. ` not a list, rule or heading underline. In a table cell a pipe is
 *  escaped inside code spans too: GFM splits a row on every unescaped pipe before it parses code
 *  spans. */
export function inline(text: string, cell = false): string {
  return text
    .split(/(`[^`]*`)/)
    .map((part, index) => {
      if (index % 2 === 1) return cell ? part.replaceAll("|", "\\|") : part;
      return part
        .replace(/[\\*_[\]<>|#~&]/g, "\\$&")
        .replace(/^(\s*)([-+=])/gm, "$1\\$2")
        .replace(/^(\s*\d+)([.)])/gm, "$1\\$2");
    })
    .join("");
}

interface Page {
  /** Where the page goes under the content directory, such as `dispatch/reference/api.md`. */
  path: string;
  title: string;
  description: string;
  /** The repository file the page is generated from, named in its opening notice. */
  source: string;
  /** The page's Markdown below the notice. */
  body: () => string;
}

/** Writes a generated page into the content directory the generator was given as its one
 *  argument: frontmatter, a notice naming the source file and the generator, then the body. */
export function writePage({ path, title, description, source, body }: Page): void {
  const generator = relative(REPO_ROOT, Bun.main);
  const [contentDir, ...extra] = process.argv.slice(2);
  if (contentDir === undefined || extra.length > 0) {
    console.error(`usage: ${generator} <content dir>`);
    process.exit(2);
  }
  if (!existsSync(join(REPO_ROOT, source))) {
    throw new Error(`${generator} names ${source} as its source, and that file does not exist`);
  }
  const markdown = [
    "---",
    `title: ${JSON.stringify(title)}`,
    `description: ${JSON.stringify(description)}`,
    "editUrl: false",
    "---",
    "",
    `> Generated from \`${source}\` by \`${generator}\`. Edit the source, not this page.`,
    "",
    body(),
  ].join("\n");
  const file = join(resolve(contentDir), path);
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, markdown);
  console.log(`wrote ${file}`);
}
