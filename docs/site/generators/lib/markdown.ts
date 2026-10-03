// Shared by the reference generators beside this directory. The build hook runs only the
// generators' own top-level files, never this one.
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

/** Escapes Markdown outside code spans, so `<login>` in a description is text, not an HTML tag.
 *  In a table cell a pipe is escaped inside code spans too: GFM splits a row on every unescaped
 *  pipe before it parses code spans. */
export function inline(text: string, cell = false): string {
  return text
    .split(/(`[^`]*`)/)
    .map((part, index) => {
      if (index % 2 === 0) return part.replace(/[\\*_[\]<>|#]/g, "\\$&");
      return cell ? part.replaceAll("|", "\\|") : part;
    })
    .join("");
}

/** Writes `page` (a path under the content directory, such as `dispatch/reference/api.md`) into
 *  the content directory the generator was given as its one argument. */
export function writePage(generator: string, page: string, markdown: () => string): void {
  const [contentDir, ...extra] = process.argv.slice(2);
  if (contentDir === undefined || extra.length > 0) {
    console.error(`usage: ${generator} <content dir>`);
    process.exit(2);
  }
  const path = join(resolve(contentDir), page);
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, markdown());
  console.log(`wrote ${path}`);
}
