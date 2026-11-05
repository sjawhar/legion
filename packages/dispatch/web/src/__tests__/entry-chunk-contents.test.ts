import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  addressChunks,
  chunksHolding,
  DEFERRED_PARTS,
  type DocumentAddress,
  faultsInBuild,
} from "../entry-chunk-contents";

const dirs: string[] = [];

afterEach(() => {
  for (const dir of dirs.splice(0)) {
    rmSync(dir, { recursive: true, force: true });
  }
});

/** The marker of every part the entry chunk may not hold, so a build fixture can put each in a
 *  chunk of its own and the rules read one fault at a time. */
const markers = Object.fromEntries(DEFERRED_PARTS.map((part) => [part.name, part.marker]));

/** A built page: `index.html` and the chunks it names, written as the build writes them. */
function build({
  entry = "assets/entry.js",
  preloads = [],
  addresses,
  chunks,
}: {
  entry?: string;
  preloads?: string[];
  addresses: Record<string, string[]>;
  chunks: Record<string, string>;
}): string {
  const dir = mkdtempSync(join(tmpdir(), "dispatch-entry-"));
  dirs.push(dir);
  mkdirSync(join(dir, "assets"));
  for (const [name, code] of Object.entries(chunks)) {
    writeFileSync(join(dir, name), code);
  }
  const links = preloads
    .map((file) => `    <link rel="modulepreload" crossorigin href="/${file}">\n`)
    .join("");
  writeFileSync(
    join(dir, "index.html"),
    `<!doctype html>
<html lang="en">
  <head>
${links}    <script>
      window.__dispatchAddressChunks = ${JSON.stringify(addresses)};
    </script>
    <script type="module" crossorigin src="/${entry}"></script>
  </head>
  <body><div id="root"></div></body>
</html>
`
  );
  return dir;
}

/** Every part in its own lazy chunk, which is what the build this check guards looks like. */
function partChunks(): Record<string, string> {
  return Object.fromEntries(
    DEFERRED_PARTS.map((part, index) => [
      `assets/part-${index}.js`,
      `export const part=${JSON.stringify(part.marker)};`,
    ])
  );
}

/** Where each part's chunk ends up, by name, for an address list to preload. */
function chunkOf(part: string): string {
  return `assets/part-${DEFERRED_PARTS.findIndex((candidate) => candidate.name === part)}.js`;
}

/** The chunks a build has to preload at `address`: one per part that address's reader needs. */
function neededAt(address: DocumentAddress): string[] {
  return DEFERRED_PARTS.filter((part) => part.neededBy.includes(address)).map((part) =>
    chunkOf(part.name)
  );
}

/** Both lists of a build that preloads everything it owes, which is what this check passes. */
const preloadsEverything = { document: neededAt("document"), issue: neededAt("issue") };

test("a build with every part in a lazy chunk, and the document address's chunks preloaded, passes", () => {
  const dir = build({
    addresses: preloadsEverything,
    chunks: { "assets/entry.js": 'export const build="abc";', ...partChunks() },
  });
  expect(faultsInBuild(dir)).toEqual([]);
});

test("a part the entry chunk holds is named with the marker that found it", () => {
  const dir = build({
    addresses: preloadsEverything,
    chunks: {
      "assets/entry.js": `export const palette=${JSON.stringify(markers["the search palette"])};`,
      ...partChunks(),
    },
  });
  const faults = faultsInBuild(dir);
  expect(faults.map((fault) => fault.part?.name)).toEqual(["the search palette"]);
  expect(faults[0].fault).toContain("assets/entry.js is evaluated in the page's first task");
  expect(faults[0].fault).toContain(markers["the search palette"]);
});

test("a part in a chunk the entry statically imports is caught too: one task evaluates both", () => {
  const dir = build({
    entry: "assets/entry.js",
    preloads: ["assets/shared.js"],
    addresses: preloadsEverything,
    chunks: {
      "assets/entry.js": 'import"/assets/shared.js";',
      "assets/shared.js": `export const m=${JSON.stringify(markers["the margin"])};`,
      ...partChunks(),
    },
  });
  expect(faultsInBuild(dir).map((fault) => fault.fault)).toEqual([
    expect.stringContaining("assets/shared.js is evaluated in the page's first task") as string,
  ]);
});

test("a marker no chunk holds is a drifted string, not a passing build", () => {
  const chunks = partChunks();
  chunks[chunkOf("the issue page")] = 'export const part="issue-spec-pane";';
  const dir = build({
    addresses: preloadsEverything,
    chunks: { "assets/entry.js": 'export const build="abc";', ...chunks },
  });
  expect(faultsInBuild(dir).map((fault) => fault.fault)).toEqual([
    expect.stringContaining("the string has drifted") as string,
  ]);
});

test("a part a document address needs, in a chunk that address does not preload, is a second wait", () => {
  const dir = build({
    // The project document page is what a project document's address draws, and only the issue
    // address's list is written.
    addresses: { document: [], issue: neededAt("issue") },
    chunks: { "assets/entry.js": 'export const build="abc";', ...partChunks() },
  });
  const faults = faultsInBuild(dir);
  expect(faults.map((fault) => fault.part?.name)).toEqual(["the project document page"]);
  expect(faults[0].fault).toContain("which the document address needs");
  expect(faults[0].fault).toContain("index.html preloads none of them there");
});

test("an address preloading a chunk the build does not hold is a fault of its own", () => {
  const dir = build({
    addresses: { ...preloadsEverything, issue: [...neededAt("issue"), "assets/gone.js"] },
    chunks: { "assets/entry.js": 'export const build="abc";', ...partChunks() },
  });
  expect(faultsInBuild(dir).map((fault) => fault.fault)).toEqual([
    "the issue address preloads assets/gone.js, which this build does not hold",
  ]);
});

test("a page the build wrote no address chunks into is no build to check", () => {
  const dir = mkdtempSync(join(tmpdir(), "dispatch-entry-"));
  dirs.push(dir);
  mkdirSync(join(dir, "assets"));
  writeFileSync(
    join(dir, "index.html"),
    '<!doctype html><html><head><script type="module" src="/assets/entry.js"></script></head></html>'
  );
  expect(() => faultsInBuild(dir)).toThrow("names no __dispatchAddressChunks");
});

test("the chunks holding a marker are every chunk that holds it, in path order", () => {
  const dir = build({
    addresses: { document: [], issue: [] },
    chunks: {
      "assets/entry.js": "export const a=1;",
      "assets/one.js": 'export const m="marker";',
      "assets/two.js": 'export const n="marker";',
    },
  });
  expect(chunksHolding(dir, "marker")).toEqual(["assets/one.js", "assets/two.js"]);
});

test("the address lists are read with their leading slash stripped, as the build writes them", () => {
  expect(
    addressChunks('<script>window.__dispatchAddressChunks = {"issue":["/assets/a.js"]};</script>')
  ).toEqual({ issue: ["assets/a.js"] });
});
