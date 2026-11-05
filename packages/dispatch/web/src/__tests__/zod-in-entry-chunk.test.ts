import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { entryScripts, zodInEntryChunks, zodMarkersIn } from "../zod-in-entry-chunk";

const dirs: string[] = [];

afterEach(() => {
  for (const dir of dirs.splice(0)) {
    rmSync(dir, { recursive: true, force: true });
  }
});

/** A built page: its `index.html` and the files it names, written as the build writes them. */
function build(files: Record<string, string>): string {
  const dir = mkdtempSync(join(tmpdir(), "dispatch-dist-"));
  dirs.push(dir);
  for (const [name, content] of Object.entries(files)) {
    writeFileSync(join(dir, name), content);
  }
  return dir;
}

/** The page as Vite writes it: the entry's module script, and a preload for each chunk it
 *  imports statically. */
function indexHtml(entry: string, preloads: string[] = []): string {
  const links = preloads
    .map((file) => `    <link rel="modulepreload" crossorigin href="/${file}">\n`)
    .join("");
  return `<!doctype html>
<html lang="en">
  <head>
${links}    <script type="module" crossorigin src="/${entry}"></script>
  </head>
  <body><div id="root"></div></body>
</html>
`;
}

test("the scripts the page evaluates first are its module script and every preload, once each", () => {
  const html = indexHtml("entry.js", ["shared.js", "shared.js"]);
  expect(entryScripts(html)).toEqual(["entry.js", "shared.js"]);
});

test("a chunk with no zod in it names no marker", () => {
  expect(zodMarkersIn("const a={_private:1};class Zodiac{}\nexport{a,Zodiac};")).toEqual([]);
});

test("a build whose entry chunk holds no zod passes, whatever a lazy chunk holds", () => {
  const dir = build({
    "index.html": indexHtml("entry.js", ["shared.js"]),
    "entry.js": 'export const build="abc";',
    "shared.js": "export function render(){}",
    // A lazy chunk is downloaded and evaluated when the page asks for it, not in the first task.
    "lazy.js": "class $ZodType{constructor(){this._zod={def:1}}}export{$ZodType};",
  });
  expect(zodInEntryChunks(dir)).toEqual([]);
});

test("zod in the entry chunk is named with every marker and how often it occurs", () => {
  const dir = build({
    "index.html": indexHtml("entry.js"),
    "entry.js": "class $ZodType{constructor(){this._zod={}}}\nconst s=new $ZodType();s._zod.def;",
  });
  expect(zodInEntryChunks(dir)).toEqual([
    {
      file: "entry.js",
      markers: [
        { marker: "_zod", count: 2 },
        { marker: "$ZodType", count: 2 },
      ],
    },
  ]);
});

test("zod in a chunk the entry statically imports is caught too: the browser evaluates it in the same task", () => {
  const dir = build({
    "index.html": indexHtml("entry.js", ["shared.js"]),
    "entry.js": 'import{s}from"/shared.js";export{s};',
    "shared.js": "export const s={_zod:{}};",
  });
  expect(zodInEntryChunks(dir)).toEqual([
    { file: "shared.js", markers: [{ marker: "_zod", count: 1 }] },
  ]);
});

test("a page with no module script is no build to check", () => {
  const dir = build({ "index.html": "<!doctype html><html><body></body></html>" });
  expect(() => zodInEntryChunks(dir)).toThrow("names no module script");
});
